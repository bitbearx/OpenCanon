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

// TestZeroACKFlowControl 验证明确零接收后等待并使用新序号发送相同帧，只累计最终完整确认。
func TestZeroACKFlowControl(t *testing.T) {
	device := newTestDevice(t, nil, func(conn net.Conn) error {
		var previous time.Time
		for attempt := range 3 {
			packet, err := readRequest(conn)
			if err != nil {
				return err
			}
			if string(packet.payload) != "flow controlled data" || packet.sequence != uint16(attempt+2) || packet.session != 0x1234 {
				return fmt.Errorf("unexpected flow-control frame: %+v", packet)
			}
			if attempt > 0 && time.Since(previous) < 30*time.Millisecond {
				return errors.New("zero ACK did not impose flow-control delay")
			}
			accepted := 0
			if attempt == 2 {
				accepted = len(packet.payload)
			}
			previous = time.Now()
			if _, err := conn.Write(ackPacket(packet, accepted)); err != nil {
				return err
			}
		}
		_, err := readRequest(conn)
		if err != io.EOF {
			return fmt.Errorf("unexpected data after full ACK: %v", err)
		}
		return nil
	})
	receipt, err := (Client{Address: device.address, Timeout: time.Second}).Submit(context.Background(), strings.NewReader("flow controlled data"), "flow control")
	if err != nil || receipt.Bytes != 20 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	waitDevice(t, device)
}

// TestZeroACKDeadline 验证持续零接收不能刷新单帧总期限。
func TestZeroACKDeadline(t *testing.T) {
	var attempts atomic.Int32
	device := newTestDevice(t, nil, func(conn net.Conn) error {
		for {
			packet, err := readRequest(conn)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			attempts.Add(1)
			if _, err := conn.Write(ackPacket(packet, 0)); err != nil {
				return err
			}
		}
	})
	start := time.Now()
	receipt, err := (Client{Address: device.address, Timeout: 150 * time.Millisecond}).Submit(context.Background(), strings.NewReader("data"), "busy forever")
	if !errors.Is(err, ErrUncertain) || receipt.Bytes != 0 || time.Since(start) > time.Second {
		t.Fatalf("receipt=%+v err=%v elapsed=%v", receipt, err, time.Since(start))
	}
	if count := attempts.Load(); count < 1 || count > 4 {
		t.Fatalf("unexpected attempt count: %d", count)
	}
	waitDevice(t, device)
}

// TestZeroACKOutlastsNetworkDeadline 验证持续明确拒收可以等待设备打印，但丢失 ACK 仍快速失败。
func TestZeroACKOutlastsNetworkDeadline(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint("lost=", lost), func(t *testing.T) {
			device := newTestDevice(t, nil, func(conn net.Conn) error {
				for attempt := 0; ; attempt++ {
					packet, err := readRequest(conn)
					if err == io.EOF {
						return nil
					}
					if err != nil {
						return err
					}
					if lost && attempt == 1 {
						var extra [1]byte
						if n, _ := conn.Read(extra[:]); n != 0 {
							return errors.New("lost ACK caused a retransmission")
						}
						return nil
					}
					accepted := 0
					if attempt == 4 {
						accepted = len(packet.payload)
					}
					if _, err := conn.Write(ackPacket(packet, accepted)); err != nil {
						return err
					}
				}
			})
			start := time.Now()
			receipt, err := (Client{Address: device.address, Timeout: 100 * time.Millisecond, FlowControlTimeout: 2 * time.Second}).Submit(context.Background(), strings.NewReader("data"), "slow device")
			if lost {
				if !errors.Is(err, ErrUncertain) || receipt.Bytes != 0 || time.Since(start) > time.Second {
					t.Fatalf("lost ACK: receipt=%+v err=%v elapsed=%v", receipt, err, time.Since(start))
				}
			} else if err != nil || receipt.Bytes != 4 {
				t.Fatalf("explicit flow control: receipt=%+v err=%v", receipt, err)
			}
			waitDevice(t, device)
		})
	}
}

// TestZeroACKThenLostACK 验证零接收允许的一次重送不扩大为丢 ACK 重试。
func TestZeroACKThenLostACK(t *testing.T) {
	device := newTestDevice(t, nil, func(conn net.Conn) error {
		first, err := readRequest(conn)
		if err != nil {
			return err
		}
		if _, err := conn.Write(ackPacket(first, 0)); err != nil {
			return err
		}
		second, err := readRequest(conn)
		if err != nil {
			return err
		}
		if !bytes.Equal(first.payload, second.payload) || second.sequence != first.sequence+1 {
			return errors.New("invalid zero-ACK retransmission")
		}
		var extra [1]byte
		if n, _ := conn.Read(extra[:]); n != 0 {
			return errors.New("lost ACK caused another transmission")
		}
		return nil
	})
	receipt, err := (Client{Address: device.address, Timeout: 150 * time.Millisecond}).Submit(context.Background(), strings.NewReader("data"), "lost after throttle")
	if !errors.Is(err, ErrUncertain) || receipt.Bytes != 0 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	waitDevice(t, device)
}

