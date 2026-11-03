package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// discoveryRunner 模拟队列和 Bonjour 命令的实际输出。
type discoveryRunner struct {
	address, uri string
	calls        int
	cancel       bool
	conflict     bool
	gotName      string
}

// Run 提供可变的发现结果。
func (r *discoveryRunner) Run(ctx context.Context, _ time.Duration, command string, args ...string) ([]byte, error) {
	r.calls++
	if strings.HasSuffix(command, "ipptool") {
		return []byte("device-uri (uri) = " + r.uri + "\n"), nil
	}
	if args[0] == "-L" {
		r.gotName = args[1]
		return []byte("service can be reached at printer.local.:8611 (interface 14)\n"), context.DeadlineExceeded
	}
	if r.cancel {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	result := fmt.Sprintf("12:00:00.000 Add 40000002 14 printer.local. %s 60\n", r.address)
	if r.conflict {
		result += "12:00:00.001 Add 40000002 15 printer.local. 10.0.0.9 60\n"
	}
	return []byte(result), context.DeadlineExceeded
}

// TestScanPersistence 验证队列派生、重启恢复、重新发现和失败保留。
func TestScanPersistence(t *testing.T) {
	dir := t.TempDir()
	r := &discoveryRunner{uri: "canonijnetwork://Canon/Office%20A+B?TYPE=" + "_canon-" + "bjnp1._tcp.", address: "192.168.1.7"}
	s, err := New(dir, r)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Address != r.address || r.gotName != "Office A+B" {
		t.Fatalf("unexpected result: %+v %q", first, r.gotName)
	}
	info, err := os.Stat(filepath.Join(dir, "printer-discovery.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file mode: %v %v", info, err)
	}
	restored, err := New(dir, r)
	if err != nil || restored.Snapshot().Address != first.Address {
		t.Fatalf("restore: %+v %v", restored, err)
	}
	r.address = "192.168.1.8"
	before := r.calls
	second, err := restored.Scan(context.Background())
	if err != nil || second.Address != r.address || r.calls != before+3 {
		t.Fatalf("rescan: %+v %v", second, err)
	}
	r.conflict = true
	failed, err := restored.Scan(context.Background())
	if err == nil || failed.Address != second.Address || failed.LastError == "" {
		t.Fatalf("conflict: %+v %v", failed, err)
	}
	r.conflict = false
	r.uri = "socket://printer.local:9100"
	failed, err = restored.Scan(context.Background())
	if err == nil || failed.Address != second.Address {
		t.Fatalf("unsupported: %+v %v", failed, err)
	}
	r.uri = "canonijnetwork://Canon/Office%20A+B?TYPE=" + "_canon-" + "bjnp1._tcp."
	r.address = ""
	failed, err = restored.Scan(context.Background())
	if err == nil || failed.Address != second.Address {
		t.Fatalf("empty discovery: %+v %v", failed, err)
	}
	r.address = "192.168.1.8"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failed, err = restored.Scan(ctx)
	if err == nil || failed.Address != second.Address {
		t.Fatalf("cancel: %+v %v", failed, err)
	}
	again, err := New(dir, r)
	if err != nil || again.Snapshot().Address != second.Address {
		t.Fatalf("saved result changed: %v", err)
	}
}

// TestCorruptRecord 验证损坏记录不会暴露地址。
func TestCorruptRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "printer-discovery.json"), []byte(`{"address":"192.168.1.1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, nil)
	if err != nil || s.Snapshot().Address != "" || s.Snapshot().LastError == "" {
		t.Fatalf("corrupt: %v", err)
	}
}

// TestAddressEvents 验证移除事件和不同接口的重复地址。
func TestAddressEvents(t *testing.T) {
	out := "t Add 0 1 printer.local. 10.0.0.1 60\nt Add 0 2 printer.local. 10.0.0.1 60\nt Add 0 1 printer.local. 10.0.0.2 60\nt Rmv 0 1 printer.local. 10.0.0.2 0\n"
	if ip, ok := resolveAddress(out, "printer.local.", 3*time.Second); !ok || ip != "10.0.0.1" {
		t.Fatalf("address %q %v", ip, ok)
	}
}

// TestExpiredAddress 验证过期结果不能覆盖上次确认的地址。
func TestExpiredAddress(t *testing.T) {
	for _, ttl := range []string{"0", "1", "3"} {
		out := "12:00:00.000 Add 0 1 printer.local. 10.0.0.1 " + ttl + "\n"
		if ip, ok := resolveAddress(out, "printer.local.", 3*time.Second); ok {
			t.Fatalf("expired address accepted: %s (TTL %s)", ip, ttl)
		}
	}
}
