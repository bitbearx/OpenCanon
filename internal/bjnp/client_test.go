package bjnp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testPacket 用独立编解码表示模拟设备实际收到的线上帧。
type testPacket struct {
	command           byte
	sequence, session uint16
	payload           []byte
}

// readRequest 逐段读取请求，并独立校验打印请求头与负载上限。
func readRequest(conn net.Conn) (testPacket, error) {
	var header [16]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return testPacket{}, err
	}
	if string(header[:4]) != "BJNP" || header[4] != 1 || header[6] != 0 || header[7] != 0 {
		return testPacket{}, fmt.Errorf("invalid request header: %x", header)
	}
	n := binary.BigEndian.Uint32(header[12:])
	if n > 4096 {
		return testPacket{}, fmt.Errorf("oversized print block: %d", n)
	}
	packet := testPacket{command: header[5], sequence: binary.BigEndian.Uint16(header[8:]), session: binary.BigEndian.Uint16(header[10:]), payload: make([]byte, n)}
	_, err := io.ReadFull(conn, packet.payload)
	return packet, err
}

// responsePacket 独立构造模拟设备响应，避免测试复用生产帧编码器。
func responsePacket(command byte, sequence, session uint16, payload []byte) []byte {
	data := make([]byte, 16+len(payload))
	copy(data, []byte{'B', 'J', 'N', 'P', 0x81, command, 0, 0})
	binary.BigEndian.PutUint16(data[8:], sequence)
	binary.BigEndian.PutUint16(data[10:], session)
	binary.BigEndian.PutUint32(data[12:], uint32(len(payload)))
	copy(data[16:], payload)
	return data
}

// ackPacket 构造当前帧接受量的响应，而不是累计字节偏移。
func ackPacket(packet testPacket, accepted int) []byte {
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], uint32(accepted))
	return responsePacket(0x21, packet.sequence, packet.session, count[:])
}

// textPayload 为身份或旧状态构造带两字节前缀的文本。
func textPayload(text string) []byte {
	payload := make([]byte, 2+len(text))
	binary.BigEndian.PutUint16(payload, uint16(len(payload)))
	copy(payload[2:], text)
	return payload
}

// testDevice 提供共用地址的真实 UDP/TCP 模拟设备与可观察的会话命令。
type testDevice struct {
	address        string
	starts, closes atomic.Int32
	done           chan error
}

// newTestDevice 启动真实回环服务；修改器可改变 UDP 回包或故意丢包。
func newTestDevice(t *testing.T, modify func(byte, []byte) []byte, handle func(net.Conn) error) *testDevice {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().(*net.TCPAddr)
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: address.IP, Port: address.Port})
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	device := &testDevice{address: listener.Addr().String(), done: make(chan error, 1)}
	t.Cleanup(func() { listener.Close(); udp.Close() })
	go func() {
		var data [65535]byte
		for {
			n, source, err := udp.ReadFromUDP(data[:])
			if err != nil {
				return
			}
			if n < 16 || string(data[:4]) != "BJNP" || data[4] != 1 || int(binary.BigEndian.Uint32(data[12:16])) != n-16 {
				t.Errorf("invalid UDP request: %x", data[:n])
				continue
			}
			command := data[5]
			sequence, session := binary.BigEndian.Uint16(data[8:10]), binary.BigEndian.Uint16(data[10:12])
			var payload []byte
			switch command {
			case 0x30:
				payload = textPayload("MFG:Canon;MDL:E560 series;DES:Canon E560;")
			case 0x20:
				if sequence != 1 || (session != 0 && session != 0x1234) {
					t.Errorf("invalid status request: seq=%d session=%d", sequence, session)
				}
				payload = textPayload("BST:00000000;DJS:;CTK:SET;")
			case 0x10:
				device.starts.Add(1)
				if n != 408 || sequence != 0 || session != 0 {
					t.Errorf("invalid session request: length=%d seq=%d session=%d", n, sequence, session)
				}
				session = 0x1234
			case 0x11:
				device.closes.Add(1)
				if session != 0x1234 || sequence == 0 {
					t.Errorf("invalid close request: seq=%d session=%d", sequence, session)
				}
			default:
				t.Errorf("unexpected UDP command: %x", command)
			}
			response := responsePacket(command, sequence, session, payload)
			if modify != nil {
				response = modify(command, response)
			}
			if response != nil {
				if _, err := udp.WriteToUDP(response, source); err != nil {
					return
				}
			}
		}
	}()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			device.done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(8 * time.Second))
		if handle != nil {
			device.done <- handle(conn)
		} else {
			device.done <- errors.New("unexpected TCP connection")
		}
	}()
	return device
}

// waitDevice 将模拟服务端的协议断言传播到测试协程。
func waitDevice(t *testing.T, device *testDevice) {
	t.Helper()
	select {
	case err := <-device.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("device did not finish")
	}
}

