package quark

import (
	"context"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/generationsource"
)

// GenerationSource consumes ready cloud renditions without polling or explicitly
// requesting transcode preparation. The cache belongs to this mounted account.
func (d *Driver) GenerationSource(ctx context.Context, fileID string, refresh bool) (*drives.GenerationSource, error) {
	return d.generation.Resolve(ctx, fileID, refresh, d.resolveGenerationSource)
}

func (d *Driver) resolveGenerationSource(ctx context.Context, fileID string) (*drives.GenerationSource, error) {
	sources, err := d.playbackSources(ctx, fileID)
	if err != nil {
		return nil, err
	}
	source, err := generationsource.ReadyHLS(sources)
	if err == nil {
		source.RefreshStatuses = []int{403, 412}
	}
	return source, err
}

var _ drives.GenerationStreamProvider = (*Driver)(nil)
