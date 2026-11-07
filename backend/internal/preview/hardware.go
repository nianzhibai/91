package preview

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nianzhibai/91/backend/internal/drives"
)

// Hardware is shared by every drive. Detection is explicit and cached; asset
// generation only reads a snapshot and never probes devices for each segment.
type Hardware struct {
	cfg           Config
	probeMu       sync.Mutex
	mu            sync.RWMutex
	enabled       bool
	checked       bool
	backend       *hardwareBackend
	reason        string
	fallbackCount uint64
}

type HardwareStatus struct {
	Enabled       bool   `json:"enabled"`
	Checked       bool   `json:"checked"`
	Available     bool   `json:"available"`
	Backend       string `json:"backend,omitempty"`
	Device        string `json:"device,omitempty"`
	Name          string `json:"name,omitempty"`
	Reason        string `json:"reason,omitempty"`
	FallbackCount uint64 `json:"fallbackCount"`
}

type hardwareBackend struct {
	kind     string
	device   string
	name     string
	lowPower bool
}

func NewHardware(cfg Config) *Hardware {
	cfg.Hardware = nil
	return &Hardware{cfg: New(cfg).cfg}
}

func (h *Hardware) Status() HardwareStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s := HardwareStatus{Enabled: h.enabled, Checked: h.checked, Available: h.backend != nil, Reason: h.reason, FallbackCount: h.fallbackCount}
	if h.backend != nil {
		s.Backend, s.Device = h.backend.kind, h.backend.device
		s.Name = h.backend.name
	}
	return s
}

// Check reuses the last probe; Probe explicitly retries after driver or device
// changes. Both calls serialize so concurrent administrators share a check.
func (h *Hardware) Check(ctx context.Context) HardwareStatus {
	return h.check(ctx, false)
}

func (h *Hardware) Probe(ctx context.Context) HardwareStatus {
	return h.check(ctx, true)
}

