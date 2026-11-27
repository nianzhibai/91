package applog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const (
	dbFileName         = "logs.db"
	writeQueueCapacity = 4096
	writeBatchSize     = 128
	writeFlushInterval = 100 * time.Millisecond
	pruneIntervalCount = 256
)

const storeSchemaSQL = `
CREATE TABLE IF NOT EXISTS logs (
	id          INTEGER PRIMARY KEY,
	ts_ns       INTEGER NOT NULL,
	level       TEXT    NOT NULL,
	source      TEXT    NOT NULL,
	component   TEXT    NOT NULL DEFAULT '',
	message     TEXT    NOT NULL DEFAULT '',
	error       TEXT    NOT NULL DEFAULT '',
	stack       TEXT    NOT NULL DEFAULT '',
	request_id  TEXT    NOT NULL DEFAULT '',
	task_id     TEXT    NOT NULL DEFAULT '',
	drive_id    TEXT    NOT NULL DEFAULT '',
	video_id    TEXT    NOT NULL DEFAULT '',
	file_id     TEXT    NOT NULL DEFAULT '',
	stage       TEXT    NOT NULL DEFAULT '',
	http_method TEXT    NOT NULL DEFAULT '',
	http_path   TEXT    NOT NULL DEFAULT '',
	http_route  TEXT    NOT NULL DEFAULT '',
	http_status INTEGER NOT NULL DEFAULT 0,
	http_bytes  INTEGER NOT NULL DEFAULT 0,
	http_dur_us INTEGER NOT NULL DEFAULT 0,
	http_remote TEXT    NOT NULL DEFAULT '',
	attrs_json  TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_logs_ts         ON logs(ts_ns, id);
CREATE INDEX IF NOT EXISTS idx_logs_level      ON logs(level, id);
CREATE INDEX IF NOT EXISTS idx_logs_source     ON logs(source, id);
CREATE INDEX IF NOT EXISTS idx_logs_component  ON logs(component, id)  WHERE component != '';
CREATE INDEX IF NOT EXISTS idx_logs_request_id ON logs(request_id, id) WHERE request_id != '';
CREATE INDEX IF NOT EXISTS idx_logs_task_id    ON logs(task_id, id)    WHERE task_id != '';
CREATE INDEX IF NOT EXISTS idx_logs_drive_id   ON logs(drive_id, id)   WHERE drive_id != '';
CREATE INDEX IF NOT EXISTS idx_logs_video_id   ON logs(video_id, id)   WHERE video_id != '';
CREATE INDEX IF NOT EXISTS idx_logs_status     ON logs(http_status, id) WHERE http_status > 0;

CREATE VIRTUAL TABLE IF NOT EXISTS logs_fts USING fts5(
	message,
	error,
	stack,
	component,
	request_id,
	task_id,
	drive_id,
	video_id,
	file_id,
	stage,
	http_method,
	http_path,
	http_route,
	http_remote,
	attrs_json,
	content='logs',
	content_rowid='id',
	tokenize='trigram'
);

CREATE TRIGGER IF NOT EXISTS logs_ai AFTER INSERT ON logs BEGIN
	INSERT INTO logs_fts(
		rowid, message, error, stack, component, request_id, task_id,
		drive_id, video_id, file_id, stage, http_method, http_path, http_route, http_remote, attrs_json
	) VALUES (
		new.id, new.message, new.error, new.stack, new.component, new.request_id, new.task_id,
		new.drive_id, new.video_id, new.file_id, new.stage, new.http_method, new.http_path, new.http_route, new.http_remote, new.attrs_json
	);
END;

CREATE TRIGGER IF NOT EXISTS logs_ad AFTER DELETE ON logs BEGIN
	INSERT INTO logs_fts(
		logs_fts, rowid, message, error, stack, component, request_id, task_id,
		drive_id, video_id, file_id, stage, http_method, http_path, http_route, http_remote, attrs_json
	) VALUES (
		'delete', old.id, old.message, old.error, old.stack, old.component, old.request_id, old.task_id,
		old.drive_id, old.video_id, old.file_id, old.stage, old.http_method, old.http_path, old.http_route, old.http_remote, old.attrs_json
	);
END;
`

