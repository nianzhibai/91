package preview

import (
	"context"

	"github.com/nianzhibai/91/backend/internal/drives"
)

// generatePath and generatePathWithLinkRefresh serve tests that only check
// the teaser file.
func (g *Generator) generatePath(ctx context.Context, link *drives.StreamLink, duration float64) (string, error) {
	media, err := g.Generate(ctx, link, duration, MediaRequest{Teaser: true})
	return media.TeaserPath, err
}

func (g *Generator) generatePathWithLinkRefresh(ctx context.Context, first *drives.StreamLink, duration float64, refresh func(context.Context) (*drives.StreamLink, error)) (string, error) {
	media, err := g.GenerateWithLinkRefresh(ctx, first, duration, MediaRequest{Teaser: true}, refresh)
	return media.TeaserPath, err
}
