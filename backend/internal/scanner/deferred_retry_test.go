package scanner

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/url"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

type observedDiscoverySource struct {
	*discoveryTestSource
	order      []string
	beforeList func(context.Context, string) error
}

func (d *observedDiscoverySource) List(ctx context.Context, dirID string) ([]drives.Entry, error) {
	d.order = append(d.order, dirID)
	if d.beforeList != nil {
		if err := d.beforeList(ctx, dirID); err != nil {
			return nil, err
		}
	}
	return d.discoveryTestSource.List(ctx, dirID)
}

func TestDiscoveryDefersTransientDirectoryErrorsUntilAfterTraversal(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"network timeout", &url.Error{Op: "Get", URL: "https://example.test", Err: &net.DNSError{IsTimeout: true}}},
		{"request timeout", context.DeadlineExceeded},
		{"connection reset", syscall.ECONNRESET},
		{"broken pipe", syscall.EPIPE},
		{"EOF", io.EOF},
		{"truncated response", io.ErrUnexpectedEOF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := &observedDiscoverySource{discoveryTestSource: &discoveryTestSource{
				entries: map[string][]drives.Entry{
					"root": {
						{ID: "broken", Name: "Same name", IsDir: true},
						{ID: "broken", Name: "Same name", IsDir: true},
						{ID: "other", Name: "Same name", IsDir: true},
						{ID: "healthy", Name: "Healthy", IsDir: true},
					},
					"broken":  {{ID: "recovered", Name: "recovered.mp4", Size: 123}},
					"other":   {{ID: "other-file", Name: "other.mp4", Size: 234}},
					"healthy": {{ID: "healthy-file", Name: "healthy.mp4", Size: 345}},
				},
				errorSequences: map[string][]error{
					"broken": {tt.err, tt.err, tt.err},
					"other":  {tt.err, tt.err, tt.err},
				},
				listCalls: map[string]int{},
			}}
			scan := New(nil, source, []string{".mp4"}, nil, nil)
			var waits []time.Duration
			scan.RetryWait = func(ctx context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				return ctx.Err()
			}
			scan.OnProgress = func(stats Stats) {
				if stats.Errors != 0 {
					t.Errorf("pending/recovered errors counted as final: %d", stats.Errors)
				}
			}
			snapshot, stats, err := scan.Discover(context.Background(), "")
			if err != nil || !snapshot.Complete() || !snapshot.PresenceAuthoritative() || len(snapshot.FailedDirIDs) != 0 {
				t.Fatalf("recovered discovery: snapshot=%+v err=%v", snapshot, err)
			}
			wantOrder := []string{"root", "broken", "broken", "broken", "other", "other", "other", "healthy", "broken", "other"}
			if !reflect.DeepEqual(source.order, wantOrder) {
				t.Fatalf("directory reads = %v, want %v", source.order, wantOrder)
			}
			if stats.Scanned != 3 || stats.Errors != 0 || len(snapshot.Files) != 3 || len(snapshot.SeenFileIDs) != 3 {
				t.Fatalf("recovered files/stats = %v / %+v", snapshot.Files, stats)
			}
			if !reflect.DeepEqual(waits, []time.Duration{0, time.Second, 0, time.Second}) {
				t.Fatalf("retry waits = %v", waits)
			}
		})
	}
}

