package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

// sourceColorRepair is scoped to one generation task. Refreshed source links
// are probed independently, while subsequent segments reuse a successful plan.
type sourceColorRepair struct {
	sourceURL    string
	checked      bool
	inputOptions []string
}

type sourceColorStream struct {
	Index          int    `json:"index"`
	CodecName      string `json:"codec_name"`
	ColorSpace     string `json:"color_space"`
	ColorPrimaries string `json:"color_primaries"`
	ColorTransfer  string `json:"color_transfer"`
}

// runMediaCommand only probes after a concrete color-metadata failure. Apply
// the repair before decoding: an output flag or setparams filter runs too late
// to prevent FFmpeg from rejecting a reserved input colorspace.
func (g *Generator) runMediaCommand(
	ctx context.Context,
	source, prepared *drives.StreamLink,
	beforeInput, afterInput []string,
	repair *sourceColorRepair,
) ([]byte, error) {
	if repair.sourceURL != source.URL {
		*repair = sourceColorRepair{sourceURL: source.URL}
	}
	run := func() ([]byte, error) {
		args := append([]string{}, beforeInput...)
		args = append(args, ffmpegHTTPInputOptions(prepared)...)
		args = append(args, repair.inputOptions...)
		args = append(args, "-i", prepared.URL)
		args = append(args, afterInput...)
		return exec.CommandContext(ctx, g.cfg.FFmpegPath, args...).CombinedOutput()
	}
	out, err := run()
	if err == nil || ctx.Err() != nil || repair.checked || !invalidColorMetadataText(string(out)) {
		return out, err
	}
	options, probeErr := g.probeColorRepair(ctx, prepared)
	if probeErr != nil {
		// Preserve remote read and rate-limit errors for the caller's recovery
		// policy. A failed probe must remain retryable, even for the same URL.
		return nil, probeErr
	}
	repair.checked = true
	if len(options) == 0 {
		return out, err
	}
	repair.inputOptions = options
	log.Printf("[media] normalize reserved source color metadata: %s", strings.Join(options, " "))
	return run()
}

func invalidColorMetadataText(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "invalid color space") ||
		strings.Contains(text, "invalid color primaries") ||
		strings.Contains(text, "invalid color transfer")
}

func (g *Generator) probeColorRepair(ctx context.Context, link *drives.StreamLink) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{
		"-v", "error", "-select_streams", "v",
		"-show_entries", "stream=index,codec_name,color_space,color_primaries,color_transfer",
		"-of", "json",
	}
	args = append(args, ffmpegHTTPInputOptions(link)...)
	args = append(args, link.URL)
	cmd := exec.CommandContext(ctx, g.cfg.FFprobePath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, ffmpegCommandError("ffprobe color metadata", err, stderr.Bytes())
	}
	var probe struct {
		Streams []sourceColorStream `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return nil, fmt.Errorf("ffprobe color metadata output: %w", err)
	}
	return colorRepairInputOptions(probe.Streams), nil
}

func colorRepairInputOptions(streams []sourceColorStream) []string {
	var options []string
	for _, stream := range streams {
		var filter string
		switch stream.CodecName {
		case "h264":
			filter = "h264_metadata"
		case "hevc":
			filter = "hevc_metadata"
		default:
			continue
		}
		var fields []string
		for _, field := range []struct{ value, option string }{
			{stream.ColorSpace, "matrix_coefficients"},
			{stream.ColorPrimaries, "colour_primaries"},
			{stream.ColorTransfer, "transfer_characteristics"},
		} {
			if field.value == "reserved" || field.value == "reserved0" {
				// 2 is the codec's legal "unspecified" value. Retain valid SDR/HDR
				// metadata and range rather than guessing a particular standard.
				fields = append(fields, field.option+"=2")
			}
		}
		if len(fields) > 0 {
			options = append(options, "-bsf:"+strconv.Itoa(stream.Index), filter+"="+strings.Join(fields, ":"))
		}
	}
	return options
}
