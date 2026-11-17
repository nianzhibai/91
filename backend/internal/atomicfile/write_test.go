package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteJSONReplacesFileWithoutLeavingTemporaries(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(directory, "state.json")
	if err := WriteJSON(path, map[string]int{"version": 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(path, map[string]int{"version": 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{\n  \"version\": 2\n}\n" {
		t.Fatalf("content = %q", data)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory entries = %d, want only the target file", len(entries))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestWriteJSONRejectsUnencodableValueWithoutTouchingTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(path, func() {}, 0o600); err == nil {
		t.Fatal("WriteJSON accepted an unencodable value")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "previous" {
		t.Fatalf("target changed to %q", data)
	}
}
