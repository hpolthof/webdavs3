package webui

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

const csrfCookieName = "webui_csrf"
const csrfTTL = 24 * time.Hour

type csrfStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time // token → expiry
}

func newCSRFStore() *csrfStore {
	return &csrfStore{tokens: map[string]time.Time{}}
}

// newToken generates a fresh CSRF token, stores it in a cookie, and returns the token.
func (c *csrfStore) newToken(w http.ResponseWriter, r *http.Request) string {
	var b [32]byte
	rand.Read(b[:])
	token := hex.EncodeToString(b[:])

	expires := time.Now().Add(csrfTTL)
	c.mu.Lock()
	c.tokens[token] = expires
	c.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	})
	return token
}

// valid returns true if the submitted token matches the cookie token and has not expired.
func (c *csrfStore) valid(r *http.Request, submitted string) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil {
		return false
	}
	if submitted == "" || submitted != cookie.Value {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.tokens[submitted]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(c.tokens, submitted)
		return false
	}
	return true
}
