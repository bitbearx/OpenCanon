package bjnp

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// discoveryReply 构造含 MAC 与 IPv4 的设备响应；负载 IP 故意不同于真实来源。
func discoveryReply() []byte {
	payload := []byte{0, 0, 0, 0, 6, 4, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 192, 0, 2, 99}
	return responsePacket(0x01, 0, 0, payload)
}

// TestDiscoveryCollectsAndDeduplicates 通过真实 UDP 服务验证畸形响应忽略、来源地址及 MAC 去重。
func TestDiscoveryCollectsAndDeduplicates(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		var request [256]byte
		for range 2 {
			n, source, err := server.ReadFromUDP(request[:])
			if err != nil {
				done <- err
				return
			}
			if n != 16 || string(request[:4]) != "BJNP" || request[4] != 1 || request[5] != 1 || binary.BigEndian.Uint16(request[8:]) != 0 || binary.BigEndian.Uint16(request[10:]) != 0 {
				t.Errorf("invalid discovery request: %x", request[:n])
			}
			malformed := discoveryReply()
			malformed[20] = 8
			for _, response := range [][]byte{malformed, discoveryReply(), discoveryReply()} {
				if _, err := server.WriteToUDP(response, source); err != nil {
					done <- err
					return
				}
			}
		}
		done <- nil
	}()
	target := discoveryTarget{local: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, broadcast: server.LocalAddr().(*net.UDPAddr)}
	devices, err := discoverTargets(context.Background(), []discoveryTarget{target, target})
	if err != nil || len(devices) != 1 {
		t.Fatalf("devices=%+v err=%v", devices, err)
	}
	if devices[0].Address != server.LocalAddr().String() || devices[0].MAC != "00:11:22:33:44:55" {
		t.Fatalf("unexpected device: %+v", devices[0])
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestDiscoveryRejectsMalformedResponses 校验发现解析边界，拒绝错误协议关联与地址布局。
func TestDiscoveryRejectsMalformedResponses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"header", func(p []byte) []byte { return p[:15] }},
		{"magic", func(p []byte) []byte { p[0] = 'X'; return p }},
		{"type", func(p []byte) []byte { p[4] = 0x82; return p }},
		{"command", func(p []byte) []byte { p[5] = 0x20; return p }},
		{"sequence", func(p []byte) []byte { p[9]++; return p }},
		{"session", func(p []byte) []byte { p[11]++; return p }},
		{"length", func(p []byte) []byte { p[15]++; return p }},
		{"mac-size", func(p []byte) []byte { p[20] = 5; return p }},
		{"ip-size", func(p []byte) []byte { p[21] = 16; return p }},
		{"truncated", func(p []byte) []byte { return p[:len(p)-1] }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseDiscovery(test.mutate(discoveryReply()), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8611})
			if err == nil {
				t.Fatal("accepted malformed discovery response")
			}
		})
	}
}

// TestDiscoveryCancellation 验证广播收集窗口可取消，取消不会伪装为空设备列表。
func TestDiscoveryCancellation(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var data [32]byte
		server.ReadFromUDP(data[:])
		cancel()
	}()
	start := time.Now()
	_, err = discoverTargets(ctx, []discoveryTarget{{local: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, broadcast: server.LocalAddr().(*net.UDPAddr)}})
	if err != context.Canceled || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
	}
}
