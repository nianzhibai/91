package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"time"
)

var shortsBinaryMagic = [4]byte{'S', 'F', 'B', '1'}

const shortsBinaryHeaderSize = 8

const (
	shortsModeCodeRecommend byte = 1
	shortsModeCodeLatest    byte = 2
	shortsModeCodeHot       byte = 3
)

// ShortsRowIDSnapshot stores a Fisher-Yates shuffled sequence of SQLite rowids
// as a compact binary blob (4 bytes/video when max rowid fits uint32, else 8).
// A 20,000-video recommend session takes only ~80 KB in SQLite and RAM, needs
// zero JSON parsing, and serves every page via O(count log N) rowid IN lookups.
type ShortsRowIDSnapshot struct {
	Width byte
	Count int
	Blob  []byte // complete encoded SFB1 payload (header + packed rowids)
}

// RowIDAt returns the packed rowid at slot index in [0, Count).
func (r *ShortsRowIDSnapshot) RowIDAt(index int) int64 {
	offset := shortsBinaryHeaderSize + index*int(r.Width)
	if r.Width == 4 {
		return int64(binary.LittleEndian.Uint32(r.Blob[offset : offset+4]))
	}
	return int64(binary.LittleEndian.Uint64(r.Blob[offset : offset+8]))
}

// SliceRowIDs returns rowids in [start, end) without copying the full blob.
func (r *ShortsRowIDSnapshot) SliceRowIDs(start, end int) []int64 {
	if r == nil || start >= end || start >= r.Count {
		return nil
	}
	if start < 0 {
		start = 0
	}
	if end > r.Count {
		end = r.Count
	}
	out := make([]int64, end-start)
	for i := start; i < end; i++ {
		out[i-start] = r.RowIDAt(i)
	}
	return out
}

// ShortsFeedSnapshot represents a persisted shorts feed session.
// Ordered modes (latest/hot) store bounded ID slices; recommend stores a true
// Fisher-Yates shuffled compact binary rowid blob. Legacy v1 JSON string arrays
// decode with Mode == "".
type ShortsFeedSnapshot struct {
	Version  int                  `json:"v,omitempty"`
	Mode     string               `json:"mode,omitempty"`
	VideoIDs []string             `json:"ids,omitempty"`
	RowIDs   *ShortsRowIDSnapshot `json:"-"`
}

// Total returns the snapshot's logical item count.
func (s *ShortsFeedSnapshot) Total() int {
	if s == nil {
		return 0
	}
	if s.RowIDs != nil {
		return s.RowIDs.Count
	}
	return len(s.VideoIDs)
}

func modeToCode(mode string) byte {
	switch mode {
	case "recommend":
		return shortsModeCodeRecommend
	case "latest":
		return shortsModeCodeLatest
	case "hot":
		return shortsModeCodeHot
	default:
		return 0
	}
}

func codeToMode(code byte) string {
	switch code {
	case shortsModeCodeRecommend:
		return "recommend"
	case shortsModeCodeLatest:
		return "latest"
	case shortsModeCodeHot:
		return "hot"
	default:
		return ""
	}
}

func encodeRowIDSnapshot(mode string, rowIDs []int64) *ShortsRowIDSnapshot {
	if len(rowIDs) == 0 {
		return &ShortsRowIDSnapshot{Width: 4, Count: 0}
	}
	var maxRowID int64
	for _, id := range rowIDs {
		if id > maxRowID {
			maxRowID = id
		}
	}
	var width byte = 4
	if maxRowID > math.MaxUint32 {
		width = 8
	}
	buf := make([]byte, shortsBinaryHeaderSize+len(rowIDs)*int(width))
	copy(buf[0:4], shortsBinaryMagic[:])
	buf[4] = modeToCode(mode)
	buf[5] = width
	if width == 4 {
		for i, id := range rowIDs {
			binary.LittleEndian.PutUint32(buf[shortsBinaryHeaderSize+i*4:], uint32(id))
		}
	} else {
		for i, id := range rowIDs {
			binary.LittleEndian.PutUint64(buf[shortsBinaryHeaderSize+i*8:], uint64(id))
		}
	}
	return &ShortsRowIDSnapshot{
		Width: width,
		Count: len(rowIDs),
		Blob:  buf,
	}
}