// TestZeroACKCancellation 验证流控暂停可取消，取消后不启动下一次数据发送。
func TestZeroACKCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := newTestDevice(t, nil, func(conn net.Conn) error {
		packet, err := readRequest(conn)
		if err != nil {
			return err
		}
		if _, err := conn.Write(ackPacket(packet, 0)); err != nil {
			return err
		}
		cancel()
		var extra [1]byte
		if n, _ := conn.Read(extra[:]); n != 0 {
			return errors.New("flow control sent data after cancellation")
		}
		return nil
	})
	start := time.Now()
	receipt, err := (Client{Address: device.address, Timeout: time.Minute}).Submit(ctx, strings.NewReader("data"), "cancel throttle")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUncertain) || receipt.Bytes != 0 || time.Since(start) > time.Second {
		t.Fatalf("receipt=%+v err=%v elapsed=%v", receipt, err, time.Since(start))
	}
	waitDevice(t, device)
}

// TestBJLFrameBoundaries 验证全部跨块标记切点、相邻标记及 EOF 残缺前缀，逐字节保持打印内容。
func TestBJLFrameBoundaries(t *testing.T) {
	for _, position := range []int{0, 1, 4091, 4092, 4093, 4094, 4095, 4096, 4097, 8191} {
		t.Run(fmt.Sprint(position), func(t *testing.T) {
			marker := []byte{0x1b, 0x5b, 0x4b, 2, 0, 0}
			input := bytes.Repeat([]byte{'x'}, position)
			input = append(input, marker...)
			input = append(input, bytes.Repeat([]byte{'y'}, 5000)...)
			input = append(input, marker...)
			input = append(input, marker...)
			input = append(input, marker[:5]...)
			device := newTestDevice(t, nil, func(conn net.Conn) error {
				var received []byte
				boundaries := make(map[int]bool)
				for {
					packet, err := readRequest(conn)
					if err == io.EOF {
						break
					}
					if err != nil {
						return err
					}
					if len(packet.payload) == 0 || len(packet.payload) > 4096 {
						return fmt.Errorf("unexpected BJL frame length: %d", len(packet.payload))
					}
					boundaries[len(received)] = true
					received = append(received, packet.payload...)
					if _, err := conn.Write(ackPacket(packet, len(packet.payload))); err != nil {
						return err
					}
				}
				if !bytes.Equal(received, input) {
					return errors.New("BJL splitting changed stream bytes")
				}
				for offset := 0; offset < len(input); {
					next := bytes.Index(input[offset:], marker)
					if next < 0 {
						break
					}
					offset += next
					if !boundaries[offset] {
						return fmt.Errorf("BJL marker at %d does not begin a frame", offset)
					}
					offset++
				}
				return nil
			})
			receipt, err := (Client{Address: device.address, Timeout: time.Second}).Submit(context.Background(), bytes.NewReader(input), "BJL boundaries")
			if err != nil || receipt.Bytes != int64(len(input)) {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			waitDevice(t, device)
		})
	}
}

// TestSessionStatusRechecksResources 验证会话申请后的资源竞争、错误和未知状态均在 TCP 打印前拒绝。
func TestSessionStatusRechecksResources(t *testing.T) {
	for _, status := range []string{"BST:80000000;", "BST:08000000;", "DJS:;"} {
		t.Run(status, func(t *testing.T) {
			device := newTestDevice(t, func(command byte, response []byte) []byte {
				if command == 0x20 && binary.BigEndian.Uint16(response[10:]) == 0x1234 {
					return responsePacket(0x20, 1, 0x1234, textPayload(status))
				}
				return response
			}, nil)
			receipt, err := (Client{Address: device.address, Timeout: time.Second}).Submit(context.Background(), strings.NewReader("data"), "resource race")
			if err == nil || errors.Is(err, ErrUncertain) || receipt.Bytes != 0 || device.starts.Load() != 1 || device.closes.Load() != 1 {
				t.Fatalf("receipt=%+v err=%v starts=%d closes=%d", receipt, err, device.starts.Load(), device.closes.Load())
			}
		})
	}
}

// cancelAtEOF 在来源结束的同时取消调用，用于验证 EOF 与取消竞争不会误报成功。
type cancelAtEOF struct {
	cancel context.CancelFunc
}

// Read 同步取消后返回 EOF。
func (r cancelAtEOF) Read([]byte) (int, error) {
	r.cancel()
	return 0, io.EOF
}

// TestCancellationAtEOF 验证即使选择到 EOF 分支，结束会话前取消仍返回错误。
func TestCancellationAtEOF(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := newTestDevice(t, nil, func(conn net.Conn) error {
		_, err := readRequest(conn)
		if err != io.EOF {
			return fmt.Errorf("expected empty cancelled stream, got %v", err)
		}
		return nil
	})
	receipt, err := (Client{Address: device.address, Timeout: time.Second}).Submit(ctx, cancelAtEOF{cancel: cancel}, "EOF cancellation")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUncertain) || receipt.Bytes != 0 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	waitDevice(t, device)
}

// TestCancellationDuringClose 验证已完整确认打印数据后取消结束会话仍返回不确定。
func TestCancellationDuringClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := newTestDevice(t, func(command byte, response []byte) []byte {
		if command == 0x11 {
			cancel()
			return nil
		}
		return response
	}, func(conn net.Conn) error {
		packet, err := readRequest(conn)
		if err != nil {
			return err
		}
		_, err = conn.Write(ackPacket(packet, len(packet.payload)))
		return err
	})
	receipt, err := (Client{Address: device.address, Timeout: time.Minute}).Submit(ctx, strings.NewReader("data"), "close cancellation")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUncertain) || receipt.Bytes != 4 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	waitDevice(t, device)
}
