package webui

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/hpolthof/webdavs3/internal/bucket"
	"github.com/hpolthof/webdavs3/internal/meta"
	"github.com/hpolthof/webdavs3/internal/object"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Deps holds dependencies for the user web UI.
type Deps struct {
	Structure     meta.StructureDB
	BucketService bucket.Service
	ObjectService object.ObjectService
}

// Server is the HTTP handler for the user-facing web UI.
type Server struct {
	deps     Deps
	sessions *sessionStore
	csrf     *csrfStore
	mux      *http.ServeMux
	tmpls    *template.Template
}

var templateFuncs = template.FuncMap{
	"humanBytes": func(n int64) string {
		if n == 0 {
			return "0 B"
		}
		units := []string{"B", "KB", "MB", "GB", "TB"}
		f := float64(n)
		i := 0
		for f >= 1024 && i < len(units)-1 {
			f /= 1024
			i++
		}
		return fmt.Sprintf("%.2f %s", f, units[i])
	},
	"baseName": path.Base,
	"dirName": func(p string) string {
		p = strings.TrimSuffix(p, "/")
		d := path.Dir(p)
		if d == "." || d == "/" {
			return ""
		}
		return d + "/"
	},
	"trimSuffix": strings.TrimSuffix,
	"splitPrefix": func(p string) []string {
		if p == "" {
			return nil
		}
		p = strings.TrimSuffix(p, "/")
		return strings.Split(p, "/")
	},
}

// NewServer creates and wires up a user web UI Server.
func NewServer(deps Deps) *Server {
	s := &Server{
		deps:     deps,
		sessions: newSessionStore(),
		csrf:     newCSRFStore(),
		mux:      http.NewServeMux(),
	}

	s.tmpls = template.Must(template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.html"))

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(fmt.Sprintf("load static assets: %v", err))
	}
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticSub)))

	s.mux.HandleFunc("GET /login", s.handleGetLogin)
	s.mux.HandleFunc("POST /login", s.handlePostLogin)
	s.mux.HandleFunc("POST /logout", s.handlePostLogout)

	s.mux.Handle("GET /", s.requireSession(http.HandlerFunc(s.handleRootRedirect)))
	s.mux.Handle("GET /buckets", s.requireSession(http.HandlerFunc(s.handleListBuckets)))
	s.mux.Handle("POST /buckets", s.requireSession(http.HandlerFunc(s.handleCreateBucket)))
	s.mux.Handle("POST /buckets/{name}/delete", s.requireSession(http.HandlerFunc(s.handleDeleteBucket)))

	s.mux.Handle("GET /buckets/{name}/browse", s.requireSession(http.HandlerFunc(s.handleBrowse)))
	s.mux.Handle("GET /buckets/{name}/download", s.requireSession(http.HandlerFunc(s.handleDownload)))
	s.mux.Handle("POST /buckets/{name}/upload", s.requireSession(http.HandlerFunc(s.handleUpload)))
	s.mux.Handle("POST /buckets/{name}/mkdir", s.requireSession(http.HandlerFunc(s.handleMkdir)))
	s.mux.Handle("POST /buckets/{name}/objects/delete", s.requireSession(http.HandlerFunc(s.handleDeleteObject)))
	s.mux.Handle("POST /buckets/{name}/objects/rename", s.requireSession(http.HandlerFunc(s.handleRenameObject)))

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// ctxKey is used for context values.
type ctxKey int

const ctxUserKey ctxKey = 1

func userFromCtx(ctx context.Context) meta.User {
	u, _ := ctx.Value(ctxUserKey).(meta.User)
	return u
}

// requireSession is a middleware that redirects unauthenticated requests to /login.
// It also loads the authenticated user and stores it in the request context.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accessKey, ok := s.sessions.accessKey(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		user, err := s.deps.Structure.GetUserByAccessKey(accessKey)
		if err != nil || !user.Enabled {
			s.sessions.delete(w, r)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUserKey, user)))
	})
}

// csrfOrForbidden validates the CSRF token for POST requests.
func (s *Server) csrfOrForbidden(w http.ResponseWriter, r *http.Request) bool {
	if !s.csrf.valid(r, r.FormValue("csrf_token")) {
		slog.Warn("csrf validation failed", "path", r.URL.Path)
		http.Error(w, "Forbidden: invalid CSRF token", http.StatusForbidden)
		return false
	}
	return true
}

// isHTMX returns true if the request was made by HTMX.
func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
