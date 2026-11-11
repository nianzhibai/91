// frontend-package prepares the verified frontend bundle for native and Docker builds.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nianzhibai/91/backend/internal/frontend"
	"github.com/nianzhibai/91/backend/internal/httpapi"
)

type pinnedFrontend struct {
	Repository string `json:"repository"`
	Name       string `json:"name"`
	Version    string `json:"version"` // Release tag, such as v1.0.0.
	SHA256     string `json:"sha256"`
}

type lockFile struct {
	Default   string           `json:"default"` // Name of the frontend served on first start.
	Frontends []pinnedFrontend `json:"frontends"`
}

func main() {
	lockPath := flag.String("lock", "frontend.lock.json", "pinned frontend releases")
	archives := flag.String("archives", "", "optional directory with <name>.tar.gz for every pinned frontend")
	output := flag.String("output", "", "new bundle directory")
	flag.Parse()
	if err := prepare(*lockPath, *archives, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func readLock(lockPath string) (lockFile, error) {
	var lock lockFile
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return lock, err
	}
	if err = json.Unmarshal(data, &lock); err != nil {
		return lock, err
	}
	if len(lock.Frontends) == 0 {
		return lock, errors.New("lock file pins no frontends")
	}
	names := map[string]bool{}
	for _, pinned := range lock.Frontends {
		if err := (frontend.ReleaseRequest{Repository: pinned.Repository, Version: pinned.Version}).Validate(); err != nil {
			return lock, err
		}
		if pinned.Version == "" || len(pinned.SHA256) != 64 {
			return lock, fmt.Errorf("%s requires a release tag and SHA-256", pinned.Name)
		}
		if names[pinned.Name] {
			return lock, fmt.Errorf("%s is pinned more than once", pinned.Name)
		}
		names[pinned.Name] = true
	}
	if !names[lock.Default] {
		return lock, errors.New("the default frontend must be pinned")
	}
	return lock, nil
}

func prepare(lockPath, archives, output string) error {
	if output == "" {
		return errors.New("--output is required; existing directories are never overwritten")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		return errors.New("output directory must not already exist")
	}
	lock, err := readLock(lockPath)
	if err != nil {
		return err
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
	ids := make([]string, 0, len(lock.Frontends))
	defaultID := ""
	for _, pinned := range lock.Frontends {
		release, err := install(manager, pinned, archives)
		if err != nil {
			return fmt.Errorf("%s: %w", pinned.Name, err)
		}
		if !strings.EqualFold(release.SHA256, pinned.SHA256) || release.Name != pinned.Name || "v"+release.Version != pinned.Version {
			return fmt.Errorf("%s does not match the pinned release", pinned.Name)
		}
		ids = append(ids, release.ID())
		if pinned.Name == lock.Default {
			defaultID = release.ID()
		}
		fmt.Printf("Verified %s from %s (%s)\n", release.ID(), pinned.Repository, release.SHA256)
	}
	if err := manager.WriteBundle(output, defaultID, ids); err != nil {
		return err
	}
	fmt.Printf("Prepared %d frontends; %s is the default\n", len(ids), defaultID)
	return nil
}

func install(manager *frontend.Manager, pinned pinnedFrontend, archives string) (frontend.Release, error) {
	if archives == "" {
		return frontend.NewReleaseSource().Install(context.Background(), manager, frontend.ReleaseRequest{Repository: pinned.Repository, Version: pinned.Version})
	}
	// Offline builds must provide every pinned archive; nothing is downloaded.
	file, err := os.Open(filepath.Join(archives, pinned.Name+".tar.gz"))
	if err != nil {
		return frontend.Release{}, err
	}
	defer file.Close()
	return manager.Install(file, frontend.InstallOptions{
		SHA256: pinned.SHA256, Repository: pinned.Repository,
		Name: pinned.Name, Version: strings.TrimPrefix(pinned.Version, "v"),
	})
}
