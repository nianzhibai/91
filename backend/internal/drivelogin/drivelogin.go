// Package drivelogin runs cloud drive QR logins behind one protocol: start a
// session for a drive kind, poll it until it ends, and receive credentials
// keyed by the drive's own credential fields. Provider differences stay in
// the provider adapters; clients never see them.
package drivelogin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// State is the provider-independent progress of a login.
type State string

const (
	Waiting  State = "waiting"  // The QR code is shown and has not been scanned.
	Scanned  State = "scanned"  // Scanned; the user still has to confirm on the phone.
	Success  State = "success"  // Credentials are available.
	Expired  State = "expired"  // The QR code can no longer be used.
	Rejected State = "rejected" // The user declined or canceled on the phone.
	Failed   State = "failed"   // The provider reported an unrecoverable problem.
)

// Done reports whether the login has ended.
func (s State) Done() bool { return s != Waiting && s != Scanned }

// Challenge is what a provider returns when a login starts.
type Challenge struct {
	QRCodeURL      string
	QRImageDataURL string
	// ExpiresAt is the end of the QR code's validity. Zero uses DefaultTTL.
	ExpiresAt time.Time
	// Interval is the polling delay the provider expects. Zero uses DefaultInterval.
	Interval time.Duration
	// Poll checks the provider once. It is never called concurrently for one session.
	Poll func(context.Context) (Status, error)
}

// Status is one observation of a login.
type Status struct {
	State   State
	Message string
	// Credentials are set only on Success; empty values are dropped.
	Credentials map[string]string
	// Interval optionally replaces the polling delay from this point on.
	Interval time.Duration
}

// Provider starts a QR login for one drive kind.
type Provider func(context.Context) (Challenge, error)

const (
	DefaultTTL      = 5 * time.Minute
	DefaultInterval = 2 * time.Second
	minInterval     = time.Second
	maxSessions     = 16
	// FinishedRetention keeps a final result available to repeated polls, so a
	// client that lost the response carrying credentials can ask again.
	FinishedRetention = time.Minute
)

var (
	ErrUnsupported = errors.New("this drive kind does not support QR login")
	ErrNotFound    = errors.New("login session not found")
)

// Session is the client-facing description of a started login.
type Session struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	QRCodeURL      string    `json:"qrCodeUrl,omitempty"`
	QRImageDataURL string    `json:"qrImageDataUrl"`
	ExpiresAt      time.Time `json:"expiresAt"`
	IntervalMs     int64     `json:"intervalMs"`
}

// Result is the client-facing outcome of one poll.
type Result struct {
	State       State             `json:"state"`
	Message     string            `json:"message,omitempty"`
	Credentials map[string]string `json:"credentials,omitempty"`
	IntervalMs  int64             `json:"intervalMs"`
}

type session struct {
	expiresAt time.Time
	poll      func(context.Context) (Status, error)
	mu        sync.Mutex    // Serializes polls of one session.
	interval  time.Duration // Guarded by mu.

	// Guarded by Manager.mu, which pruning holds without polling.
	final    *Result
	forgetAt time.Time
}

// Manager keeps active logins in memory. A login lost on restart is simply
// started again; QR codes expire within minutes anyway.
type Manager struct {
	providers map[string]Provider
	now       func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
}

func New(providers map[string]Provider) *Manager {
	return &Manager{providers: providers, now: time.Now, sessions: map[string]*session{}}
}

// Kinds lists the drive kinds that support QR login.
func (m *Manager) Kinds() []string {
	kinds := make([]string, 0, len(m.providers))
	for kind := range m.providers {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

func (m *Manager) Start(ctx context.Context, kind string) (Session, error) {
	provider, ok := m.providers[kind]
	if !ok {
		return Session{}, ErrUnsupported
	}
	challenge, err := provider(ctx)
	if err != nil {
		return Session{}, err
	}
	if challenge.Poll == nil || challenge.QRImageDataURL == "" {
		return Session{}, errors.New("drive login provider returned an incomplete QR code")
	}
	now := m.now()
	expiresAt := challenge.ExpiresAt
	if expiresAt.IsZero() || !expiresAt.After(now) {
		expiresAt = now.Add(DefaultTTL)
	}
	s := &session{
		expiresAt: expiresAt,
		interval:  clampInterval(challenge.Interval),
		poll:      challenge.Poll,
		// An unfinished login past its expiry still reports "expired" once polled.
		forgetAt: expiresAt.Add(FinishedRetention),
	}
	id, err := newID()
	if err != nil {
		return Session{}, err
	}

	m.mu.Lock()
	m.pruneLocked(now)
	if len(m.sessions) >= maxSessions {
		m.evictOldestLocked()
	}
	m.sessions[id] = s
	m.mu.Unlock()

	return Session{
		ID:             id,
		Kind:           kind,
		QRCodeURL:      challenge.QRCodeURL,
		QRImageDataURL: challenge.QRImageDataURL,
		ExpiresAt:      expiresAt.UTC(),
		IntervalMs:     s.interval.Milliseconds(),
	}, nil
}

// Poll checks a login once. A final state is kept for FinishedRetention and
// returned again to later polls; after that, or after Cancel, polls return
// ErrNotFound.
func (m *Manager) Poll(ctx context.Context, id string) (Result, error) {
	s, err := m.lookup(id)
	if err != nil {
		return Result{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// A concurrent poll may have finished the login while this one waited.
	if final := m.finalResult(s); final != nil {
		return *final, nil
	}
	var status Status
	if m.now().After(s.expiresAt) {
		status = Status{State: Expired, Message: "二维码已过期，请重新生成"}
	} else {
		if status, err = s.poll(ctx); err != nil {
			return Result{}, err
		}
	}
	status = normalize(status)
	if status.Interval > 0 {
		s.interval = clampInterval(status.Interval)
	}
	result := Result{
		State:       status.State,
		Message:     status.Message,
		Credentials: status.Credentials,
		IntervalMs:  s.interval.Milliseconds(),
	}
	if status.State.Done() {
		m.mu.Lock()
		s.final = &result
		s.forgetAt = m.now().Add(FinishedRetention)
		m.mu.Unlock()
	}
	return result, nil
}

func (m *Manager) lookup(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	if m.now().After(s.forgetAt) {
		delete(m.sessions, id)
		return nil, ErrNotFound
	}
	return s, nil
}

func (m *Manager) finalResult(s *session) *Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	return s.final
}

// Cancel forgets a login. Unknown IDs are ignored.
func (m *Manager) Cancel(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

func normalize(status Status) Status {
	switch status.State {
	case Waiting, Scanned, Expired, Rejected, Failed:
		status.Credentials = nil
	case Success:
		credentials := make(map[string]string, len(status.Credentials))
		for key, value := range status.Credentials {
			if value != "" {
				credentials[key] = value
			}
		}
		if len(credentials) == 0 {
			return Status{State: Failed, Message: "登录成功但未获取到登录凭据，请重新生成二维码"}
		}
		status.Credentials = credentials
	default:
		return Status{State: Failed, Message: "扫码状态异常，请重新生成二维码"}
	}
	return status
}

func clampInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return DefaultInterval
	}
	return max(interval, minInterval)
}

func (m *Manager) pruneLocked(now time.Time) {
	for id, s := range m.sessions {
		if now.After(s.forgetAt) {
			delete(m.sessions, id)
		}
	}
}

func (m *Manager) evictOldestLocked() {
	oldestID := ""
	for id, s := range m.sessions {
		if oldestID == "" || s.expiresAt.Before(m.sessions[oldestID].expiresAt) {
			oldestID = id
		}
	}
	delete(m.sessions, oldestID)
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
