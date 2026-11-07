package yun139

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

type uploadPart struct {
	Number      int   `json:"partNumber"`
	Size        int64 `json:"partSize"`
	HashContext struct {
		Offset int64 `json:"partOffset"`
	} `json:"parallelHashCtx"`
}

type uploadURL struct {
	Number int    `json:"partNumber"`
	URL    string `json:"uploadUrl"`
}

type uploadResult struct {
	Data struct {
		FileID      string      `json:"fileId"`
		FileName    string      `json:"fileName"`
		UploadID    string      `json:"uploadId"`
		Exists      bool        `json:"exist"`
		RapidUpload bool        `json:"rapidUpload"`
		Parts       []uploadURL `json:"partInfos"`
	} `json:"data"`
}

func (d *Driver) Upload(ctx context.Context, parentID, name string, r io.Reader, size int64) (string, error) {
	if r == nil || size < 0 || !validName(name) {
		return "", errors.New("yun139 upload: invalid reader, size or name")
	}
	if parentID == "" {
		parentID = d.rootID
	}
	// Hash and spool once: arbitrary input streams become replayable without
	// retaining video-sized buffers in memory. Every exit removes the spool.
	if d.uploadTempDir != "" {
		if err := os.MkdirAll(d.uploadTempDir, 0o755); err != nil {
			return "", err
		}
	}
	tmp, err := os.CreateTemp(d.uploadTempDir, "yun139-upload-*")
	if err != nil {
		return "", fmt.Errorf("yun139 upload: create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha256.New()
	copied, err := io.Copy(io.MultiWriter(tmp, hash), &contextReader{ctx: ctx, r: r})
	if err != nil {
		return "", fmt.Errorf("yun139 upload: stage input: %w", err)
	}
	if copied != size {
		return "", fmt.Errorf("yun139 upload: size mismatch: declared %d, read %d", size, copied)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	partSize := d.partSize
	if partSize <= 0 {
		partSize = 100 << 20
		if size > 30<<30 {
			partSize = 512 << 20
		}
	}
	parts := make([]uploadPart, 0)
	for offset, number := int64(0), 1; offset < size || number == 1; number++ {
		part := uploadPart{Number: number, Size: min(partSize, size-offset)}
		part.HashContext.Offset = offset
		parts = append(parts, part)
		offset += partSize
	}
	first := parts[:min(100, len(parts))]
	var result uploadResult
	if err := d.post(ctx, "/file/create", map[string]any{
		"contentHash": digest, "contentHashAlgorithm": "SHA256", "contentType": "application/octet-stream",
		"parallelUpload": false, "partInfos": first, "size": size, "parentFileId": parentID,
		"name": name, "type": "file", "fileRenameMode": "auto_rename",
	}, &result); err != nil {
		return "", err
	}
	if result.Data.FileID == "" {
		return "", errors.New("yun139 upload: empty file ID")
	}
	if !result.Data.Exists && (!result.Data.RapidUpload || len(result.Data.Parts) > 0) {
		if result.Data.UploadID == "" {
			return "", errors.New("yun139 upload: empty upload ID")
		}
		if err := d.uploadParts(ctx, tmp, first, result.Data.Parts); err != nil {
			return "", err
		}
		for start := 100; start < len(parts); start += 100 {
			batch := parts[start:min(start+100, len(parts))]
			_, account, err := d.ensureAuthorization(ctx)
			if err != nil {
				return "", err
			}
			var more uploadResult
			if err := d.post(ctx, "/file/getUploadUrl", map[string]any{
				"fileId": result.Data.FileID, "uploadId": result.Data.UploadID, "partInfos": batch,
				"commonAccountInfo": map[string]any{"account": account, "accountType": 1},
			}, &more); err != nil {
				return "", err
			}
			if err := d.uploadParts(ctx, tmp, batch, more.Data.Parts); err != nil {
				return "", err
			}
		}
		if err := d.post(ctx, "/file/complete", map[string]string{
			"fileId": result.Data.FileID, "uploadId": result.Data.UploadID,
			"contentHash": digest, "contentHashAlgorithm": "SHA256",
		}, nil); err != nil {
			return "", err
		}
	}
	// Preserve the provider's chosen name on a conflict; uploading a new file
	// must never silently trash an existing file with the same name.
	actualName := result.Data.FileName
	if actualName == "" {
		actualName = name
	}
	d.fileMu.Lock()
	d.files[result.Data.FileID] = drives.Entry{ID: result.Data.FileID, Name: actualName, Size: size, ParentID: parentID}
	d.fileMu.Unlock()
	return result.Data.FileID, nil
}

func (d *Driver) uploadParts(ctx context.Context, file *os.File, parts []uploadPart, urls []uploadURL) error {
	if len(urls) != len(parts) {
		return errors.New("yun139 upload: incomplete part URLs")
	}
	byNumber := make(map[int]string, len(urls))
	for _, part := range urls {
		if byNumber[part.Number] != "" || !validHTTPURL(part.URL) {
			return errors.New("yun139 upload: invalid or duplicate part URL")
		}
		byNumber[part.Number] = part.URL
	}
	for _, part := range parts {
		endpoint := byNumber[part.Number]
		if endpoint == "" {
			return fmt.Errorf("yun139 upload: missing URL for part %d", part.Number)
		}
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				timer := time.NewTimer(time.Duration(attempt) * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, io.NewSectionReader(file, part.HashContext.Offset, part.Size))
			if err != nil {
				return err
			}
			req.ContentLength = part.Size
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Origin", "https://yun.139.com")
			req.Header.Set("Referer", "https://yun.139.com/")
			resp, err := d.uploadClient.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// The signed upload URL is a credential: do not include url.Error.
				lastErr = errors.New("yun139 upload: part request failed")
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				lastErr = nil
				break
			}
			lastErr = fmt.Errorf("yun139 upload: part %d HTTP status=%d", part.Number, resp.StatusCode)
			if resp.StatusCode == http.StatusTooManyRequests {
				return &drives.RateLimitError{Provider: Kind, RetryAfter: 10 * time.Minute, Err: lastErr}
			}
			if resp.StatusCode < 500 {
				return lastErr
			}
		}
		if lastErr != nil {
			return lastErr
		}
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
