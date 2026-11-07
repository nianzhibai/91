package p115

import (
	"context"
	"errors"
	"net/http"
	"time"

	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/nianzhibai/91/backend/internal/readretry"
)

const p115ReadTimeout = 15 * time.Second

// newSDKClient gives each SDK operation its own mutable Request field. The
// HTTP client (including its connection pool and cookie jar) is shared. Resty
// headers, cookies and middleware are local so binding ctx cannot affect another
// operation. The SDK creates its own requests and has no context parameter.
// Direct Client.R() calls already create independent requests.
// Do not copy the SDK client: its upload auth fields are mutable and belong to
// the upload path, which serializes access through uploadGate.
func (d *Driver) newSDKClient(ctx context.Context) *sdk.Pan115Client {
	shared := d.client.Client
	client := resty.NewWithClient(shared.GetClient())
	client.Header = shared.Header.Clone()
	client.Cookies = append([]*http.Cookie(nil), shared.Cookies...)
	client.OnBeforeRequest(func(_ *resty.Client, request *resty.Request) error {
		request.SetContext(ctx)
		return ctx.Err()
	})
	return &sdk.Pan115Client{Client: client}
}

func (d *Driver) getFile(ctx context.Context, fileID string) (*sdk.File, error) {
	if d.client == nil || d.client.Client == nil {
		return nil, errors.New("115 client not initialized")
	}
	ctx, cancel := context.WithTimeout(ctx, p115ReadTimeout)
	defer cancel()
	// Keep read retries outside Resty: its zero-delay callback selects default
	// backoff rather than the immediate first retry required by our policy.
	return readretry.Do(ctx, func() (*sdk.File, error) {
		return d.newSDKClient(ctx).GetFile(fileID)
	}, readretry.Options{})
}
