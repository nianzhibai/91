// Package telegramstorage serves Telegram library files from managed storage.
// Only catalogued opaque IDs are accepted; session files are never addressable.
package telegramstorage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/drives"
)

const DriveID = catalog.TelegramLocalDriveID

type Driver struct {
	cat  *catalog.Catalog
	root func() string
}

// root resolves the website's current shared directory even when receiving is
// disabled. Individual operations use one snapshot of this location.
func New(cat *catalog.Catalog, root func() string) *Driver { return &Driver{cat: cat, root: root} }
func (d *Driver) Kind() string                             { return DriveID }
func (d *Driver) ID() string                               { return DriveID }
func (d *Driver) RootID() string                           { return catalog.TelegramLibraryRootID }
func (d *Driver) Init(context.Context) error               { return nil }
func (d *Driver) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}

// RelativePath addresses only files within the library, never session state.
func RelativePath(file catalog.TelegramLocalFile) (string, error) {
	if file.FileID == "" {
		return "", errors.New("TG 视频路径无效")
	}
	for _, component := range []string{file.FileID, file.Directory} {
		if component == "" {
			continue
		}
		if filepath.Base(component) != component || strings.ContainsAny(component, "/\\\x00:\r\n") || component == "." || component == ".." {
			return "", errors.New("TG 视频路径无效")
		}
	}
	return filepath.Join("library", file.Directory, file.FileID), nil
}

// Path resolves a library identity under the website's storage root. Missing files
// retain their valid path so deletion and interrupted acquisitions are idempotent.
func Path(root string, file catalog.TelegramLocalFile) (string, error) {
	relative, err := RelativePath(file)
	if err != nil || !filepath.IsAbs(root) {
		return "", errors.New("TG 视频路径无效")
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("TG 视频存储目录不可用: %w", err)
	}
	library := filepath.Join(base, "library")
	resolved, err := filepath.EvalSymlinks(library)
	if err != nil {
		return "", fmt.Errorf("TG 视频存储目录不可用: %w", err)
	}
	if resolved != library {
		return "", errors.New("TG 视频存储目录不能包含符号链接")
	}
	if file.Directory != "" {
		info, err := os.Lstat(filepath.Join(library, file.Directory))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("无法检查 TG 视频分组目录: %w", err)
		}
		if err == nil && !info.IsDir() {
			return "", errors.New("TG 视频分组目录必须为普通目录，不能包含符号链接")
		}
	}
	path := filepath.Join(base, relative)
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("无法检查 TG 视频文件: %w", err)
	}
	if err == nil && !info.Mode().IsRegular() {
		return "", errors.New("TG 视频不是普通文件")
	}
	return path, nil
}

// Prepare creates only validated library directories before a new acquisition.
// Check each existing parent before creating a child so symlinks cannot redirect
// even an empty album directory outside managed storage.
func Prepare(root string, file catalog.TelegramLocalFile) (string, error) {
	if _, err := RelativePath(file); err != nil || !filepath.IsAbs(root) {
		return "", errors.New("TG 视频路径无效")
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("TG 视频存储目录不可用: %w", err)
	}
	if err := os.Mkdir(filepath.Join(base, "library"), 0750); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("无法创建 TG 视频存储目录: %w", err)
	}
	path, err := Path(base, file)
	if err != nil {
		return "", err
	}
	if file.Directory != "" {
		if err := os.Mkdir(filepath.Dir(path), 0750); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("无法创建 TG 视频分组目录: %w", err)
		}
	}
	return Path(base, file)
}

func (d *Driver) LocalPath(ctx context.Context, id string) (string, error) {
	f, err := d.cat.TelegramLocalFile(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %w", os.ErrNotExist, err)
	}
	if err != nil {
		return "", fmt.Errorf("无法读取 TG 视频位置: %w", err)
	}
	return Path(d.root(), f)
}
func (d *Driver) Stat(ctx context.Context, id string) (*drives.Entry, error) {
	path, err := d.LocalPath(ctx, id)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, os.ErrNotExist
	}
	return &drives.Entry{ID: id, Name: id, Size: info.Size(), ModTime: info.ModTime()}, nil
}
func (d *Driver) StreamURL(ctx context.Context, id string) (*drives.StreamLink, error) {
	path, err := d.LocalPath(ctx, id)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return nil, os.ErrNotExist
	}
	// A local lookup is cheap and its root can change through configuration.
	// Leave expiry unset so the playback proxy does not cache a physical path.
	return &drives.StreamLink{URL: path}, nil
}
func (d *Driver) Remove(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := d.cat.TelegramLocalFile(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("无法读取 TG 视频位置: %w", err)
	}
	path, err := Path(d.root(), f)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("无法删除 TG 视频，请检查视频目录写入权限: %w", err)
	}
	if err := os.Remove(path + ".part"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("无法清理未完成的 TG 下载: %w", err)
	}
	if f.Directory != "" {
		// Remove an empty album directory. Other files keep their directory;
		// leftover empty directories must not make a completed deletion fail.
		_ = os.Remove(filepath.Dir(path))
	}
	return d.cat.DeleteTelegramLocalFile(ctx, id)
}
