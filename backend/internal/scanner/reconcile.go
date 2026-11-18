package scanner

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/dedupe"
	"github.com/nianzhibai/91/backend/internal/videoid"
	"github.com/nianzhibai/91/backend/internal/videoname"
)

func (s *Scanner) reconcile(ctx context.Context, result *Result, progress progressFunc) error {
	if err := validateScanner(s); err != nil {
		return err
	}
	if result.Snapshot.DriveID != s.Drive.ID() || result.Snapshot.DriveKind != s.Drive.Kind() {
		return fmt.Errorf(
			"snapshot source %s/%s does not match scanner source %s/%s",
			result.Snapshot.DriveKind, result.Snapshot.DriveID, s.Drive.Kind(), s.Drive.ID(),
		)
	}
	for _, file := range result.Snapshot.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.reconcileFile(ctx, file, result)
		progress("reconcile", file.DirName)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) reconcileFile(ctx context.Context, file File, result *Result) error {
	entry := file.Entry
	id := videoid.ForDrive(s.Drive.Kind(), s.Drive.ID(), entry.ID)
	deleted, err := s.Catalog.IsDeletedVideoCandidate(
		ctx, id, s.Drive.ID(), entry.ID, entry.Hash, entry.Name, entry.Size,
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		result.addIssue(ctx, file, IssueTombstone, err)
		return nil
	}
	if deleted {
		result.Tombstoned++
		return nil
	}

	parsed := Parse(entry.Name)
	displayTitle := videoname.TitleFromFileName(entry.Name)
	if displayTitle == "" {
		displayTitle = strings.TrimSpace(entry.Name)
	}
	// Migrated rows retain their original public IDs. Only the current
	// provider identity can authorize updating a row from this scan.
	existing, err := s.Catalog.ListVideosByDriveFileID(ctx, s.Drive.ID(), entry.ID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		result.addIssue(ctx, file, IssueLookup, err)
		return nil
	}
	assignments, tagErr := s.Catalog.MatchTagAssignments(ctx, parsed.Title, entry.Name, parsed.Author, file.DirName, file.AncestorDirNames...)
	if tagErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		result.addIssue(ctx, file, IssueTags, tagErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if len(existing) == 0 {
		return s.insertNew(ctx, file, id, displayTitle, parsed.Author, assignments, result)
	}
	for _, row := range existing {
		if err := s.reconcileExisting(ctx, file, displayTitle, parsed.Author, row, assignments, tagErr == nil, result); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) reconcileExisting(
	ctx context.Context,
	file File,
	displayTitle string,
	parsedAuthor string,
	existing *catalog.Video,
	assignments []catalog.TagAssignment,
	tagLookupOK bool,
	result *Result,
) error {
	entry := file.Entry
	patch := catalog.VideoMetaPatch{ExpectedSource: existing}
	hash := dedupe.NormalizeHash(entry.Hash)
	previousHash := existing.ContentHash
	sampled := existing.SampledSHA256
	// Some providers omit hashes from directory listings. The stored evidence
	// remains usable until a positive observation changes the size or hash.
	contentChanged := entry.Size != existing.Size || (hash != "" && previousHash != "" && hash != previousHash)
	if contentChanged {
		patch.ResetContent = true
		patch.Size = entry.Size
		patch.ContentHash = hash
		sampled = ""
	} else if hash != "" && hash != previousHash {
		patch.ContentHash = hash
	} else if hash == "" {
		hash = previousHash
	}
	if existing.ParentID != file.ParentID {
		patch.ParentID = file.ParentID
		patch.ParentIDSet = true
	}
	if existing.DirName != file.DirName {
		patch.DirName = file.DirName
		patch.DirNameSet = true
	}
	if !slices.Equal(existing.AncestorDirIDs, file.AncestorDirIDs) {
		patch.AncestorDirIDs = append([]string(nil), file.AncestorDirIDs...)
		patch.AncestorDirIDsSet = true
	}
	if !slices.Equal(existing.AncestorDirNames, file.AncestorDirNames) {
		patch.AncestorDirNames = append([]string(nil), file.AncestorDirNames...)
		patch.AncestorDirNamesSet = true
	}
	if entry.Name != "" && existing.FileName != entry.Name {
		patch.FileName = entry.Name
		patch.Author = parsedAuthor
		patch.AuthorSet = true
	}
	if existing.Title != displayTitle {
		patch.Title = displayTitle
		patch.TitleSet = true
	}
	if metadataPatchSet(patch) {
		if err := s.Catalog.UpdateVideoMeta(ctx, existing.ID, patch); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			result.addIssue(ctx, file, IssueMetadata, err)
			return nil
		}
		result.Updated++
	}
	if tagLookupOK {
		if _, err := s.Catalog.ReplaceAutoVideoTags(ctx, existing.ID, assignments); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			result.addIssue(ctx, file, IssueTags, err)
		}
	}

	source := &catalog.Video{
		ID: existing.ID, DriveID: s.Drive.ID(), ContentHash: hash, SampledSHA256: sampled,
		FileID: entry.ID, FileName: entry.Name, Title: displayTitle, Size: entry.Size,
		ParentID: file.ParentID, DirName: file.DirName,
		AncestorDirIDs:   append([]string(nil), file.AncestorDirIDs...),
		AncestorDirNames: append([]string(nil), file.AncestorDirNames...),
	}
	duplicate, err := s.Catalog.FindScannedVideoDuplicate(ctx, source, result.Snapshot.SeenFileIDs)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		result.addIssue(ctx, file, IssueDuplicate, err)
		return nil
	}
	if duplicate != nil {
		if err := s.Catalog.RecordScannedDuplicate(ctx, source, duplicate); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			result.addIssue(ctx, file, IssueDuplicate, err)
			return nil
		}
		result.Duplicates++
		return nil
	}
	return ctx.Err()
}

