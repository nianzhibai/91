package scriptcrawler

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/nianzhibai/91/backend/internal/applog"
)

const ffmpegHLSCapabilityProbeTimeout = 5 * time.Second

// ffmpegHLSCapabilities describes optional HLS demuxer flags. These flags
// cannot be selected by FFmpeg's version number: distributions backport them
// independently, including between patch releases of the same FFmpeg series.
type ffmpegHLSCapabilities struct {
	allowedSegmentExtensions bool
	extensionPicky           bool
}

func (c *Importer) ffmpegHLSInputOptions(ctx context.Context) []string {
	c.hlsCapsOnce.Do(func() {
		// Capability discovery should not be permanently poisoned by a canceled
		// crawl. Bound it independently and cache the result for this crawler.
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ffmpegHLSCapabilityProbeTimeout)
		defer cancel()

		caps, err := probeFFmpegHLSCapabilities(probeCtx, c.cfg.FFmpegPath)
		if err != nil {
			applog.Warn(probeCtx, "inspect ffmpeg HLS capabilities failed; using legacy-compatible options", err, "component", "scriptcrawler")
			return
		}
		c.hlsCaps = caps
	})
	return buildFFmpegHLSInputOptions(c.hlsCaps)
}

func probeFFmpegHLSCapabilities(ctx context.Context, ffmpegPath string) (ffmpegHLSCapabilities, error) {
	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-h", "demuxer=hls").CombinedOutput()
	if err != nil {
		return ffmpegHLSCapabilities{}, mediaCommandError("ffmpeg hls capability probe", err, out)
	}
	return parseFFmpegHLSCapabilities(string(out)), nil
}

func parseFFmpegHLSCapabilities(help string) ffmpegHLSCapabilities {
	return ffmpegHLSCapabilities{
		allowedSegmentExtensions: ffmpegHelpHasOption(help, "allowed_segment_extensions"),
		extensionPicky:           ffmpegHelpHasOption(help, "extension_picky"),
	}
}

func ffmpegHelpHasOption(help, name string) bool {
	want := "-" + name
	for _, line := range strings.Split(help, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == want {
			return true
		}
	}
	return false
}

func buildFFmpegHLSInputOptions(caps ffmpegHLSCapabilities) []string {
	args := []string{
		"-protocol_whitelist", "http,https,tcp,tls,crypto",
		"-allowed_extensions", "ALL",
	}
	if caps.allowedSegmentExtensions {
		args = append(args, "-allowed_segment_extensions", "ALL")
	}
	if caps.extensionPicky {
		args = append(args, "-extension_picky", "0")
	}
	return args
}
