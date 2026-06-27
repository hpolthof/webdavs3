package webdav_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	ourwebdav "github.com/hpolthof/webdavs3/internal/webdav"
)

// mockFileInfo implements os.FileInfo with a fixed size.
type mockFileInfo struct{ size int64 }

func (m mockFileInfo) Name() string       { return "" }
func (m mockFileInfo) Size() int64        { return m.size }
func (m mockFileInfo) Mode() os.FileMode  { return 0 }
func (m mockFileInfo) ModTime() time.Time { return time.Time{} }
func (m mockFileInfo) IsDir() bool        { return false }
func (m mockFileInfo) Sys() interface{}   { return nil }

// mockClient is a Client stub for retry tests.
type mockClient struct {
	uploadErrs []error // errors returned in sequence; last element repeated if exhausted
	uploadCall int
	statInfo   os.FileInfo
	statErr    error
	statCalls  int
}

func (m *mockClient) UploadFromFile(_ context.Context, _, _ string) error {
	idx := m.uploadCall
	if idx >= len(m.uploadErrs) {
		idx = len(m.uploadErrs) - 1
	}
	m.uploadCall++
	return m.uploadErrs[idx]
}

func (m *mockClient) Stat(_ context.Context, _ string) (os.FileInfo, error) {
	m.statCalls++
	return m.statInfo, m.statErr
}

func (m *mockClient) Upload(_ context.Context, _ string, _ io.Reader, _ int64) error {
	return nil
}
func (m *mockClient) Download(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, nil
}
func (m *mockClient) Delete(_ context.Context, _ string) error   { return nil }
func (m *mockClient) MkdirAll(_ context.Context, _ string) error { return nil }
func (m *mockClient) Exists(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (m *mockClient) Rename(_ context.Context, _, _ string, _ bool) error { return nil }
func (m *mockClient) DownloadToFile(_ context.Context, _, _ string) error { return nil }
func (m *mockClient) ReadDir(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (m *mockClient) Ping(_ context.Context) error { return nil }

// Compile-time check.
var _ ourwebdav.Client = (*mockClient)(nil)
var _ ourwebdav.Client = (*ourwebdav.RetryClient)(nil)

// writeTempFile creates a temp file with the given content and returns its path.
func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "retry-test-*.bin")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer f.Close()
	f.WriteString(content)
	return f.Name()
}

func newRetryClientWithMock(inner ourwebdav.Client, retries int) (*ourwebdav.RetryClient, *[]time.Duration) {
	var slept []time.Duration
	rc := ourwebdav.NewRetryClient(inner, retries, time.Second)
	rc.SleepFn = func(d time.Duration) { slept = append(slept, d) }
	return rc, &slept
}

