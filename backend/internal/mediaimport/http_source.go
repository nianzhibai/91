package mediaimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/catalog"
)

type downloadMetadata struct {
	Size               int64
	Total              int64
	ContentDisposition string
	ContentType        string
	FinalURL           *url.URL
	OriginalURL        *url.URL
}

func (m *Manager) download(
	ctx context.Context,
	job *catalog.RemoteUploadJob,
	dst *os.File,
) (downloadMetadata, error) {
	u, err := m.validateURL(ctx, job.SourceURL)
	if err != nil {
		return downloadMetadata{}, err
	}
	bodyCtx, cancelBody := context.WithCancel(ctx)
	defer cancelBody()
	req, err := http.NewRequestWithContext(bodyCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return downloadMetadata{}, taskError("无法创建远程下载请求")
	}
	req.Header.Set("Accept-Encoding", "identity")
	response, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return downloadMetadata{}, ctx.Err()
		}
		applog.Error(ctx, "Remote video connection failed", err, applog.Fields{Stage: "connect"})
		return downloadMetadata{}, taskError("远程视频连接失败")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return downloadMetadata{}, taskError(
			"远程服务器返回 HTTP " + strconv.Itoa(response.StatusCode),
		)
	}
	contentType := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Type")))
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = strings.ToLower(mediaType)
	}
	if isHLSContentType(contentType) ||
		(response.Request != nil && response.Request.URL != nil && isHLSPath(response.Request.URL.EscapedPath())) {
		return downloadMetadata{}, taskError("不支持 HLS/m3u8 链接")
	}

	total := response.ContentLength
	if total < 0 {
		total = 0
	}
	if total > 0 {
		if err := m.ensureDiskSpace(total); err != nil {
			return downloadMetadata{}, err
		}
	}
	if err := m.catalog.UpdateRemoteUploadProgress(ctx, job.ID, 0, total); err != nil {
		return downloadMetadata{}, err
	}

	var idleFired atomic.Bool
	watchdog := time.AfterFunc(m.idleTimeout, func() {
		idleFired.Store(true)
		cancelBody()
	})
	defer watchdog.Stop()

	buffer := make([]byte, 1<<20)
	var downloaded int64
	lastProgress := time.Now()
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			watchdog.Reset(m.idleTimeout)
			if err := m.ensureDiskSpace(int64(n)); err != nil {
				return downloadMetadata{}, err
			}
			written, writeErr := dst.Write(buffer[:n])
			if writeErr != nil || written != n {
				if writeErr == nil {
					writeErr = io.ErrShortWrite
				}
				applog.Error(ctx, "Write download file failed", writeErr, applog.Fields{Stage: "write_file"})
				return downloadMetadata{}, taskError("无法写入下载文件")
			}
			downloaded += int64(n)
			if time.Since(lastProgress) >= time.Second {
				if err := m.catalog.UpdateRemoteUploadProgress(ctx, job.ID, downloaded, total); err != nil {
					return downloadMetadata{}, err
				}
				lastProgress = time.Now()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			if idleFired.Load() {
				return downloadMetadata{}, taskError(
					fmt.Sprintf("远程服务器连续 %d 秒未发送数据", int(m.idleTimeout.Seconds())),
				)
			}
			if ctx.Err() != nil {
				return downloadMetadata{}, ctx.Err()
			}
			applog.Error(ctx, "Read remote video failed", readErr, applog.Fields{Stage: "read"})
			return downloadMetadata{}, taskError("远程视频下载中断")
		}
	}
	if err := dst.Sync(); err != nil {
		applog.Error(ctx, "Sync download file failed", err, applog.Fields{Stage: "sync"})
		return downloadMetadata{}, taskError("无法同步下载文件")
	}

	finalURL := u
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL
	}
	return downloadMetadata{
		Size:               downloaded,
		Total:              total,
		ContentDisposition: response.Header.Get("Content-Disposition"),
		ContentType:        contentType,
		FinalURL:           finalURL,
		OriginalURL:        u,
	}, nil
}
