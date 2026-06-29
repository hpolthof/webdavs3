package webui

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

const sessionCookieName = "webui_session"
const sessionTTL = 24 * time.Hour

type session struct {
	accessKey string
	expiresAt time.Time
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]session // token → session
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: map[string]session{}}
}

// create generates a new session token for the given access key, stores it, and sets the cookie.
func (s *sessionStore) create(w http.ResponseWriter, r *http.Request, accessKey string) string {
	var b [32]byte
	rand.Read(b[:])
	token := hex.EncodeToString(b[:])

	expires := time.Now().Add(sessionTTL)
	s.mu.Lock()
	s.sessions[token] = session{accessKey: accessKey, expiresAt: expires}
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	})
	return token
}

// valid returns true if the request carries a valid, non-expired session cookie.
func (s *sessionStore) valid(r *http.Request) bool {
	_, ok := s.get(r)
	return ok
}

// accessKey returns the access key associated with the request's session, if valid.
func (s *sessionStore) accessKey(r *http.Request) (string, bool) {
	return s.get(r)
}

func (s *sessionStore) get(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[c.Value]
	if !ok {
		return "", false
	}
	if time.Now().After(sess.expiresAt) {
		delete(s.sessions, c.Value)
		return "", false
	}
	return sess.accessKey, true
}

// delete removes the session identified by the request cookie.
func (s *sessionStore) delete(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookieName)
	if err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:    sessionCookieName,
		Value:   "",
		Path:    "/",
		Expires: time.Unix(0, 0),
		MaxAge:  -1,
	})
}
