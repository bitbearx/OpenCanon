// Package discovery 直接发现 BJNP 设备并持久化经过身份确认的 E560 地址。
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bitbearx/opencanon/internal/bjnp"
)

// ErrScanInProgress 表示已有扫描正在运行。
var ErrScanInProgress = errors.New("打印机扫描正在进行。")

// State 描述最近确认的设备及当前扫描状态，地址缓存不证明设备在线。
type State struct {
	Name         string     `json:"name"`
	Address      string     `json:"address"`
	Hostname     string     `json:"hostname"`
	DeviceURI    string     `json:"deviceURI"`
	DiscoveredAt *time.Time `json:"discoveredAt,omitempty"`
	Scanning     bool       `json:"scanning"`
	LastError    string     `json:"lastError"`
}

// Service 管理配置地址、广播发现和原子持久化。
type Service struct {
	configured string
	path       string
	scanMu     sync.Mutex
	mu         sync.Mutex
	state      State
	detect     func(context.Context, string) (State, error)
}

// NormalizeAddress 将管理员配置的主机或主机端口规范化为 BJNP 地址。
func NormalizeAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", nil
	}
	if strings.ContainsAny(address, "/\\?#@\x00 \t\r\n") {
		return "", errors.New("打印机地址必须是主机或主机:端口")
	}
	if ip := net.ParseIP(address); ip != nil {
		return net.JoinHostPort(address, "8611"), nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		if strings.Contains(address, ":") {
			return "", errors.New("打印机端口无效")
		}
		host, port = address, "8611"
	}
	n, err := strconv.Atoi(port)
	if host == "" || err != nil || n < 1 || n > 65535 {
		return "", errors.New("打印机地址或端口无效")
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// New 加载 BJNP 地址记录；旧 CUPS 记录必须重新发现后才能使用。
func New(dataDir, address string) (*Service, error) {
	address, err := NormalizeAddress(address)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	s := &Service{configured: address, path: filepath.Join(dir, "printer-discovery.json"), detect: detect}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	var state State
	if err != nil || json.Unmarshal(b, &state) != nil || !validSaved(state) {
		s.state.LastError = "已保存的设备记录不可用，请重新扫描。"
		return s, nil
	}
	state.Scanning, state.LastError = false, ""
	s.state = state
	return s, nil
}

// Snapshot 返回独立状态副本，避免并发修改发现时间。
func (s *Service) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state
	if state.DiscoveredAt != nil {
		t := *state.DiscoveredAt
		state.DiscoveredAt = &t
	}
	return state
}

// Endpoint 优先使用管理员配置，否则返回上次确认的 BJNP 地址。
func (s *Service) Endpoint() string {
	if s.configured != "" {
		return s.configured
	}
	state := s.Snapshot()
	if !validSaved(state) {
		return ""
	}
	u, _ := url.Parse(state.DeviceURI)
	return u.Host
}

// Scan 对配置地址直接查询或广播发现，失败时保留原记录。
func (s *Service) Scan(ctx context.Context) (State, error) {
	if !s.scanMu.TryLock() {
		return s.Snapshot(), ErrScanInProgress
	}
	defer s.scanMu.Unlock()
	s.mu.Lock()
	s.state.Scanning = true
	s.state.LastError = ""
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	state, err := s.detect(ctx, s.configured)
	if err == nil && !validSaved(state) {
		err = errors.New("设备发现结果无效。")
	}
	if err == nil {
		err = s.save(ctx, state)
	}
	s.mu.Lock()
	if err != nil {
		s.state.Scanning = false
		s.state.LastError = err.Error()
	} else {
		s.state = state
	}
	s.mu.Unlock()
	return s.Snapshot(), err
}

// detect 用 BJNP 身份查询筛选精确型号，多个 E560 时要求明确配置地址。
func detect(ctx context.Context, configured string) (State, error) {
	var addresses []string
	if configured != "" {
		addresses = []string{configured}
	} else {
		devices, err := bjnp.Discover(ctx)
		if err != nil {
			return State{}, errors.New("BJNP 发现失败，请检查网络或设置 printer-address。")
		}
		for _, d := range devices {
			addresses = append(addresses, d.Address)
		}
	}
	var matches []State
	for _, address := range addresses {
		info, err := (bjnp.Client{Address: address, Timeout: 3 * time.Second}).Probe(ctx)
		if err != nil {
			continue
		}
		model := strings.ToLower(strings.TrimSpace(info.Model))
		model = strings.TrimPrefix(strings.TrimPrefix(model, "canon "), "pixma ")
		if model != "e560" && model != "e560 series" {
			continue
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			continue
		}
		now := time.Now().UTC()
		matches = append(matches, State{Name: "Canon E560 series", Address: host, Hostname: host, DeviceURI: "bjnp://" + address, DiscoveredAt: &now})
	}
	if ctx.Err() != nil {
		return State{}, errors.New("打印机扫描已取消或超时。")
	}
	if len(matches) == 0 {
		return State{}, errors.New("未发现可读取身份和状态的 E560，请检查电源、网络或设置 printer-address。")
	}
	if len(matches) != 1 {
		return State{}, errors.New("发现多个 E560，请通过 printer-address 指定设备。")
	}
	return matches[0], nil
}

// validSaved 仅恢复完整且使用 BJNP 的精确型号记录。
func validSaved(s State) bool {
	u, err := url.Parse(s.DeviceURI)
	if err != nil || u.Scheme != "bjnp" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	address, err := NormalizeAddress(u.Host)
	host, _, splitErr := net.SplitHostPort(address)
	return err == nil && splitErr == nil && address == u.Host && s.Address == host && s.Name == "Canon E560 series" && s.DiscoveredAt != nil && !s.DiscoveredAt.IsZero()
}

// save 在同目录以私有权限原子替换已确认设备记录。
func (s *Service) save(ctx context.Context, state State) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".printer-discovery-*")
	if err != nil {
		return errors.New("无法保存设备发现结果。")
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("无法保存设备发现结果。")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return errors.New("无法保存设备发现结果。")
	}
	return nil
}
