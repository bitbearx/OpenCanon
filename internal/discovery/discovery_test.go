package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// detectedState 创建完整的设备身份记录，用于验证持久化而非协议模拟。
func detectedState(host string) State {
	now := time.Now().UTC()
	return State{Name: "Canon E560 series", Address: host, Hostname: host, DeviceURI: "bjnp://" + host + ":8611", DiscoveredAt: &now}
}

// TestScanPersistence 验证重新扫描更新地址、重启恢复以及失败不覆盖最后确认结果。
func TestScanPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	s.detect = func(context.Context, string) (State, error) { return detectedState("192.168.1.7"), nil }
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "printer-discovery.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file: %v %v", info, err)
	}
	restored, err := New(dir, "")
	if err != nil || restored.Endpoint() != "192.168.1.7:8611" {
		t.Fatalf("restore: %v", err)
	}
	restored.detect = func(context.Context, string) (State, error) { return detectedState("192.168.1.8"), nil }
	if _, err := restored.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored.detect = func(context.Context, string) (State, error) { return State{}, errors.New("offline") }
	failed, err := restored.Scan(context.Background())
	if err == nil || failed.Address != "192.168.1.8" || failed.LastError == "" || failed.Scanning {
		t.Fatalf("failure: %+v %v", failed, err)
	}
	again, err := New(dir, "")
	if err != nil || again.Endpoint() != "192.168.1.8:8611" {
		t.Fatalf("saved result changed: %v", err)
	}
}

// TestConcurrentScan 验证扫描互斥，后来的请求不能启动第二次网络发现。
func TestConcurrentScan(t *testing.T) {
	s, err := New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	s.detect = func(ctx context.Context, _ string) (State, error) {
		close(started)
		select {
		case <-release:
			return detectedState("192.168.1.7"), nil
		case <-ctx.Done():
			return State{}, ctx.Err()
		}
	}
	go func() { _, err := s.Scan(context.Background()); done <- err }()
	<-started
	state, err := s.Scan(context.Background())
	if !errors.Is(err, ErrScanInProgress) || !state.Scanning {
		t.Fatalf("concurrent: %+v %v", state, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestCancelledScanKeepsRecord 验证取消后的新地址不会写入持久记录。
func TestCancelledScanKeepsRecord(t *testing.T) {
	s, err := New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	s.detect = func(context.Context, string) (State, error) { return detectedState("192.168.1.7"), nil }
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.detect = func(context.Context, string) (State, error) { return detectedState("192.168.1.8"), nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state, err := s.Scan(ctx)
	if err == nil || state.Address != "192.168.1.7" {
		t.Fatalf("cancel: %+v %v", state, err)
	}
}

// TestInvalidRecordAndConfiguredAddress 验证损坏或旧协议记录不会覆盖管理员配置。
func TestInvalidRecordAndConfiguredAddress(t *testing.T) {
	for _, raw := range []string{`{"address":"192.168.1.1"}`, `{"name":"Canon E560 series","address":"192.168.1.1","deviceURI":"canonijnetwork://Canon/E560"}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "printer-discovery.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		s, err := New(dir, "192.168.1.9")
		if err != nil || s.Snapshot().Address != "" || s.Snapshot().LastError == "" || s.Endpoint() != "192.168.1.9:8611" {
			t.Fatalf("record: %v", err)
		}
	}
}
