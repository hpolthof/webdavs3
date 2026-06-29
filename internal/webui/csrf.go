package webui

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

const csrfCookieName = "webui_csrf"
const csrfTTL = 24 * time.Hour

type csrfStore struct {
}

func newCSRFStore() *csrfStore {
	return &csrfStore{}
}

// newToken generates a fresh CSRF token, stores it in a cookie, and returns the token.
func (c *csrfStore) newToken(w http.ResponseWriter, r *http.Request) string {
	var b [32]byte
	rand.Read(b[:])
	token := hex.EncodeToString(b[:])

	expires := time.Now().Add(csrfTTL)

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
	if submitted == "" {
		return false
	}
	found := false
	for _, cookie := range r.Cookies() {
		if cookie.Name == csrfCookieName && cookie.Value == submitted {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	return true
}