func TestScanFinalDirectoryPassPreservesAncestryAndReconcilesOnce(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if _, err := cat.EnsureTag(ctx, "旅行", "user"); err != nil {
		t.Fatal(err)
	}
	source := &observedDiscoverySource{discoveryTestSource: &discoveryTestSource{
		entries: map[string][]drives.Entry{
			"root": {
				{ID: "parent", Name: "旅行", IsDir: true},
				{ID: "healthy", Name: "healthy.mp4", Size: 123},
			},
			"parent": {{ID: "broken", Name: "Album", IsDir: true}},
			"broken": {
				{ID: "child", Name: "Videos", IsDir: true},
				{ID: "root", IsDir: true}, // Cycle must not reopen successful directories.
				{ID: "excluded", Name: "Excluded", IsDir: true},
			},
			"child": {{ID: "recovered", Name: "recovered.mp4", Size: 234}},
		},
		errorSequences: map[string][]error{"broken": {io.EOF, io.EOF, io.EOF}},
		listCalls:      map[string]int{},
	}}
	source.beforeList = func(ctx context.Context, dirID string) error {
		if dirID == "broken" && source.listCalls[dirID] == 3 {
			if _, err := cat.GetVideo(ctx, "fake-drive-healthy"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("catalog was reconciled before final discovery pass: %v", err)
			}
		}
		return nil
	}
	scan := New(cat, source, []string{".mp4"}, []string{"excluded"}, nil)
	scan.RetryWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	result, err := scan.Scan(ctx, "")
	if err != nil || len(result.Issues) != 0 || result.Stats.Scanned != 2 || result.Stats.Added != 2 || len(result.NewVideos) != 2 {
		t.Fatalf("scan result = %+v, err=%v", result, err)
	}
	video, err := cat.GetVideo(ctx, "fake-drive-recovered")
	if err != nil {
		t.Fatal(err)
	}
	if video.ParentID != "child" || video.DirName != "Videos" ||
		!reflect.DeepEqual(video.AncestorDirIDs, []string{"root", "parent", "broken", "child"}) ||
		!reflect.DeepEqual(video.AncestorDirNames, []string{"", "旅行", "Album", "Videos"}) ||
		!reflect.DeepEqual(video.Tags, []string{"旅行"}) {
		t.Fatalf("recovered file lost directory metadata/tags: %+v", video)
	}
	if source.listCalls["root"] != 1 || source.listCalls["parent"] != 1 || source.listCalls["child"] != 1 || source.listCalls["excluded"] != 0 {
		t.Fatalf("unexpected directory reads: %v", source.listCalls)
	}
	if _, excluded := result.Snapshot.ExcludedDirIDs["excluded"]; !excluded {
		t.Fatal("retry pass lost skipped-directory protection")
	}
}

func TestDiscoveryFinalPassDoesNotQueueNewFailures(t *testing.T) {
	source := &discoveryTestSource{
		entries: map[string][]drives.Entry{
			"root": {{ID: "parent", Name: "Parent", IsDir: true}},
			"parent": {
				{ID: "child", Name: "Child", IsDir: true},
				{ID: "child", Name: "Child", IsDir: true},
				{ID: "sibling", Name: "sibling.mp4", Size: 123},
			},
		},
		errorSequences: map[string][]error{"parent": {io.EOF, io.EOF, io.EOF}},
		errors:         map[string]error{"child": io.ErrUnexpectedEOF},
		listCalls:      map[string]int{},
	}
	scan := New(nil, source, []string{".mp4"}, nil, nil)
	scan.RetryWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	snapshot, stats, err := scan.Discover(context.Background(), "")
	if err != nil || source.listCalls["parent"] != 4 || source.listCalls["child"] != 3 {
		t.Fatalf("directory reads = %v, err=%v", source.listCalls, err)
	}
	if stats.Errors != 1 || stats.Scanned != 1 || len(snapshot.Issues) != 1 || snapshot.Issues[0].DirID != "child" ||
		!errors.Is(snapshot.Issues[0].Err, io.ErrUnexpectedEOF) || snapshot.PresenceAuthoritative() {
		t.Fatalf("final discovery = %+v, stats=%+v", snapshot, stats)
	}
	if _, failed := snapshot.FailedDirIDs["parent"]; failed {
		t.Fatal("recovered parent's failure was retained")
	}
	if _, failed := snapshot.FailedDirIDs["child"]; !failed {
		t.Fatal("failed descendant was not protected")
	}
}

func TestDiscoveryKeepsOnlyFinalDirectoryError(t *testing.T) {
	certificateErr := x509.UnknownAuthorityError{Cert: &x509.Certificate{}}
	accessErr := errors.New("access denied")
	for _, tt := range []struct {
		name     string
		failures []error
		wantErr  error
	}{
		{"permanent after deferral", []error{io.EOF, io.EOF, io.EOF, fs.ErrNotExist}, fs.ErrNotExist},
		{"all attempts fail", []error{io.EOF, io.EOF, io.EOF, io.EOF, io.EOF, syscall.ECONNRESET}, syscall.ECONNRESET},
		{"missing directory", []error{fs.ErrNotExist}, fs.ErrNotExist},
		{"certificate", []error{certificateErr}, certificateErr},
		{"access denied", []error{accessErr}, accessErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := &discoveryTestSource{
				entries:        map[string][]drives.Entry{"root": {{ID: "broken", Name: "Broken", IsDir: true}}},
				errorSequences: map[string][]error{"broken": tt.failures},
				listCalls:      map[string]int{},
			}
			scan := New(nil, source, []string{".mp4"}, nil, nil)
			scan.RetryWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
			snapshot, stats, err := scan.Discover(context.Background(), "")
			if err != nil || source.listCalls["broken"] != len(tt.failures) || stats.Errors != 1 || len(snapshot.Issues) != 1 ||
				!errors.Is(snapshot.Issues[0].Err, tt.wantErr) {
				t.Fatalf("reads=%v stats=%+v issues=%v err=%v", source.listCalls, stats, snapshot.Issues, err)
			}
		})
	}
}

