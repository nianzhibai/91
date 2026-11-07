package preview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

// Completed segments and local output are usable; the selected stage blocks
// until its command context stops it. exec avoids leaving a child holding the
// command's output pipes open after cancellation.
func newTimeoutTeaserGenerator(t *testing.T, okSegments int, blockedStage string) *Generator {
	t.Helper()
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "ffmpeg")
	script := fmt.Sprintf(`#!/bin/sh
out=""
for arg in "$@"; do out="$arg"; done
case " $* " in
  *" -f concat "*)
    : > %[1]q/concat-started
    if [ %[3]q = concat ]; then exec sleep 30; fi
    printf 'concat-output' > "$out"
    exit 0
    ;;
esac
count=$(cat %[1]q/segment-count 2>/dev/null || echo 0)
count=$((count + 1))
printf '%%s' "$count" > %[1]q/segment-count
if [ "$count" -gt %[2]d ]; then
  : > %[1]q/segments-blocked
  if [ %[3]q = refresh ]; then
    printf 'Server returned 403 Forbidden' >&2
    exit 1
  fi
  exec sleep 30
fi
printf 'segment-output' > "$out"
`, dir, okSegments, blockedStage)
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ffprobe := filepath.Join(dir, "ffprobe")
	probeScript := fmt.Sprintf(`#!/bin/sh
input=""
for arg in "$@"; do input="$arg"; done
case "$input" in
  *teaser-seg-*) ;;
  *)
    : > %[1]q/validation-started
    if [ %[2]q = validation ]; then exec sleep 30; fi
    ;;
esac
printf '%%s' '{"streams":[{"codec_type":"video","duration":"3.0"}],"format":{"duration":"3.0"}}'
`, dir, blockedStage)
	if err := os.WriteFile(ffprobe, []byte(probeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return New(Config{FFmpegPath: ffmpeg, FFprobePath: ffprobe, LocalDir: filepath.Join(dir, "preview")})
}

func assertTeaserTemporaryFiles(t *testing.T, gen *Generator, output string) {
	t.Helper()
	entries, err := os.ReadDir(gen.cfg.LocalDir)
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	if output != "" {
		want = 1
	}
	if len(entries) != want || (want == 1 && entries[0].Name() != filepath.Base(output)) {
		t.Fatalf("remaining files = %v, want only output %q", entries, output)
	}
}

func TestGenerateFinishesUsableSegmentsAfterSegmentBudgetExpires(t *testing.T) {
	for _, tc := range []struct {
		name       string
		segments   int
		duration   float64
		wantOutput string
	}{
		{name: "three long-video segments", segments: 3, duration: 204, wantOutput: "concat-output"},
		{name: "two long-video segments", segments: 2, duration: 204, wantOutput: "concat-output"},
		{name: "one long-video segment", segments: 1, duration: 204},
		{name: "no long-video segments", segments: 0, duration: 204},
		{name: "one short-video segment", segments: 1, duration: 20, wantOutput: "segment-output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gen := newTimeoutTeaserGenerator(t, tc.segments, "segments")
			gen.segmentBudget = time.Second
			store, logs := capturePreviewLogs(t)
			fields := applog.Fields{Component: "preview", TaskID: "budget-task", DriveID: "drive-1", VideoID: "video-1", FileID: "file-1"}
			ctx := applog.WithFields(context.Background(), fields)
			refreshes := 0
			path, err := gen.GenerateWithLinkRefresh(ctx, &drives.StreamLink{URL: "source.mp4"}, tc.duration,
				func(context.Context) (*drives.StreamLink, error) {
					refreshes++
					return nil, errors.New("unexpected refresh after segment budget expired")
				})
			if tc.wantOutput != "" {
				if err != nil {
					t.Fatalf("finish usable segments: %v", err)
				}
				body, err := os.ReadFile(path)
				if err != nil || string(body) != tc.wantOutput {
					t.Fatalf("output = %q, err=%v, want %q", body, err, tc.wantOutput)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) ||
				!strings.Contains(err.Error(), fmt.Sprintf("only generated %d/4 teaser segments", tc.segments)) {
				t.Fatalf("error = %v, want insufficient segments caused by deadline", err)
			}
			if refreshes != 0 {
				t.Fatalf("refreshes = %d after segment budget expired", refreshes)
			}
			count, err := os.ReadFile(filepath.Join(filepath.Dir(gen.cfg.FFmpegPath), "segment-count"))
			if err != nil || string(count) != strconv.Itoa(tc.segments+1) {
				t.Fatalf("segment attempts = %s, err=%v, want %d", count, err, tc.segments+1)
			}
			if !strings.Contains(logs.String(), "teaser segment budget exhausted") {
				t.Fatalf("missing segment budget log: %s", logs.String())
			}
			if tc.duration >= 30 && tc.wantOutput != "" &&
				!strings.Contains(logs.String(), fmt.Sprintf("degraded teaser accepted segments=%d/4", tc.segments)) {
				t.Fatalf("missing explicit degradation log: %s", logs.String())
			}
			snapshot, err := store.Query(ctx, applog.Query{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			wantEntries := 1
			if tc.duration >= 30 && tc.wantOutput != "" {
				wantEntries = 2
			}
			if len(snapshot.Entries) != wantEntries {
				t.Fatalf("entries = %+v, want %d warnings", snapshot.Entries, wantEntries)
			}
			fields.Stage = "segments"
			for _, entry := range snapshot.Entries {
				if entry.Level != applog.LevelWarning || entry.Fields != fields ||
					!strings.Contains(entry.Error, "context deadline exceeded") {
					t.Fatalf("segment diagnostic = %+v, want warning with task context and cause", entry)
				}
			}
			assertTeaserTemporaryFiles(t, gen, path)
		})
	}
}

func TestGenerateFinishesSegmentsWhenLinkRefreshUsesRemainingBudget(t *testing.T) {
	gen := newTimeoutTeaserGenerator(t, 3, "refresh")
	gen.segmentBudget = time.Second
	refreshes := 0
	path, err := gen.GenerateWithLinkRefresh(context.Background(), &drives.StreamLink{URL: "source.mp4"}, 204,
		func(ctx context.Context) (*drives.StreamLink, error) {
			refreshes++
			<-ctx.Done()
			return nil, ctx.Err()
		})
	if err != nil {
		t.Fatalf("finish segments after refresh deadline: %v", err)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	assertTeaserTemporaryFiles(t, gen, path)
}

func TestGenerateFinishBudgetCoversConcatAndValidation(t *testing.T) {
	for _, tc := range []struct{ stage, wantError string }{
		{stage: "concat", wantError: "ffmpeg concat"},
		{stage: "validation", wantError: "ffprobe teaser"},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			gen := newTimeoutTeaserGenerator(t, 4, tc.stage)
			gen.finishTimeout = 100 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			path, err := gen.Generate(ctx, &drives.StreamLink{URL: "source.mp4"}, 204)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want %s deadline", err, tc.wantError)
			}
			if ctx.Err() != nil {
				t.Fatal("finishing did not stop within its own budget")
			}
			assertTeaserTemporaryFiles(t, gen, path)
		})
	}
}

func TestGenerateCancellationStopsEveryStageWithoutDegradation(t *testing.T) {
	for _, tc := range []struct{ stage, marker string }{
		{stage: "segments", marker: "segments-blocked"},
		{stage: "concat", marker: "concat-started"},
		{stage: "validation", marker: "validation-started"},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			okSegments := 4
			if tc.stage == "segments" {
				okSegments = 3
			}
			gen := newTimeoutTeaserGenerator(t, okSegments, tc.stage)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			var path string
			var err error
			go func() {
				defer close(done)
				path, err = gen.Generate(ctx, &drives.StreamLink{URL: "source.mp4"}, 204)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("generation did not stop after cancellation")
				}
			})
			marker := filepath.Join(filepath.Dir(gen.cfg.FFmpegPath), tc.marker)
			waitForDispatch(t, func() bool {
				_, err := os.Stat(marker)
				return err == nil
			})
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("generation did not stop after cancellation")
			}
			if path != "" || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled generation = %q, %v", path, err)
			}
			if tc.stage == "segments" {
				if _, err := os.Stat(filepath.Join(filepath.Dir(gen.cfg.FFmpegPath), "concat-started")); !os.IsNotExist(err) {
					t.Fatalf("cancellation started degraded concat: %v", err)
				}
			}
			assertTeaserTemporaryFiles(t, gen, path)
		})
	}
}
