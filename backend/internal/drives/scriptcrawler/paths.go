package scriptcrawler

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/nianzhibai/91/backend/internal/localpath"
)

// ScriptPath resolves an imported script_file relative to the import directory.
// script_path is reserved for independently managed external scripts.
func ScriptPath(credentials map[string]string, importDir string) (string, error) {
	if file := strings.TrimSpace(credentials["script_file"]); file != "" {
		if filepath.IsAbs(file) {
			return "", errors.New("script_file must be relative to the crawler script directory")
		}
		path, ok := localpath.Managed(importDir, file)
		if !ok {
			return "", errors.New("script_file escapes the crawler script directory")
		}
		return path, nil
	}
	path := strings.TrimSpace(credentials["script_path"])
	if path == "" {
		return "", errors.New("scriptcrawler: script path is required")
	}
	return filepath.Abs(path)
}

// SetScriptPath stores imported scripts as relative identities and external
// scripts as explicit absolute paths. The caller owns credentials.
func SetScriptPath(credentials map[string]string, importDir, path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if relative, ok := localpath.RelativeWithin(importDir, absolute); ok && relative != "." {
		credentials["script_file"] = filepath.ToSlash(relative)
		delete(credentials, "script_path")
	} else {
		credentials["script_path"] = absolute
		delete(credentials, "script_file")
	}
	return nil
}
