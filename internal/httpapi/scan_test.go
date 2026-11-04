package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bitbearx/opencanon/internal/discovery"
)

// scanCounter 记录实际扫描入口调用以验证同源保护和参数拒绝。
type scanCounter struct{ calls atomic.Int32 }

// Snapshot 读取缓存不会访问设备。
func (s *scanCounter) Snapshot() discovery.State { return discovery.State{} }

// Scan 模拟设备不可达而不访问真实网络。
func (s *scanCounter) Scan(context.Context) (discovery.State, error) {
	s.calls.Add(1)
	return discovery.State{}, errors.New("offline")
}

// TestScanEndpoint 验证非法来源、缺失标识和客户端地址注入不会触发设备发现。
func TestScanEndpoint(t *testing.T) {
	scanner := &scanCounter{}
	handler := (&Server{Discovery: scanner}).Handler()
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
	if w := request("GET", "/api/printer", "", "", false); w.Code != 200 {
		t.Fatalf("GET: %d", w.Code)
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
	if scanner.calls.Load() != 0 {
		t.Fatal("invalid request triggered network discovery")
	}
	if w := request("POST", "/api/printer/scan", "http://example.com", "", true); w.Code != 503 || scanner.calls.Load() != 1 {
		t.Fatalf("valid request: %d calls=%d", w.Code, scanner.calls.Load())
	}
}
