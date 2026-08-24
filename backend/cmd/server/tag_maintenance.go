package main

import (
	"context"
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/video-site/backend/internal/api"
	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives/scriptcrawler"
	"github.com/video-site/backend/internal/mediasim"
)

const (
	tagRetagUpgradeMarker        = "tags.retag.v2_done"
	tagMaintenanceLastRunSetting = "tags.maintenance.last_run_ms"
	tagRetagBatchSize            = 500
	tagClusterTitleThreshold     = 0.85
	tagMaintenanceStartupRetries = 8
)

func (a *App) beginTagJob(kind string) bool {
	if a == nil || a.cat == nil {
		return false
	}
	a.tagJobMu.Lock()
	defer a.tagJobMu.Unlock()
	if a.tagJobState.Running {
		return false
	}
	a.tagJobState = api.TagJobStatus{
		State:     "running",
		Running:   true,
		Kind:      kind,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	return true
}

func (a *App) finishTagJob(state string, err error) {
	a.tagJobMu.Lock()
	defer a.tagJobMu.Unlock()
	a.tagJobState.State = state
	a.tagJobState.Running = false
	a.tagJobState.LastFinishedAt = time.Now().Format(time.RFC3339)
	if err != nil {
		a.tagJobState.LastError = err.Error()
	}
}

func (a *App) tagJobStatus() api.TagJobStatus {
	if a == nil {
		return api.TagJobStatus{State: "idle"}
	}
	a.tagJobMu.Lock()
	status := a.tagJobState
	a.tagJobMu.Unlock()
	if status.State == "" {
		status.State = "idle"
	}
	return status
}

func (a *App) startTagRetag(ctx context.Context) bool {
	return a.startTagRetagInternal(ctx, false)
}

func (a *App) startTagRetagInternal(ctx context.Context, writeUpgradeMarker bool) bool {
	if !a.beginTagJob("retag") {
		return false
	}
	go a.runTagRetag(ctx, writeUpgradeMarker)
	return true
}

func (a *App) startPostStartupTagMaintenance(ctx context.Context) {
	if a == nil || a.cat == nil {
		return
	}
	if !a.beginTagJob("retag") {
		log.Printf("[tag-retag] post-startup maintenance not started because another tag job is running")
		return
	}
	go a.runPostStartupTagMaintenance(ctx)
}

func (a *App) runPostStartupTagMaintenance(ctx context.Context) {
	a.tagMaintenanceMu.Lock()
	defer a.tagMaintenanceMu.Unlock()

	total, err := a.cat.CountVideosForRetag(ctx, 0)
	if err != nil {
		a.finishTagJob("failed", err)
		return
	}
	a.tagJobMu.Lock()
	a.tagJobState.Total = total
	a.tagJobMu.Unlock()

	log.Printf("[tag-retag] post-startup tag maintenance started")
	if err := a.runPostStartupTagMaintenanceWithRetry(ctx); err != nil {
		state := "failed"
		if ctx.Err() != nil {
			state = "canceled"
		}
		a.finishTagJob(state, err)
		log.Printf("[tag-retag] post-startup tag maintenance failed: %v", err)
		return
	}
	a.tagJobMu.Lock()
	a.tagJobState.Processed = total
	a.tagJobMu.Unlock()
	a.finishTagJob("completed", nil)
	log.Printf("[tag-retag] post-startup tag maintenance completed")
}

func (a *App) runPostStartupTagMaintenanceWithRetry(ctx context.Context) error {
	var err error
	for attempt := 1; attempt <= tagMaintenanceStartupRetries; attempt++ {
		if err = a.runPostStartupTagMaintenanceAttempt(ctx); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isTransientSQLiteLock(err) || attempt == tagMaintenanceStartupRetries {
			return err
		}
		delay := time.Duration(attempt) * 2 * time.Second
		log.Printf("[tag-retag] post-startup tag maintenance waiting for database lock to clear (attempt %d/%d): %v", attempt, tagMaintenanceStartupRetries, err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func (a *App) runPostStartupTagMaintenanceAttempt(ctx context.Context) error {
	if err := a.cat.RunPostStartupTagMaintenance(ctx); err != nil {
		return err
	}
	if err := a.ensureAllScriptCrawlerNameTags(ctx); err != nil {
		log.Printf("[tag-retag] ensure crawler name tags: %v", err)
	}
	return a.cat.SetSetting(ctx, tagRetagUpgradeMarker, "1")
}

func isTransientSQLiteLock(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked")
}

func (a *App) runTagRetag(ctx context.Context, writeUpgradeMarker bool) {
	a.tagMaintenanceMu.Lock()
	defer a.tagMaintenanceMu.Unlock()
	a.runTagRetagLocked(ctx, writeUpgradeMarker, true)
}

func (a *App) runTagRetagLocked(ctx context.Context, writeUpgradeMarker bool, resetGeneratedState bool) {
	total, err := a.cat.CountVideosForRetag(ctx, 0)
	if err != nil {
		a.finishTagJob("failed", err)
		return
	}
	a.tagJobMu.Lock()
	a.tagJobState.Total = total
	a.tagJobMu.Unlock()

	if err := ctx.Err(); err != nil {
		a.finishTagJob("canceled", err)
		return
	}
	if err := a.cat.RunPostStartupTagMaintenance(ctx); err != nil {
		a.finishTagJob("failed", err)
		return
	}
	if err := a.ensureAllScriptCrawlerNameTags(ctx); err != nil {
		log.Printf("[tag-retag] ensure crawler name tags: %v", err)
	}
	if _, err := a.cat.PruneUnreferencedTags(ctx); err != nil {
		a.finishTagJob("failed", err)
		return
	}
	if writeUpgradeMarker {
		if err := a.cat.SetSetting(ctx, tagRetagUpgradeMarker, "1"); err != nil {
			a.finishTagJob("failed", err)
			return
		}
	}
	a.tagJobMu.Lock()
	a.tagJobState.Processed = total
	a.tagJobMu.Unlock()
	a.finishTagJob("completed", nil)
}

func (a *App) ensureAllScriptCrawlerNameTags(ctx context.Context) error {
	drives, err := a.cat.ListDrives(ctx)
	if err != nil {
		return err
	}
	for _, drive := range drives {
		if drive == nil || drive.Kind != scriptcrawler.Kind {
			continue
		}
		tagName := strings.TrimSpace(drive.Name)
		if tagName == "" {
			tagName = strings.TrimSpace(drive.ID)
		}
		if tagName == "" {
			continue
		}
		prefix := scriptcrawler.BuildVideoID(drive.ID, "")
		if _, err := a.cat.EnsureCrawlerTagForVideoIDPrefix(ctx, prefix, tagName); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) runNightlyTagMaintenance(ctx context.Context) error {
	if a == nil || a.cat == nil {
		return nil
	}
	a.tagMaintenanceMu.Lock()
	defer a.tagMaintenanceMu.Unlock()

	var stepErrors []error
	runStep := func(name string, fn func() error) {
		if ctx.Err() != nil {
			return
		}
		log.Printf("[tag-maintenance] %s", name)
		if err := fn(); err != nil {
			log.Printf("[tag-maintenance] %s: %v", name, err)
			stepErrors = append(stepErrors, errors.New(name+": "+err.Error()))
		}
	}

	runStep("refresh existing tag matches", func() error {
		if err := a.cat.RunPostStartupTagMaintenance(ctx); err != nil {
			return err
		}
		return a.cat.SetSetting(ctx, tagMaintenanceLastRunSetting, strconv.FormatInt(time.Now().UnixMilli(), 10))
	})
	runStep("crawler name tags", func() error {
		return a.ensureAllScriptCrawlerNameTags(ctx)
	})
	runStep("prune retired generated tags", func() error {
		_, err := a.cat.PruneUnreferencedTags(ctx)
		return err
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.Join(stepErrors...)
}

type tagClusterCandidate struct {
	video   *catalog.Video
	keys    []string
	qgrams  map[string]struct{}
	buckets []string
}

func collectTagTitleClusters(videos []*catalog.Video) [][]*catalog.Video {
	candidates := make([]tagClusterCandidate, 0, len(videos))
	for _, video := range videos {
		if video == nil || strings.TrimSpace(video.ID) == "" || strings.TrimSpace(video.Title) == "" {
			continue
		}
		keys := mediasim.TitleKeys(video.Title)
		if len(keys) == 0 {
			continue
		}
		buckets := mediasim.TitlePrefixBuckets(keys, 12)
		if len(buckets) == 0 {
			continue
		}
		candidates = append(candidates, tagClusterCandidate{
			video:   video,
			keys:    keys,
			qgrams:  mediasim.TitleQGrams(keys, 4),
			buckets: buckets,
		})
	}
	if len(candidates) < 2 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].video.ID < candidates[j].video.ID
	})

	sets := newTagClusterDisjointSet(len(candidates))
	bucketIndex := make(map[string][]int)
	seenPairs := make(map[uint64]struct{})
	for i, right := range candidates {
		for _, bucket := range right.buckets {
			for _, j := range bucketIndex[bucket] {
				pairKey := tagClusterPairKey(i, j)
				if _, ok := seenPairs[pairKey]; ok {
					continue
				}
				seenPairs[pairKey] = struct{}{}
				left := candidates[j]
				if !tagClusterTitlePrefilter(left, right) {
					continue
				}
				if mediasim.TitleSimilarity(left.video.Title, right.video.Title) >= tagClusterTitleThreshold {
					sets.union(i, j)
				}
			}
		}
		for _, bucket := range right.buckets {
			bucketIndex[bucket] = append(bucketIndex[bucket], i)
		}
	}

	groupByRoot := make(map[int][]*catalog.Video)
	for i, candidate := range candidates {
		root := sets.find(i)
		groupByRoot[root] = append(groupByRoot[root], candidate.video)
	}
	roots := make([]int, 0, len(groupByRoot))
	for root, group := range groupByRoot {
		if len(group) >= 2 {
			roots = append(roots, root)
		}
	}
	sort.Ints(roots)
	groups := make([][]*catalog.Video, 0, len(roots))
	for _, root := range roots {
		groups = append(groups, groupByRoot[root])
	}
	return groups
}

func tagClusterTitlePrefilter(left, right tagClusterCandidate) bool {
	if !mediasim.TitleLengthCouldReachThreshold(left.keys, right.keys, tagClusterTitleThreshold) {
		return false
	}
	return mediasim.QGramContainment(left.qgrams, right.qgrams) >= 0.45
}

func (a *App) propagateTagsAcrossTitleClusters(ctx context.Context) (int, error) {
	return 0, nil
}

func (a *App) propagateTagsAcrossTitleClustersRetired(ctx context.Context) (int, error) {
	videos, err := a.cat.ListVideoMaintenanceCandidates(ctx)
	if err != nil {
		return 0, err
	}
	manualIDs, err := a.cat.ListManualTagVideoIDs(ctx)
	if err != nil {
		return 0, err
	}
	groups := collectTagTitleClusters(videos)
	addedTotal := 0
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return addedTotal, err
		}
		votes := make(map[string]int)
		canonical := make(map[string]string)
		for _, video := range group {
			seen := make(map[string]bool)
			for _, label := range video.Tags {
				label = strings.TrimSpace(label)
				key := strings.ToLower(label)
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
				votes[key]++
				if canonical[key] == "" {
					canonical[key] = label
				}
			}
		}
		eligible := make([]string, 0)
		for key, count := range votes {
			if count >= 2 && float64(count)/float64(len(group)) >= 0.5 {
				eligible = append(eligible, key)
			}
		}
		sort.Strings(eligible)
		if len(eligible) == 0 {
			continue
		}
		for _, video := range group {
			if manualIDs[video.ID] {
				continue
			}
			existing := make(map[string]bool, len(video.Tags))
			for _, label := range video.Tags {
				existing[strings.ToLower(strings.TrimSpace(label))] = true
			}
			assignments := make([]catalog.TagAssignment, 0, len(eligible))
			for _, key := range eligible {
				if existing[key] {
					continue
				}
				assignments = append(assignments, catalog.TagAssignment{
					Label:    canonical[key],
					Source:   "propagated",
					Evidence: "标题聚类",
				})
			}
			added, err := a.cat.AddVideoTagAssignments(ctx, video.ID, assignments)
			if err != nil {
				return addedTotal, err
			}
			addedTotal += added
		}
	}
	return addedTotal, nil
}