func TestDiscoveryRetriesStartingDirectoryOnceAtEnd(t *testing.T) {
	for _, recovers := range []bool{true, false} {
		t.Run(map[bool]string{true: "recovery", false: "exhausted"}[recovers], func(t *testing.T) {
			failures := []error{io.EOF, io.EOF, io.EOF}
			if !recovers {
				failures = append(failures, io.EOF, io.EOF, io.EOF)
			}
			source := &discoveryTestSource{
				entries:        map[string][]drives.Entry{"selected": {{ID: "video", Name: "video.mp4", Size: 123}}},
				errorSequences: map[string][]error{"selected": failures},
				listCalls:      map[string]int{},
			}
			scan := New(nil, source, []string{".mp4"}, nil, nil)
			scan.RetryWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
			snapshot, stats, err := scan.Discover(context.Background(), "selected")
			if recovers {
				if err != nil || source.listCalls["selected"] != 4 || !snapshot.PresenceAuthoritative() || stats.Scanned != 1 || stats.Errors != 0 {
					t.Fatalf("root recovery: reads=%v snapshot=%+v err=%v", source.listCalls, snapshot, err)
				}
			} else if !errors.Is(err, io.EOF) || source.listCalls["selected"] != 6 || snapshot.PresenceAuthoritative() || stats.Scanned != 0 {
				t.Fatalf("unreadable root must remain fatal: reads=%v snapshot=%+v err=%v", source.listCalls, snapshot, err)
			}
		})
	}
}

func TestScanFinalPassCancellationDoesNotReconcile(t *testing.T) {
	for _, cancelDuringWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "request", true: "retry wait"}[cancelDuringWait], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cat, err := catalog.Open(t.TempDir() + "/catalog.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cat.Close() })
			source := &observedDiscoverySource{discoveryTestSource: &discoveryTestSource{
				entries: map[string][]drives.Entry{"root": {
					{ID: "broken", Name: "Broken", IsDir: true},
					{ID: "healthy", Name: "healthy.mp4", Size: 123},
				}},
				errors:    map[string]error{"broken": io.EOF},
				listCalls: map[string]int{},
			}}
			source.beforeList = func(ctx context.Context, dirID string) error {
				if !cancelDuringWait && dirID == "broken" && source.listCalls[dirID] == 3 {
					cancel()
				}
				return ctx.Err()
			}
			scan := New(cat, source, []string{".mp4"}, nil, nil)
			scan.RetryWait = func(ctx context.Context, delay time.Duration) error {
				if cancelDuringWait && source.listCalls["broken"] == 5 && delay > 0 {
					cancel()
				}
				return ctx.Err()
			}
			result, err := scan.Scan(ctx, "")
			if !errors.Is(err, context.Canceled) || result.Stats.Scanned != 1 || result.Stats.Added != 0 || len(result.Issues) != 0 {
				t.Fatalf("canceled final pass: result=%+v err=%v", result, err)
			}
			if _, err := cat.GetVideo(context.Background(), "fake-drive-healthy"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("canceled discovery wrote catalog: %v", err)
			}
		})
	}
}

func TestDiscoveryFinalPassSharesRateLimitBudget(t *testing.T) {
	limited := &drives.RateLimitError{Provider: "fake", RetryAfter: time.Second}
	source := &discoveryTestSource{
		entries: map[string][]drives.Entry{"root": {
			{ID: "broken", Name: "Broken", IsDir: true},
			{ID: "healthy", Name: "healthy.mp4", Size: 123},
		}},
		errorSequences: map[string][]error{"broken": {limited, limited, io.EOF, io.EOF, io.EOF, limited, limited}},
		listCalls:      map[string]int{},
	}
	scan := New(nil, source, []string{".mp4"}, nil, nil)
	scan.RetryWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	snapshot, stats, err := scan.Discover(context.Background(), "")
	if !errors.Is(err, ErrRateLimitBudgetExhausted) || scan.RateLimitBudget.UsedRetries() != RateLimitRetryLimit || source.listCalls["broken"] != 7 {
		t.Fatalf("rate-limit budget was reset: used=%d reads=%v err=%v", scan.RateLimitBudget.UsedRetries(), source.listCalls, err)
	}
	if stats.Scanned != 1 || len(snapshot.Issues) != 0 {
		t.Fatalf("fatal rate limit became a recoverable issue: %+v / %+v", snapshot, stats)
	}
}