// TestSubmitFragmentedACK 验证跨 TCP 分段的逐帧 ACK、4096 字节分块与会话关闭。
func TestSubmitFragmentedACK(t *testing.T) {
	input := bytes.Repeat([]byte{0x1b, 0x42, 0x00}, 3000)
	device := newTestDevice(t, nil, func(conn net.Conn) error {
		var received []byte
		for i, size := range []int{4096, 4096, 808} {
			packet, err := readRequest(conn)
			if err != nil {
				return err
			}
			if packet.command != 0x21 || packet.session != 0x1234 || packet.sequence != uint16(i+2) || len(packet.payload) != size {
				return fmt.Errorf("unexpected print frame: %+v", packet)
			}
			received = append(received, packet.payload...)
			ack := ackPacket(packet, size)
			for _, part := range [][]byte{ack[:3], ack[3:11], ack[11:17], ack[17:]} {
				if _, err := conn.Write(part); err != nil {
					return err
				}
				time.Sleep(time.Millisecond)
			}
		}
		if !bytes.Equal(received, input) {
			return errors.New("print stream changed")
		}
		_, err := readRequest(conn)
		if err != io.EOF {
			return fmt.Errorf("expected closed TCP session, got %v", err)
		}
		return nil
	})
	receipt, err := (Client{Address: device.address, Timeout: time.Second}).Submit(context.Background(), bytes.NewReader(input), "测试 job")
	if err != nil || receipt.Bytes != int64(len(input)) || receipt.Session != 0x1234 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if device.starts.Load() != 1 || device.closes.Load() != 1 {
		t.Fatalf("starts=%d closes=%d", device.starts.Load(), device.closes.Load())
	}
	waitDevice(t, device)
}

// TestSubmitRejectsWrongACK 验证错误 ACK 不推进确认量且绝不重复发送当前数据。
func TestSubmitRejectsWrongACK(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"sequence", func(p []byte) []byte { p[9]++; return p }},
		{"session", func(p []byte) []byte { p[11]++; return p }},
		{"command", func(p []byte) []byte { p[5] = 0x20; return p }},
		{"magic", func(p []byte) []byte { p[0] = 'X'; return p }},
		{"device-type", func(p []byte) []byte { p[4] = 0x82; return p }},
		{"short-count", func(p []byte) []byte { binary.BigEndian.PutUint32(p[12:], 2); return p[:18] }},
		{"oversized", func(p []byte) []byte { binary.BigEndian.PutUint32(p[12:], 2049); return p }},
		{"partial", func(p []byte) []byte { binary.BigEndian.PutUint32(p[16:], 100); return p }},
		{"lost", func(p []byte) []byte { return nil }},
		{"truncated-header", func(p []byte) []byte { return p[:8] }},
		{"truncated-payload", func(p []byte) []byte { return p[:18] }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			device := newTestDevice(t, nil, func(conn net.Conn) error {
				packet, err := readRequest(conn)
				if err != nil {
					return err
				}
				ack := test.mutate(ackPacket(packet, len(packet.payload)))
				if len(ack) > 0 {
					if _, err := conn.Write(ack); err != nil {
						return err
					}
				}
				var extra [1]byte
				n, _ := conn.Read(extra[:])
				if n != 0 {
					return errors.New("client sent more print data after invalid or missing ACK")
				}
				return nil
			})
			receipt, err := (Client{Address: device.address, Timeout: 100 * time.Millisecond}).Submit(context.Background(), strings.NewReader(strings.Repeat("x", 5000)), "bad ACK")
			if !errors.Is(err, ErrUncertain) || receipt.Bytes != 0 {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			if device.starts.Load() != 1 {
				t.Fatalf("session was retried: %d", device.starts.Load())
			}
			waitDevice(t, device)
		})
	}
}

// TestSubmitCancellationPreservesConfirmedBytes 验证 ACK 等待可立即取消且保留之前的确认量。
func TestSubmitCancellationPreservesConfirmedBytes(t *testing.T) {
	pending := make(chan struct{})
	device := newTestDevice(t, nil, func(conn net.Conn) error {
		first, err := readRequest(conn)
		if err != nil {
			return err
		}
		if _, err := conn.Write(ackPacket(first, len(first.payload))); err != nil {
			return err
		}
		if _, err := readRequest(conn); err != nil {
			return err
		}
		close(pending)
		var extra [1]byte
		n, _ := conn.Read(extra[:])
		if n != 0 {
			return errors.New("resent data while waiting for ACK")
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-pending; cancel() }()
	start := time.Now()
	receipt, err := (Client{Address: device.address, Timeout: 30 * time.Second}).Submit(ctx, strings.NewReader(strings.Repeat("x", 5000)), "cancel")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUncertain) || receipt.Bytes != 4096 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("context cancellation did not interrupt socket read")
	}
	waitDevice(t, device)
}

