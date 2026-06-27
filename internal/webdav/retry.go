package webdav

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

// RetryClient wraps a Client and retries UploadFromFile on failure,
// verifying the remote file size after each successful upload.
type RetryClient struct {
	inner   Client
	retries int
	base    time.Duration
	// SleepFn is called between attempts. Defaults to time.Sleep.
	// Tests inject a no-op so they run instantly.
	SleepFn func(time.Duration)
}

// NewRetryClient wraps inner with retry + size-verify logic for uploads.
// retries is the total number of attempts. base is the initial backoff delay.
func NewRetryClient(inner Client, retries int, base time.Duration) *RetryClient {
	return &RetryClient{
		inner:   inner,
		retries: retries,
		base:    base,
		SleepFn: time.Sleep,
	}
}

// UploadFromFile uploads src to path, retrying up to r.retries times on
// failure or size mismatch, with exponential backoff between attempts.
func (r *RetryClient) UploadFromFile(ctx context.Context, path string, src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat local file %s: %w", src, err)
	}
	expectedSize := fi.Size()

	var lastErr error
	for attempt := 0; attempt < r.retries; attempt++ {
		if attempt > 0 {
			delay := r.base * (1 << (attempt - 1))
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			r.SleepFn(delay)
		}

		if err := r.inner.UploadFromFile(ctx, path, src); err != nil {
			slog.Warn("upload attempt failed", "attempt", attempt+1, "path", path, "err", err)
			lastErr = err
			continue
		}

		remoteInfo, err := r.inner.Stat(ctx, path)
		if err != nil {
			slog.Warn("upload verify stat failed", "attempt", attempt+1, "path", path, "err", err)
			lastErr = fmt.Errorf("stat after upload: %w", err)
			continue
		}

		if remoteInfo.Size() != expectedSize {
			slog.Warn("upload size mismatch", "attempt", attempt+1, "path", path,
				"expected", expectedSize, "got", remoteInfo.Size())
			lastErr = fmt.Errorf("upload size mismatch: expected %d got %d", expectedSize, remoteInfo.Size())
			continue
		}

		return nil
	}
	return lastErr
}

// Upload delegates to inner without retry.
func (r *RetryClient) Upload(ctx context.Context, path string, reader io.Reader, size int64) error {
	return r.inner.Upload(ctx, path, reader, size)
}

// Download delegates to inner.
func (r *RetryClient) Download(ctx context.Context, path string) (io.ReadCloser, error) {
	return r.inner.Download(ctx, path)
}

// Delete delegates to inner.
func (r *RetryClient) Delete(ctx context.Context, path string) error {
	return r.inner.Delete(ctx, path)
}

// MkdirAll delegates to inner.
func (r *RetryClient) MkdirAll(ctx context.Context, path string) error {
	return r.inner.MkdirAll(ctx, path)
}

// Exists delegates to inner.
func (r *RetryClient) Exists(ctx context.Context, path string) (bool, error) {
	return r.inner.Exists(ctx, path)
}

// Rename delegates to inner.
func (r *RetryClient) Rename(ctx context.Context, oldpath, newpath string, overwrite bool) error {
	return r.inner.Rename(ctx, oldpath, newpath, overwrite)
}

// DownloadToFile delegates to inner.
func (r *RetryClient) DownloadToFile(ctx context.Context, path string, dest string) error {
	return r.inner.DownloadToFile(ctx, path, dest)
}

// ReadDir delegates to inner.
func (r *RetryClient) ReadDir(ctx context.Context, path string) ([]string, error) {
	return r.inner.ReadDir(ctx, path)
}

// Ping delegates to inner.
func (r *RetryClient) Ping(ctx context.Context) error {
	return r.inner.Ping(ctx)
}

// Stat delegates to inner.
func (r *RetryClient) Stat(ctx context.Context, path string) (os.FileInfo, error) {
	return r.inner.Stat(ctx, path)
}
