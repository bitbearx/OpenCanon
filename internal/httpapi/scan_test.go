package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitbearx/opencanon/internal/discovery"
)

// scanRunner 控制扫描执行以验证接口并发行为。
type scanRunner struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

// Run 暂停队列查询直到测试允许继续。
func (r *scanRunner) Run(ctx context.Context, _ time.Duration, _ string, _ ...string) ([]byte, error) {
	r.calls.Add(1)
	select {
	case r.started <- struct{}{}:
	default:
	}
	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []byte("device-uri (uri) = socket://unsupported\n"), nil
}

// TestScanEndpoint 验证 GET 不扫描、来源检查、参数拒绝和并发状态。
func TestScanEndpoint(t *testing.T) {
	r := &scanRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	d, err := discovery.New(t.TempDir(), r)
	if err != nil {
		t.Fatal(err)
	}
	handler := (&Server{Discovery: d}).Handler()
	request := func(method, target, origin, body string, header bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if header {
			req.Header.Set("X-OpenCanon-Request", "1")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	if w := request("GET", "/api/printer", "", "", false); w.Code != 200 || r.calls.Load() != 0 {
		t.Fatalf("GET: %d %d", w.Code, r.calls.Load())
	}
	if w := request("POST", "/api/printer/scan", "http://evil.example", "", true); w.Code != 403 {
		t.Fatalf("origin: %d", w.Code)
	}
	if w := request("POST", "/api/printer/scan", "", "", false); w.Code != 403 {
		t.Fatalf("header: %d", w.Code)
	}
	for _, tc := range []struct{ target, body string }{{"/api/printer/scan?address=10.0.0.1", ""}, {"/api/printer/scan", "{}"}} {
		if w := request("POST", tc.target, "", tc.body, true); w.Code != 400 {
			t.Fatalf("parameters: %d", w.Code)
		}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request("POST", "/api/printer/scan", "http://example.com", "", true) }()
	<-r.started
	if w := request("POST", "/api/printer/scan", "", "", true); w.Code != 409 || !strings.Contains(w.Body.String(), "scan_in_progress") {
		t.Fatalf("busy: %d %s", w.Code, w.Body.String())
	}
	close(r.release)
	if w := <-done; w.Code != 503 || !strings.Contains(w.Body.String(), "discovery_unavailable") {
		t.Fatalf("failure: %d %s", w.Code, w.Body.String())
	}
}
