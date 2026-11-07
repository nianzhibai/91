package p115

import (
	"context"
	"net/url"

	"github.com/nianzhibai/91/backend/internal/drives"
)

func (d *Driver) GenerationSource(ctx context.Context, fileID string, refresh bool) (*drives.GenerationSource, error) {
	link, err := d.GenerationStreamURL(ctx, fileID, refresh)
	if err != nil {
		return nil, err
	}
	root, err := url.Parse(link.URL)
	if err != nil {
		return nil, err
	}
	return &drives.GenerationSource{
		Protocol: "hls", Link: link,
		Relay:           &drives.HLSRelay{AllowedHosts: []string{".115.com", root.Host}},
		RefreshStatuses: []int{403},
	}, nil
}

var _ drives.GenerationStreamProvider = (*Driver)(nil)
