// Package bjnp 实现打印机的 BJNP 会话与传输；ACK 仅表示设备接受字节，不能证明出纸。
// 协议事实依据 cups-bjnp 的公开协议结构独立实现：
// https://github.com/bitwiseworks/cups-bjnp-os2/tree/5f1a04542c5e1a714344563972cd1914b4a19185
package bjnp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	headerSize        = 16
	printBlock        = 4096
	maxResponse       = 2048
	cmdDiscover       = 0x01
	cmdStart          = 0x10
	cmdClose          = 0x11
	cmdStatus         = 0x20
	cmdPrint          = 0x21
	cmdIdentity       = 0x30
	keepaliveInterval = 3 * time.Second
)

var (
	ErrUncertain = errors.New("BJNP 打印数据可能已被接受，禁止自动重发")
	ErrBusy      = errors.New("BJNP 打印机忙")
)

// Client 保存设备地址与单次网络操作超时；每次调用独立管理会话。
type Client struct {
	Address string
	Timeout time.Duration
	// FlowControlTimeout 限制完整零 ACK 的累计等待；未设置时沿用单次网络时限。
	FlowControlTimeout time.Duration
}

// Info 描述设备声明的身份与状态；无法解释的状态通过错误返回。
type Info struct {
	Identity, Model, Status string
	Busy, DeviceError       bool
}

// Receipt 记录会话及已获得完整 ACK 的打印数据量。
type Receipt struct {
	Session uint16
	Bytes   int64
}

// Device 描述发现响应的来源地址及设备 MAC。
type Device struct {
	Address, MAC string
}

// frame 保存已经通过长度校验的协议帧。
type frame struct {
	command           byte
	sequence, session uint16
	payload           []byte
}

// exchange 持有单次调用的序号与会话，避免客户端并发调用共享状态。
type exchange struct {
	client            Client
	ctx               context.Context
	sequence, session uint16
}

// encodeHeader 生成网络大端序的请求头。
func encodeHeader(dst []byte, command byte, sequence, session uint16, length int) {
	copy(dst, "BJNP")
	dst[4], dst[5], dst[6], dst[7] = 0x01, command, 0, 0
	binary.BigEndian.PutUint16(dst[8:10], sequence)
	binary.BigEndian.PutUint16(dst[10:12], session)
	binary.BigEndian.PutUint32(dst[12:16], uint32(length))
}

// decodeHeader 校验响应标识并限制分配量，保留请求关联字段。
func decodeHeader(header []byte, limit int) (frame, int, error) {
	if len(header) != headerSize || string(header[:4]) != "BJNP" || header[4] != 0x81 {
		return frame{}, 0, errors.New("BJNP 响应头无效")
	}
	n := binary.BigEndian.Uint32(header[12:16])
	if uint64(n) > uint64(limit) {
		return frame{}, 0, fmt.Errorf("BJNP 响应负载过长: %d", n)
	}
	return frame{command: header[5], sequence: binary.BigEndian.Uint16(header[8:10]), session: binary.BigEndian.Uint16(header[10:12])}, int(n), nil
}

// match 校验命令、序号和会话；建立会话时允许设备返回新会话号。
func (f frame) match(command byte, sequence, session uint16) error {
	if f.command != command || f.sequence != sequence || (command != cmdStart && f.session != session) {
		return fmt.Errorf("BJNP 响应关联错误: command=%02x sequence=%d session=%d", f.command, f.sequence, f.session)
	}
	return nil
}

