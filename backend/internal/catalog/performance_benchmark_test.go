package catalog

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const publicReadBenchmarkVideoCount = 20_000

var publicReadBenchmarkWriteSequence atomic.Int64

// BenchmarkCatalogPublicReads20K keeps the performance investigation in
// docs/PERFORMANCE_OPTIMIZATION.md reproducible. Run it with:
//
//	go test ./internal/catalog -run '^$' -bench BenchmarkCatalogPublicReads20K -benchmem -count 5
//
// The fixture is deterministic: every video has all three deduplication keys,
// every twentieth video has the benchmark tag, and every thumbnail is ready.
func BenchmarkCatalogPublicReads20K(b *testing.B) {
	ctx := context.Background()
	cat, ids := newPublicReadBenchmarkCatalog(b, publicReadBenchmarkVideoCount)

	b.Run("list_latest_with_total", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := cat.ListVideos(ctx, ListParams{
				Sort:                  "latest",
				PreferReadyThumbnails: true,
				Page:                  1,
				PageSize:              24,
			}); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("list_latest_without_total", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := cat.ListVideos(ctx, ListParams{
				Sort:                  "latest",
				PreferReadyThumbnails: true,
				SkipTotal:             true,
				Page:                  1,
				PageSize:              24,
			}); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("list_tag_with_total", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := cat.ListVideos(ctx, ListParams{
				Tag:      "benchmark",
				Sort:     "latest",
				Page:     1,
				PageSize: 24,
			}); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("list_tag_without_total_related_pool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := cat.ListVideos(ctx, ListParams{
				Tag:                "benchmark",
				Sort:               "latest",
				ThumbnailReadyOnly: true,
				SkipTotal:          true,
				Page:               1,
				PageSize:           30,
			}); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("list_global_without_total_related_pool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := cat.ListVideos(ctx, ListParams{
				Sort:               "latest",
				ThumbnailReadyOnly: true,
				SkipTotal:          true,
				Page:               1,
				PageSize:           200,
			}); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("list_keyword_sparse_with_total", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := cat.ListVideos(ctx, ListParams{
				Keyword:  "Video 0199",
				Sort:     "latest",
				Page:     1,
				PageSize: 24,
			}); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("count_visible", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := cat.CountVisibleVideos(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("latest_96", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := cat.ListVisibleVideoIDsLatest(ctx, 96); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("thumbnail_readiness", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := cat.ListVisibleVideoIDsByThumbnailReadiness(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("visible_chunk_32", func(b *testing.B) {
		b.ReportAllocs()
		chunk := ids[:32]
		for i := 0; i < b.N; i++ {
			if _, err := cat.VisibleVideosByIDs(ctx, chunk); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("upsert_existing_unchanged_dedup_keys", func(b *testing.B) {
		video, err := cat.GetVideo(ctx, ids[0])
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := cat.UpsertVideo(ctx, video); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("upsert_new_unique", func(b *testing.B) {
		sequenceEnd := publicReadBenchmarkWriteSequence.Add(int64(b.N))
		sequenceStart := sequenceEnd - int64(b.N)
		createdAt := time.UnixMilli(1_900_000_000_000)
		videos := make([]Video, b.N)
		for i := range videos {
			sequence := sequenceStart + int64(i)
			id := fmt.Sprintf("benchmark-write-%012d", sequence)
			videos[i] = Video{
				ID:                 id,
				DriveID:            "benchmark-drive",
				FileID:             id + "-file",
				FileName:           id + ".mp4",
				ContentHash:        id + "-hash",
				SampledSHA256:      id + "-sample",
				FingerprintStatus:  "ready",
				Title:              id,
				Size:               2_000_000 + sequence,
				Ext:                "mp4",
				ThumbnailURL:       "/p/thumb/" + id,
				ThumbnailUpdatedAt: createdAt,
				PublishedAt:        createdAt,
				CreatedAt:          createdAt,
			}
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := range videos {
			if err := cat.UpsertVideo(ctx, &videos[i]); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func newPublicReadBenchmarkCatalog(b *testing.B, count int) (*Catalog, []string) {
	b.Helper()
	cat, err := Open(filepath.Join(b.TempDir(), "catalog.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := cat.Close(); err != nil {
			b.Error(err)
		}
	})

	ctx := context.Background()
	tx, err := cat.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()

	now := int64(1_800_000_000_000)
	result, err := tx.ExecContext(ctx, `
INSERT INTO tags (label, aliases, match_rules, source, created_at, updated_at)
VALUES ('benchmark', '[]', '{}', 'user', ?, ?)
`, now, now)
	if err != nil {
		b.Fatal(err)
	}
	tagID, err := result.LastInsertId()
	if err != nil {
		b.Fatal(err)
	}

	insertVideo, err := tx.PrepareContext(ctx, `
INSERT INTO videos (
  id, drive_id, file_id, file_name, content_hash, sampled_sha256,
  fingerprint_status, title, author, tags, duration_seconds, size_bytes,
  ext, thumbnail_url, thumbnail_updated_at, thumbnail_status,
  preview_status, published_at, created_at, updated_at
)
VALUES (?, 'benchmark-drive', ?, ?, ?, ?, 'ready', ?, 'benchmark', ?, 600, ?,
        'mp4', ?, ?, 'ready', 'pending', ?, ?, ?)
`)
	if err != nil {
		b.Fatal(err)
	}
	defer insertVideo.Close()

	insertTag, err := tx.PrepareContext(ctx, `
INSERT INTO video_tags (video_id, tag_id, source, evidence, created_at)
VALUES (?, ?, 'manual', 'benchmark fixture', ?)
`)
	if err != nil {
		b.Fatal(err)
	}
	defer insertTag.Close()

	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("video-%06d", i)
		ids = append(ids, id)
		publishedAt := now - int64(i*1_000)
		tags := "[]"
		if i%20 == 0 {
			tags = `["benchmark"]`
		}
		if _, err := insertVideo.ExecContext(
			ctx,
			id,
			fmt.Sprintf("file-%06d", i),
			fmt.Sprintf("video-%06d.mp4", i),
			fmt.Sprintf("hash-%06d", i),
			fmt.Sprintf("sample-%06d", i),
			fmt.Sprintf("Benchmark Video %06d", i),
			tags,
			int64(1_000_000+i),
			"/p/thumb/"+id,
			publishedAt,
			publishedAt,
			publishedAt,
			publishedAt,
		); err != nil {
			b.Fatal(err)
		}
		if i%20 == 0 {
			if _, err := insertTag.ExecContext(ctx, id, tagID, publishedAt); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return cat, ids
}
