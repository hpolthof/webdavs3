package webdav

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
)

// RefreshableClient wraps a webdav.Client pointer so the underlying client
// can be swapped at runtime (e.g. when a location is added or edited in the
// admin UI after the daemon has started).
type RefreshableClient struct {
	mu      sync.RWMutex
	current Client
}

// NewRefreshable creates a RefreshableClient wrapping the given initial client.
func NewRefreshable(initial Client) *RefreshableClient {
	return &RefreshableClient{current: initial}
}

// SetClient replaces the underlying client.
func (r *RefreshableClient) SetClient(c Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = c
}

func (r *RefreshableClient) currentClient() (Client, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.current == nil {
		return nil, fmt.Errorf("no webdav client configured")
	}
	return r.current, nil
}

func (r *RefreshableClient) Upload(ctx context.Context, path string, reader io.Reader, size int64) error {
	c, err := r.currentClient()
	if err != nil {
		return err
	}
	return c.Upload(ctx, path, reader, size)
}

func (r *RefreshableClient) Download(ctx context.Context, path string) (io.ReadCloser, error) {
	c, err := r.currentClient()
	if err != nil {
		return nil, err
	}
	return c.Download(ctx, path)
}

func (r *RefreshableClient) Delete(ctx context.Context, path string) error {
	c, err := r.currentClient()
	if err != nil {
		return err
	}
	return c.Delete(ctx, path)
}

func (r *RefreshableClient) MkdirAll(ctx context.Context, path string) error {
	c, err := r.currentClient()
	if err != nil {
		return err
	}
	return c.MkdirAll(ctx, path)
}

func (r *RefreshableClient) Exists(ctx context.Context, path string) (bool, error) {
	c, err := r.currentClient()
	if err != nil {
		return false, err
	}
	return c.Exists(ctx, path)
}

func (r *RefreshableClient) Rename(ctx context.Context, oldpath, newpath string, overwrite bool) error {
	c, err := r.currentClient()
	if err != nil {
		return err
	}
	return c.Rename(ctx, oldpath, newpath, overwrite)
}

func (r *RefreshableClient) DownloadToFile(ctx context.Context, path string, dest string) error {
	c, err := r.currentClient()
	if err != nil {
		return err
	}
	return c.DownloadToFile(ctx, path, dest)
}

func (r *RefreshableClient) UploadFromFile(ctx context.Context, path string, src string) error {
	c, err := r.currentClient()
	if err != nil {
		return err
	}
	return c.UploadFromFile(ctx, path, src)
}

func (r *RefreshableClient) ReadDir(ctx context.Context, path string) ([]string, error) {
	c, err := r.currentClient()
	if err != nil {
		return nil, err
	}
	return c.ReadDir(ctx, path)
}

func (r *RefreshableClient) ReadDirInfo(ctx context.Context, path string) ([]os.FileInfo, error) {
	c, err := r.currentClient()
	if err != nil {
		return nil, err
	}
	return c.ReadDirInfo(ctx, path)
}

func (r *RefreshableClient) Ping(ctx context.Context) error {
	c, err := r.currentClient()
	if err != nil {
		return err
	}
	return c.Ping(ctx)
}

func (r *RefreshableClient) Stat(ctx context.Context, path string) (os.FileInfo, error) {
	c, err := r.currentClient()
	if err != nil {
		return nil, err
	}
	return c.Stat(ctx, path)
}
