package preview

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

// Distributions backport HLS extension checks independently of FFmpeg versions.
// Probe each configured executable once, including ffprobe's own demuxer.
var hlsInputCapabilities sync.Map

type hlsInputCapability struct {
	once    sync.Once
	options []string
}

func mediaInputOptions(ctx context.Context, executable string, link *drives.StreamLink) []string {
	options := ffmpegHTTPInputOptions(link)
	if link == nil || link.Protocol != "hls" {
		return options
	}
	value, _ := hlsInputCapabilities.LoadOrStore(executable, &hlsInputCapability{})
	capability := value.(*hlsInputCapability)
	capability.once.Do(func() {
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		help, err := exec.CommandContext(probeCtx, executable, "-hide_banner", "-h", "demuxer=hls").CombinedOutput()
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(help), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			switch fields[0] {
			case "-allowed_segment_extensions":
				capability.options = append(capability.options, "-allowed_segment_extensions", "ALL")
			case "-extension_picky":
				capability.options = append(capability.options, "-extension_picky", "0")
			}
		}
	})
	options = append(options, "-f", "hls", "-protocol_whitelist", "http,https,tcp,tls,crypto", "-allowed_extensions", "ALL")
	return append(options, capability.options...)
}

func mediaInputCommand(ctx context.Context, executable string, link *drives.StreamLink, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, executable, args...)
	if link != nil && link.Protocol == "hls" {
		// Go owns upstream transport; ambient proxies must not intercept loopback.
		cmd.Env = append(os.Environ(), "http_proxy=", "https_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=", "all_proxy=", "ALL_PROXY=")
	}
	return cmd
}

// Preserve a relay's upstream error even when the decoder exits successfully
// or reports only a generic 4XX/cancellation error. Call this before recovery.
func mediaInputError(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}
