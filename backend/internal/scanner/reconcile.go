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
	// Exclude unavailable sources before a usable file can match one of them.
	if err := s.Catalog.MarkInvalidSizeSources(ctx, s.Drive.ID(), result.Snapshot.InvalidSizeFileIDs); err != nil {
		return err
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
	// Tombstones keep deleted sources from being admitted again. They never
	// suppress a live row that still owns this file.
	id := videoid.ForDrive(s.Drive.Kind(), s.Drive.ID(), entry.ID)
	if len(existing) == 0 {
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
	}

	displayTitle := videoname.TitleFromFileName(entry.Name)
	if displayTitle == "" {
		displayTitle = strings.TrimSpace(entry.Name)
	}
	if len(existing) == 0 {
		assignments, _, err := s.matchTags(ctx, file, displayTitle, "", result)
		if err != nil {
			return err
		}
		return s.insertNew(ctx, file, id, displayTitle, assignments, result)
	}
	for _, row := range existing {
		assignments, tagLookupOK, err := s.matchTags(ctx, file, displayTitle, row.Author, result)
		if err != nil {
			return err
		}
		if err := s.reconcileExisting(ctx, file, displayTitle, row, assignments, tagLookupOK, result); err != nil {
			return err
		}
	}
	return nil
}

// matchTags runs the catalog tag matcher for one video. A lookup failure is
// recorded as an issue and reported through ok=false so callers keep existing
// tags; only context cancellation is returned as an error.
func (s *Scanner) matchTags(ctx context.Context, file File, title, author string, result *Result) (assignments []catalog.TagAssignment, ok bool, err error) {
	assignments, matchErr := s.Catalog.MatchTagAssignments(ctx, title, file.Entry.Name, author, file.DirName, file.AncestorDirNames...)
	if matchErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		result.addIssue(ctx, file, IssueTags, matchErr)
	}
	return assignments, matchErr == nil, ctx.Err()
}

func (s *Scanner) reconcileExisting(
	ctx context.Context,
	file File,
	displayTitle string,
	existing *catalog.Video,
	assignments []catalog.TagAssignment,
	tagLookupOK bool,
	result *Result,
) error {
	entry := file.Entry
	patch := catalog.VideoMetaPatch{ExpectedSource: existing, ResumeAfterInvalidSize: existing.SizeUnavailable && entry.Size > 0}
	hash := dedupe.NormalizeHash(entry.Hash)
	previousHash := existing.ContentHash
	sampled := existing.SampledSHA256
	modTime := catalog.FormatSourceModTime(entry.ModTime)
	// The first timestamp observed for a row is its baseline, not a change.
	modTimeChanged := modTime != "" && existing.SourceModTime != "" && modTime != existing.SourceModTime
	hashComparable := hash != "" && previousHash != ""
	// A different size or provider hash proves new bytes.
	contentChanged := entry.Size != existing.Size || (hashComparable && hash != previousHash)
	// Equal provider hashes confirm the bytes even if the timestamp changed.
	// Without them a new timestamp is only a suspicion: the fingerprint step
	// compares the sampled fingerprint again before any asset is discarded.
	// A row without a fingerprint has nothing to compare and is reset.
	if !contentChanged && !hashComparable && modTimeChanged {
		if existing.SampledSHA256 != "" {
			patch.VerifyContent = true
			sampled = ""
		} else {
			contentChanged = true
		}
	}
	// A reset rewrites the timestamp along with the rest of the observation.
	if contentChanged || modTime != existing.SourceModTime {
		patch.SourceModTime = modTime
	}
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
		SourceModTime:    catalog.FormatSourceModTime(entry.ModTime),
		ParentID:         file.ParentID,
		DirName:          file.DirName,
		AncestorDirIDs:   append([]string(nil), file.AncestorDirIDs...),
		AncestorDirNames: append([]string(nil), file.AncestorDirNames...),
		Title:            displayTitle,
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
	applog.Error(ctx, "File reconciliation failed: "+file.Entry.Name, err, "component", "scanner", "driveId", r.Snapshot.DriveID, "fileId", file.Entry.ID, "stage", string(stage))
}

func metadataPatchSet(patch catalog.VideoMetaPatch) bool {
	return patch.ResumeAfterInvalidSize || patch.ResetContent || patch.VerifyContent || patch.SourceModTime != "" || patch.ContentHash != "" || patch.FileName != "" || patch.ParentIDSet ||
		patch.DirNameSet || patch.AncestorDirIDsSet || patch.AncestorDirNamesSet || patch.TitleSet
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
