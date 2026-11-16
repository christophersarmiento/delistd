package web

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/drumandbytes/delistd/internal/config"
)

// Session is the setup wizard's state. It lives on the server so the mail
// password never goes to the browser, which only holds an opaque id.
type Session struct {
	ID         string
	Step       string
	Profile    config.Profile
	Email      config.EmailConfig
	ManualSend bool // sending by hand, so no SMTP account
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// SessionStore holds sessions in memory until they have been idle for ttl.
// Expired ones are dropped when they are looked up and whenever a new one is
// created, so no background goroutine is needed.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
	ttl      time.Duration
}

func NewSessionStore(ttl time.Duration) *SessionStore {
	return &SessionStore{sessions: map[string]*Session{}, ttl: ttl}
}

// Create starts an empty session and returns its id: 256 random bits.
func (s *SessionStore) Create() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	for k, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = &Session{ID: id, CreatedAt: now, ExpiresAt: now.Add(s.ttl)}
	return id, nil
}

// live returns the unexpired session for id, dropping it if it has expired.
// The caller holds s.mu.
func (s *SessionStore) live(id string) *Session {
	sess := s.sessions[id]
	if sess == nil {
		return nil
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.sessions, id)
		return nil
	}
	return sess
}

// Get returns a snapshot of the session, or nil when there is none or it has
// expired. It is a copy: changes go through Update, so a reader never sees a
// session another request is halfway through changing.
func (s *SessionStore) Get(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.live(id)
	if sess == nil {
		return nil
	}
	snapshot := *sess
	return &snapshot
}

// Update applies change to the session and restarts its idle timer. It
// reports false when there is no live session.
func (s *SessionStore) Update(id string, change func(*Session)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.live(id)
	if sess == nil {
		return false
	}
	change(sess)
	sess.ExpiresAt = time.Now().Add(s.ttl)
	return true
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}
