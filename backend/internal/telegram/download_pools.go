package telegram

import (
	"context"
	"errors"
	"sync"

	mt "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// Pools belong to one receiver session, whose download concurrency is fixed.
// Reuse each DC's connections across videos to retain their TCP warm-up.
type downloadPools struct {
	mu     sync.Mutex
	pools  map[int]mt.CloseInvoker
	closed bool
}

func (p *downloadPools) get(ctx context.Context, dc, threads int, open func(context.Context, int, int64) (mt.CloseInvoker, error)) (mt.CloseInvoker, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.closed {
		return nil, errors.New("Telegram 下载连接已关闭")
	}
	if pool := p.pools[dc]; pool != nil {
		return pool, nil
	}
	pool, err := open(ctx, dc, int64(threads))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = pool.Close()
		return nil, err
	}
	if p.pools == nil {
		p.pools = make(map[int]mt.CloseInvoker)
	}
	p.pools[dc] = pool
	return pool, nil
}

func (p *downloadPools) close() {
	p.mu.Lock()
	p.closed = true
	pools := p.pools
	p.pools = nil
	p.mu.Unlock()
	for _, pool := range pools {
		_ = pool.Close()
	}
}

type downloadRoute int

const (
	routeHome downloadRoute = iota
	routeMedia
	routeData
)

// The session is already authorized on its home DC. gotd's DC and media pools
// always transfer authorization, which Telegram rejects there with DC_ID_INVALID.
func downloadRouteFor(cfg tg.Config, dc int) downloadRoute {
	if dc == cfg.ThisDC {
		return routeHome
	}
	for _, option := range cfg.DCOptions {
		if option.ID == dc && option.MediaOnly {
			return routeMedia
		}
	}
	// Some DCs advertise no media-only address. Keep their file connections
	// separate from the update receiver, using the regular DC entry points.
	return routeData
}

func (c *mtprotoClient) openDownloadPool(ctx context.Context, dc int, threads int64) (mt.CloseInvoker, error) {
	switch downloadRouteFor(c.client.Config(), dc) {
	case routeHome:
		return c.client.Pool(threads)
	case routeMedia:
		return c.client.MediaOnly(ctx, dc, threads)
	default:
		return c.client.DC(ctx, dc, threads)
	}
}