type StoreConfig struct {
	Directory         string
	MaxTotalSizeBytes int64
	RetentionDays     int
	LevelFunc         func() Level
}

type controlOp uint8

const (
	controlFlush controlOp = iota + 1
	controlClear
	controlPrune
	controlClose
)

type controlRequest struct {
	op   controlOp
	done chan error
}

// Store persists structured log records in a dedicated SQLite WAL database
// (`logs.db`) using a single asynchronous batch writer and concurrent readers.
type Store struct {
	directory string
	dbPath    string
	writeDB   *sql.DB
	readDB    *sql.DB
	hub       *Hub
	levelFunc func() Level

	nextID atomic.Int64
	closed atomic.Bool

	limitsMu          sync.RWMutex
	maxTotalSizeBytes int64
	retentionDays     int

	healthMu         sync.Mutex
	writeHealth      WriteHealth
	lastWriteWarning time.Time

	recordsCh chan Record
	ctrlCh    chan controlRequest
	doneCh    chan struct{}

	now func() time.Time
}

func Open(cfg StoreConfig) (*Store, error) {
	directory := strings.TrimSpace(cfg.Directory)
	if directory == "" {
		return nil, errors.New("log directory is required")
	}
	absDir, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve log directory: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	if cfg.MaxTotalSizeBytes <= 0 {
		cfg.MaxTotalSizeBytes = DefaultMaxTotalSizeBytes
	}
	if cfg.RetentionDays < 0 {
		cfg.RetentionDays = 0
	}

	dbPath := filepath.Join(absDir, dbFileName)
	writeDB, readDB, err := openLogDatabasesWithRecovery(dbPath)
	if err != nil {
		return nil, err
	}

	var maxID int64
	if err := readDB.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM logs`).Scan(&maxID); err != nil {
		_ = readDB.Close()
		_ = writeDB.Close()
		return nil, fmt.Errorf("recover last log id: %w", err)
	}

	store := &Store{
		directory:         absDir,
		dbPath:            dbPath,
		writeDB:           writeDB,
		readDB:            readDB,
		hub:               NewHub(),
		levelFunc:         cfg.LevelFunc,
		maxTotalSizeBytes: cfg.MaxTotalSizeBytes,
		retentionDays:     cfg.RetentionDays,
		recordsCh:         make(chan Record, writeQueueCapacity),
		ctrlCh:            make(chan controlRequest),
		doneCh:            make(chan struct{}),
		now:               time.Now,
	}
	store.nextID.Store(maxID)
	go store.writerLoop()
	return store, nil
}

func openLogDatabasesWithRecovery(dbPath string) (*sql.DB, *sql.DB, error) {
	writeDB, readDB, err := openLogDatabases(dbPath)
	if err == nil {
		return writeDB, readDB, nil
	}
	// Logs are disposable runtime telemetry: if the database file is corrupted,
	// move it aside and recreate a clean database instead of blocking startup.
	if _, statErr := os.Stat(dbPath); statErr == nil {
		stamp := time.Now().UTC().Format("20060102T150405Z")
		_ = os.Rename(dbPath, dbPath+".corrupt-"+stamp)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
		return openLogDatabases(dbPath)
	}
	return nil, nil, err
}

func openLogDatabases(dbPath string) (*sql.DB, *sql.DB, error) {
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)"
	writeDB, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, nil, fmt.Errorf("open log writer db: %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)

	if err := initLogDatabase(writeDB); err != nil {
		_ = writeDB.Close()
		return nil, nil, err
	}

	readDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = writeDB.Close()
		return nil, nil, fmt.Errorf("open log reader db: %w", err)
	}
	readDB.SetMaxOpenConns(4)
	readDB.SetMaxIdleConns(2)
	return writeDB, readDB, nil
}

func initLogDatabase(db *sql.DB) error {
	var integrity string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("check log db integrity: %w", err)
	}
	if !strings.EqualFold(integrity, "ok") {
		return fmt.Errorf("corrupt log db: %s", integrity)
	}
	var autoVacuum int
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil {
		return fmt.Errorf("inspect log db auto_vacuum: %w", err)
	}
	if autoVacuum != 2 {
		if _, err := db.Exec(`PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
			return fmt.Errorf("enable incremental auto_vacuum: %w", err)
		}
		var pageCount int64
		if err := db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err == nil && pageCount > 0 {
			if _, err := db.Exec(`VACUUM`); err != nil {
				return fmt.Errorf("apply incremental auto_vacuum: %w", err)
			}
		}
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("enable WAL mode: %w", err)
	}
	if _, err := db.Exec(storeSchemaSQL); err != nil {
		return fmt.Errorf("apply log schema: %w", err)
	}
	return nil
}

