package main

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/filelock"
)

func seedLegacyDatabase(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "video-site.db")
	cat, err := catalog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateUser(context.Background(), "existing-owner", "existing-password-hash", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDatabaseFilenameMigrationPreservesDataAndResumesPublication(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "interrupted-publication"}[interrupted], func(t *testing.T) {
			directory := t.TempDir()
			legacy := seedLegacyDatabase(t, directory)
			target := filepath.Join(directory, "91.db")
			before, _ := os.Stat(legacy)
			if interrupted {
				if err := os.Link(legacy, target); err != nil {
					t.Fatal(err)
				}
			}
			for cycle := 0; cycle < 2; cycle++ {
				if err := migrateDatabaseFilename(context.Background(), target); err != nil {
					t.Fatal(err)
				}
				cat, err := catalog.Open(target)
				if err != nil {
					t.Fatal(err)
				}
				user, err := cat.GetUserByUsername(context.Background(), "existing-owner")
				if err != nil || user.Password != "existing-password-hash" || user.Role != "admin" {
					t.Fatalf("migrated account = %+v, error = %v", user, err)
				}
				if err := cat.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(legacy); !os.IsNotExist(err) {
					t.Fatalf("legacy database still exists: %v", err)
				}
				after, err := os.Stat(target)
				if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != before.Mode().Perm() {
					t.Fatalf("migration changed database identity/permissions: %v", err)
				}
			}
		})
	}
}

func TestDatabaseFilenameMigrationRecoversWAL(t *testing.T) {
	if path := os.Getenv("TEST_LEGACY_WAL_DATABASE"); path != "" {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE migration_probe(value TEXT); INSERT INTO migration_probe VALUES ('committed only in WAL')`); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // Simulate a terminated server without SQLite connection cleanup.
	}
	directory := t.TempDir()
	legacy := filepath.Join(directory, "video-site.db")
	command := exec.Command(os.Args[0], "-test.run=^TestDatabaseFilenameMigrationRecoversWAL$")
	command.Env = append(os.Environ(), "TEST_LEGACY_WAL_DATABASE="+legacy)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("seed uncheckpointed WAL: %v\n%s", err, output)
	}
	wal, err := os.Stat(legacy + "-wal")
	if err != nil || wal.Size() == 0 {
		t.Fatalf("test requires uncheckpointed WAL: %v", err)
	}
	target := filepath.Join(directory, "91.db")
	if err := migrateDatabaseFilename(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT value FROM migration_probe`).Scan(&value); err != nil || value != "committed only in WAL" {
		t.Fatalf("WAL data lost: %q, %v", value, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if _, err := os.Stat(legacy + suffix); !os.IsNotExist(err) {
			t.Fatalf("legacy SQLite file remains: %s, %v", suffix, err)
		}
	}
}

func TestDatabaseFilenameMigrationRejectsConflictsAndInvalidSources(t *testing.T) {
	for _, scenario := range []string{"both-databases", "target-wal", "corrupt-source", "source-symlink", "migration-locked"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			legacy := seedLegacyDatabase(t, directory)
			target := filepath.Join(directory, "91.db")
			switch scenario {
			case "both-databases":
				if err := os.WriteFile(target, []byte("another database"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "target-wal":
				if err := os.WriteFile(target+"-wal", []byte("unresolved WAL"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "corrupt-source":
				if err := os.WriteFile(legacy, []byte("not SQLite"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "source-symlink":
				if err := os.Rename(legacy, legacy+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(legacy+".original", legacy); err != nil {
					t.Skip(err)
				}
			case "migration-locked":
				lock, err := os.OpenFile(filepath.Join(directory, ".91-database-migration.lock"), os.O_CREATE|os.O_RDWR, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if err := filelock.TryLock(lock); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(legacy)
			if err := migrateDatabaseFilename(context.Background(), target); err == nil {
				t.Fatal("unsafe migration succeeded")
			}
			after, err := os.ReadFile(legacy)
			if err != nil || string(after) != string(before) {
				t.Fatalf("legacy data changed: %v", err)
			}
			if scenario == "both-databases" {
				body, _ := os.ReadFile(target)
				if string(body) != "another database" {
					t.Fatal("overwrote target")
				}
			} else if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("unexpected target database: %v", err)
			}
		})
	}
}

func TestDatabaseFilenameMigrationRefusesOpenLegacyDatabase(t *testing.T) {
	directory := t.TempDir()
	legacy := seedLegacyDatabase(t, directory)
	active, err := catalog.Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	target := filepath.Join(directory, "91.db")
	if err := migrateDatabaseFilename(ctx, target); err == nil || !strings.Contains(err.Error(), "stop other processes") {
		t.Fatalf("expected active database rejection: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("published active database: %v", err)
	}
	if _, err := active.GetUserByUsername(context.Background(), "existing-owner"); err != nil {
		t.Fatal(err)
	}
}
