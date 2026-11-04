package bjnp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Probe 查询身份与状态，不建立打印会话；未知状态不报告为就绪。
func (c Client) Probe(ctx context.Context) (Info, error) {
	x := exchange{client: c, ctx: ctx}
	identity, err := x.udp(cmdIdentity, nil)
	if err != nil {
		return Info{}, fmt.Errorf("BJNP 身份查询: %w", err)
	}
	info, err := parseIdentity(identity.payload)
	if err != nil {
		return info, err
	}
	status, err := x.udp(cmdStatus, nil)
	if err != nil {
		return info, fmt.Errorf("BJNP 状态查询: %w", err)
	}
	if err := parseStatus(&info, status.payload); err != nil {
		return info, err
	}
	return info, nil
}

// fields 提取 IEEE1284 与旧式状态字段，拒绝重复字段造成的歧义。
func fields(text string) (map[string]string, error) {
	result := make(map[string]string)
	for _, part := range strings.Split(text, ";") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		key, value, ok := strings.Cut(part, ":")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, errors.New("BJNP 文本字段格式无效")
		}
		key = strings.TrimSpace(key)
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("BJNP 重复字段: %s", key)
		}
		result[key] = strings.TrimSpace(value)
	}
	return result, nil
}

// parseIdentity 验证包含自身的身份长度，并优先读取 MDL 型号。
func parseIdentity(payload []byte) (Info, error) {
	var info Info
	if len(payload) < 2 || len(payload) > 1024 || int(binary.BigEndian.Uint16(payload[:2])) != len(payload) {
		return info, errors.New("BJNP 身份长度无效")
	}
	info.Identity = strings.TrimRight(string(payload[2:]), "\x00")
	if strings.ContainsRune(info.Identity, '\x00') {
		return info, errors.New("BJNP 身份含嵌入空字符")
	}
	values, err := fields(info.Identity)
	if err != nil {
		return info, err
	}
	info.Model = values["MDL"]
	if info.Model == "" {
		info.Model = values["DES"]
	}
	if info.Model == "" {
		return info, errors.New("BJNP 身份缺少 MDL/DES 型号")
	}
	return info, nil
}

// jobDetails 生成固定长度的作业信息；非 ASCII 字符替换为问号，避免猜测设备字符集。
func jobDetails(title string) []byte {
	payload := make([]byte, 392)
	hostname, _ := os.Hostname()
	putJobText(payload[8:72], hostname)
	putJobText(payload[72:136], "opencanon")
	putJobText(payload[136:], title)
	return payload
}

// putJobText 使用已知的双字节 ASCII 格式，并保留结尾零字符。
func putJobText(dst []byte, text string) {
	position := 0
	for len(text) > 0 && position+2 < len(dst) {
		r, width := utf8.DecodeRuneInString(text)
		text = text[width:]
		if r < 0x20 || r > 0x7e {
			r = '?'
		}
		dst[position+1] = byte(r)
		position += 2
	}
}

// sourceResult 在读取协程与网络协程间传递一个块，数据缓冲在确认后才复用。
type sourceResult struct {
	n   int
	err error
}

// bjlMarker 是公开后端在 BJNP 新帧开始处保留的 BJL 命令前缀。
const bjlMarker = "\x1b\x5b\x4b\x02\x00\x00"

// streamSource 用五字节前瞻识别跨 4096 边界的 BJL 标记；确认后才移动和复用缓冲。
// 普通 Reader 的阻塞 Read 无法由接口取消；调用方拥有 Reader，Submit 不等待该 Read。
func streamSource(ctx context.Context, src io.Reader, buffer []byte, requests <-chan struct{}, results chan<- sourceResult) {
	available, consumed := 0, 0
	var sourceErr error
	for {
		select {
		case <-ctx.Done():
			return
		case <-requests:
		}
		available = copy(buffer, buffer[consumed:available])
		if sourceErr == nil {
			n, err := io.ReadFull(src, buffer[available:])
			available += n
			sourceErr = err
		}
		consumed = min(available, printBlock)
		if available > 1 {
			// 忽略帧首标记，遇到下一个完整标记时将它留到下一帧。
			if next := bytes.Index(buffer[1:available], []byte(bjlMarker)); next >= 0 && next+1 < consumed {
				consumed = next + 1
			}
		}
		var resultErr error
		if consumed == available {
			resultErr = sourceErr
		}
		select {
		case results <- sourceResult{n: consumed, err: resultErr}:
		case <-ctx.Done():
			return
		}
		if resultErr != nil {
			return
		}
	}
}

