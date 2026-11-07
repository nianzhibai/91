package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoragePathsCommandUsesRuntimePathsWithoutCreatingData(t *testing.T) {
	for _, data := range []string{
		"storage: {}\n",
		"storage: {data_dir: './data store', db_dir: './databases'}\n",
	} {
		t.Run(data, func(t *testing.T) {
			root := t.TempDir()
			filename := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			handled, err := runServerCommand([]string{"storage-paths", "--config", filename}, nil, &output, io.Discard)
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			var paths map[string]string
			if err := json.Unmarshal(output.Bytes(), &paths); err != nil {
				t.Fatal(err)
			}
			workingDir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			wantData := filepath.Join(workingDir, "data")
			wantDB := filepath.Join(wantData, "91.db")
			if strings.Contains(data, "data store") {
				wantData = filepath.Join(workingDir, "data store")
				wantDB = filepath.Join(workingDir, "databases", "91.db")
			}
			if paths["data_dir"] != wantData || paths["db_path"] != wantDB {
				t.Fatalf("paths=%v, want data=%q database=%q", paths, wantData, wantDB)
			}
			after, err := os.ReadFile(filename)
			if err != nil || string(after) != data {
				t.Fatalf("configuration changed: %s %v", after, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("command created files: %v %v", entries, err)
			}
		})
	}
}

func TestStoragePathsCommandRejectsMissingConfigAndInvalidArguments(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	for _, args := range [][]string{{"--config", missing}, {"--unknown"}, {"unexpected"}} {
		var output bytes.Buffer
		if err := runStoragePathsCommand(args, &output, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
		if output.Len() != 0 {
			t.Fatal("failed command printed a storage manifest")
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing configuration was created: %v", err)
	}
}

func TestStoragePathsSubprocessDoesNotStartServer(t *testing.T) {
	if os.Getenv("VIDEO_TEST_STORAGE_PATHS") == "1" {
		for index, arg := range os.Args {
			if arg == "--" {
				os.Args = append(os.Args[:1], os.Args[index+1:]...)
				main()
				os.Exit(0)
			}
		}
		t.Fatal("missing command arguments")
	}
	root := t.TempDir()
	filename := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(filename, []byte("storage: {data_dir: './app data', db_dir: './databases'}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStoragePathsSubprocessDoesNotStartServer$", "--", "storage-paths", "--config", filename)
	command.Dir = root
	command.Env = append(os.Environ(), "VIDEO_TEST_STORAGE_PATHS=1")
	var output, errorOutput bytes.Buffer
	command.Stdout, command.Stderr = &output, &errorOutput
	if err := command.Run(); err != nil {
		t.Fatalf("command failed: %v; stderr=%s", err, errorOutput.String())
	}
	var paths map[string]string
	if err := json.Unmarshal(output.Bytes(), &paths); err != nil {
		t.Fatal(err)
	}
	if paths["data_dir"] != filepath.Join(root, "app data") || paths["db_path"] != filepath.Join(root, "databases", "91.db") {
		t.Fatalf("paths=%v", paths)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("command ran startup or created files: %v %v", entries, err)
	}
}
