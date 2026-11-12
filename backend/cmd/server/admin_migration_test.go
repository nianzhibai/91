package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

func TestMigrateLegacyAdmin(t *testing.T) {
	for _, test := range []struct {
		name           string
		source         string
		existing       string
		role           string
		removeExisting bool
		wantSetup      bool
		wantUser       string
	}{
		{name: "fresh config", source: "{}", wantSetup: true},
		{name: "empty credentials", source: "server:\n  admin: {}\n", wantSetup: true},
		{name: "public defaults", source: "server:\n  admin:\n    username: admin\n    password: admin123\n", wantSetup: true},
		{name: "legacy import", source: legacyMigrationConfig, wantUser: "owner"},
		{name: "database password wins", source: legacyMigrationConfig, existing: "owner", role: "admin", wantUser: "owner"},
		{name: "renamed admin", source: legacyMigrationConfig, existing: "renamed-owner", role: "admin", wantUser: "renamed-owner"},
		{name: "no config credentials", source: "{}", existing: "owner", role: "admin", wantUser: "owner"},
		{name: "only regular account", source: legacyMigrationConfig, existing: "viewer", role: "user", wantUser: "viewer"},
		{name: "deleted account", source: legacyMigrationConfig, existing: "owner", role: "admin", removeExisting: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			path := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(path, []byte(test.source), 0o600); err != nil {
				t.Fatal(err)
			}
			cat, err := catalog.Open(filepath.Join(root, "catalog.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			if test.existing != "" {
				hash, err := auth.HashPassword("database-secret")
				if err != nil {
					t.Fatal(err)
				}
				id, err := cat.CreateUser(ctx, test.existing, hash, test.role)
				if err != nil {
					t.Fatal(err)
				}
				if test.removeExisting {
					if err := cat.DeleteUser(ctx, id); err != nil {
						t.Fatal(err)
					}
				}
			}
			for i := 0; i < 2; i++ {
				manager, err := config.NewManager(path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := migrateLegacyAdmin(ctx, cat, manager); err != nil {
					t.Fatal(err)
				}
				if required, err := cat.AdminSetupRequired(ctx); err != nil || required != test.wantSetup {
					t.Fatalf("setup required=%v err=%v", required, err)
				}
			}
			written, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(written), "admin:") || strings.Contains(string(written), "config-secret") {
				t.Fatal("config still contains credentials")
			}
			wantCount := 0
			if test.wantUser != "" {
				wantCount = 1
			}
			if count, err := cat.CountUsers(ctx); err != nil || count != wantCount {
				t.Fatalf("users=%d err=%v", count, err)
			}
			if test.wantUser != "" {
				password := "config-secret"
				role := "admin"
				if test.existing != "" {
					password = "database-secret"
					role = test.role
				}
				a := &auth.Authenticator{Catalog: cat}
				got, err := a.UserLogin(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/auth/login", nil), test.wantUser, password)
				if err != nil || got != role {
					t.Fatalf("role=%q err=%v", got, err)
				}
			}
		})
	}
}

const legacyMigrationConfig = "server:\n  admin:\n    username: owner\n    password: config-secret\n"

func TestMigrationRetryDoesNotRestoreOldPassword(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(legacyMigrationConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	manager, err := config.NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrateLegacyAdmin(ctx, cat, manager); err != nil {
		t.Fatal(err)
	}
	u, err := cat.GetUserByUsername(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword("new-database-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateUserPassword(ctx, u.ID, hash); err != nil {
		t.Fatal(err)
	}
	// Simulate old config surviving a crash or being restored independently.
	if err := os.WriteFile(path, []byte(legacyMigrationConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateLegacyAdmin(ctx, cat, manager); err != nil {
		t.Fatal(err)
	}
	u, err = cat.GetUserByUsername(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if u.Password != hash {
		t.Fatal("migration overwrote the current database password")
	}
}
