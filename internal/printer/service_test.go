package printer

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// TestJobStatusDoesNotClaimGlobalBusy 验证外部作业占用设备时不会冒称历史任务正在处理。
func TestJobStatusDoesNotClaimGlobalBusy(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var request [2048]byte
		for range 2 {
			n, peer, err := conn.ReadFromUDP(request[:])
			if err != nil {
				done <- err
				return
			}
			if n < 16 {
				done <- net.ErrClosed
				return
			}
			text := "MFG:Canon;MDL:E560 series;"
			if request[5] == 0x20 {
				text = "BST:20000000;DJS:;"
			}
			response := make([]byte, 18+len(text))
			copy(response, request[:16])
			response[4] = 0x81
			binary.BigEndian.PutUint32(response[12:16], uint32(2+len(text)))
			binary.BigEndian.PutUint16(response[16:18], uint16(2+len(text)))
			copy(response[18:], text)
			if _, err := conn.WriteToUDP(response, peer); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	s := &Service{jobs: map[string]jobReceipt{"previous": {address: conn.LocalAddr().String(), expires: time.Now().Add(time.Hour)}}}
	state, _ := s.JobStatus(context.Background(), "previous")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if state != "unknown" {
		t.Fatalf("unrelated device activity attributed to previous job: %s", state)
	}
}
