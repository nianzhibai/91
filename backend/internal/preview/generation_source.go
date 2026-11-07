package preview

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nianzhibai/91/backend/internal/drives"
	"github.com/nianzhibai/91/backend/internal/hlsrelay"
)

// generationInput owns one task's source, relay and refresh budget. Probing and
// generating reuse it, so a rejected source is not retried by each stage.
type generationInput struct {
	drive     drives.Drive
	fileID    string
	source    *drives.GenerationSource
	link      *drives.StreamLink
	relay     *hlsrelay.Loopback
	refreshed bool
	failure   error
}

func resolveGenerationInput(ctx context.Context, drive drives.Drive, fileID string) (*generationInput, error) {
	if drive == nil {
		return nil, errors.New("missing drive")
	}
	input := &generationInput{drive: drive, fileID: fileID}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if provider, ok := drive.(drives.GenerationStreamProvider); ok {
		source, err := provider.GenerationSource(ctx, fileID, false)
		if err == nil && validGenerationSource(source) {
			input.source, input.link = source, source.Link
			return input, nil
		}
		if err != nil && !errors.Is(err, drives.ErrGenerationStreamUnavailable) {
			return nil, err
		}
	}
	if err := input.original(ctx); err != nil {
		return nil, err
	}
	return input, nil
}

func validGenerationSource(source *drives.GenerationSource) bool {
	return source != nil && source.Link != nil && strings.TrimSpace(source.Link.URL) != "" && (source.Protocol == "file" || source.Protocol == "hls")
}
func (in *generationInput) Close() {
	if in.relay != nil {
		in.relay.Close()
		in.relay = nil
	}
}
func (in *generationInput) original(ctx context.Context) error {
	in.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	link, err := in.drive.StreamURL(ctx, in.fileID)
	if err != nil {
		return err
	}
	if link == nil || strings.TrimSpace(link.URL) == "" {
		return errors.New("empty original stream link")
	}
	in.source, in.link = nil, link
	return nil
}
func (in *generationInput) rejected(err error) bool {
	return in.source != nil && drives.ErrorMentionsHTTPStatus(err, in.source.RefreshStatuses...)
}
func (in *generationInput) terminal(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return true
	}
	var providerErr *drives.ProviderError
	if errors.As(err, &providerErr) && providerErr.Kind == drives.ProviderErrorAuth {
		return true
	}
	// Existing 115 decoders may classify a signed-media 403 as a rate limit.
	// Actual 405/429 limits always take precedence over refreshable rejections.
	return isRateLimitError(err) && (!in.rejected(err) || drives.ErrorMentionsHTTPStatus(err, 405, 429))
}
func (in *generationInput) attempt(ctx context.Context, operation func(context.Context, *drives.StreamLink) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in.source != nil && in.source.Protocol == "hls" {
		if in.relay == nil {
			policy := drives.HLSRelay{}
			if in.source.Relay != nil {
				policy = *in.source.Relay
			}
			relay, err := hlsrelay.Start(ctx, in.source.Link, policy)
			if err != nil {
				return err
			}
			in.relay = relay
			in.link = &drives.StreamLink{URL: relay.URL, Protocol: "hls"}
		}
		in.relay.ResetError()
	}
	inputCtx := ctx
	if in.relay != nil {
		inputCtx = in.relay.Context()
	}
	err := operation(inputCtx, in.link)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A demuxer can successfully decode other segments after a rejected read.
	// Source failures still take precedence over a successful process exit.
	if in.relay != nil && (err != nil || inputCtx.Err() != nil) {
		if upstream := in.relay.Error(); upstream != nil {
			return upstream
		}
	}
	return mediaInputError(inputCtx, err)
}

func (in *generationInput) run(ctx context.Context, operation func(context.Context, *drives.StreamLink) error) (err error) {
	if in.failure != nil {
		return in.failure
	}
	defer func() {
		if err != nil && in.terminal(ctx, err) {
			in.failure = err
		}
	}()
	err = in.attempt(ctx, operation)
	if err == nil || errors.Is(err, errDurationUnavailable) || in.source == nil || in.terminal(ctx, err) {
		return err
	}
	if in.rejected(err) && !in.refreshed {
		in.refreshed = true
		provider := in.drive.(drives.GenerationStreamProvider)
		source, refreshErr := provider.GenerationSource(ctx, in.fileID, true)
		if refreshErr == nil && !validGenerationSource(source) {
			refreshErr = drives.ErrGenerationStreamUnavailable
		}
		if refreshErr == nil {
			in.Close()
			in.source, in.link = source, source.Link
			err = in.attempt(ctx, operation)
			if err == nil || errors.Is(err, errDurationUnavailable) || in.terminal(ctx, err) {
				return err
			}
		} else if !errors.Is(refreshErr, drives.ErrGenerationStreamUnavailable) {
			return refreshErr
		}
	}
	if err := in.original(ctx); err != nil {
		return err
	}
	return in.attempt(ctx, operation)
}

func (in *generationInput) probe(ctx context.Context, operation func(context.Context, *drives.StreamLink) (float64, error)) (float64, error) {
	var duration float64
	err := in.run(ctx, func(ctx context.Context, link *drives.StreamLink) error {
		var err error
		duration, err = operation(ctx, link)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("generation duration: %w", err)
	}
	return duration, nil
}