func (h *Hardware) check(ctx context.Context, force bool) HardwareStatus {
	h.probeMu.Lock()
	defer h.probeMu.Unlock()
	if !force && h.Status().Checked {
		return h.Status()
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	backend, err := h.detect(ctx)
	// A disconnected client must not overwrite a successful cached probe.
	if ctx.Err() != nil {
		s := h.Status()
		s.Available = false
		s.Reason = "硬件加速检测超时或已取消，请重试"
		return s
	}
	h.mu.Lock()
	h.checked, h.backend, h.reason = true, backend, ""
	if err != nil {
		h.reason = err.Error()
	}
	h.mu.Unlock()
	return h.Status()
}

func (h *Hardware) SetEnabled(ctx context.Context, enabled bool) {
	if enabled {
		status := h.Check(ctx)
		if !status.Available {
			// An unavailable GPU in a restored or externally edited config must
			// not prevent the website from starting or generating assets.
			log.Printf("[media] GPU unavailable, using CPU: %s", status.Reason)
		}
	}
	h.mu.Lock()
	h.enabled = enabled
	h.mu.Unlock()
}

func (h *Hardware) snapshot() *hardwareBackend {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.enabled {
		return nil
	}
	return h.backend
}

func (h *Hardware) detect(ctx context.Context) (*hardwareBackend, error) {
	return h.probeCandidates(ctx, hardwareCandidates("/dev"))
}

func hardwareCandidates(deviceDir string) []*hardwareBackend {
	devices, _ := filepath.Glob(filepath.Join(deviceDir, "dri", "renderD*"))
	candidates := make([]*hardwareBackend, 0, len(devices)*2+2)
	for _, node := range []string{"mpp_service", "mpp-service"} {
		device := filepath.Join(deviceDir, node)
		if info, err := os.Stat(device); err == nil && !info.IsDir() {
			candidates = append(candidates, &hardwareBackend{kind: "rkmpp", device: device})
			break
		}
	}
	for _, device := range devices {
		candidates = append(candidates,
			&hardwareBackend{kind: "vaapi", device: device},
			// Some Intel generations only expose the low-power H.264 entrypoint.
			&hardwareBackend{kind: "vaapi", device: device, lowPower: true},
		)
	}
	return append(candidates, &hardwareBackend{kind: "cuda", device: "0"})
}

func (h *Hardware) probeCandidates(ctx context.Context, candidates []*hardwareBackend) (*hardwareBackend, error) {
	dir, err := os.MkdirTemp("", "91-gpu-probe-*")
	if err != nil {
		return nil, fmt.Errorf("无法创建 GPU 检测文件：%w", err)
	}
	defer os.RemoveAll(dir)
	source := filepath.Join(dir, "source.mp4")
	// Use a compressed source and downscale it, keeping the output large enough
	// for hardware encoders that reject very small frame dimensions.
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=512x512:rate=10", "-frames:v", "5", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-y", source}
	out, err := exec.CommandContext(ctx, h.cfg.FFmpegPath, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("FFmpeg 检测样片生成失败：%s", probeError(err, out))
	}
	failures := make([]string, 0, len(candidates)+1)
	if len(candidates) == 1 && candidates[0].kind == "cuda" {
		failures = append(failures, "VAAPI：未找到 /dev/dri/renderD* 设备，请检查设备映射")
		failures = append(failures, "Rockchip：未找到 /dev/mpp_service 或 /dev/mpp-service，请检查设备映射")
	}
	for _, backend := range candidates {
		probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		gen := New(Config{FFmpegPath: h.cfg.FFmpegPath, FFprobePath: h.cfg.FFprobePath, FFmpegThreads: h.cfg.FFmpegThreads, Width: 256, LocalDir: dir})
		gen.hardware = backend
		link := &drives.StreamLink{URL: source}
		err := gen.generateThumbnailAtOffset(probeCtx, link, filepath.Join(dir, "cover.jpg"), 0, &sourceColorRepair{})
		if err == nil {
			_, err = gen.generateSingleSegment(probeCtx, 0, 0, 0.4, true, link, &sourceColorRepair{})
		}
		cancel()
		if err == nil {
			backend.name = h.deviceName(ctx, backend)
			return backend, nil
		}
		failures = append(failures, fmt.Sprintf("%s (%s)：%s", backend.kind, backend.device, probeError(err, nil)))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("没有可用的 GPU 硬件加速后端。%s", strings.Join(failures, "；"))
}

func probeError(err error, output []byte) string {
	message := strings.TrimSpace(string(output))
	if message == "" {
		message = err.Error()
	}
	if len(message) > 600 {
		message = message[:600]
	}
	return message
}

func (b *hardwareBackend) inputOptions() []string {
	if b == nil {
		return nil
	}
	if b.kind == "rkmpp" {
		return []string{"-hwaccel", "rkmpp", "-hwaccel_output_format", "drm_prime"}
	}
	return []string{
		"-init_hw_device", b.kind + "=gpu:" + b.device,
		"-filter_hw_device", "gpu",
		"-hwaccel", b.kind,
		"-hwaccel_device", "gpu",
		"-hwaccel_output_format", b.kind,
	}
}

// Download only the resized image. Fade and JPEG range conversion use the
// existing software filters, so full-resolution frames stay on the GPU.
func (b *hardwareBackend) downloadFilter(width int) string {
	scaler := "scale_" + b.kind
	if b.kind == "rkmpp" {
		scaler = "scale_rkrga"
	}
	return scaler + "=w=" + strconv.Itoa(width) + ":h=-2:format=nv12,hwdownload,format=nv12"
}

func (b *hardwareBackend) encoderInputFilter() string {
	// MPP accepts software NV12 after fade; VAAPI/NVENC need hardware frames.
	if b.kind == "rkmpp" {
		return "format=nv12"
	}
	return "format=nv12,hwupload"
}

func (g *Generator) thumbnailFilter() string {
	filter := thumbnailVideoFilter(g.cfg.Width)
	if g.hardware != nil {
		filter = g.hardware.downloadFilter(g.cfg.Width) + "," + filter
	}
	return filter
}

func (g *Generator) segmentFilter(fade bool, duration float64) string {
	filter := teaserSegmentVideoFilter(g.cfg.Width, fade, duration)
	if g.hardware != nil {
		filter = g.hardware.downloadFilter(g.cfg.Width) + "," + filter + "," + g.hardware.encoderInputFilter()
	}
	return filter
}

func (g *Generator) encoderOptions() []string {
	if g.hardware == nil {
		return []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "28"}
	}
	if g.hardware.kind == "rkmpp" {
		return []string{"-c:v", "h264_rkmpp", "-rc_mode", "CQP", "-qp_init", "28"}
	}
	if g.hardware.kind == "vaapi" {
		options := []string{"-c:v", "h264_vaapi", "-qp", "28"}
		if g.hardware.lowPower {
			options = append(options, "-low_power", "1")
		}
		return options
	}
	return []string{"-c:v", "h264_nvenc", "-preset", "p4", "-rc", "vbr", "-cq", "28", "-b:v", "0"}
}

func (g *Generator) forTask() *Generator {
	task := *g
	task.hardware = g.cfg.Hardware.snapshot()
	return &task
}

func (g *Generator) fallbackToCPU(ctx context.Context, err error) bool {
	if ctx.Err() != nil || g.hardware == nil || !hardwareFailure(err) {
		return false
	}
	log.Printf("[media] GPU %s failed; retrying asset with CPU: %v", g.hardware.kind, err)
	if h := g.cfg.Hardware; h != nil {
		h.mu.Lock()
		h.fallbackCount++
		h.mu.Unlock()
	}
	g.hardware = nil
	return true
}

// Tag decode/filter/encode failures at the command boundary. Driver errors can
// be as opaque as "Cannot allocate memory"; matching GPU names in stderr would
// miss them. Transport and invalid-container errors retain their own recovery.
type hardwareProcessingError struct {
	err error
}

func (e *hardwareProcessingError) Error() string { return e.err.Error() }
func (e *hardwareProcessingError) Unwrap() error { return e.err }

func (g *Generator) processingError(err error) error {
	if err == nil || g.hardware == nil {
		return err
	}
	return &hardwareProcessingError{err: err}
}

func hardwareFailure(err error) bool {
	if err == nil || isRateLimitError(err) || drives.ErrorMentionsHTTPStatus(err,
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestTimeout, http.StatusRequestedRangeNotSatisfiable, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	) || transientRemoteMediaReadText(err.Error()) {
		return false
	}
	if deterministicMediaInputError(err.Error()) {
		return false
	}
	var processing *hardwareProcessingError
	return errors.As(err, &processing)
}