// TestProbeAndPreflight 验证真实 UDP 交互的状态解释及无会话拒绝路径。
func TestProbeAndPreflight(t *testing.T) {
	cases := []struct {
		name, status               string
		busy, deviceError, unknown bool
	}{
		{"ready", "BST:00000000;DJS:;", false, false, false},
		{"printing", "BST:80000000;", true, false, false},
		{"busy", "BST:20000000;", true, false, false},
		{"device-error", "BST:08000000;", false, true, false},
		{"paper-out", "BST:00000000;DJS:ID;", false, true, false},
		{"ink-out", "BST:00000000;CTK:IO;", false, true, false},
		{"missing-bst", "DJS:;CTK:SET;", false, false, true},
		{"invalid-bst", "BST:zz;", false, false, true},
		{"invalid-bst-suffix", "BST:00junk;", false, false, true},
		{"duplicate-bst", "BST:00;BST:80;", false, false, true},
		{"xml", "<?xml version=\"1.0\"?><status>ready</status>", false, false, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			device := newTestDevice(t, func(command byte, response []byte) []byte {
				if command == 0x20 {
					payload := textPayload(test.status)
					if strings.HasPrefix(test.status, "<?xml") {
						payload = []byte(test.status)
					}
					return responsePacket(0x20, 1, 0, payload)
				}
				return response
			}, nil)
			client := Client{Address: device.address, Timeout: time.Second}
			info, err := client.Probe(context.Background())
			if (err != nil) != test.unknown || info.Model != "E560 series" || info.Busy != test.busy || info.DeviceError != test.deviceError {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			if test.busy || test.deviceError || test.unknown {
				_, err := client.Submit(context.Background(), strings.NewReader("data"), "blocked")
				if err == nil || errors.Is(err, ErrUncertain) || errors.Is(err, ErrBusy) != test.busy {
					t.Fatalf("unexpected preflight error: %v", err)
				}
			}
			if device.starts.Load() != 0 {
				t.Fatal("probe or failed preflight opened a print session")
			}
		})
	}
}

// TestProbeRejectsMalformedUDP 验证数据报外层长度、身份内层长度及关联字段。
func TestProbeRejectsMalformedUDP(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"short-header", func(p []byte) []byte { return p[:10] }},
		{"outer-length", func(p []byte) []byte { p[15]++; return p }},
		{"identity-length", func(p []byte) []byte { p[17]--; return p }},
		{"sequence", func(p []byte) []byte { p[9]++; return p }},
		{"session", func(p []byte) []byte { p[11]++; return p }},
		{"command", func(p []byte) []byte { p[5]++; return p }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			device := newTestDevice(t, func(command byte, response []byte) []byte { return test.mutate(response) }, nil)
			_, err := (Client{Address: device.address, Timeout: time.Second}).Probe(context.Background())
			if err == nil {
				t.Fatal("accepted malformed UDP response")
			}
		})
	}
}

// TestProbeCancellation 验证无响应 UDP 查询被 context 立即打断。
func TestProbeCancellation(t *testing.T) {
	waiting := make(chan struct{})
	device := newTestDevice(t, func(command byte, response []byte) []byte {
		if command == 0x20 {
			close(waiting)
			return nil
		}
		return response
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-waiting; cancel() }()
	start := time.Now()
	_, err := (Client{Address: device.address, Timeout: time.Minute}).Probe(ctx)
	if !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("cancellation err=%v elapsed=%v", err, time.Since(start))
	}
}

// TestSubmitKeepaliveWhileSourceWaits 验证只有输入停顿时发送空帧且空 ACK 不累计打印量。
func TestSubmitKeepaliveWhileSourceWaits(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	device := newTestDevice(t, nil, func(conn net.Conn) error {
		keepalive, err := readRequest(conn)
		if err != nil {
			return err
		}
		if len(keepalive.payload) != 0 {
			return errors.New("expected idle keepalive")
		}
		if _, err := conn.Write(ackPacket(keepalive, 0)); err != nil {
			return err
		}
		go func() { writer.Write([]byte("rendered")); writer.Close() }()
		packet, err := readRequest(conn)
		if err != nil {
			return err
		}
		if string(packet.payload) != "rendered" || packet.sequence != keepalive.sequence+1 {
			return fmt.Errorf("unexpected data after keepalive: %+v", packet)
		}
		_, err = conn.Write(ackPacket(packet, len(packet.payload)))
		return err
	})
	receipt, err := (Client{Address: device.address, Timeout: time.Second}).Submit(context.Background(), reader, "slow source")
	if err != nil || receipt.Bytes != 8 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	waitDevice(t, device)
}

// shortWriter 模拟合法短写与无进展写入，直接检验帧发送的完整性。
type shortWriter struct {
	bytes.Buffer
	zero bool
}

// Write 每次最多接受两个字节。
func (w *shortWriter) Write(data []byte) (int, error) {
	if w.zero {
		return 0, nil
	}
	if len(data) > 2 {
		data = data[:2]
	}
	return w.Buffer.Write(data)
}

// TestWriteFull 验证短写会推进而无进展写入会失败。
func TestWriteFull(t *testing.T) {
	writer := &shortWriter{}
	if err := writeFull(writer, []byte("BJNP payload")); err != nil || writer.String() != "BJNP payload" {
		t.Fatalf("output=%q err=%v", writer.String(), err)
	}
	if err := writeFull(&shortWriter{zero: true}, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero write error=%v", err)
	}
}
