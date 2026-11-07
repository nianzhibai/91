package p115

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/nianzhibai/91/backend/internal/applog"
	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/streamhttp"
)

const (
	playbackPreparationCooldown   = 30 * time.Minute
	playbackPreparationMaxEntries = 1024
)

type playbackPreparationAttempt struct {
	done  chan struct{}
	until time.Time
	err   error
}

type playbackVideoInfo struct {
	pickCode       string
	FileID         string `json:"file_id"`
	FileStatus     *int   `json:"file_status"`
	SHA1           string `json:"sha1"`
	VideoPushState bool   `json:"video_push_state"`
}

func (i playbackVideoInfo) needsTranscode(fileID string) bool {
	_, err := hex.DecodeString(i.SHA1)
	return i.FileID == fileID && i.FileStatus != nil && *i.FileStatus == 0 && len(i.SHA1) == 40 && err == nil
}

func (d *Driver) playbackVideoInfo(ctx context.Context, fileID string, inspectOnly bool) (playbackVideoInfo, error) {
	pickCode := d.rememberedPickCode(fileID)
	if pickCode == "" {
		var err error
		pickCode, err = d.lookupPickCode(ctx, fileID)
		if err != nil {
			return playbackVideoInfo{}, err
		}
	}
	query := url.Values{"pickcode": {pickCode}}
	if inspectOnly {
		query.Set("local", "1")
	}
	var info playbackVideoInfo
	err := d.playbackRequest(ctx, http.MethodGet, "https://webapi.115.com/files/video?"+query.Encode(), nil, &info)
	info.pickCode = pickCode
	info.SHA1 = strings.ToUpper(strings.TrimSpace(info.SHA1))
	return info, err
}

// PreparePlayback only submits the account's VIP acceleration. The separate
// query path never submits jobs, and provider state is checked after a restart.
func (d *Driver) PreparePlayback(ctx context.Context, fileID string) ([]drives.PlaybackSource, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return nil, errors.New("115 playback: empty file ID")
	}
	info, err := d.playbackVideoInfo(ctx, fileID, true)
	if err != nil {
		return nil, err
	}
	if !info.needsTranscode(fileID) {
		return d.PlaybackSources(ctx, fileID)
	}
	if !info.VideoPushState {
		if err := d.submitPlaybackTranscode(ctx, fileID, info.SHA1); err != nil {
			return nil, err
		}
	}
	return []drives.PlaybackSource{{
		ID: "cloud", Label: "兼容播放", Kind: "transcoded", Protocol: "hls", Status: "processing",
		MediaType: "application/vnd.apple.mpegurl", Message: "视频正在转码，完成后将自动播放",
	}}, nil
}

func (d *Driver) submitPlaybackTranscode(ctx context.Context, fileID, sha1 string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, now := fileID+"/"+sha1, time.Now()
	d.playbackMu.Lock()
	for key, attempt := range d.playbackAttempts {
		if now.After(attempt.until) {
			delete(d.playbackAttempts, key)
		}
	}
	if attempt := d.playbackAttempts[key]; attempt != nil {
		d.playbackMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-attempt.done:
			return attempt.err
		}
	}
	if len(d.playbackAttempts) >= playbackPreparationMaxEntries {
		d.playbackMu.Unlock()
		return errors.New("115 playback: too many pending preparations")
	}
	attempt := &playbackPreparationAttempt{done: make(chan struct{}), until: now.Add(playbackPreparationCooldown)}
	d.playbackAttempts[key] = attempt
	d.playbackMu.Unlock()

	// The official player first requests compatibility mode (without local=1),
	// which can enqueue normal conversion, before applying VIP acceleration.
	// Both operations belong to this explicit, deduplicated preparation path.
	info, err := d.playbackVideoInfo(ctx, fileID, false)
	if err == nil && info.FileStatus != nil && *info.FileStatus == 1 {
		// Conversion completed while this viewer was requesting preparation.
	} else if err == nil && info.needsTranscode(fileID) && info.SHA1 == sha1 {
		if !info.VideoPushState {
			err = d.playbackRequest(ctx, http.MethodPost, "https://115.com/?ct=play&ac=push", url.Values{
				"op": {"vip_push"}, "pickcode": {info.pickCode}, "sha1": {sha1},
			}, nil)
		}
	} else if err == nil {
		err = errors.New("115 playback: conversion state could not be confirmed")
	}
	// A timeout can occur after 115 accepted the request. Retain the attempt
	// even then: refreshing metadata must not blindly repeat a submission.
	d.playbackMu.Lock()
	attempt.err = err
	close(attempt.done)
	d.playbackMu.Unlock()
	return err
}

func (d *Driver) playbackRequest(ctx context.Context, method, endpoint string, form url.Values, result any) error {
	ctx, cancel := context.WithTimeout(ctx, p115ReadTimeout)
	defer cancel()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", d.cookie)
	req.Header.Set("User-Agent", p115HLSUserAgent)
	req.Header.Set("Referer", p115HLSReferer)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := d.hlsClient
	if client == nil {
		client = streamhttp.NewClient(20 * time.Second)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("115 playback request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return wrap115StreamTransientError("115 playback", fmt.Errorf("status=%d", resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, p115HLSMaxPlaylist+1))
	if err != nil {
		return err
	}
	if len(data) > p115HLSMaxPlaylist {
		return errors.New("115 playback: response too large")
	}
	var status sdk.BasicResp
	if err := json.Unmarshal(data, &status); err != nil {
		return errors.New("115 playback: invalid provider response")
	}
	if !status.State {
		if int(status.Errno) == 429 || status.ErrNo == 429 || int(status.Errno) == 405 || status.ErrNo == 405 {
			return wrap115StreamTransientError("115 playback", errors.New("status=429"))
		}
		reason := strings.TrimSpace(status.Error + " " + status.Msg)
		reason = applog.Redact(reason)
		if len(reason) > 256 {
			reason = reason[:256]
		}
		err := status.Err()
		if errors.Is(err, sdk.ErrDownloadFileNotExistOrHasDeleted) || errors.Is(err, sdk.ErrPickCodeNotExist) || errors.Is(err, sdk.ErrPickCodeIsNotExistOrHasDeleted) {
			return fmt.Errorf("115 playback: %w", os.ErrNotExist)
		}
		return fmt.Errorf("115 playback: %s: %w", reason, err)
	}
	if result != nil {
		return json.Unmarshal(data, result)
	}
	return nil
}

var _ drives.PlaybackSourcePreparer = (*Driver)(nil)