func decodeRowIDSnapshot(raw []byte) (*ShortsFeedSnapshot, error) {
	if len(raw) < shortsBinaryHeaderSize || !bytes.Equal(raw[0:4], shortsBinaryMagic[:]) {
		return nil, errors.New("invalid shorts binary snapshot header")
	}
	mode := codeToMode(raw[4])
	width := raw[5]
	if width != 4 && width != 8 {
		return nil, errors.New("invalid shorts binary snapshot width")
	}
	payloadLen := len(raw) - shortsBinaryHeaderSize
	if payloadLen%int(width) != 0 {
		return nil, errors.New("truncated shorts binary snapshot")
	}
	copied := make([]byte, len(raw))
	copy(copied, raw)
	return &ShortsFeedSnapshot{
		Version: 2,
		Mode:    mode,
		RowIDs: &ShortsRowIDSnapshot{
			Width: width,
			Count: payloadLen / int(width),
			Blob:  copied,
		},
	}, nil
}

// CreateShortsRecommendSnapshot queries all visible video rowids, applies a
// true Fisher-Yates shuffle, and packs them into a compact binary blob.
func (c *Catalog) CreateShortsRecommendSnapshot(ctx context.Context) (*ShortsFeedSnapshot, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT videos.rowid
		   FROM videos
		  WHERE COALESCE(videos.hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL+`
		  ORDER BY videos.rowid ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rowIDs []int64
	for rows.Next() {
		var rowID int64
		if err := rows.Scan(&rowID); err != nil {
			return nil, err
		}
		rowIDs = append(rowIDs, rowID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rand.Shuffle(len(rowIDs), func(i, j int) {
		rowIDs[i], rowIDs[j] = rowIDs[j], rowIDs[i]
	})
	return &ShortsFeedSnapshot{
		Version: 2,
		Mode:    "recommend",
		RowIDs:  encodeRowIDSnapshot("recommend", rowIDs),
	}, nil
}

// VisibleVideosByRowIDs fetches currently visible videos matching rowIDs via
// SQLite's primary rowid B-tree index and returns a map keyed by rowid.
func (c *Catalog) VisibleVideosByRowIDs(ctx context.Context, rowIDs []int64) (map[int64]*Video, error) {
	if len(rowIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(rowIDs)), ",")
	args := make([]any, len(rowIDs))
	for i, id := range rowIDs {
		args[i] = id
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT videos.rowid, `+allVideoCols+`
		   FROM videos
		  WHERE videos.rowid IN (`+placeholders+`)
		    AND COALESCE(videos.hidden, 0) = 0
		    AND `+activeDriveWhereSQL+`
		    AND `+uniqueVideoWhereSQL,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byRowID := make(map[int64]*Video, len(rowIDs))
	for rows.Next() {
		var rowID int64
		video, err := scanVideo(prefixRowScanner{inner: rows, first: &rowID})
		if err != nil {
			return nil, err
		}
		byRowID[rowID] = video
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return byRowID, nil
}

type prefixRowScanner struct {
	inner rowScanner
	first any
}

func (p prefixRowScanner) Scan(dest ...any) error {
	args := make([]any, 0, len(dest)+1)
	args = append(args, p.first)
	args = append(args, dest...)
	return p.inner.Scan(args...)
}

// SaveShortsFeedSnapshot persists a structured shorts feed snapshot (upsert by token).
// RowID snapshots are written directly as compact binary BLOBs; ID-list snapshots
// are written as compact JSON.
func (c *Catalog) SaveShortsFeedSnapshot(
	ctx context.Context,
	token string,
	snapshot *ShortsFeedSnapshot,
	lastAccess time.Time,
) error {
	if token == "" {
		return errors.New("empty shorts feed token")
	}
	if snapshot == nil || snapshot.Total() == 0 {
		return errors.New("empty shorts feed snapshot")
	}
	var stored any
	if snapshot.RowIDs != nil {
		stored = snapshot.RowIDs.Blob
	} else {
		payload := *snapshot
		payload.Version = 2
		encoded, err := json.Marshal(&payload)
		if err != nil {
			return err
		}
		stored = string(encoded)
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO shorts_feed_sessions (token, video_ids, last_access)
		 VALUES (?, ?, ?)
		 ON CONFLICT(token) DO UPDATE SET
		   video_ids = excluded.video_ids,
		   last_access = excluded.last_access`,
		token, stored, lastAccess.UnixMilli(),
	)
	return err
}

// LoadShortsFeedSnapshot reads a persisted shorts snapshot. Compact binary SFB1
// blobs, v2 JSON objects, and legacy v1 JSON arrays are all decoded cleanly.
// Missing tokens return (nil, zero, nil).
func (c *Catalog) LoadShortsFeedSnapshot(
	ctx context.Context,
	token string,
) (*ShortsFeedSnapshot, time.Time, error) {
	var raw []byte
	var lastAccess int64
	err := c.db.QueryRowContext(ctx,
		`SELECT video_ids, last_access FROM shorts_feed_sessions WHERE token = ?`,
		token,
	).Scan(&raw, &lastAccess)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(raw) == 0 {
		return nil, time.Time{}, nil
	}
	if len(raw) >= 4 && bytes.Equal(raw[0:4], shortsBinaryMagic[:]) {
		snapshot, err := decodeRowIDSnapshot(raw)
		if err != nil {
			return nil, time.Time{}, err
		}
		return snapshot, time.UnixMilli(lastAccess), nil
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, time.Time{}, nil
	}
	if trimmed[0] == '[' {
		var videoIDs []string
		if err := json.Unmarshal(trimmed, &videoIDs); err != nil {
			return nil, time.Time{}, err
		}
		return &ShortsFeedSnapshot{VideoIDs: videoIDs}, time.UnixMilli(lastAccess), nil
	}

	var snapshot ShortsFeedSnapshot
	if err := json.Unmarshal(trimmed, &snapshot); err != nil {
		return nil, time.Time{}, err
	}
	return &snapshot, time.UnixMilli(lastAccess), nil
}

// SaveShortsFeed 持久化一个洗牌后的短视频 feed 快照（同 token 覆盖写）。
func (c *Catalog) SaveShortsFeed(ctx context.Context, token string, videoIDs []string, lastAccess time.Time) error {
	if token == "" {
		return errors.New("empty shorts feed token")
	}
	encoded, err := json.Marshal(videoIDs)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx,
		`INSERT INTO shorts_feed_sessions (token, video_ids, last_access)
		 VALUES (?, ?, ?)
		 ON CONFLICT(token) DO UPDATE SET
		   video_ids = excluded.video_ids,
		   last_access = excluded.last_access`,
		token, string(encoded), lastAccess.UnixMilli(),
	)
	return err
}

// LoadShortsFeed 读取快照；token 不存在时返回 (nil, 零值, nil)。
// 是否过期由调用方按自己的 TTL 判断。
func (c *Catalog) LoadShortsFeed(ctx context.Context, token string) ([]string, time.Time, error) {
	snapshot, lastAccess, err := c.LoadShortsFeedSnapshot(ctx, token)
	if err != nil || snapshot == nil {
		return nil, lastAccess, err
	}
	return snapshot.VideoIDs, lastAccess, nil
}

// TouchShortsFeed 刷新快照的最近访问时间，维持活跃会话不过期。
func (c *Catalog) TouchShortsFeed(ctx context.Context, token string, lastAccess time.Time) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE shorts_feed_sessions SET last_access = ? WHERE token = ?`,
		lastAccess.UnixMilli(), token,
	)
	return err
}

// DeleteShortsFeed 删除单个快照（读取到过期或损坏数据时清理）。
func (c *Catalog) DeleteShortsFeed(ctx context.Context, token string) error {
	_, err := c.db.ExecContext(ctx,
		`DELETE FROM shorts_feed_sessions WHERE token = ?`, token,
	)
	return err
}

// PruneShortsFeeds 删除最近访问早于 activeSince 的快照，并把总数收敛到
// maxSessions 以内（超出部分按最近访问从旧到新淘汰）。
func (c *Catalog) PruneShortsFeeds(ctx context.Context, activeSince time.Time, maxSessions int) error {
	if _, err := c.db.ExecContext(ctx,
		`DELETE FROM shorts_feed_sessions WHERE last_access < ?`,
		activeSince.UnixMilli(),
	); err != nil {
		return err
	}
	if maxSessions <= 0 {
		return nil
	}
	_, err := c.db.ExecContext(ctx,
		`DELETE FROM shorts_feed_sessions WHERE token NOT IN (
		   SELECT token FROM shorts_feed_sessions
		    ORDER BY last_access DESC, token DESC
		    LIMIT ?
		 )`, maxSessions,
	)
	return err
}
