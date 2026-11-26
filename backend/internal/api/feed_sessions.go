package api

import (
	crand "crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type feedSessionEntry[K comparable, V any] struct {
	value      V
	lastAccess time.Time
	sharedKey  *K
}

// feedSessionStore is a bounded, TTL-pruned in-memory session cache shared by
// both /api/feed and /api/shorts/next. Deterministic feeds may optionally pass
// a sharedKey and equality predicate to deduplicate identical immutable
// snapshots across concurrent viewers.
type feedSessionStore[K comparable, V any] struct {
	mu       sync.Mutex
	sessions map[string]*feedSessionEntry[K, V]
	shared   map[K]string
}

func (s *feedSessionStore[K, V]) put(
	token string,
	value V,
	now time.Time,
	ttl time.Duration,
	maxSessions int,
	sharedKey *K,
	sameValue func(a, b V) bool,
) (string, V) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sessions == nil {
		s.sessions = make(map[string]*feedSessionEntry[K, V])
	}
	if sharedKey != nil && s.shared == nil {
		s.shared = make(map[K]string)
	}
	s.pruneLocked(now, ttl)

	if sharedKey != nil && sameValue != nil {
		if existingToken := s.shared[*sharedKey]; existingToken != "" {
			if existing := s.sessions[existingToken]; existing != nil {
				if sameValue(existing.value, value) {
					existing.lastAccess = now
					return existingToken, existing.value
				}
			} else {
				delete(s.shared, *sharedKey)
			}
		}
	}

	for maxSessions > 0 && len(s.sessions) >= maxSessions {
		var oldestToken string
		var oldestAccess time.Time
		for candidateToken, entry := range s.sessions {
			if oldestToken == "" || entry.lastAccess.Before(oldestAccess) {
				oldestToken = candidateToken
				oldestAccess = entry.lastAccess
			}
		}
		if oldestToken == "" {
			break
		}
		s.deleteLocked(oldestToken)
	}

	var storedKey *K
	if sharedKey != nil {
		key := *sharedKey
		storedKey = &key
	}
	s.sessions[token] = &feedSessionEntry[K, V]{
		value:      value,
		lastAccess: now,
		sharedKey:  storedKey,
	}
	if storedKey != nil {
		s.shared[*storedKey] = token
	}
	return token, value
}

func (s *feedSessionStore[K, V]) get(token string, now time.Time, ttl time.Duration) (V, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked(now, ttl)
	entry := s.sessions[token]
	if entry == nil {
		var zero V
		return zero, false
	}
	entry.lastAccess = now
	return entry.value, true
}

func (s *feedSessionStore[K, V]) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *feedSessionStore[K, V]) peek(token string) (V, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.sessions[token]
	if entry == nil {
		var zero V
		return zero, false
	}
	return entry.value, true
}

func (s *feedSessionStore[K, V]) pruneLocked(now time.Time, ttl time.Duration) {
	for token, entry := range s.sessions {
		if now.Sub(entry.lastAccess) >= ttl {
			s.deleteLocked(token)
		}
	}
}

func (s *feedSessionStore[K, V]) deleteLocked(token string) {
	entry := s.sessions[token]
	if entry == nil {
		return
	}
	delete(s.sessions, token)
	if entry.sharedKey != nil && s.shared[*entry.sharedKey] == token {
		delete(s.shared, *entry.sharedKey)
	}
}

func feedQueryInt(r *http.Request, name string, fallback int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	return strconv.Atoi(raw)
}

func newFeedToken() (string, error) {
	var token [16]byte
	if _, err := crand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}
