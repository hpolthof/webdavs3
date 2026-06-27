package webdav

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	gowebdav "github.com/studio-b12/gowebdav"
)

// Client is the WebDAV abstraction used by other packages.
type Client interface {
	Upload(ctx context.Context, path string, r io.Reader, size int64) error
	Download(ctx context.Context, path string) (io.ReadCloser, error)
	Delete(ctx context.Context, path string) error
	MkdirAll(ctx context.Context, path string) error
	Exists(ctx context.Context, path string) (bool, error)
	Rename(ctx context.Context, oldpath, newpath string, overwrite bool) error
	DownloadToFile(ctx context.Context, path string, dest string) error
	UploadFromFile(ctx context.Context, path string, src string) error
	// ReadDir lists the names of direct children under path.
	ReadDir(ctx context.Context, path string) ([]string, error)
	// Ping checks that the WebDAV server is reachable and credentials work.
	Ping(ctx context.Context) error
	// Stat returns metadata for the file at path on the WebDAV server.
	Stat(ctx context.Context, path string) (os.FileInfo, error)
}

// ClientImpl wraps github.com/studio-b12/gowebdav and implements Client.
type ClientImpl struct {
	c *gowebdav.Client
}

// preemptiveBasicAuth sends a Basic Authorization header on every request.
// Using it avoids gowebdav's auto-negotiating authorizer, which buffers
// non-seekable request bodies in memory so it can retry authentication.
// That buffering prevents streaming large uploads without huge memory use.
type preemptiveBasicAuth struct {
	user string
	pw   string
}

func (a *preemptiveBasicAuth) Authorize(_ *http.Client, rq *http.Request, _ string) error {
	rq.SetBasicAuth(a.user, a.pw)
	return nil
}

func (a *preemptiveBasicAuth) Verify(_ *http.Client, rs *http.Response, _ string) (bool, error) {
	if rs.StatusCode == http.StatusUnauthorized {
		return false, gowebdav.NewPathError("Authorize", "", rs.StatusCode)
	}
	return false, nil
}

func (a *preemptiveBasicAuth) Clone() gowebdav.Authenticator { return a }
func (a *preemptiveBasicAuth) Close() error                   { return nil }

// New creates a new ClientImpl for the given WebDAV root URL.
// It prefers preemptive Basic auth so request bodies can be streamed without
// being fully buffered in memory. If the server rejects Basic auth, it falls
// back to gowebdav's auto-negotiating auth (which may buffer streams).
func New(url, username, password string) *ClientImpl {
	preemptive := gowebdav.NewAuthClient(url, gowebdav.NewPreemptiveAuth(&preemptiveBasicAuth{user: username, pw: password}))
	if _, err := preemptive.Stat("/"); err == nil {
		return &ClientImpl{c: preemptive}
	}
	c := gowebdav.NewClient(url, username, password)
	return &ClientImpl{c: c}
}

func (cl *ClientImpl) Upload(ctx context.Context, path string, r io.Reader, size int64) error {
	return cl.c.WriteStream(path, r, 0644)
}

func (cl *ClientImpl) Download(ctx context.Context, path string) (io.ReadCloser, error) {
	return cl.c.ReadStream(path)
}

func (cl *ClientImpl) Delete(ctx context.Context, path string) error {
	return cl.c.Remove(path)
}

func (cl *ClientImpl) MkdirAll(ctx context.Context, path string) error {
	return cl.c.MkdirAll(path, 0755)
}

func (cl *ClientImpl) Exists(ctx context.Context, path string) (bool, error) {
	_, err := cl.c.Stat(path)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (cl *ClientImpl) Rename(ctx context.Context, oldpath, newpath string, overwrite bool) error {
	return cl.c.Rename(oldpath, newpath, overwrite)
}

// Ping performs a lightweight check by stat-ing the WebDAV root.
func (cl *ClientImpl) ReadDir(ctx context.Context, path string) ([]string, error) {
	infos, err := cl.c.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(infos))
	for _, fi := range infos {
		names = append(names, fi.Name())
	}
	return names, nil
}

func (cl *ClientImpl) Ping(ctx context.Context) error {
	_, err := cl.c.Stat("/")
	return err
}

// Stat returns file metadata from the WebDAV server. ctx is not forwarded; gowebdav does not expose a context-aware Stat.
func (cl *ClientImpl) Stat(ctx context.Context, path string) (os.FileInfo, error) {
	return cl.c.Stat(path)
}

func (cl *ClientImpl) DownloadToFile(ctx context.Context, path string, dest string) error {
	rc, err := cl.Download(ctx, path)
	if err != nil {
		return fmt.Errorf("download %s: %w", path, err)
	}
	defer rc.Close()

	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, rc); err != nil {
		return fmt.Errorf("copy to %s: %w", dest, err)
	}
	return nil
}

func (cl *ClientImpl) UploadFromFile(ctx context.Context, path string, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	return cl.Upload(ctx, path, f, fi.Size())
}

// isNotFound returns true when err represents a 404 from the WebDAV server.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	// gowebdav returns errors whose message contains the HTTP status.
	type statusCoder interface{ StatusCode() int }
	if sc, ok := err.(statusCoder); ok {
		return sc.StatusCode() == http.StatusNotFound
	}
	// Fallback: check error string
	msg := err.Error()
	return containsStr(msg, "404") || containsStr(msg, "Not Found")
}

func containsStr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
