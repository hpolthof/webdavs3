package adminui_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hpolthof/webdav3s/internal/adminui"
)

func TestSession_LoginLogout(t *testing.T) {
	srv := adminui.NewAdminServer(adminui.AdminDeps{
		AdminPasswordHash: "$2a$10$UVdKbqzndmqLzRIJu2wrXunPESvTqk6KhPsWb9yCjgdAmKz5MtLBC", // "secret"
		AdminUsername:     "admin",
	})

	// GET /admin/login should return 200
	req := httptest.NewRequest("GET", "/admin/login", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET /admin/login: got %d want 200", w.Code)
	}

	// POST /admin/login with wrong password → 401
	form := url.Values{"username": {"admin"}, "password": {"wrong"}}
	req2 := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("POST /admin/login wrong pw: got %d want 401", w2.Code)
	}

	// POST /admin/login with correct password → redirect + Set-Cookie
	form2 := url.Values{"username": {"admin"}, "password": {"secret"}}
	req3 := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form2.Encode()))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w3 := httptest.NewRecorder()
	srv.ServeHTTP(w3, req3)
	if w3.Code != http.StatusSeeOther && w3.Code != http.StatusFound {
		t.Errorf("POST /admin/login correct pw: got %d want 302/303", w3.Code)
	}
	cookie := w3.Result().Cookies()
	sessionFound := false
	for _, c := range cookie {
		if c.Name == "session" && c.Value != "" {
			sessionFound = true
		}
	}
	if !sessionFound {
		t.Error("expected session cookie after successful login")
	}
}

func TestSession_RequireSession(t *testing.T) {
	srv := adminui.NewAdminServer(adminui.AdminDeps{
		AdminPasswordHash: "$2a$10$UVdKbqzndmqLzRIJu2wrXunPESvTqk6KhPsWb9yCjgdAmKz5MtLBC",
		AdminUsername:     "admin",
	})

	// Without session cookie, protected routes should redirect to /admin/login
	req := httptest.NewRequest("GET", "/admin/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Errorf("GET /admin/ without session: got %d want redirect", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/admin/login") {
		t.Errorf("expected redirect to /admin/login, got %q", loc)
	}
}
