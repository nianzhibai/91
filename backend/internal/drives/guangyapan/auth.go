package guangyapan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

const authMaxAttempts = 3

func isRejectedCredential(err error) bool {
	var provider *drives.ProviderError
	return errors.As(err, &provider) && provider.Kind == drives.ProviderErrorAuth
}

// requestAccount retries only the credential validation/refresh endpoints.
// SMS and QR requests have separate state transitions and must not inherit
// these retries. A lost refresh response may already have rotated the token,
// so transport failures on POST are retried only when dialing failed.
func (d *Driver) requestAccount(ctx context.Context, operation, method, path, accessToken string, body, out any) error {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("guangyapan %s: %w", operation, err)
		}
		if err := d.waitAPIRate(ctx, "account:"+path); err != nil {
			return fmt.Errorf("guangyapan %s: %w", operation, err)
		}
		request := d.accountClient.R().SetContext(ctx)
		if accessToken != "" {
			request.SetHeader("Authorization", "Bearer "+accessToken)
		}
		if body != nil {
			request.SetBody(body)
		}
		resp, requestErr := request.Execute(method, path)
		var failure error
		var retry bool
		var retryAfter time.Duration
		if requestErr != nil {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("guangyapan %s: %w", operation, err)
			}
			failure = &drives.ProviderError{
				Kind: drives.ProviderErrorUnavailable,
				Err:  fmt.Errorf("guangyapan %s: network request failed: %w", operation, requestErr),
			}
			retry = retryAccountTransport(method, requestErr)
		} else {
			status := resp.StatusCode()
			var account tokenResp
			decodeErr := json.Unmarshal(resp.Body(), &account)
			message := d.accountResponseMessage(account)
			retryAfter = parseRetryAfterHeader(resp.Header().Get("Retry-After"))
			if retryAfter > 0 {
				message += fmt.Sprintf(" retry_after=%s", retryAfter)
			}
			if decodeErr != nil {
				message += fmt.Sprintf(" invalid error response: %v", decodeErr)
			}
			if status == http.StatusTooManyRequests || status == 509 || account.ErrorCode == http.StatusTooManyRequests {
				return d.guangYaPanRateLimitError("account:"+path, resp.Header().Get("Retry-After"), status, account.ErrorCode, message)
			}
			if status >= 400 || account.Error != "" || account.ErrorCode != 0 {
				kind := accountErrorKind(status, account.Error)
				failure = &drives.ProviderError{
					Kind: kind,
					Err:  fmt.Errorf("guangyapan %s: status=%d %s", operation, status, message),
				}
				retry = kind == drives.ProviderErrorUnavailable &&
					(status == 408 || status == 500 || status == 502 || status == 503 || status == 504)
			} else {
				if decodeErr == nil {
					decodeErr = json.Unmarshal(resp.Body(), out)
				}
				if decodeErr != nil {
					return &drives.ProviderError{
						Kind: drives.ProviderErrorOther,
						Err:  fmt.Errorf("guangyapan %s: status=%d invalid response: %w", operation, status, decodeErr),
					}
				}
				return nil
			}
		}

		// Keep retries bounded in both count and backoff. A longer Retry-After
		// is returned to the caller without issuing an early retry.
		if !retry || attempt == authMaxAttempts || retryAfter > 2*time.Second {
			return fmt.Errorf("%w (attempts=%d)", failure, attempt)
		}
		delay := d.authRetryDelay * time.Duration(1<<(attempt-1))
		if retryAfter > delay {
			delay = retryAfter
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w; retry canceled: %w", failure, ctx.Err())
		case <-timer.C:
		}
	}
}

func accountErrorKind(status int, code string) drives.ProviderErrorKind {
	if status == http.StatusRequestTimeout || status >= 500 {
		return drives.ProviderErrorUnavailable
	}
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "invalid_client", "unauthorized_client", "unsupported_grant_type", "invalid_request", "invalid_scope":
		return drives.ProviderErrorOther
	case "invalid_grant", "invalid_token", "unauthenticated", "token_expired", "expired_token", "invalid_access_token", "invalid_refresh_token":
		return drives.ProviderErrorAuth
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return drives.ProviderErrorAuth
	}
	return drives.ProviderErrorOther
}

func retryAccountTransport(method string, err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	if method != http.MethodGet {
		return false
	}
	var network net.Error
	return (errors.As(err, &network) && (network.Timeout() || network.Temporary())) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE)
}

func (d *Driver) accountResponseMessage(out tokenResp) string {
	// Never log the raw response: it may contain newly issued credentials.
	message := fmt.Sprintf("error=%q error_code=%d description=%q", out.Error, out.ErrorCode, out.ErrorDesc)
	d.credentialsMu.RLock()
	defer d.credentialsMu.RUnlock()
	for _, secret := range []string{d.accessToken, d.refreshToken, d.captchaToken, out.AccessToken, out.RefreshToken} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	if len(message) > 1024 {
		message = message[:1024] + "...(truncated)"
	}
	return message
}
