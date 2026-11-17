package pikpak

import (
	"crypto/sha1"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

// referenceGCID 用最朴素的方式重新算一遍 GCID，作为黄金值。
// 由于 PikPak 没公开测试向量，我们用与算法定义一致的、不同实现的
// 朴素版本来交叉验证。
func referenceGCID(data []byte) string {
	size := int64(len(data))
	var blockSize int64 = 0x40000
	for size > 0 && float64(size)/float64(blockSize) > 0x200 && blockSize < 0x200000 {
		blockSize <<= 1
	}
	outer := sha1.New()
	for off := int64(0); off < size; off += blockSize {
		end := off + blockSize
		if end > size {
			end = size
		}
		inner := sha1.New()
		inner.Write(data[off:end])
		outer.Write(inner.Sum(nil))
	}
	return hex.EncodeToString(outer.Sum(nil))
}

func computeGCID(t *testing.T, data []byte) string {
	t.Helper()
	h := NewGCID(int64(len(data)))
	if _, err := io.Copy(h, strings.NewReader(string(data))); err != nil {
		t.Fatalf("write: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestGCIDMatchesReferenceForVariousSizes(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"sub-block", 1024},
		{"exactly-one-block", 0x40000},
		{"slightly-over-one-block", 0x40000 + 1},
		{"two-blocks", 0x80000},
		{"crossing-resize-threshold", 0x40000 * 0x201}, // 触发 blockSize 翻倍
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := make([]byte, c.size)
			for i := range data {
				data[i] = byte(i % 251)
			}
			got := computeGCID(t, data)
			want := referenceGCID(data)
			if got != want {
				t.Fatalf("size=%d:\n got  = %s\n want = %s", c.size, got, want)
			}
			if len(got) != 40 {
				t.Fatalf("digest length = %d, want 40", len(got))
			}
		})
	}
}

func TestGCIDStreamingMatchesSingleWrite(t *testing.T) {
	data := make([]byte, 0x40000*3+777) // 3 整块 + 一段尾巴，覆盖多次写入跨块边界
	for i := range data {
		data[i] = byte(i ^ 0xA5)
	}

	full := NewGCID(int64(len(data)))
	full.Write(data)
	wantHex := hex.EncodeToString(full.Sum(nil))

	// 用 7 这个奇数大小做 chunked write，确保每次 Write 都可能跨越块边界
	chunked := NewGCID(int64(len(data)))
	for off := 0; off < len(data); off += 7 {
		end := off + 7
		if end > len(data) {
			end = len(data)
		}
		chunked.Write(data[off:end])
	}
	gotHex := hex.EncodeToString(chunked.Sum(nil))

	if gotHex != wantHex {
		t.Fatalf("chunked vs single:\n got  = %s\n want = %s", gotHex, wantHex)
	}
}

func TestGCIDSumIsIdempotent(t *testing.T) {
	data := make([]byte, 1234)
	for i := range data {
		data[i] = byte(i)
	}
	h := NewGCID(int64(len(data)))
	h.Write(data)

	first := hex.EncodeToString(h.Sum(nil))
	second := hex.EncodeToString(h.Sum(nil))
	if first != second {
		t.Fatalf("Sum(nil) is not idempotent:\n first = %s\n second = %s", first, second)
	}
}

func TestGCIDResetClearsState(t *testing.T) {
	h := NewGCID(2048)
	h.Write([]byte("first batch"))
	h.Reset()
	h.Write([]byte("hello"))
	got := hex.EncodeToString(h.Sum(nil))

	fresh := NewGCID(2048)
	fresh.Write([]byte("hello"))
	want := hex.EncodeToString(fresh.Sum(nil))

	if got != want {
		t.Fatalf("after reset:\n got  = %s\n want = %s", got, want)
	}
}

func TestGCIDBlockSizeSelectorMatchesSpec(t *testing.T) {
	// blockSize 选择规则：从 0x40000 起翻倍，直到 size/blockSize <= 512 或者上限 0x200000
	cases := []struct {
		size      int64
		blockSize int
	}{
		{0, 0x40000},                 // 空文件
		{1024, 0x40000},              // 极小
		{0x40000 * 200, 0x40000},     // size/0x40000 = 200 ≤ 512，停在 0x40000
		{0x40000 * 0x201, 0x80000},   // 200 翻倍到 400 后仍超阈值，停在 0x80000
		{0x200000 * 0x300, 0x200000}, // 上限封顶
	}
	for _, c := range cases {
		got := gcidBlockSize(c.size)
		if got != int64(c.blockSize) {
			t.Fatalf("size=%d: blockSize=%#x, want %#x", c.size, got, c.blockSize)
		}
	}
}
