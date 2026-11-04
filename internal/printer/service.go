package printer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bitbearx/opencanon/internal/bjnp"
	"github.com/bitbearx/opencanon/internal/render"
)

// ErrUncertain 表示设备可能已接收打印数据，禁止自动重印。
var ErrUncertain = bjnp.ErrUncertain

// jobReceipt 保留本进程任务实际发送到的设备和失效时间。
type jobReceipt struct {
	address string
	expires time.Time
}

// Service 串行化单台 E560 的渲染与网络提交，并保存接收凭据。
type Service struct {
	address   func() string
	renderer  render.Renderer
	renderErr error
	printing  sync.Mutex
	mu        sync.Mutex
	jobs      map[string]jobReceipt
}

// NewService 检查独立编码器并创建设备服务；缺依赖时保持不可打印。
func NewService(ctx context.Context, address func() string, renderer render.Renderer) *Service {
	return &Service{address: address, renderer: renderer, renderErr: renderer.Check(ctx), jobs: make(map[string]jobReceipt)}
}

// isE560 仅接受精确 E560 型号，避免将其他设备套用该编码器。
func isE560(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimPrefix(model, "canon ")
	model = strings.TrimPrefix(model, "pixma ")
	return model == "e560" || model == "e560 series"
}

// Available 检查编码器以及设备真实状态，缓存地址不代表可打印。
func (s *Service) Available(ctx context.Context) (bool, string) {
	if s.renderErr != nil {
		return false, "独立 E560 编码器不可用，请检查 Gutenprint、opencanon-render 和 pdftoppm。"
	}
	if !s.printing.TryLock() {
		return false, "服务正在发送打印任务，请稍后再试。"
	}
	s.printing.Unlock()
	address := s.address()
	if address == "" {
		return false, "尚未发现 E560，请重新扫描或配置 printer-address。"
	}
	info, err := (bjnp.Client{Address: address, Timeout: 3 * time.Second}).Probe(ctx)
	if err != nil {
		return false, "无法直接连接打印机，请检查电源、网络和设备地址。"
	}
	if !isE560(info.Model) {
		return false, "目标设备不是受支持的 Canon E560。"
	}
	if info.DeviceError {
		return false, "打印机报告错误，请检查纸张、墨盒和设备面板。"
	}
	if info.Busy {
		return false, "打印机正忙，请等待当前任务结束。"
	}
	return true, ""
}

// Submit 先完整渲染再发送；传输中断不重试，ACK 不作为物理完成证明。
func (s *Service) Submit(ctx context.Context, input, title string, job render.Job) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()
	if s.renderErr != nil {
		return "", fmt.Errorf("renderer unavailable: %w", s.renderErr)
	}
	if !s.printing.TryLock() {
		return "", bjnp.ErrBusy
	}
	defer s.printing.Unlock()
	address := s.address()
	if address == "" {
		return "", errors.New("no printer address")
	}
	client := bjnp.Client{Address: address, Timeout: 15 * time.Second, FlowControlTimeout: 2 * time.Minute}
	info, err := client.Probe(ctx)
	if err != nil {
		return "", err
	}
	if !isE560(info.Model) {
		return "", errors.New("unsupported printer model")
	}
	if info.Busy || info.DeviceError {
		return "", bjnp.ErrBusy
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	id := "bjnp-" + hex.EncodeToString(token[:])
	path, err := s.renderer.Render(ctx, input, job, filepath.Dir(input))
	if err != nil {
		return "", fmt.Errorf("render: %w", err)
	}
	defer os.Remove(path)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := client.Submit(ctx, f, title); err != nil {
		return "", err
	}
	s.mu.Lock()
	now := time.Now()
	for key, receipt := range s.jobs {
		if now.After(receipt.expires) {
			delete(s.jobs, key)
		}
	}
	s.jobs[id] = jobReceipt{address: address, expires: now.Add(time.Hour)}
	s.mu.Unlock()
	return id, nil
}

// JobStatus 报告接收凭据及设备当前状况，不把设备空闲推断成作业完成。
func (s *Service) JobStatus(ctx context.Context, id string) (string, string) {
	s.mu.Lock()
	receipt, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok || time.Now().After(receipt.expires) {
		return "unknown", "任务记录不存在或已过期，请检查实际出纸。"
	}
	info, err := (bjnp.Client{Address: receipt.address, Timeout: 3 * time.Second}).Probe(ctx)
	if err != nil {
		return "unknown", "设备已确认接收数据，目前无法读取设备状态；请检查实际出纸，勿重复提交。"
	}
	if info.DeviceError {
		return "unknown", "设备已接收数据，但当前报告错误；请检查设备面板和实际出纸，勿重复提交。"
	}
	if info.Busy {
		return "unknown", "设备已接收本任务的数据，目前处于忙状态；无法确定正在处理哪一项任务，请检查实际出纸。"
	}
	return "unknown", "设备已确认接收全部数据，当前空闲；BJNP 无法确认本任务的物理完成，请检查实际出纸。"
}