func (s *Scanner) insertNew(
	ctx context.Context,
	file File,
	id string,
	displayTitle string,
	parsedAuthor string,
	assignments []catalog.TagAssignment,
	result *Result,
) error {
	entry := file.Entry
	if err := ctx.Err(); err != nil {
		return err
	}

	now := time.Now()
	video := &catalog.Video{
		ID:               id,
		DriveID:          s.Drive.ID(),
		FileID:           entry.ID,
		FileName:         entry.Name,
		ContentHash:      entry.Hash,
		ParentID:         file.ParentID,
		DirName:          file.DirName,
		AncestorDirIDs:   append([]string(nil), file.AncestorDirIDs...),
		AncestorDirNames: append([]string(nil), file.AncestorDirNames...),
		Title:            displayTitle,
		Author:           parsedAuthor,
		Ext:              strings.TrimPrefix(strings.ToLower(path.Ext(entry.Name)), "."),
		Size:             entry.Size,
		PreviewStatus:    "pending",
		PublishedAt:      now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	inserted, err := s.Catalog.InsertScannedVideo(ctx, video, result.Snapshot.SeenFileIDs)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		result.addIssue(ctx, file, IssueUpsert, err)
		return nil
	}
	if !inserted {
		result.Duplicates++
		return nil
	}
	if len(assignments) > 0 {
		if _, err := s.Catalog.ReplaceAutoVideoTags(ctx, video.ID, assignments); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			result.addIssue(ctx, file, IssueTags, err)
		} else {
			video.Tags = assignmentLabels(assignments)
		}
	}
	result.Stats.Added++
	result.NewVideos = append(result.NewVideos, video)
	if s.OnNewVideo != nil {
		s.OnNewVideo(video)
	}
	return ctx.Err()
}

func (r *Result) addIssue(ctx context.Context, file File, stage IssueStage, err error) {
	issue := Issue{
		Stage:  stage,
		DirID:  file.ParentID,
		FileID: file.Entry.ID,
		Name:   file.Entry.Name,
		Err:    err,
	}
	r.Issues = append(r.Issues, issue)
	r.Stats.Errors++
	applog.Error(ctx, "File reconciliation failed: "+file.Entry.Name, err, applog.Fields{Component: "scanner", DriveID: r.Snapshot.DriveID, FileID: file.Entry.ID, Stage: string(stage)})
}

func metadataPatchSet(patch catalog.VideoMetaPatch) bool {
	return patch.ResetContent || patch.ContentHash != "" || patch.FileName != "" || patch.ParentIDSet ||
		patch.DirNameSet || patch.AncestorDirIDsSet || patch.AncestorDirNamesSet || patch.TitleSet || patch.AuthorSet
}

func assignmentLabels(assignments []catalog.TagAssignment) []string {
	labels := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		labels = append(labels, assignment.Label)
	}
	return labels
}

func videoIDFilePart(fileID string) string {
	return videoid.FilePart(fileID)
}
