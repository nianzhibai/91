package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestMobileAuthorizationRefreshRespectsAdministratorReplacement(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	drive := &catalog.Drive{ID: "mobile", Kind: "yun139", Name: "移动网盘", Credentials: map[string]string{"authorization": "initial"}}
	if err := cat.UpsertDrive(ctx, drive); err != nil {
		t.Fatal(err)
	}
	app := &App{cat: cat}
	lease := app.beginDriveCredentialLease(drive.ID)
	configureDriveCredentialLease(lease, drive)
	app.persistDriveCredentials(lease, map[string]string{"authorization": "refreshed"})
	got, err := cat.GetDrive(ctx, drive.ID)
	if err != nil || got.Credentials["authorization"] != "refreshed" {
		t.Fatalf("refresh not persisted: %v", err)
	}
	if err := cat.PatchDriveCredentials(ctx, drive.ID, map[string]string{"authorization": "administrator"}); err != nil {
		t.Fatal(err)
	}
	app.persistDriveCredentials(lease, map[string]string{"authorization": "late-refresh"})
	got, err = cat.GetDrive(ctx, drive.ID)
	if err != nil || got.Credentials["authorization"] != "administrator" {
		t.Fatalf("stale refresh overwrote administrator: %v", err)
	}
}