// writeFull 处理合法短写，并拒绝无进展写入。
func writeFull(dst io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := dst.Write(data)
		if n < 0 || n > len(data) {
			return errors.New("BJNP 写入返回非法长度")
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// operationDeadline 计算单次网络交互的期限，并服从整单取消时限。
func (x *exchange) operationDeadline() time.Time {
	timeout := x.client.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if d, ok := x.ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return deadline
}

// deadline 为 UDP 等单次交互设置截止时间。
func (x *exchange) deadline(conn net.Conn) error {
	return conn.SetDeadline(x.operationDeadline())
}

// networkError 保留取消语义，避免关闭连接掩盖 context 错误。
func (x *exchange) networkError(err error) error {
	if x.ctx.Err() != nil {
		return x.ctx.Err()
	}
	return err
}

// udp 发送一次 UDP 命令并严格检查完整数据报；会话命令不自动重试。
func (x *exchange) udp(command byte, payload []byte) (frame, error) {
	sequence, session := x.sequence, x.session
	if command == cmdStart || command == cmdIdentity {
		sequence, session = 0, 0
	} else {
		x.sequence++
		sequence = x.sequence
	}
	conn, err := (&net.Dialer{}).DialContext(x.ctx, "udp", x.client.Address)
	if err != nil {
		return frame{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(x.ctx, func() { conn.Close() })
	defer stop()
	if err := x.deadline(conn); err != nil {
		return frame{}, err
	}
	request := make([]byte, headerSize+len(payload))
	encodeHeader(request, command, sequence, session, len(payload))
	copy(request[headerSize:], payload)
	n, err := conn.Write(request)
	if err != nil {
		return frame{}, x.networkError(err)
	}
	if n != len(request) {
		return frame{}, io.ErrShortWrite
	}
	var response [headerSize + maxResponse + 1]byte
	n, err = conn.Read(response[:])
	if err != nil {
		return frame{}, x.networkError(err)
	}
	if n < headerSize {
		return frame{}, errors.New("BJNP UDP 响应头不完整")
	}
	f, length, err := decodeHeader(response[:headerSize], maxResponse)
	if err != nil {
		return frame{}, err
	}
	if length != n-headerSize {
		return frame{}, errors.New("BJNP UDP 响应长度不符")
	}
	if err := f.match(command, sequence, session); err != nil {
		return frame{}, err
	}
	f.payload = append([]byte(nil), response[headerSize:n]...)
	return f, nil
}

// printFrame 只在完整且匹配的零 ACK 明确拒收后流控重送；总期限覆盖所有尝试。
func (x *exchange) printFrame(conn net.Conn, payload []byte, started *bool) error {
	deadline := x.operationDeadline()
	if x.client.FlowControlTimeout > 0 {
		deadline = time.Now().Add(x.client.FlowControlTimeout)
		if d, ok := x.ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
	}
	for {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("BJNP 单帧流控超时: %w", context.DeadlineExceeded)
		}
		networkDeadline := x.operationDeadline()
		if deadline.Before(networkDeadline) {
			networkDeadline = deadline
		}
		if err := conn.SetDeadline(networkDeadline); err != nil {
			return err
		}
		accepted, err := x.printAttempt(conn, payload, started)
		if err != nil {
			return err
		}
		if accepted == uint32(len(payload)) {
			return nil
		}
		if accepted != 0 {
			return fmt.Errorf("BJNP ACK 接受字节不符: %d/%d", accepted, len(payload))
		}
		// 非空帧 accepted=0 是上游定义的 THROTTLE，只有此确认拒收分支允许再次发送。
		delay := min(40*time.Millisecond, time.Until(deadline))
		timer := time.NewTimer(max(delay, 0))
		select {
		case <-x.ctx.Done():
			timer.Stop()
			return x.ctx.Err()
		case <-timer.C:
		}
	}
}

// printAttempt 发送一个新序号帧并完整读取 ACK，任何传输或关联错误都直接终止。
func (x *exchange) printAttempt(conn net.Conn, payload []byte, started *bool) (uint32, error) {
	x.sequence++
	var header [headerSize]byte
	encodeHeader(header[:], cmdPrint, x.sequence, x.session, len(payload))
	if len(payload) != 0 {
		*started = true
	}
	if err := writeFull(conn, header[:]); err != nil {
		return 0, x.networkError(err)
	}
	if err := writeFull(conn, payload); err != nil {
		return 0, x.networkError(err)
	}
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return 0, x.networkError(err)
	}
	response, length, err := decodeHeader(header[:], maxResponse)
	if err != nil {
		return 0, err
	}
	if err := response.match(cmdPrint, x.sequence, x.session); err != nil {
		return 0, err
	}
	if length < 4 {
		return 0, errors.New("BJNP ACK 缺少接受字节数")
	}
	var ack [maxResponse]byte
	if _, err := io.ReadFull(conn, ack[:length]); err != nil {
		return 0, x.networkError(err)
	}
	return binary.BigEndian.Uint32(ack[:4]), nil
}
