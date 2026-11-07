// Package scriptcrawler provides a generic local drive for script-based
// crawlers. A crawler script discovers videos; the Go runner downloads them
// into this drive and the existing preview/fingerprint workers consume them
// through the normal drives.Drive interface.
package scriptcrawler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

const Kind = "scriptcrawler"

type Config struct {
	ID      string
	RootDir string
}

type Driver struct {
	id      string
	rootDir string
}

func New(c Config) *Driver {
	return &Driver{id: c.ID, rootDir: c.RootDir}
}

func (d *Driver) Kind() string { return Kind }

func (d *Driver) ID() string { return d.id }

func (d *Driver) RootID() string { return "/" }

func (d *Driver) Init(context.Context) error {
	if strings.TrimSpace(d.id) == "" {
		return errors.New("scriptcrawler: empty drive id")
	}
	if strings.TrimSpace(d.rootDir) == "" {
		return errors.New("scriptcrawler: empty root dir")
	}
	for _, sub := range []string{"videos", "thumbs", "output", ".crawl"} {
		if err := os.MkdirAll(filepath.Join(d.rootDir, sub), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) RootDir() string { return d.rootDir }

func (d *Driver) VideosDir() string { return filepath.Join(d.rootDir, "videos") }

func (d *Driver) ThumbsDir() string { return filepath.Join(d.rootDir, "thumbs") }

func (d *Driver) OutputDir() string { return filepath.Join(d.rootDir, "output") }

func (d *Driver) CrawlDir() string { return filepath.Join(d.rootDir, ".crawl") }

func (d *Driver) VideoPath(fileID string) (string, error) {
	return safeJoin(d.VideosDir(), fileID)
}

func (d *Driver) ThumbPath(fileID string) (string, error) {
	return safeJoin(d.ThumbsDir(), fileID)
}

func (d *Driver) OutputPath(fileName string) (string, error) {
	return safeJoin(d.OutputDir(), fileName)
}

func (d *Driver) List(context.Context, string) ([]drives.Entry, error) {
	entries, err := os.ReadDir(d.VideosDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]drives.Entry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, drives.Entry{
			ID:      e.Name(),
			Name:    e.Name(),
			Size:    info.Size(),
			IsDir:   false,
			ModTime: info.ModTime(),
		})
	}
	return out, nil
}

func (d *Driver) Stat(ctx context.Context, fileID string) (*drives.Entry, error) {
	path, err := d.VideoPath(fileID)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &drives.Entry{
		ID:      fileID,
		Name:    fileID,
		Size:    info.Size(),
		IsDir:   info.IsDir(),
		ModTime: info.ModTime(),
	}, nil
}

func (d *Driver) StreamURL(ctx context.Context, fileID string) (*drives.StreamLink, error) {
	path, err := d.VideoPath(fileID)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() || info.Size() == 0 {
		return nil, os.ErrNotExist
	}
	return &drives.StreamLink{
		URL:     path,
		Expires: time.Now().Add(24 * time.Hour),
	}, nil
}

func (d *Driver) Remove(ctx context.Context, fileID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	videoPath, err := d.VideoPath(fileID)
	if err != nil {
		return err
	}
	info, err := os.Stat(videoPath)
	if err != nil {
		if os.IsNotExist(err) {
			removeThumbCandidates(d.ThumbPath, strings.TrimSuffix(fileID, filepath.Ext(fileID)))
			return nil
		}
		return err
	}
	if info.IsDir() {
		return errors.New("scriptcrawler: refusing to remove directory")
	}
	if err := os.Remove(videoPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	removeThumbCandidates(d.ThumbPath, strings.TrimSuffix(fileID, filepath.Ext(fileID)))
	return nil
}

func removeThumbCandidates(pathFor func(string) (string, error), stem string) {
	stem = strings.TrimSpace(stem)
	if stem == "" {
		return
	}
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp"} {
		path, err := pathFor(stem + ext)
		if err != nil {
			continue
		}
		_ = os.Remove(path)
	}
}

func safeJoin(root, fileID string) (string, error) {
	id := strings.TrimSpace(fileID)
	if id == "" || filepath.Base(id) != id {
		return "", errors.New("scriptcrawler: invalid file id")
	}
	if strings.TrimSpace(root) == "" {
		return "", errors.New("scriptcrawler: empty root")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	pathAbs, err := filepath.Abs(filepath.Join(rootAbs, id))
	if err != nil {
		return "", err
	}
	if pathAbs != rootAbs && !strings.HasPrefix(pathAbs, rootAbs+string(os.PathSeparator)) {
		return "", errors.New("scriptcrawler: file id escapes root")
	}
	return pathAbs, nil
}

var _ drives.Drive = (*Driver)(nil)
var _ drives.Remover = (*Driver)(nil)
