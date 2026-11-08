package frontend

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const MaxArchiveBytes int64 = 128 << 20
const maxExpandedBytes int64 = 256 << 20
const maxFiles = 10000

func unpack(reader io.Reader, directory string) error {
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var expanded int64
	seen := map[string]bool{}
	for count := 0; ; count++ {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(header.Name, "/")
		if count >= maxFiles || !fs.ValidPath(name) || name == "." || path.Clean(name) != name || strings.ContainsAny(name, "\\:\x00") || seen[name] {
			return fmt.Errorf("invalid or duplicate frontend archive path: %q", header.Name)
		}
		seen[name] = true
		if header.Size < 0 || header.Size > 64<<20 || expanded+header.Size > maxExpandedBytes {
			return errors.New("frontend archive exceeds extraction limits")
		}
		expanded += header.Size
		target := filepath.Join(directory, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, tr)
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		default:
			return errors.New("frontend archives may only contain regular files and directories")
		}
	}
	// Consume the gzip trailer so truncated or corrupt archives cannot be activated.
	_, err = io.Copy(io.Discard, gz)
	return err
}

func treeDigest(directory string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(directory, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("frontend contains non-regular files")
		}
		relative, err := filepath.Rel(directory, name)
		if err != nil {
			return err
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		fileHash := sha256.New()
		_, copyErr := io.Copy(fileHash, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%x\n", filepath.ToSlash(relative), fileHash.Sum(nil))
		return nil
	})
	return hex.EncodeToString(hash.Sum(nil)), err
}
