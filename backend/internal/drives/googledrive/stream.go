package googledrive

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

var _ drives.StreamRequester = (*Driver)(nil)

// RequestStream keeps cached links usable across token rotations. The proxy
// supplies the URL and Range; authentication and recovery belong to the driver.
func (d *Driver) RequestStream(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if err := d.linkCooldownError(time.Now()); err != nil {
		return nil, err
	}
	resp, err := d.requestStream(req)
	if wait, limited := drives.RateLimitRetryAfter(err); limited {
		until := d.pauseLinkCooldown(wait)
		log.Printf("[googledrive] media stream cooling down drive=%s until=%s err=%v", d.id, until.Format(time.RFC3339), err)
	}
	return resp, err
}

func (d *Driver) requestStream(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		tokens := d.tokenSnapshot()
		upstream := req.Clone(req.Context())
		upstream.Header.Set("Authorization", "Bearer "+tokens.access)
		resp, err := d.mediaClient.Do(upstream)
		if err != nil {
			return nil, fmt.Errorf("googledrive media request: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			if err := d.refresh(req.Context(), tokens); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode >= http.StatusBadRequest && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			return nil, mediaResponseError(resp)
		}
		return resp, nil
	}
}

func mediaResponseError(resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var payload apiErrorResp
	_ = json.Unmarshal(body, &payload)
	if isGoogleHTTPRateLimit(resp.StatusCode, resp.Header, payload.Error) {
		return googleHTTPRateLimitError(resp.StatusCode, resp.Header, body, payload.Error.Message)
	}
	err := fmt.Errorf("googledrive media: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &drives.ProviderError{Kind: drives.ProviderErrorAuth, Err: err}
	case resp.StatusCode >= http.StatusInternalServerError:
		return &drives.ProviderError{Kind: drives.ProviderErrorUnavailable, Err: err}
	default:
		return err
	}
}
