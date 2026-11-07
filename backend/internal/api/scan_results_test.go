package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/scanjob"
)

func TestDriveListExposesLastScanResultWithoutOverridingActiveProgress(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.UpsertDrive(ctx, &catalog.Drive{ID: "drive", Kind: "quark", Name: "Drive"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	result := scanjob.Result{DriveID: "drive", State: scanjob.Partial, StartedAt: now, FinishedAt: now, ScannedCount: 7, AddedCount: 2, CleanedCount: 3, ErrorCount: 1, Issues: []scanjob.Issue{{Stage: "discovery", Message: "subdirectory unavailable"}}}
	if err := cat.SaveScanResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	for _, active := range []bool{false, true} {
		server := &AdminServer{Catalog: cat, GetDriveGenerationStatuses: func() map[string]DriveGenerationStatuses {
			if active {
				return map[string]DriveGenerationStatuses{"drive": {Scan: GenerationStatus{State: "scanning", ScannedCount: 3}}}
			}
			return nil
		}}
		response := httptest.NewRecorder()
		server.handleListDrives(response, httptest.NewRequest("GET", "/drives", nil))
		if response.Code != 200 {
			t.Fatalf("status %d: %s", response.Code, response.Body.String())
		}
		var rows []struct {
			Scan GenerationStatus `json:"scanGenerationStatus"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %+v", rows)
		}
		if active {
			if rows[0].Scan.State != "scanning" || rows[0].Scan.ScannedCount != 3 || rows[0].Scan.Result != nil {
				t.Fatalf("active status overwritten: %+v", rows[0].Scan)
			}
		} else if rows[0].Scan.State != "partial" || rows[0].Scan.Result == nil || rows[0].Scan.Result.ErrorCount != 1 || rows[0].Scan.Result.CleanedCount != 3 || rows[0].Scan.AddedCount != 2 {
			t.Fatalf("finished result missing: %+v", rows[0].Scan)
		}
	}
}
