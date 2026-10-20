package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/video-site/backend/internal/catalog"
)

func TestPlaybackSourceLabelsFollowLocalAndMigratedStorage(t *testing.T) {
	ctx := context.Background()
	c := openRemoteUploadAPICatalog(t)
	s := &Server{Catalog: c}
	if err := c.UpsertDrive(ctx, &catalog.Drive{ID: "cloud", Kind: "onedrive", Name: "My Drive", Status: "ok", RootID: "root"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"local-upload-normal", "local-upload-telegram", "shared-telegram"} {
		v := &catalog.Video{ID: id, DriveID: localUploadDriveID, FileID: id + ".mp4", FileName: id + ".mp4", Title: id, Ext: "mp4"}
		if id == "shared-telegram" {
			v.DriveID = catalog.TelegramLocalDriveID
		}
		if id == "local-upload-telegram" {
			v.Tags = []string{"TG"}
		}
		if err := c.UpsertVideo(ctx, v); err != nil {
			t.Fatal(err)
		}
		check := func(wantLabel, wantSrc string) {
			t.Helper()
			w := httptest.NewRecorder()
			s.handleVideoDetail(w, requestWithVideoID(http.MethodGet, "/api/video/"+id, id, strings.NewReader("")))
			if w.Code != http.StatusOK {
				t.Fatalf("detail: %d %s", w.Code, w.Body.String())
			}
			var got VideoDetailDTO
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.SourceLabel != wantLabel || got.VideoSrc != wantSrc {
				t.Fatalf("detail label/source=%q/%q, want %q/%q", got.SourceLabel, got.VideoSrc, wantLabel, wantSrc)
			}
			saved, err := c.GetVideo(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			shared := s.mapSharedVideoDetail(ctx, saved, "share")
			if shared.SourceLabel != wantLabel {
				t.Fatalf("shared label=%q, want %q", shared.SourceLabel, wantLabel)
			}
		shorts := s.mapShortsItems(ctx, []*catalog.Video{saved, saved}, nil, nil, nil)
			for _, item := range shorts {
				if item.SourceLabel != wantLabel {
					t.Fatalf("shorts label=%q, want %q", item.SourceLabel, wantLabel)
				}
			}
		}
		// Built-in local storage deliberately has no configured drive row.
		if _, err := c.GetDrive(ctx, localUploadDriveID); err == nil {
			t.Fatal("fixture unexpectedly contains a local drive row")
		}
		if id == "shared-telegram" {
			check("本地存储", "/p/stream/telegram-local/"+v.FileID)
			if _, err := s.availableVideo(ctx, id); err != nil {
				t.Fatal("shared video unavailable", err)
			}
		} else {
			check("本地存储", "/p/upload/"+id)
		}
		if err := c.MigrateVideoToDrive(ctx, id, catalog.VideoDriveMigration{DriveID: "cloud", FileID: "remote-" + id}); err != nil {
			t.Fatal(err)
		}
		// The logical ID stays local-upload-prefixed after moving to the cloud.
		check("OneDrive", "/p/stream/cloud/remote-"+id)
	}
}