// Submit 探测设备、建立会话并逐块确认；任何已开始打印后的失败均禁止自动重印。
func (c Client) Submit(ctx context.Context, src io.Reader, title string) (receipt Receipt, resultErr error) {
	if src == nil {
		return receipt, errors.New("BJNP 打印源为空")
	}
	info, err := c.Probe(ctx)
	if err != nil {
		return receipt, err
	}
	if info.Busy {
		return receipt, ErrBusy
	}
	if info.DeviceError {
		return receipt, errors.New("BJNP 打印机报告设备错误")
	}
	x := exchange{client: c, ctx: ctx}
	response, err := x.udp(cmdStart, jobDetails(title))
	if err != nil {
		return receipt, fmt.Errorf("BJNP 建立会话: %w", err)
	}
	// E560 的 BJNP2 响应另带四字节不透明扩展；会话号仍取自已校验的帧头。
	if len(response.payload) != 0 && len(response.payload) != 4 {
		return receipt, errors.New("BJNP 建立会话响应负载无效")
	}
	x.session = response.session
	receipt.Session = x.session
	started := false
	// 错误清理仍尝试结束会话；已取消的调用不另开后台网络操作。
	defer func() {
		if ctx.Err() == nil {
			closed, closeErr := x.udp(cmdClose, nil)
			if closeErr == nil && len(closed.payload) != 0 {
				closeErr = errors.New("BJNP 结束会话响应负载无效")
			}
			if resultErr == nil && closeErr != nil {
				resultErr = fmt.Errorf("BJNP 结束会话: %w", closeErr)
			}
		}
		if resultErr == nil && ctx.Err() != nil {
			resultErr = ctx.Err()
		}
		if resultErr != nil && started {
			resultErr = errors.Join(ErrUncertain, resultErr)
		}
	}()
	// 建立会话后再次检查资源，避免首次探测与申请会话之间的外部作业竞争。
	status, err := x.udp(cmdStatus, nil)
	if err != nil {
		return receipt, fmt.Errorf("BJNP 会话状态查询: %w", err)
	}
	var sessionInfo Info
	if err := parseStatus(&sessionInfo, status.payload); err != nil {
		return receipt, err
	}
	if sessionInfo.Busy {
		return receipt, ErrBusy
	}
	if sessionInfo.DeviceError {
		return receipt, errors.New("BJNP 会话状态报告设备错误")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", c.Address)
	if err != nil {
		return receipt, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	var buffer [printBlock + len(bjlMarker) - 1]byte
	requests := make(chan struct{}, 1)
	results := make(chan sourceResult, 1)
	go streamSource(readCtx, src, buffer[:], requests, results)
	requests <- struct{}{}
	timer := time.NewTimer(keepaliveInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return receipt, ctx.Err()
		case <-timer.C:
			if err := x.printFrame(conn, nil, &started); err != nil {
				return receipt, err
			}
			timer.Reset(keepaliveInterval)
		case block := <-results:
			if block.n > 0 {
				if err := x.printFrame(conn, buffer[:block.n], &started); err != nil {
					return receipt, err
				}
				receipt.Bytes += int64(block.n)
			}
			if block.err == io.EOF || block.err == io.ErrUnexpectedEOF {
				return receipt, nil
			}
			if block.err != nil {
				return receipt, fmt.Errorf("BJNP 读取打印源: %w", block.err)
			}
			timer.Reset(keepaliveInterval)
			requests <- struct{}{}
		}
	}
}
