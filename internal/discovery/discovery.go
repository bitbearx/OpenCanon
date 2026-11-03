// Package discovery 发现固定打印队列对应的局域网设备。
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bitbearx/opencanon/internal/printer"
)

// ErrScanInProgress 表示已有扫描正在运行。
var ErrScanInProgress = errors.New("打印机扫描正在进行。")

// State 描述最近确认的发现结果及当前扫描状态。
type State struct {
	Name         string     `json:"name"`
	Address      string     `json:"address"`
	Hostname     string     `json:"hostname"`
	DeviceURI    string     `json:"deviceURI"`
	DiscoveredAt *time.Time `json:"discoveredAt,omitempty"`
	Scanning     bool       `json:"scanning"`
	LastError    string     `json:"lastError"`
	ServiceName  string     `json:"serviceName,omitempty"`
	ServiceType  string     `json:"serviceType,omitempty"`
}

// Service 管理固定队列的发现和已确认结果。
type Service struct {
	runner printer.Runner
	path   string
	scanMu sync.Mutex
	mu     sync.Mutex
	state  State
}

// New 初始化发现服务并加载已确认的结果。
func New(dataDir string, runner printer.Runner) (*Service, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, errors.New("无法初始化打印机发现数据。")
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("无法初始化打印机发现数据。")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, errors.New("无法初始化打印机发现数据。")
	}
	s := &Service{runner: runner, path: filepath.Join(dir, "printer-discovery.json")}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	var state State
	if err != nil || json.Unmarshal(b, &state) != nil || !validSaved(state) {
		s.state.LastError = "已保存的打印机发现记录不可用，请重新扫描。"
		return s, nil
	}
	state.Scanning = false
	state.LastError = ""
	s.state = state
	return s, nil
}

// Snapshot 返回当前状态的独立副本。
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

// Scan 重新发现固定队列设备，并仅在完整成功后保存结果。
func (s *Service) Scan(ctx context.Context) (State, error) {
	if !s.scanMu.TryLock() {
		return s.Snapshot(), ErrScanInProgress
	}
	defer s.scanMu.Unlock()
	s.mu.Lock()
	s.state.Scanning = true
	s.state.LastError = ""
	s.mu.Unlock()
	state, err := s.scan(ctx)
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

// scan 执行队列查询、发现和持久化。
func (s *Service) scan(ctx context.Context) (State, error) {
	failure := func(message string) (State, error) { return State{}, errors.New(message) }
	if s.runner == nil {
		return failure("打印机发现服务不可用，请检查服务配置。")
	}
	out, err := s.runner.Run(ctx, 15*time.Second, "/usr/bin/ipptool", "-t", "-v", "ipp://%2Fprivate%2Fvar%2Frun%2Fcupsd/printers/"+printer.Queue, "/usr/share/cups/ipptool/get-printer-attributes.test")
	if ctx.Err() != nil {
		return failure("打印机扫描已取消。")
	}
	if err != nil {
		return failure("无法查询本机打印队列，请检查 CUPS 配置。")
	}
	uri, name, typ, ok := queueURI(string(out))
	if !ok {
		return failure("当前队列不是受支持的 Canon 网络连接。")
	}
	out, err = s.runner.Run(ctx, 3*time.Second, "/usr/bin/dns-sd", "-L", name, typ, "local.")
	if ctx.Err() != nil {
		return failure("打印机扫描已取消。")
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return failure("无法发现打印机，请检查网络连接。")
	}
	host, ok := resolveHost(string(out))
	if !ok {
		return failure("未能确定唯一的打印机主机，请检查网络连接或地址冲突。")
	}
	addressStarted := time.Now()
	out, err = s.runner.Run(ctx, 3*time.Second, "/usr/bin/dns-sd", "-G", "v4", host)
	if ctx.Err() != nil {
		return failure("打印机扫描已取消。")
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return failure("无法查询打印机地址，请检查网络连接。")
	}
	addr, ok := resolveAddress(string(out), host, time.Since(addressStarted))
	if !ok {
		return failure("未能确定唯一的打印机局域网地址，请检查网络连接或地址冲突。")
	}
	now := time.Now().UTC()
	state := State{Name: name, Address: addr, Hostname: host, DeviceURI: uri, DiscoveredAt: &now, ServiceName: name, ServiceType: typ}
	if err := s.save(ctx, state); err != nil {
		return failure(err.Error())
	}
	return state, nil
}

// uriAttribute 匹配 CUPS 的结构化设备 URI 属性。
var uriAttribute = regexp.MustCompile(`^\s*device-uri\s*\(uri\)\s*=\s*(\S+)\s*$`)

// queueURI 从结构化队列属性提取并验证唯一 Canon 服务。
func queueURI(output string) (string, string, string, bool) {
	var values []string
	for _, line := range strings.Split(output, "\n") {
		if match := uriAttribute.FindStringSubmatch(line); match != nil {
			values = append(values, match[1])
		}
	}
	if len(values) != 1 {
		return "", "", "", false
	}
	name, typ, ok := parseURI(values[0])
	return values[0], name, typ, ok
}

// parseURI 校验 Canon 网络 URI 并解码服务名。
func parseURI(raw string) (string, string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	typ := "_canon-" + "bjnp1._tcp."
	query, queryErr := url.ParseQuery(u.RawQuery)
	if queryErr != nil || u.Scheme != "canonijnetwork" || u.Host != "Canon" || u.User != nil || u.Fragment != "" || len(query) != 1 || len(query["TYPE"]) != 1 || query.Get("TYPE") != typ || !strings.HasPrefix(u.EscapedPath(), "/") {
		return "", "", false
	}
	name, err := url.PathUnescape(strings.TrimPrefix(u.EscapedPath(), "/"))
	if err != nil || name == "" || len(name) > 255 || strings.ContainsAny(name, "/\\") {
		return "", "", false
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return "", "", false
		}
	}
	return name, typ, true
}

