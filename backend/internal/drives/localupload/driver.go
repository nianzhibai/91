package localupload

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

const DriveID = "local-upload"

type Driver struct {
	uploadDirPath string
}

func New(uploadDir string) *Driver {
	return &Driver{uploadDirPath: uploadDir}
}

func (d *Driver) Kind() string { return "local-upload" }

func (d *Driver) ID() string { return DriveID }

func (d *Driver) Init(context.Context) error {
	return os.MkdirAll(d.uploadDir(), 0o755)
}

func (d *Driver) List(context.Context, string) ([]drives.Entry, error) {
	return nil, drives.ErrNotSupported
}

func (d *Driver) Stat(ctx context.Context, fileID string) (*drives.Entry, error) {
	path, err := d.uploadPath(fileID)
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
	path, err := d.uploadPath(fileID)
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
	path, err := d.uploadPath(fileID)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return errors.New("localupload: refusing to remove directory")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (d *Driver) RootID() string { return d.uploadDir() }

func (d *Driver) uploadDir() string {
	return d.uploadDirPath
}

var _ drives.Remover = (*Driver)(nil)

func (d *Driver) uploadPath(fileID string) (string, error) {
	if strings.TrimSpace(fileID) == "" || filepath.Base(fileID) != fileID {
		return "", errors.New("invalid upload file id")
	}
	root, err := filepath.Abs(d.uploadDir())
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(root, fileID))
	if err != nil {
		return "", err
	}
	if path != root && !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return "", errors.New("invalid upload file id")
	}
	return path, nil
}

func (d *Driver) LocalPath(ctx context.Context, id string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return d.uploadPath(id)
}