func TestRetryClient_SuccessFirstAttempt(t *testing.T) {
	src := writeTempFile(t, "hello")
	fi, _ := os.Stat(src)

	mock := &mockClient{
		uploadErrs: []error{nil},
		statInfo:   mockFileInfo{size: fi.Size()},
	}
	rc, slept := newRetryClientWithMock(mock, 3)

	if err := rc.UploadFromFile(context.Background(), "/test.bin", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.uploadCall != 1 {
		t.Errorf("uploadCall = %d, want 1", mock.uploadCall)
	}
	if mock.statCalls != 1 {
		t.Errorf("statCalls = %d, want 1", mock.statCalls)
	}
	if len(*slept) != 0 {
		t.Errorf("no sleep expected on first-attempt success, got %v", *slept)
	}
}

func TestRetryClient_FailOnceThenSucceed(t *testing.T) {
	src := writeTempFile(t, "hello")
	fi, _ := os.Stat(src)

	mock := &mockClient{
		uploadErrs: []error{errors.New("connection reset"), nil},
		statInfo:   mockFileInfo{size: fi.Size()},
	}
	rc, slept := newRetryClientWithMock(mock, 3)

	if err := rc.UploadFromFile(context.Background(), "/test.bin", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.uploadCall != 2 {
		t.Errorf("uploadCall = %d, want 2", mock.uploadCall)
	}
	if len(*slept) != 1 || (*slept)[0] != time.Second {
		t.Errorf("expected 1 sleep of 1s, got %v", *slept)
	}
}

func TestRetryClient_AllAttemptsFailReturnsError(t *testing.T) {
	src := writeTempFile(t, "hello")
	sentinel := errors.New("network dead")

	mock := &mockClient{
		uploadErrs: []error{sentinel},
	}
	rc, _ := newRetryClientWithMock(mock, 3)

	err := rc.UploadFromFile(context.Background(), "/test.bin", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want sentinel", err)
	}
	if mock.uploadCall != 3 {
		t.Errorf("uploadCall = %d, want 3", mock.uploadCall)
	}
}

func TestRetryClient_SizeMismatchIsRetried(t *testing.T) {
	src := writeTempFile(t, "hello")
	fi, _ := os.Stat(src)

	mock := &mockClient{
		// upload always succeeds
		uploadErrs: []error{nil},
		// stat always returns wrong size — all retries exhaust
		statInfo: mockFileInfo{size: fi.Size() + 1}, // always wrong → all retries exhaust
	}
	rc, slept := newRetryClientWithMock(mock, 3)

	err := rc.UploadFromFile(context.Background(), "/test.bin", src)
	if err == nil {
		t.Fatal("expected error from size mismatch, got nil")
	}
	if mock.uploadCall != 3 {
		t.Errorf("uploadCall = %d, want 3", mock.uploadCall)
	}
	if len(*slept) != 2 {
		t.Errorf("expected 2 sleeps, got %v", *slept)
	}
}

func TestRetryClient_FailThenSucceedWithCorrectSize(t *testing.T) {
	src := writeTempFile(t, "hello")
	fi, _ := os.Stat(src)

	// First upload fails, second succeeds; stat always returns correct size.
	mock := &mockClient{
		uploadErrs: []error{errors.New("connection reset"), nil},
		statInfo:   mockFileInfo{size: fi.Size()},
	}
	rc, slept := newRetryClientWithMock(mock, 3)

	if err := rc.UploadFromFile(context.Background(), "/test.bin", src); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if mock.uploadCall != 2 {
		t.Errorf("uploadCall = %d, want 2", mock.uploadCall)
	}
	if mock.statCalls != 1 {
		t.Errorf("statCalls = %d, want 1", mock.statCalls)
	}
	if len(*slept) != 1 {
		t.Errorf("expected 1 sleep, got %v", *slept)
	}
}

func TestRetryClient_StatErrorIsRetried(t *testing.T) {
	src := writeTempFile(t, "hello")

	mock := &mockClient{
		uploadErrs: []error{nil},
		statErr:    fmt.Errorf("stat failed"),
	}
	rc, _ := newRetryClientWithMock(mock, 3)

	err := rc.UploadFromFile(context.Background(), "/test.bin", src)
	if err == nil {
		t.Fatal("expected error from stat failure, got nil")
	}
	if mock.uploadCall != 3 {
		t.Errorf("uploadCall = %d, want 3", mock.uploadCall)
	}
}

func TestRetryClient_ExponentialBackoffDelays(t *testing.T) {
	src := writeTempFile(t, "hello")
	sentinel := errors.New("fail")

	mock := &mockClient{uploadErrs: []error{sentinel}}
	rc, slept := newRetryClientWithMock(mock, 3)
	rc.UploadFromFile(context.Background(), "/test.bin", src)

	want := []time.Duration{time.Second, 2 * time.Second}
	if len(*slept) != len(want) {
		t.Fatalf("sleeps = %v, want %v", *slept, want)
	}
	for i, d := range *slept {
		if d != want[i] {
			t.Errorf("sleep[%d] = %v, want %v", i, d, want[i])
		}
	}
}
