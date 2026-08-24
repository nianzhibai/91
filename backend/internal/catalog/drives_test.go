package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUpsertDriveUsesRootIDAsScanRootID(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	if err := cat.UpsertDrive(ctx, &Drive{
		ID:         "drive",
		Kind:       "p115",
		Name:       "115",
		RootID:     "root-folder",
		ScanRootID: "ignored-scan-root",
	}); err != nil {
		t.Fatalf("upsert drive: %v", err)
	}

	got, err := cat.GetDrive(ctx, "drive")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.RootID != "root-folder" {
		t.Fatalf("rootId = %q, want root-folder", got.RootID)
	}
	if got.ScanRootID != "root-folder" {
		t.Fatalf("scanRootId = %q, want root-folder", got.ScanRootID)
	}
}

func TestUpsertDrivePreservingSkipDirIDsKeepsConcurrentSetting(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &Drive{
		ID:         "drive",
		Kind:       "p115",
		Name:       "Old name",
		RootID:     "root",
		SkipDirIDs: []string{"old-skip"},
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	if err := cat.SetDriveSkipDirIDs(ctx, "drive", []string{"latest-skip"}); err != nil {
		t.Fatalf("save latest skip dirs: %v", err)
	}

	if err := cat.UpsertDrivePreservingSkipDirIDs(ctx, &Drive{
		ID:         "drive",
		Kind:       "p115",
		Name:       "New name",
		RootID:     "root",
		SkipDirIDs: []string{"stale-skip"},
	}); err != nil {
		t.Fatalf("upsert preserving skip dirs: %v", err)
	}

	got, err := cat.GetDrive(ctx, "drive")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Name != "New name" {
		t.Fatalf("name = %q, want New name", got.Name)
	}
	if len(got.SkipDirIDs) != 1 || got.SkipDirIDs[0] != "latest-skip" {
		t.Fatalf("skip dir ids = %#v, want latest setting", got.SkipDirIDs)
	}
}

func TestEnsureDriveSkipDirIDSerializesConcurrentAppends(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.UpsertDrive(ctx, &Drive{
		ID: "drive", Kind: "p115", Name: "Drive", RootID: "root",
		SkipDirIDs: []string{"stale-admin-dir"},
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	// Model the admin save that lands after a worker captured an older Drive.
	// Every later atomic ensure must merge with this latest replacement.
	if err := cat.SetDriveSkipDirIDs(ctx, "drive", []string{"latest-admin-dir"}); err != nil {
		t.Fatalf("save latest admin skip dirs: %v", err)
	}

	const appendCount = 24
	start := make(chan struct{})
	errCh := make(chan error, appendCount)
	var wg sync.WaitGroup
	for i := 0; i < appendCount; i++ {
		dirID := fmt.Sprintf("system-dir-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errCh <- cat.EnsureDriveSkipDirID(ctx, "drive", dirID)
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("ensure skip dir: %v", err)
		}
	}
	// Repeating an append must remain idempotent.
	if err := cat.EnsureDriveSkipDirID(ctx, "drive", "system-dir-00"); err != nil {
		t.Fatalf("repeat ensure skip dir: %v", err)
	}

	got, err := cat.GetDrive(ctx, "drive")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	seen := make(map[string]int, len(got.SkipDirIDs))
	for _, id := range got.SkipDirIDs {
		seen[id]++
	}
	if seen["latest-admin-dir"] != 1 || seen["stale-admin-dir"] != 0 {
		t.Fatalf("admin skip dirs after ensures = %#v, want latest value only", seen)
	}
	for i := 0; i < appendCount; i++ {
		dirID := fmt.Sprintf("system-dir-%02d", i)
		if seen[dirID] != 1 {
			t.Fatalf("skip dir %q count = %d, want 1", dirID, seen[dirID])
		}
	}
	if len(got.SkipDirIDs) != appendCount+1 {
		t.Fatalf("skip dir count = %d, want %d", len(got.SkipDirIDs), appendCount+1)
	}
}

func TestUpsertDriveWithOptionsPreservesOmittedTeaserSetting(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &Drive{
		ID:            "drive",
		Kind:          "onedrive",
		Name:          "Old name",
		RootID:        "root",
		TeaserEnabled: true,
		Credentials:   map[string]string{"refresh_token": "keep-token"},
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	if err := cat.SetDriveTeaserEnabled(ctx, "drive", false); err != nil {
		t.Fatalf("save latest teaser setting: %v", err)
	}
	if err := cat.UpsertDriveWithOptions(ctx, &Drive{
		ID:            "drive",
		Kind:          "onedrive",
		Name:          "New name",
		RootID:        "root",
		TeaserEnabled: true, // stale form snapshot; omitted by the request
		Credentials:   map[string]string{},
	}, DriveUpsertOptions{PatchCredentials: true}); err != nil {
		t.Fatalf("partial config upsert: %v", err)
	}

	got, err := cat.GetDrive(ctx, "drive")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Name != "New name" || got.TeaserEnabled {
		t.Fatalf("drive after partial save = %+v, want new name and preserved disabled teaser", got)
	}
	if got.Credentials["refresh_token"] != "keep-token" {
		t.Fatalf("credentials = %#v, want latest token preserved", got.Credentials)
	}
}

func TestPatchDriveCredentialsPreservesSettingsChangedAfterAttach(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &Drive{
		ID:            "drive",
		Kind:          "pikpak",
		Name:          "PikPak",
		RootID:        "root-folder",
		Status:        "ok",
		TeaserEnabled: true,
		SkipDirIDs:    []string{"old-skip"},
		Credentials: map[string]string{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"username":      "account",
		},
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	// Simulate settings saved after the runtime driver captured its attachment
	// snapshot but before that driver refreshed its tokens.
	if err := cat.SetDriveSkipDirIDs(ctx, "drive", []string{"keep-this-dir"}); err != nil {
		t.Fatalf("save skip dirs: %v", err)
	}
	if err := cat.SetDriveTeaserEnabled(ctx, "drive", false); err != nil {
		t.Fatalf("disable teaser: %v", err)
	}
	if err := cat.PatchDriveCredentials(ctx, "drive", map[string]string{
		"access_token":  "new-access",
		"refresh_token": "new-refresh",
		"captcha_token": "new-captcha",
	}); err != nil {
		t.Fatalf("patch credentials: %v", err)
	}

	got, err := cat.GetDrive(ctx, "drive")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.RootID != "root-folder" || got.Name != "PikPak" || got.Status != "ok" {
		t.Fatalf("drive settings changed: %+v", got)
	}
	if got.TeaserEnabled {
		t.Fatal("teaser setting was rolled back")
	}
	if len(got.SkipDirIDs) != 1 || got.SkipDirIDs[0] != "keep-this-dir" {
		t.Fatalf("skip dir ids = %#v, want preserved latest setting", got.SkipDirIDs)
	}
	if got.Credentials["access_token"] != "new-access" ||
		got.Credentials["refresh_token"] != "new-refresh" ||
		got.Credentials["captcha_token"] != "new-captcha" ||
		got.Credentials["username"] != "account" {
		t.Fatalf("credentials = %#v, want merged token patch", got.Credentials)
	}
}

func TestPatchDriveCredentialsIfMatchRejectsReplacedRefreshToken(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &Drive{
		ID:     "drive",
		Kind:   "onedrive",
		Name:   "OneDrive",
		RootID: "root",
		Credentials: map[string]string{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
		},
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	applied, err := cat.PatchDriveCredentialsIfMatch(ctx, "drive", "onedrive", "refresh_token", "old-refresh", map[string]string{
		"access_token":  "runtime-access",
		"refresh_token": "runtime-refresh",
	})
	if err != nil {
		t.Fatalf("matching conditional patch: %v", err)
	}
	if !applied {
		t.Fatal("matching conditional credential patch was rejected")
	}
	if err := cat.PatchDriveCredentials(ctx, "drive", map[string]string{"refresh_token": "admin-refresh"}); err != nil {
		t.Fatalf("replace refresh token: %v", err)
	}

	applied, err = cat.PatchDriveCredentialsIfMatch(ctx, "drive", "onedrive", "refresh_token", "runtime-refresh", map[string]string{
		"access_token":  "late-access",
		"refresh_token": "late-refresh",
	})
	if err != nil {
		t.Fatalf("conditional patch: %v", err)
	}
	if applied {
		t.Fatal("stale conditional credential patch was applied")
	}
	got, err := cat.GetDrive(ctx, "drive")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Credentials["access_token"] != "runtime-access" || got.Credentials["refresh_token"] != "admin-refresh" {
		t.Fatalf("credentials = %#v, want administrator replacement preserved", got.Credentials)
	}
}

func TestPatchDriveCredentialsIfMatchRejectsChangedKind(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	if err := cat.UpsertDrive(ctx, &Drive{
		ID: "drive", Kind: "onedrive", Name: "Drive", RootID: "root",
		Credentials: map[string]string{"refresh_token": "same-token"},
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	if err := cat.UpsertDrive(ctx, &Drive{
		ID: "drive", Kind: "googledrive", Name: "Drive", RootID: "root",
		Credentials: map[string]string{"refresh_token": "same-token"},
	}); err != nil {
		t.Fatalf("change drive kind: %v", err)
	}
	applied, err := cat.PatchDriveCredentialsIfMatch(ctx, "drive", "onedrive", "refresh_token", "same-token", map[string]string{
		"access_token": "late-old-kind-token",
	})
	if err != nil {
		t.Fatalf("conditional patch: %v", err)
	}
	if applied {
		t.Fatal("old-kind credential callback was applied to replacement provider")
	}
}

func TestUpsertDrivePatchingCredentialsUsesLatestStoredTokens(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.UpsertDrive(ctx, &Drive{
		ID:     "pikpak-main",
		Kind:   "pikpak",
		Name:   "PikPak",
		RootID: "",
		Credentials: map[string]string{
			"username":      "old-user",
			"password":      "old-password",
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"captcha_token": "old-captcha",
			"device_id":     "device",
		},
	}); err != nil {
		t.Fatalf("seed drive: %v", err)
	}
	if err := cat.PatchDriveCredentials(ctx, "pikpak-main", map[string]string{
		"access_token":  "latest-access",
		"refresh_token": "latest-refresh",
		"captcha_token": "latest-captcha",
	}); err != nil {
		t.Fatalf("persist runtime token refresh: %v", err)
	}

	if err := cat.UpsertDrivePatchingCredentials(ctx, &Drive{
		ID:          "pikpak-main",
		Kind:        "pikpak",
		Name:        "Renamed PikPak",
		RootID:      "",
		Credentials: map[string]string{"username": "new-user"},
	}); err != nil {
		t.Fatalf("patching upsert: %v", err)
	}

	got, err := cat.GetDrive(ctx, "pikpak-main")
	if err != nil {
		t.Fatalf("get drive: %v", err)
	}
	if got.Name != "Renamed PikPak" || got.Credentials["username"] != "new-user" {
		t.Fatalf("editable settings not updated: %+v", got)
	}
	if got.Credentials["password"] != "old-password" ||
		got.Credentials["access_token"] != "latest-access" ||
		got.Credentials["refresh_token"] != "latest-refresh" ||
		got.Credentials["captcha_token"] != "latest-captcha" ||
		got.Credentials["device_id"] != "device" {
		t.Fatalf("credentials = %#v, want latest runtime values preserved", got.Credentials)
	}
}

func TestUpsertDriveDefaultsRootIDByKind(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	cases := []struct {
		id   string
		kind string
		want string
	}{
		{id: "p115", kind: "p115", want: "0"},
		{id: "pikpak", kind: "pikpak", want: ""},
		{id: "guangyapan", kind: "guangyapan", want: ""},
		{id: "onedrive", kind: "onedrive", want: "root"},
		{id: "googledrive", kind: "googledrive", want: "root"},
		{id: "webdav", kind: "webdav", want: "/"},
		{id: "localstorage", kind: "localstorage", want: "/"},
		{id: "scriptcrawler", kind: "scriptcrawler", want: "/"},
	}
	for _, tc := range cases {
		if err := cat.UpsertDrive(ctx, &Drive{
			ID:   tc.id,
			Kind: tc.kind,
			Name: tc.kind,
		}); err != nil {
			t.Fatalf("upsert %s: %v", tc.kind, err)
		}
		got, err := cat.GetDrive(ctx, tc.id)
		if err != nil {
			t.Fatalf("get %s: %v", tc.kind, err)
		}
		if got.RootID != tc.want {
			t.Fatalf("%s rootId = %q, want %q", tc.kind, got.RootID, tc.want)
		}
		if got.ScanRootID != tc.want {
			t.Fatalf("%s scanRootId = %q, want %q", tc.kind, got.ScanRootID, tc.want)
		}
	}
}

func TestUpsertDriveIgnoresRootIDForLocalStorageAndScriptCrawler(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Fatalf("close catalog: %v", err)
		}
	})

	for _, tc := range []struct {
		id   string
		kind string
	}{
		{id: "localstorage", kind: "localstorage"},
		{id: "scriptcrawler", kind: "scriptcrawler"},
	} {
		if err := cat.UpsertDrive(ctx, &Drive{
			ID:         tc.id,
			Kind:       tc.kind,
			Name:       tc.kind,
			RootID:     "manual-root",
			ScanRootID: "manual-scan-root",
		}); err != nil {
			t.Fatalf("upsert %s: %v", tc.kind, err)
		}
		got, err := cat.GetDrive(ctx, tc.id)
		if err != nil {
			t.Fatalf("get %s: %v", tc.kind, err)
		}
		if got.RootID != "/" {
			t.Fatalf("%s rootId = %q, want /", tc.kind, got.RootID)
		}
		if got.ScanRootID != "/" {
			t.Fatalf("%s scanRootId = %q, want /", tc.kind, got.ScanRootID)
		}
	}
}

func TestDeleteDriveRemovesOwnedStateAndKeepsMigratedVideos(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	for _, drive := range []*Drive{
		{ID: "crawler-source", Kind: "scriptcrawler", Name: "Crawler"},
		{ID: "cloud-target", Kind: "pikpak", Name: "PikPak"},
	} {
		if err := cat.UpsertDrive(ctx, drive); err != nil {
			t.Fatalf("seed drive %s: %v", drive.ID, err)
		}
	}
	now := time.Now()
	migrated := &Video{
		ID:          "scriptcrawler-crawler-source-item-1",
		DriveID:     "cloud-target",
		FileID:      "remote-file",
		Title:       "Migrated video",
		PublishedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := cat.UpsertVideo(ctx, migrated); err != nil {
		t.Fatalf("seed migrated video: %v", err)
	}
	if err := cat.MarkCrawlerSourceSeen(ctx, "scriptcrawler", "crawler-source", "item-1", "imported", migrated.ID, "sampled", 123); err != nil {
		t.Fatalf("seed crawler source state: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `INSERT INTO scans (drive_id, started_at) VALUES (?, ?)`, "crawler-source", now.UnixMilli()); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	if _, err := cat.db.ExecContext(ctx, `INSERT INTO drive_scan_misses (drive_id, file_id, consecutive_misses, last_missing_at) VALUES (?, ?, ?, ?)`, "crawler-source", "missing-file", 1, now.UnixMilli()); err != nil {
		t.Fatalf("seed scan miss: %v", err)
	}

	if err := cat.DeleteDrive(ctx, "crawler-source"); err != nil {
		t.Fatalf("delete drive: %v", err)
	}
	if _, err := cat.GetDrive(ctx, "crawler-source"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted drive lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := cat.GetVideo(ctx, migrated.ID); err != nil {
		t.Fatalf("migrated video was removed: %v", err)
	}
	if _, err := cat.GetDrive(ctx, "cloud-target"); err != nil {
		t.Fatalf("target drive was removed: %v", err)
	}
	for table, query := range map[string]string{
		"crawler source": `SELECT COUNT(*) FROM crawler_seen_sources WHERE drive_id = ?`,
		"scan":           `SELECT COUNT(*) FROM scans WHERE drive_id = ?`,
		"scan miss":      `SELECT COUNT(*) FROM drive_scan_misses WHERE drive_id = ?`,
	} {
		var count int
		if err := cat.db.QueryRowContext(ctx, query, "crawler-source").Scan(&count); err != nil {
			t.Fatalf("count %s state: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s state count = %d, want 0", table, count)
		}
	}
}

func TestSetDriveRuntimeStatusTracksPlaybackFailureAndRecovery(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	drive := &Drive{
		ID:            "drive",
		Kind:          "p115",
		Name:          "115",
		RootID:        "configured-root",
		Status:        "ok",
		TeaserEnabled: true,
		SkipDirIDs:    []string{"keep-skipped"},
		Credentials: map[string]string{
			"cookie": "credential-must-be-preserved",
		},
	}
	if err := cat.UpsertDrive(ctx, drive); err != nil {
		t.Fatalf("upsert drive: %v", err)
	}
	if err := cat.SetDriveRuntimeStatus(ctx, drive.ID, "error", "user not login"); err != nil {
		t.Fatalf("set error status: %v", err)
	}

	got, err := cat.GetDrive(ctx, drive.ID)
	if err != nil {
		t.Fatalf("get failed drive: %v", err)
	}
	if got.Status != "error" || !strings.Contains(got.LastError, "not login") {
		t.Fatalf("status=%q lastError=%q, want playback error", got.Status, got.LastError)
	}
	if got.Credentials["cookie"] != "credential-must-be-preserved" {
		t.Fatalf("credentials changed: %#v", got.Credentials)
	}
	if got.RootID != "configured-root" || !got.TeaserEnabled || len(got.SkipDirIDs) != 1 || got.SkipDirIDs[0] != "keep-skipped" {
		t.Fatalf("drive settings changed: %+v", got)
	}

	// A stale admin/config snapshot must not own runtime status. This is the
	// metadata-only save path used while the mounted Driver keeps running.
	drive.Name = "Renamed 115"
	drive.Status = "disconnected"
	drive.LastError = ""
	if err := cat.UpsertDrive(ctx, drive); err != nil {
		t.Fatalf("upsert drive config: %v", err)
	}
	got, err = cat.GetDrive(ctx, drive.ID)
	if err != nil {
		t.Fatalf("get drive after config upsert: %v", err)
	}
	if got.Name != "Renamed 115" {
		t.Fatalf("name = %q, want config update", got.Name)
	}
	if got.Status != "error" || !strings.Contains(got.LastError, "not login") {
		t.Fatalf("status=%q lastError=%q, config upsert overwrote runtime state", got.Status, got.LastError)
	}

	if err := cat.SetDriveRuntimeStatus(ctx, drive.ID, "ok", ""); err != nil {
		t.Fatalf("recover status: %v", err)
	}
	got, err = cat.GetDrive(ctx, drive.ID)
	if err != nil {
		t.Fatalf("get recovered drive: %v", err)
	}
	if got.Status != "ok" || got.LastError != "" {
		t.Fatalf("status=%q lastError=%q, want recovered drive", got.Status, got.LastError)
	}
}

func TestSetDriveRuntimeStatusRejectsInvalidState(t *testing.T) {
	cat, err := Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	if err := cat.SetDriveRuntimeStatus(context.Background(), "drive", "pending", ""); err == nil {
		t.Fatal("invalid runtime status unexpectedly accepted")
	}
}
