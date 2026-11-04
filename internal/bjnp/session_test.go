package bjnp

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// TestSubmitE560SessionExtension 验证实机四字节会话扩展不会阻断数据及完整结束会话。
func TestSubmitE560SessionExtension(t *testing.T) {
	const input = "E560 session extension regression"
	device := newTestDevice(t, func(command byte, response []byte) []byte {
		if command == cmdStart {
			return responsePacket(command, 0, 0x1234, []byte{0x73, 0x6c, 0x05, 0x48})
		}
		return response
	}, func(conn net.Conn) error {
		packet, err := readRequest(conn)
		if err != nil {
			return err
		}
		if packet.command != cmdPrint || packet.sequence != 2 || packet.session != 0x1234 || string(packet.payload) != input {
			return errors.New("session extension changed print data or association")
		}
		_, err = conn.Write(ackPacket(packet, len(packet.payload)))
		return err
	})
	receipt, err := (Client{Address: device.address, Timeout: time.Second}).Submit(context.Background(), strings.NewReader(input), "E560 regression")
	if err != nil || receipt.Session != 0x1234 || receipt.Bytes != int64(len(input)) {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	waitDevice(t, device)
	if device.starts.Load() != 1 || device.closes.Load() != 1 {
		t.Fatalf("session lifecycle: starts=%d closes=%d", device.starts.Load(), device.closes.Load())
	}
}
