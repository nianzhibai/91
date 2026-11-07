package catalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDoesNotRenameSiblingDatabase(t *testing.T) {
	directory := t.TempDir()
	legacy := filepath.Join(directory, "video-site.db")
	old, err := Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.CreateUser(context.Background(), "existing-owner", "hash", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := Open(filepath.Join(directory, "91.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if _, err := cat.GetUserByUsername(context.Background(), "existing-owner"); err == nil {
		t.Fatal("opened the sibling database instead of the requested file")
	}
	after, err := os.ReadFile(legacy)
	if err != nil || string(after) != string(before) {
		t.Fatalf("opening another database changed the sibling: %v", err)
	}
}
