// frontend-package prepares a verified web release for native and Docker builds.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nianzhibai/91/backend/internal/frontend"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

func main() {
	lockPath := flag.String("lock", "frontend.lock.json", "pinned frontend release")
	archive := flag.String("archive", "", "optional offline frontend.tar.gz")
	output := flag.String("output", "", "new output directory")
	flag.Parse()
	if err := prepare(*lockPath, *archive, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func prepare(lockPath, archive, output string) error {
	if output == "" {
		return errors.New("--output is required; existing directories are never overwritten")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		return errors.New("output directory must not already exist")
	}
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return err
	}
	var lock struct {
		Repository string `json:"repository"`
		Name       string `json:"name"`
		Version    string `json:"version"`
		SHA256     string `json:"sha256"`
	}
	if err = json.Unmarshal(data, &lock); err != nil {
		return err
	}
	request := frontend.ReleaseRequest{Repository: lock.Repository, Version: lock.Version}
	if err = request.Validate(); err != nil {
		return err
	}
	if len(lock.SHA256) != 64 {
		return errors.New("lock file requires the release SHA-256")
	}
	work, err := os.MkdirTemp("", "91-bundled-frontend-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	manager, err := frontend.New(work, "", httpapi.Version)
	if err != nil {
		return err
	}
	var release frontend.Release
	if archive != "" {
		file, err := os.Open(archive)
		if err != nil {
			return err
		}
		release, err = manager.Install(file, frontend.InstallOptions{
			SHA256: lock.SHA256, Repository: lock.Repository,
			Name: lock.Name, Version: strings.TrimPrefix(lock.Version, "v"),
		})
		file.Close()
		if err != nil {
			return err
		}
	} else {
		release, err = frontend.NewReleaseSource().Install(context.Background(), manager, request)
		if err != nil {
			return err
		}
	}
	if !strings.EqualFold(release.SHA256, lock.SHA256) || release.Name != lock.Name || "v"+release.Version != lock.Version {
		return errors.New("frontend package does not match the pinned release")
	}
	if err = manager.Activate(release.ID()); err != nil {
		return err
	}
	site, err := manager.ActiveDirectory()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return err
	}
	staged, err := os.MkdirTemp(filepath.Dir(output), ".frontend-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staged)
	err = filepath.WalkDir(site, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(site, name)
		if err != nil {
			return err
		}
		target := filepath.Join(staged, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		contents, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, contents, 0644)
	})
	if err != nil {
		return err
	}
	if err = os.Rename(staged, output); err != nil {
		return err
	}
	fmt.Printf("Prepared %s from %s (%s)\n", release.ID(), lock.Repository, release.SHA256)
	return nil
}