func (s *Store) Directory() string {
	if s == nil {
		return ""
	}
	return s.directory
}

func (s *Store) DBPath() string {
	if s == nil {
		return ""
	}
	return s.dbPath
}

func (s *Store) SetLimits(maxTotalBytes int64, retentionDays int) {
	if s == nil {
		return
	}
	s.limitsMu.Lock()
	if maxTotalBytes > 0 {
		s.maxTotalSizeBytes = maxTotalBytes
	}
	if retentionDays >= 0 {
		s.retentionDays = retentionDays
	}
	s.limitsMu.Unlock()
	_ = s.sendControl(context.Background(), controlPrune)
}

func (s *Store) limits() (int64, int) {
	s.limitsMu.RLock()
	defer s.limitsMu.RUnlock()
	return s.maxTotalSizeBytes, s.retentionDays
}

// Submit assigns a monotonic ID, broadcasts to live SSE subscribers, and queues
// the record for asynchronous SQLite persistence. It never blocks callers.
func (s *Store) Submit(rec Record) int64 {
	if s == nil || s.closed.Load() {
		return 0
	}
	rec = normalizeRecord(rec)
	id := s.nextID.Add(1)
	rec.ID = id
	s.hub.Publish(rec)
	select {
	case s.recordsCh <- rec:
	default:
		s.recordDrop()
	}
	return id
}

// Append submits a record and flushes it synchronously (primarily for tests).
func (s *Store) Append(ctx context.Context, rec Record) (int64, error) {
	if s == nil {
		return 0, errors.New("log store is unavailable")
	}
	if s.closed.Load() {
		return 0, errors.New("log store is closed")
	}
	id := s.Submit(rec)
	if err := s.Flush(ctx); err != nil {
		return id, err
	}
	return id, nil
}

// Flush waits until all records queued before this call are committed to SQLite.
func (s *Store) Flush(ctx context.Context) error {
	if s == nil {
		return errors.New("log store is unavailable")
	}
	return s.sendControl(ctx, controlFlush)
}

// Prune enforces retention and storage limits immediately.
func (s *Store) Prune(ctx context.Context) error {
	if s == nil {
		return errors.New("log store is unavailable")
	}
	return s.sendControl(ctx, controlPrune)
}

// Clear atomically removes all persisted log records and reclaims disk space.
func (s *Store) Clear(ctx context.Context) error {
	if s == nil {
		return errors.New("log store is unavailable")
	}
	return s.sendControl(ctx, controlClear)
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	done := make(chan error, 1)
	select {
	case s.ctrlCh <- controlRequest{op: controlClose, done: done}:
		<-done
	case <-s.doneCh:
	}
	<-s.doneCh
	readErr := s.readDB.Close()
	writeErr := s.writeDB.Close()
	return errors.Join(readErr, writeErr)
}

func (s *Store) Subscribe(query Query) (<-chan Record, func()) {
	if s == nil {
		ch := make(chan Record)
		close(ch)
		return ch, func() {}
	}
	return s.hub.Subscribe(query)
}