// validHost 验证 Bonjour 主机名的安全 DNS 格式。
func validHost(host string) bool {
	if len(host) > 253 || !strings.HasSuffix(strings.ToLower(host), ".local.") {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// resolveHost 只接受唯一的有效 Bonjour 主机。
func resolveHost(output string) (string, bool) {
	hosts := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		_, rest, found := strings.Cut(line, " can be reached at ")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		host, port, err := net.SplitHostPort(fields[0])
		if err != nil || !validHost(host) {
			continue
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			continue
		}
		hosts[strings.ToLower(host)] = host
	}
	if len(hosts) != 1 {
		return "", false
	}
	for _, h := range hosts {
		return h, true
	}
	return "", false
}

// resolveAddress 保守地扣除完整查询窗口，避免将已过期记录当作新地址。
func resolveAddress(output, host string, elapsed time.Duration) (string, bool) {
	active := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		f := strings.Fields(line)
		if len(f) < 7 || (f[1] != "Add" && f[1] != "Rmv") || !strings.EqualFold(f[4], host) {
			continue
		}
		ip, err := netip.ParseAddr(f[5])
		if err != nil || !ip.Is4() || !ip.IsPrivate() {
			continue
		}
		key := f[3] + "/" + ip.String()
		if f[1] == "Rmv" {
			delete(active, key)
		} else if ttl, err := strconv.ParseUint(f[6], 10, 32); err == nil {
			if time.Duration(ttl)*time.Second > elapsed {
				active[key] = ip.String()
			} else {
				delete(active, key)
			}
		}
	}
	addresses := map[string]bool{}
	for _, ip := range active {
		addresses[ip] = true
	}
	if len(addresses) != 1 {
		return "", false
	}
	for ip := range addresses {
		return ip, true
	}
	return "", false
}

// validSaved 防止损坏或不完整的持久记录暴露未验证地址。
func validSaved(s State) bool {
	name, typ, ok := parseURI(s.DeviceURI)
	ip, err := netip.ParseAddr(s.Address)
	return ok && s.Name == name && s.ServiceName == name && s.ServiceType == typ && validHost(s.Hostname) && err == nil && ip.Is4() && ip.IsPrivate() && s.DiscoveredAt != nil && !s.DiscoveredAt.IsZero()
}

// save 使用同目录临时文件原子替换已确认结果。
func (s *Service) save(ctx context.Context, state State) error {
	b, err := json.Marshal(state)
	if err != nil {
		return errors.New("无法保存打印机发现结果。")
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".printer-discovery-*")
	if err != nil {
		return errors.New("无法保存打印机发现结果。")
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
		return errors.New("无法保存打印机发现结果。")
	}
	if ctx.Err() != nil {
		return errors.New("打印机扫描已取消。")
	}
	if os.Rename(f.Name(), s.path) != nil {
		return errors.New("无法保存打印机发现结果。")
	}
	return nil
}
