package mediaimport

import (
	"context"
	"time"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

// FileSource acquires a durable local file. Publication only records its identity;
// it never copies or renames the source. Discard releases a failed acquisition.
type FileSource interface {
	Available() bool
	Fetch(ctx context.Context, job *catalog.RemoteUploadJob, report func(stage string, downloaded, total int64) error) (SourceFile, error)
	Discard(ctx context.Context, job *catalog.RemoteUploadJob) error
}
type SourceFile struct {
	Name, MIME            string
	Path, DriveID, FileID string
	Size                  int64
}

// SourceError contains a safe user-facing message; it never wraps credentials.
type SourceError struct {
	Message             string
	RetryAfter          time.Duration
	WaitForAvailability bool
}

func (e *SourceError) Error() string { return e.Message }

func (m *Manager) Wake()                        { m.signal() }
func AvailableBytes(path string) (int64, error) { return diskAvailableBytes(path) }
