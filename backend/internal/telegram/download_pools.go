package telegram

import (
	"context"
	"errors"
	"sync"

	mt "github.com/gotd/td/telegram"
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

func (c *mtprotoClient) openDownloadPool(ctx context.Context, dc int, threads int64) (mt.CloseInvoker, error) {
	for _, option := range c.client.Config().DCOptions {
		if option.ID == dc && option.MediaOnly {
			return c.client.MediaOnly(ctx, dc, threads)
		}
	}
	// Some DCs advertise no media-only address. Keep their file connections
	// separate from the update receiver, using the regular DC entry points.
	return c.client.DC(ctx, dc, threads)
}
