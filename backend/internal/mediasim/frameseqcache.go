package mediasim

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nianzhibai/91/backend/internal/persistence"
)

// teaser 帧签名的磁盘缓存：签名只依赖 teaser 文件内容，用 (size, mtime)
// 作为有效性校验头。teaser 重新生成后 stat 不再匹配，自动失效重提。
// 单个签名约 110KB，只为参与配对的视频落盘。

const (
	frameSigCacheMagic   = "FSG1"
	frameSigCacheVersion = 1
)

// LoadCachedTeaserSignature 尝试从缓存读取 teaserPath 的帧签名。
// 缓存缺失、版本或 teaser (size, mtime) 不匹配、内容损坏时返回 false。
func LoadCachedTeaserSignature(cachePath, teaserPath string) (*FrameSignature, bool) {
	info, err := os.Stat(teaserPath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, false
	}
	sig, err := decodeFrameSignature(data, info.Size(), info.ModTime().UnixNano())
	if err != nil {
		return nil, false
	}
	return sig, true
}

// StoreCachedTeaserSignature 把签名写入缓存，best-effort：失败只返回错误由
// 调用方记日志，不影响比对流程。
func StoreCachedTeaserSignature(cachePath, teaserPath string, sig *FrameSignature) error {
	if sig == nil {
		return fmt.Errorf("nil signature")
	}
	info, err := os.Stat(teaserPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}
	data := encodeFrameSignature(sig, info.Size(), info.ModTime().UnixNano())
	persistence.RLock()
	defer persistence.RUnlock()
	tmp := cachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, cachePath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func encodeFrameSignature(sig *FrameSignature, teaserSize, teaserModTimeNano int64) []byte {
	out := make([]byte, 0, len(frameSigCacheMagic)+1+2+8+8+1+len(sig.Frames)*(1+frameBytes))
	out = append(out, frameSigCacheMagic...)
	out = append(out, frameSigCacheVersion)
	out = binary.LittleEndian.AppendUint16(out, uint16(FrameSignatureGridSize))
	out = binary.LittleEndian.AppendUint64(out, uint64(teaserSize))
	out = binary.LittleEndian.AppendUint64(out, uint64(teaserModTimeNano))
	out = append(out, byte(len(sig.Frames)))
	for _, frame := range sig.Frames {
		if frame == nil {
			out = append(out, 0)
			continue
		}
		out = append(out, 1)
		out = append(out, frame...)
	}
	return out
}

func decodeFrameSignature(data []byte, expectSize, expectModTimeNano int64) (*FrameSignature, error) {
	header := len(frameSigCacheMagic) + 1 + 2 + 8 + 8 + 1
	if len(data) < header {
		return nil, fmt.Errorf("frame signature cache too short")
	}
	if string(data[:len(frameSigCacheMagic)]) != frameSigCacheMagic {
		return nil, fmt.Errorf("bad magic")
	}
	offset := len(frameSigCacheMagic)
	if data[offset] != frameSigCacheVersion {
		return nil, fmt.Errorf("version mismatch")
	}
	offset++
	if binary.LittleEndian.Uint16(data[offset:]) != FrameSignatureGridSize {
		return nil, fmt.Errorf("grid mismatch")
	}
	offset += 2
	if int64(binary.LittleEndian.Uint64(data[offset:])) != expectSize {
		return nil, fmt.Errorf("teaser size changed")
	}
	offset += 8
	if int64(binary.LittleEndian.Uint64(data[offset:])) != expectModTimeNano {
		return nil, fmt.Errorf("teaser mtime changed")
	}
	offset += 8
	count := int(data[offset])
	offset++
	if count > FrameSignatureMaxFrames {
		return nil, fmt.Errorf("frame count %d out of range", count)
	}
	sig := &FrameSignature{Frames: make([][]byte, 0, count)}
	for i := 0; i < count; i++ {
		if offset >= len(data) {
			return nil, fmt.Errorf("truncated at frame %d", i)
		}
		present := data[offset]
		offset++
		if present == 0 {
			sig.Frames = append(sig.Frames, nil)
			continue
		}
		if offset+frameBytes > len(data) {
			return nil, fmt.Errorf("truncated frame %d", i)
		}
		frame := make([]byte, frameBytes)
		copy(frame, data[offset:offset+frameBytes])
		sig.Frames = append(sig.Frames, frame)
		offset += frameBytes
	}
	if offset != len(data) {
		return nil, fmt.Errorf("trailing bytes")
	}
	return sig, nil
}
