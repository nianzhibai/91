package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/video-site/backend/internal/auth"
	"github.com/video-site/backend/internal/catalog"
)

func TestAdminTelegramVideoFilterSurvivesCloudMigrationAndIgnoresTags(t *testing.T) {
	ctx := context.Background()
	c := openRemoteUploadAPICatalog(t)
	if _, err := c.TelegramPaused(ctx, 123); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 2; i++ {
		id := fmt.Sprintf("tg-%d", i)
		if err := c.AcceptTelegramUpdate(ctx, catalog.TelegramReceipt{BotID: 123, UpdateID: i, MessageID: i, ChatID: 42, SenderID: 42}, &catalog.TelegramSource{BotID: 123, SenderID: 42, FileID: id, UniqueID: id, Size: 5}, id, id, 100); err != nil {
			t.Fatal(err)
		}
		if err := c.TransitionRemoteUploadJob(ctx, id, catalog.RemoteUploadQueued, catalog.RemoteUploadSaving); err != nil {
			t.Fatal(err)
		}
		if err := c.FinalizeRemoteUpload(ctx, id, &catalog.Video{ID: id, DriveID: "local-upload", FileID: id + ".mp4", FileName: id + ".mp4", Title: id, Size: 5}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.UpsertDrive(ctx, &catalog.Drive{ID: "cloud", Name: "Cloud", Kind: "webdav", RootID: "/", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := c.MigrateVideoToDrive(ctx, "tg-2", catalog.VideoDriveMigration{DriveID: "cloud", FileID: "remote-file"}); err != nil {
		t.Fatal(err)
	}
	// Removing TG from actual imports, or attaching it to ordinary uploads,
	// must never change membership of the provenance filter.
	tag, err := c.EnsureTag(ctx, "TG", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteTag(ctx, tag.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertVideo(ctx, &catalog.Video{ID: "ordinary", DriveID: "local-upload", FileID: "ordinary.mp4", Title: "ordinary", Tags: []string{"TG"}, Size: 5}); err != nil {
		t.Fatal(err)
	}
	a := &AdminServer{Catalog: c}
	for _, tc := range []struct {
		query        string
		total, count int
		wantID       string
	}{
		{"sourceKind=telegram", 2, 2, ""},
		{"sourceKind=telegram&driveId=local-upload", 1, 1, "tg-1"},
		{"sourceKind=telegram&driveId=cloud", 1, 1, "tg-2"},
		{"sourceKind=telegram&keyword=tg-2", 1, 1, "tg-2"},
		{"sourceKind=telegram&size=1&page=2", 2, 1, ""},
		{"sourceKind=telegram&keyword=ordinary", 0, 0, ""},
	} {
		w := httptest.NewRecorder()
		a.handleAdminListVideos(w, httptest.NewRequest("GET", "/admin/api/videos?"+tc.query, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.query, w.Code, w.Body.String())
		}
		var response struct {
			Items []catalog.Video `json:"items"`
			Total int             `json:"total"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Total != tc.total || len(response.Items) != tc.count || (tc.wantID != "" && response.Items[0].ID != tc.wantID) {
			t.Fatalf("%s: %+v", tc.query, response)
		}
	}
	w := httptest.NewRecorder()
	a.handleAdminListVideos(w, httptest.NewRequest("GET", "/admin/api/videos?sourceKind=unsupported", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown source silently ignored: %d", w.Code)
	}
}

func TestTelegramAdminRoutesRejectAnonymousAndViewers(t *testing.T) {
	c := openRemoteUploadAPICatalog(t)
	a := &auth.Authenticator{Catalog: c}
	r := chi.NewRouter()
	(&AdminServer{Catalog: c, Auth: a}).Register(r)
	hash, err := auth.HashPassword("viewer-secret")
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.CreateUser(context.Background(), "tg-viewer", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CreateSessionUntil(context.Background(), "viewer", time.Now().Add(time.Hour), id); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/config.yaml"}, {"PUT", "/config.yaml"}, {"GET", "/telegram/availability"}, {"GET", "/telegram/status"}, {"POST", "/telegram/test"}, {"POST", "/telegram/resume"},
		{"GET", "/import-jobs"}, {"POST", "/import-jobs/job/cancel"}, {"POST", "/import-jobs/job/retry"},
	} {
		for _, viewer := range []bool{false, true} {
			req := httptest.NewRequest(route.method, "/admin/api"+route.path, strings.NewReader("{}"))
			expected := 401
			if viewer {
				req.AddCookie(&http.Cookie{Name: "vs_admin", Value: "viewer"})
				expected = 403
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != expected {
				t.Fatalf("%s viewer=%v status=%d", route.path, viewer, w.Code)
			}
		}
	}
}
func TestImportAPIHidesPrivateSourceAndUsesCursor(t *testing.T) {
	ctx := context.Background()
	c := openRemoteUploadAPICatalog(t)
	if _, err := c.TelegramPaused(ctx, 123); err != nil {
		t.Fatal(err)
	}
	source := &catalog.TelegramSource{BotID: 123, SenderID: 42, FileID: "PRIVATE_FILE_ID", UniqueID: "PRIVATE_UNIQUE", FileName: "raw-name.mp4"}
	if err := c.AcceptTelegramUpdate(ctx, catalog.TelegramReceipt{BotID: 123, ChatID: 42, MessageID: 1, UpdateID: 1, SenderID: 42}, source, "tg-job", "video", 100); err != nil {
		t.Fatal(err)
	}
	server := &AdminServer{Catalog: c}
	rr := httptest.NewRecorder()
	server.handleImportList(rr, httptest.NewRequest("GET", "/admin/api/import-jobs?source=telegram", nil))
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "PRIVATE") || strings.Contains(rr.Body.String(), "sourcePayload") {
		t.Fatal("private source leaked")
	}
	var jobs []ImportJobDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &jobs); err != nil || len(jobs) != 1 || jobs[0].SenderID != "42" {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	rr = httptest.NewRecorder()
	server.handleImportList(rr, httptest.NewRequest("GET", "/admin/api/import-jobs?before="+jobs[0].Sequence, nil))
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("cursor response %d %s", rr.Code, rr.Body.String())
	}
}

func TestImportAPIStateFiltersBeforePagination(t *testing.T) {
	ctx := context.Background()
	c := openRemoteUploadAPICatalog(t)
	if _, err := c.TelegramPaused(ctx, 123); err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		state                 string
		active, retry, cancel bool
	}{
		{state: "queued", active: true},
		{state: "completed"},
		{state: "downloading", active: true},
		{state: "failed"},
		{state: "validating", active: true},
		{state: "canceled"},
		{state: "saving", active: true},
		{state: "queued", active: true, retry: true},
		{state: "downloading", active: true, cancel: true},
	}
	wantIDs := map[string][]string{}
	wantStates := map[string]string{}
	var updateID int64
	// Mix more than one page of active imports with terminal jobs and HTTP jobs.
	for batch := 0; batch < 6; batch++ {
		for _, fixture := range fixtures {
			updateID++
			id := fmt.Sprintf("tg-%d", updateID)
			receipt := catalog.TelegramReceipt{BotID: 123, UpdateID: updateID, MessageID: updateID, ChatID: 42, SenderID: 42}
			source := &catalog.TelegramSource{BotID: 123, SenderID: 42, FileID: id, UniqueID: id, Size: 5}
			if err := c.AcceptTelegramUpdate(ctx, receipt, source, id, id, 100); err != nil {
				t.Fatal(err)
			}
			if fixture.state != catalog.RemoteUploadQueued {
				if err := c.TransitionRemoteUploadJob(ctx, id, catalog.RemoteUploadQueued, fixture.state); err != nil {
					t.Fatal(err)
				}
			}
			if fixture.retry {
				if err := c.TransitionRemoteUploadJob(ctx, id, catalog.RemoteUploadQueued, catalog.RemoteUploadDownloading); err != nil {
					t.Fatal(err)
				}
				if err := c.DelayImport(ctx, id, "network", time.Hour, true); err != nil {
					t.Fatal(err)
				}
			}
			if fixture.cancel {
				if _, err := c.CancelRemoteUploadJob(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			wantIDs[""] = append(wantIDs[""], id)
			wantIDs[fixture.state] = append(wantIDs[fixture.state], id)
			wantStates[id] = fixture.state
			if fixture.active {
				wantIDs["active"] = append(wantIDs["active"], id)
			}
		}
		if _, err := c.CreateRemoteUploadJob(ctx, fmt.Sprintf("http-%d", batch), "https://example.com/video.mp4", "example", "HTTP video", nil); err != nil {
			t.Fatal(err)
		}
	}
	server := &AdminServer{Catalog: c}
	for filter, want := range wantIDs {
		slices.Reverse(want)
		t.Run("state="+filter, func(t *testing.T) {
			query := url.Values{"source": {"telegram"}, "state": {filter}}
			for offset := 0; ; offset += 30 {
				rr := httptest.NewRecorder()
				server.handleImportList(rr, httptest.NewRequest("GET", "/admin/api/import-jobs?"+query.Encode(), nil))
				if rr.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
				}
				var jobs []ImportJobDTO
				if err := json.Unmarshal(rr.Body.Bytes(), &jobs); err != nil {
					t.Fatal(err)
				}
				pageSize := min(30, len(want)-offset)
				if len(jobs) != pageSize {
					t.Fatalf("offset=%d: got %d jobs, want %d", offset, len(jobs), pageSize)
				}
				for i, job := range jobs {
					if job.ID != want[offset+i] || job.State != wantStates[job.ID] {
						t.Fatalf("offset=%d index=%d: got %s (%s), want %s (%s)", offset, i, job.ID, job.State, want[offset+i], wantStates[want[offset+i]])
					}
				}
				if len(jobs) < 30 {
					break
				}
				query.Set("before", jobs[len(jobs)-1].Sequence)
			}
		})
	}
	rr := httptest.NewRecorder()
	server.handleImportList(rr, httptest.NewRequest("GET", "/admin/api/import-jobs?state=unknown", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown state accepted: %d", rr.Code)
	}
}

func TestImportAPIHonorsRecentRecordLimit(t *testing.T) {
	ctx := context.Background()
	c := openRemoteUploadAPICatalog(t)
	if _, err := c.TelegramPaused(ctx, 123); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 55; i++ {
		id := fmt.Sprintf("tg-%d", i)
		receipt := catalog.TelegramReceipt{BotID: 123, UpdateID: i, MessageID: i, ChatID: 42, SenderID: 42}
		source := &catalog.TelegramSource{BotID: 123, SenderID: 42, FileID: id, UniqueID: id, Size: 5}
		if err := c.AcceptTelegramUpdate(ctx, receipt, source, id, id, 100); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.CreateRemoteUploadJob(ctx, "http", "https://example.com/video.mp4", "example", "HTTP video", nil); err != nil {
		t.Fatal(err)
	}
	server := &AdminServer{Catalog: c}
	for _, tc := range []struct {
		limit string
		count int
	}{
		{"", 30}, {"1", 1}, {"50", 50}, {"100", 55},
	} {
		t.Run("limit="+tc.limit, func(t *testing.T) {
			query := url.Values{"source": {"telegram"}, "limit": {tc.limit}}
			rr := httptest.NewRecorder()
			server.handleImportList(rr, httptest.NewRequest("GET", "/admin/api/import-jobs?"+query.Encode(), nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var jobs []ImportJobDTO
			if err := json.Unmarshal(rr.Body.Bytes(), &jobs); err != nil {
				t.Fatal(err)
			}
			if len(jobs) != tc.count {
				t.Fatalf("got %d records, want %d", len(jobs), tc.count)
			}
			for i, job := range jobs {
				if want := fmt.Sprintf("tg-%d", 55-i); job.ID != want {
					t.Fatalf("record %d: got %s, want %s", i, job.ID, want)
				}
			}
		})
	}
	for _, limit := range []string{"0", "-1", "101", "invalid", "1.5"} {
		rr := httptest.NewRecorder()
		server.handleImportList(rr, httptest.NewRequest("GET", "/admin/api/import-jobs?source=telegram&limit="+limit, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("limit=%q: status=%d, want 400", limit, rr.Code)
		}
	}
}
