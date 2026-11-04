package bjnp

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestSessionFailureBeforePrintIsCertain 验证建立会话失败不会被标成可能已打印，也不会重试建立会话。
func TestSessionFailureBeforePrintIsCertain(t *testing.T) {
	device := newTestDevice(t, func(command byte, response []byte) []byte {
		if command == 0x10 {
			return nil
		}
		return response
	}, nil)
	receipt, err := (Client{Address: device.address, Timeout: 100 * time.Millisecond}).Submit(context.Background(), strings.NewReader("data"), "lost session")
	if err == nil || errors.Is(err, ErrUncertain) || receipt.Bytes != 0 || device.starts.Load() != 1 {
		t.Fatalf("receipt=%+v err=%v starts=%d", receipt, err, device.starts.Load())
	}
}

// TestCloseFailureAfterPrintIsUncertain 验证会话结束校验失败仍保留已接受字节，禁止整单自动重印。
func TestCloseFailureAfterPrintIsUncertain(t *testing.T) {
	device := newTestDevice(t, func(command byte, response []byte) []byte {
		if command == 0x11 {
			binary.BigEndian.PutUint16(response[10:], 0xffff)
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
	receipt, err := (Client{Address: device.address, Timeout: time.Second}).Submit(context.Background(), strings.NewReader("data"), "close failure")
	if !errors.Is(err, ErrUncertain) || receipt.Bytes != 4 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	waitDevice(t, device)
}

// failingSource 在提供有效字节后报告上游读取失败，用于检查部分输出的不确定边界。
type failingSource struct{}

// Read 返回可发送数据与真实错误，避免测试只复制内部结构。
func (failingSource) Read(buffer []byte) (int, error) {
	return copy(buffer, "data"), errors.New("source failed")
}

// TestSourceFailureAfterAcceptedData 验证源读取失败不会抹去已经确认的输出。
func TestSourceFailureAfterAcceptedData(t *testing.T) {
	device := newTestDevice(t, nil, func(conn net.Conn) error {
		packet, err := readRequest(conn)
		if err != nil {
			return err
		}
		_, err = conn.Write(ackPacket(packet, len(packet.payload)))
		return err
	})
	receipt, err := (Client{Address: device.address, Timeout: time.Second}).Submit(context.Background(), failingSource{}, "source failure")
	if !errors.Is(err, ErrUncertain) || receipt.Bytes != 4 || !strings.Contains(err.Error(), "source failed") {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	waitDevice(t, device)
}

// TestCancellationWhileSourceBlocked 验证普通 Reader 阻塞时调用仍可取消且无数据写入不标不确定。
func TestCancellationWhileSourceBlocked(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	device := newTestDevice(t, nil, func(conn net.Conn) error {
		cancel()
		_, err := readRequest(conn)
		if err != io.EOF {
			return errors.New("unexpected print data before source supplied bytes")
		}
		return nil
	})
	start := time.Now()
	receipt, err := (Client{Address: device.address, Timeout: time.Minute}).Submit(ctx, reader, "blocked input")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUncertain) || receipt.Bytes != 0 || time.Since(start) > time.Second {
		t.Fatalf("receipt=%+v err=%v elapsed=%v", receipt, err, time.Since(start))
	}
	waitDevice(t, device)
}
