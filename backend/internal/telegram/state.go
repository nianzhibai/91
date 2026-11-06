package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/updates"
	"github.com/video-site/backend/internal/atomicfile"
)

// Keys and update cursors live outside library/ and are excluded from backups.
func writePrivateFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".telegram-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	return atomicfile.SyncDirectory(filepath.Dir(path))
}

type atomicSession struct {
	path string
	mu   sync.Mutex
}

func (s *atomicSession) LoadSession(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, session.ErrNotFound
	}
	return data, err
}
func (s *atomicSession) StoreSession(ctx context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writePrivateFile(ctx, s.path, data)
}

type runtimeSnapshot struct {
	State         *updates.State  `json:"state,omitempty"`
	Channels      map[int64]int   `json:"channels"`
	ChannelHashes map[int64]int64 `json:"channelHashes"`
	UserHashes    map[int64]int64 `json:"userHashes"`
}
type runtimeState struct {
	mu     sync.Mutex
	path   string
	data   runtimeSnapshot
	cancel context.CancelFunc
	err    error
}

func openRuntimeState(path string, cancel context.CancelFunc) (*runtimeState, error) {
	s := &runtimeState{path: path, cancel: cancel}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &s.data); err != nil {
			return nil, err
		}
	}
	if s.data.Channels == nil {
		s.data.Channels = map[int64]int{}
	}
	if s.data.ChannelHashes == nil {
		s.data.ChannelHashes = map[int64]int64{}
	}
	if s.data.UserHashes == nil {
		s.data.UserHashes = map[int64]int64{}
	}
	return s, nil
}
func (s *runtimeState) failure() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *runtimeState) save(ctx context.Context) error {
	if s.err != nil {
		return s.err
	}
	data, err := json.Marshal(s.data)
	if err == nil {
		err = writePrivateFile(ctx, s.path, data)
	}
	if err != nil {
		s.err = err
		s.cancel()
	}
	return err
}
func (s *runtimeState) GetState(ctx context.Context, _ int64) (updates.State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.State == nil {
		return updates.State{}, false, s.err
	}
	return *s.data.State, true, s.err
}
func (s *runtimeState) SetState(ctx context.Context, _ int64, state updates.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.State = &state
	return s.save(ctx)
}
func (s *runtimeState) changeState(ctx context.Context, change func(*updates.State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.State == nil {
		return errors.New("Telegram update state not initialized")
	}
	change(s.data.State)
	return s.save(ctx)
}
func (s *runtimeState) GetChannelPts(ctx context.Context, _, channel int64) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pts, ok := s.data.Channels[channel]
	return pts, ok, s.err
}
func (s *runtimeState) SetChannelPts(ctx context.Context, _, channel int64, pts int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Channels[channel] = pts
	return s.save(ctx)
}
func (s *runtimeState) ForEachChannels(ctx context.Context, _ int64, f func(context.Context, int64, int) error) error {
	s.mu.Lock()
	copied := make(map[int64]int, len(s.data.Channels))
	for id, pts := range s.data.Channels {
		copied[id] = pts
	}
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for id, pts := range copied {
		if err := f(ctx, id, pts); err != nil {
			return err
		}
	}
	return nil
}
func (s *runtimeState) GetChannelAccessHash(ctx context.Context, _, channel int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, ok := s.data.ChannelHashes[channel]
	return hash, ok, s.err
}
func (s *runtimeState) SetChannelAccessHash(ctx context.Context, _, channel, hash int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.data.ChannelHashes[channel]; ok && old == hash {
		return s.err
	}
	s.data.ChannelHashes[channel] = hash
	return s.save(ctx)
}
func (s *runtimeState) GetUserAccessHash(ctx context.Context, _, user int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash, ok := s.data.UserHashes[user]
	return hash, ok, s.err
}
func (s *runtimeState) SetUserAccessHash(ctx context.Context, _, user, hash int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.data.UserHashes[user]; ok && old == hash {
		return s.err
	}
	s.data.UserHashes[user] = hash
	return s.save(ctx)
}

func (s *runtimeState) SetPts(ctx context.Context, _ int64, value int) error {
	return s.changeState(ctx, func(s *updates.State) { s.Pts = value })
}

func (s *runtimeState) SetQts(ctx context.Context, _ int64, value int) error {
	return s.changeState(ctx, func(s *updates.State) { s.Qts = value })
}

func (s *runtimeState) SetDate(ctx context.Context, _ int64, value int) error {
	return s.changeState(ctx, func(s *updates.State) { s.Date = value })
}

func (s *runtimeState) SetSeq(ctx context.Context, _ int64, value int) error {
	return s.changeState(ctx, func(s *updates.State) { s.Seq = value })
}

func (s *runtimeState) SetDateSeq(ctx context.Context, _ int64, date, seq int) error {
	return s.changeState(ctx, func(s *updates.State) { s.Date, s.Seq = date, seq })
}