func (s *Store) Health() WriteHealth {
	if s == nil {
		return WriteHealth{}
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	return s.writeHealth
}

func (s *Store) sendControl(ctx context.Context, op controlOp) error {
	if s.closed.Load() {
		return errors.New("log store is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	select {
	case s.ctrlCh <- controlRequest{op: op, done: done}:
	case <-s.doneCh:
		return errors.New("log store is closed")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-s.doneCh:
		return errors.New("log store is closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) writerLoop() {
	defer close(s.doneCh)
	batch := make([]Record, 0, writeBatchSize)
	ticker := time.NewTicker(writeFlushInterval)
	defer ticker.Stop()

	writtenSincePrune := 0
	flushBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.writeBatch(batch)
		writtenSincePrune += len(batch)
		batch = batch[:0]
		if err != nil {
			s.recordWriteError(err)
			return err
		}
		s.recordWriteSuccess()
		if writtenSincePrune >= pruneIntervalCount {
			writtenSincePrune = 0
			_ = s.pruneLocked()
		}
		return nil
	}

	drainQueued := func() {
		for {
			select {
			case rec := <-s.recordsCh:
				batch = append(batch, rec)
				if len(batch) >= writeBatchSize {
					_ = flushBatch()
				}
			default:
				return
			}
		}
	}

	for {
		select {
		case rec := <-s.recordsCh:
			batch = append(batch, rec)
			if len(batch) >= writeBatchSize {
				_ = flushBatch()
			}
		case <-ticker.C:
			_ = flushBatch()
		case req := <-s.ctrlCh:
			switch req.op {
			case controlFlush:
				drainQueued()
				req.done <- flushBatch()
			case controlPrune:
				drainQueued()
				err := flushBatch()
				if pruneErr := s.pruneLocked(); err == nil {
					err = pruneErr
				}
				req.done <- err
			case controlClear:
				drainQueued()
				batch = batch[:0]
				req.done <- s.clearLocked()
			case controlClose:
				drainQueued()
				req.done <- flushBatch()
				return
			}
		}
	}
}

func (s *Store) writeBatch(batch []Record) error {
	tx, err := s.writeDB.Begin()
	if err != nil {
		return fmt.Errorf("begin log batch: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO logs (
		id, ts_ns, level, source, component, message, error, stack,
		request_id, task_id, drive_id, video_id, file_id, stage,
		http_method, http_path, http_route, http_status, http_bytes, http_dur_us, http_remote,
		attrs_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare log insert: %w", err)
	}
	defer stmt.Close()

	for _, rec := range batch {
		var method, path, route, remote string
		var status, bytesCount int
		var durUS int64
		if rec.HTTP != nil {
			method = string(rec.HTTP.Method)
			path = rec.HTTP.Path
			route = rec.HTTP.Route
			status = rec.HTTP.Status
			bytesCount = rec.HTTP.Bytes
			durUS = int64(math.Round(rec.HTTP.DurationMs * 1000))
			remote = rec.HTTP.Remote
		}
		attrsJSON := ""
		if len(rec.Attrs) > 0 {
			if raw, marshalErr := json.Marshal(rec.Attrs); marshalErr == nil {
				attrsJSON = truncateUTF8(string(raw), maxAttrsBytes)
			}
		}
		if _, err := stmt.Exec(
			rec.ID, rec.Timestamp.UTC().UnixNano(), string(rec.Level), string(rec.Source),
			rec.Component, rec.Message, rec.Error, rec.Stack,
			rec.RequestID, rec.TaskID, rec.DriveID, rec.VideoID, rec.FileID, rec.Stage,
			method, path, route, status, bytesCount, durUS, remote, attrsJSON,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert log record: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit log batch: %w", err)
	}
	return nil
}

func (s *Store) clearLocked() error {
	tx, err := s.writeDB.Begin()
	if err != nil {
		return fmt.Errorf("begin clear logs: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM logs`); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete logs: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO logs_fts(logs_fts) VALUES('delete-all')`); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("clear log fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit clear logs: %w", err)
	}
	_, _ = s.writeDB.Exec(`PRAGMA incremental_vacuum`)
	_, _ = s.writeDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return nil
}

func (s *Store) pruneLocked() error {
	maxBytes, retentionDays := s.limits()
	changed := false
	if retentionDays > 0 {
		cutoff := s.now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour).UnixNano()
		res, err := s.writeDB.Exec(`DELETE FROM logs WHERE ts_ns < ?`, cutoff)
		if err != nil {
			return fmt.Errorf("prune expired logs: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed = true
		}
	}
	if changed {
		_, _ = s.writeDB.Exec(`PRAGMA incremental_vacuum`)
		_, _ = s.writeDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}

	if maxBytes <= 0 {
		return nil
	}
	for round := 0; round < 16; round++ {
		if s.storageBytes() <= maxBytes {
			break
		}
		var total int64
		if err := s.writeDB.QueryRow(`SELECT COUNT(*) FROM logs`).Scan(&total); err != nil {
			return fmt.Errorf("count logs for pruning: %w", err)
		}
		if total <= 1 {
			break
		}
		toDelete := max(total/5, int64(100))
		if toDelete >= total {
			toDelete = total - 1
		}
		if _, err := s.writeDB.Exec(`DELETE FROM logs WHERE id IN (SELECT id FROM logs ORDER BY id ASC LIMIT ?)`, toDelete); err != nil {
			return fmt.Errorf("prune oldest logs: %w", err)
		}
		_, _ = s.writeDB.Exec(`PRAGMA incremental_vacuum`)
		_, _ = s.writeDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	return nil
}

func (s *Store) storageBytes() int64 {
	var total int64
	for _, path := range []string{s.dbPath, s.dbPath + "-wal", s.dbPath + "-shm"} {
		if info, err := os.Stat(path); err == nil {
			total += info.Size()
		}
	}
	return total
}

func (s *Store) recordDrop() {
	now := s.now().UTC()
	s.healthMu.Lock()
	s.writeHealth.DroppedWrites++
	s.writeHealth.LastFailureAt = &now
	s.writeHealth.LastError = "log write queue full; dropped record"
	if s.lastWriteWarning.IsZero() || now.Sub(s.lastWriteWarning) >= 30*time.Second {
		fmt.Fprintf(os.Stderr, "[logging] write queue full (dropped=%d)\n", s.writeHealth.DroppedWrites)
		s.lastWriteWarning = now
	}
	s.healthMu.Unlock()
}

func (s *Store) recordWriteError(err error) {
	now := s.now().UTC()
	redactedErr := Redact(err.Error())
	s.healthMu.Lock()
	s.writeHealth.FailedWrites++
	s.writeHealth.LastError = redactedErr
	s.writeHealth.LastFailureAt = &now
	if s.lastWriteWarning.IsZero() || now.Sub(s.lastWriteWarning) >= 30*time.Second {
		fmt.Fprintf(os.Stderr, "[logging] store write failed: %s\n", redactedErr)
		s.lastWriteWarning = now
	}
	s.healthMu.Unlock()
}

func (s *Store) recordWriteSuccess() {
	now := s.now().UTC()
	s.healthMu.Lock()
	if s.writeHealth.LastError != "" && s.writeHealth.FailedWrites > 0 {
		fmt.Fprintln(os.Stderr, "[logging] store writes recovered")
	}
	s.writeHealth.LastError = ""
	s.writeHealth.LastSuccessAt = &now
	s.healthMu.Unlock()
}

// Query returns a bounded chronological slice of records matching query.
func (s *Store) Query(ctx context.Context, query Query) (Snapshot, error) {
	if s == nil {
		return Snapshot{}, errors.New("log store is unavailable")
	}
	if err := s.Flush(ctx); err != nil {
		return Snapshot{}, err
	}
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultQueryLimit
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}

	whereSQL, args := buildWhereClause(query)
	ascending := query.After > 0 && query.Before == 0
	orderDir := "DESC"
	if ascending {
		orderDir = "ASC"
	}
	stmt := `SELECT id, ts_ns, level, source, component, message, error, stack,
		request_id, task_id, drive_id, video_id, file_id, stage,
		http_method, http_path, http_route, http_status, http_bytes, http_dur_us, http_remote, attrs_json
		FROM logs` + whereSQL + ` ORDER BY id ` + orderDir + ` LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.readDB.QueryContext(ctx, stmt, args...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("query logs: %w", err)
	}
	defer rows.Close()

	entries := make([]Record, 0, min(limit, 256))
	for rows.Next() {
		rec, scanErr := scanRecord(rows)
		if scanErr != nil {
			return Snapshot{}, fmt.Errorf("scan log record: %w", scanErr)
		}
		entries = append(entries, rec)
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterate log records: %w", err)
	}

	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}
	if !ascending {
		slices.Reverse(entries)
	}
	snap := Snapshot{
		Entries: entries,
		HasMore: hasMore,
	}
	if len(entries) > 0 {
		snap.NextBefore = entries[0].ID
		snap.NextAfter = entries[len(entries)-1].ID
	}
	return snap, nil
}

// Export streams all matching records in chronological order to output as "jsonl" or "text".
func (s *Store) Export(ctx context.Context, query Query, format string, output io.Writer) error {
	if s == nil {
		return errors.New("log store is unavailable")
	}
	if err := s.Flush(ctx); err != nil {
		return err
	}
	query.Before = 0
	query.After = 0
	query.Limit = 0

	whereSQL, args := buildWhereClause(query)
	stmt := `SELECT id, ts_ns, level, source, component, message, error, stack,
		request_id, task_id, drive_id, video_id, file_id, stage,
		http_method, http_path, http_route, http_status, http_bytes, http_dur_us, http_remote, attrs_json
		FROM logs` + whereSQL + ` ORDER BY id ASC`

	rows, err := s.readDB.QueryContext(ctx, stmt, args...)
	if err != nil {
		return fmt.Errorf("export logs: %w", err)
	}
	defer rows.Close()

	asJSONL := strings.EqualFold(strings.TrimSpace(format), "jsonl")
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, scanErr := scanRecord(rows)
		if scanErr != nil {
			return fmt.Errorf("scan export record: %w", scanErr)
		}
		if asJSONL {
			if err := encoder.Encode(rec); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintln(output, FormatText(rec)); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

// Meta reports storage statistics, active runtime level, available components, and write health.
func (s *Store) Meta(ctx context.Context) (Meta, error) {
	if s == nil {
		return Meta{}, errors.New("log store is unavailable")
	}
	if err := s.Flush(ctx); err != nil {
		return Meta{}, err
	}
	maxBytes, retentionDays := s.limits()
	var totalCount int64
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs`).Scan(&totalCount); err != nil {
		return Meta{}, fmt.Errorf("count logs: %w", err)
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT DISTINCT component FROM logs WHERE component != '' ORDER BY component ASC LIMIT 200`)
	if err != nil {
		return Meta{}, fmt.Errorf("list log components: %w", err)
	}
	defer rows.Close()
	components := make([]string, 0)
	for rows.Next() {
		var comp string
		if scanErr := rows.Scan(&comp); scanErr == nil && comp != "" {
			components = append(components, comp)
		}
	}
	level := LevelInfo
	if s.levelFunc != nil {
		level = s.levelFunc()
	}
	return Meta{
		StorageBytes:    s.storageBytes(),
		MaxStorageBytes: maxBytes,
		TotalCount:      totalCount,
		Level:           level,
		RetentionDays:   retentionDays,
		Components:      components,
		WriteHealth:     s.Health(),
	}, nil
}

func buildWhereClause(q Query) (string, []any) {
	var conds []string
	var args []any

	if q.Before > 0 {
		conds = append(conds, "id < ?")
		args = append(args, q.Before)
	}
	if q.After > 0 {
		conds = append(conds, "id > ?")
		args = append(args, q.After)
	}
	if !q.From.IsZero() {
		conds = append(conds, "ts_ns >= ?")
		args = append(args, q.From.UTC().UnixNano())
	}
	if !q.To.IsZero() {
		conds = append(conds, "ts_ns <= ?")
		args = append(args, q.To.UTC().UnixNano())
	}
	if q.Source != "" {
		conds = append(conds, "source = ?")
		args = append(args, string(q.Source))
	}
	if len(q.Levels) == 1 {
		conds = append(conds, "level = ?")
		args = append(args, string(q.Levels[0]))
	} else if len(q.Levels) > 1 {
		placeholders := make([]string, len(q.Levels))
		for i, lvl := range q.Levels {
			placeholders[i] = "?"
			args = append(args, string(lvl))
		}
		conds = append(conds, "level IN ("+strings.Join(placeholders, ",")+")")
	}
	if comp := strings.TrimSpace(q.Component); comp != "" {
		conds = append(conds, "component = ?")
		args = append(args, comp)
	}
	if q.Method != "" {
		conds = append(conds, "http_method = ?")
		args = append(args, string(q.Method))
	}
	if q.StatusMin > 0 && q.StatusMax > 0 && q.StatusMin == q.StatusMax {
		conds = append(conds, "http_status = ?")
		args = append(args, q.StatusMin)
	} else {
		if q.StatusMin > 0 {
			conds = append(conds, "http_status >= ?")
			args = append(args, q.StatusMin)
		}
		if q.StatusMax > 0 {
			conds = append(conds, "http_status <= ?")
			args = append(args, q.StatusMax)
		}
	}
	if q.MinDurationMs > 0 {
		conds = append(conds, "http_dur_us >= ?")
		args = append(args, int64(math.Round(q.MinDurationMs*1000)))
	}
	if rid := strings.TrimSpace(q.RequestID); rid != "" {
		conds = append(conds, "request_id = ?")
		args = append(args, rid)
	}
	if tid := strings.TrimSpace(q.TaskID); tid != "" {
		conds = append(conds, "task_id = ?")
		args = append(args, tid)
	}
	if did := strings.TrimSpace(q.DriveID); did != "" {
		conds = append(conds, "drive_id = ?")
		args = append(args, did)
	}
	if vid := strings.TrimSpace(q.VideoID); vid != "" {
		conds = append(conds, "video_id = ?")
		args = append(args, vid)
	}
	if search := strings.TrimSpace(q.Search); search != "" {
		if utf8.RuneCountInString(search) >= 3 {
			ftsPhrase := `"` + strings.ReplaceAll(search, `"`, `""`) + `"`
			conds = append(conds, "(id IN (SELECT rowid FROM logs_fts WHERE logs_fts MATCH ?) OR CAST(http_status AS TEXT) = ?)")
			args = append(args, ftsPhrase, search)
		} else {
			pattern := "%" + escapeLike(strings.ToLower(search)) + "%"
			conds = append(conds, `(
				LOWER(message) LIKE ? ESCAPE '\' OR LOWER(error) LIKE ? ESCAPE '\' OR
				LOWER(component) LIKE ? ESCAPE '\' OR LOWER(http_path) LIKE ? ESCAPE '\' OR
				LOWER(attrs_json) LIKE ? ESCAPE '\' OR CAST(http_status AS TEXT) LIKE ? ESCAPE '\'
			)`)
			args = append(args, pattern, pattern, pattern, pattern, pattern, pattern)
		}
	}

	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func scanRecord(rows *sql.Rows) (Record, error) {
	var rec Record
	var tsNS int64
	var level, source string
	var method, path, route, remote string
	var status, bytesCount int
	var durUS int64
	var attrsJSON string
	if err := rows.Scan(
		&rec.ID, &tsNS, &level, &source, &rec.Component, &rec.Message, &rec.Error, &rec.Stack,
		&rec.RequestID, &rec.TaskID, &rec.DriveID, &rec.VideoID, &rec.FileID, &rec.Stage,
		&method, &path, &route, &status, &bytesCount, &durUS, &remote, &attrsJSON,
	); err != nil {
		return Record{}, err
	}
	rec.Timestamp = time.Unix(0, tsNS).UTC()
	rec.Level = Level(level)
	rec.Source = Source(source)
	if method != "" || path != "" || status != 0 || remote != "" {
		rec.HTTP = &HTTPRecord{
			Method:     Method(method),
			Path:       path,
			Route:      route,
			Status:     status,
			Bytes:      bytesCount,
			DurationMs: math.Round(float64(durUS)/10.0) / 100.0,
			Remote:     remote,
		}
	}
	if attrsJSON != "" {
		var attrs map[string]any
		if json.Unmarshal([]byte(attrsJSON), &attrs) == nil && len(attrs) > 0 {
			rec.Attrs = attrs
		}
	}
	return rec, nil
}
