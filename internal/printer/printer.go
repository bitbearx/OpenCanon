// Package printer 管理经过验证的打印配置和本机 CUPS 队列。
package printer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const Queue = "OpenCanon_E560"

// localCUPS 使用 macOS 域套接字，避免依赖按需运行的 localhost:631 监听。
const localCUPS = "ipp://%2Fprivate%2Fvar%2Frun%2Fcupsd"

// Runner 以参数数组执行外部程序，仅返回标准输出及执行错误。
type Runner interface {
	Run(context.Context, time.Duration, string, ...string) ([]byte, error)
}

// CommandRunner 按规定超时执行系统命令，不将诊断输出用作提交凭据。
type CommandRunner struct {
	cupsURL  string
	cupsHost string
	cupsTLS  bool
}

// NewCommandRunner 配置 CUPS 服务地址；空地址保留本机默认行为。
func NewCommandRunner(address string) (CommandRunner, error) {
	if address == "" {
		return CommandRunner{}, nil
	}
	u, err := url.Parse(address)
	if err != nil || u == nil || (u.Scheme != "ipp" && u.Scheme != "ipps") || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return CommandRunner{}, errors.New("CUPS 地址必须是 ipp:// 或 ipps:// 主机地址")
	}
	return CommandRunner{cupsURL: strings.TrimSuffix(address, "/"), cupsHost: u.Host, cupsTLS: u.Scheme == "ipps"}, nil
}

// cupsServer 返回已配置的 CUPS 地址和提交连接参数。
func (r CommandRunner) cupsServer() (string, string, bool) {
	return r.cupsURL, r.cupsHost, r.cupsTLS
}

// cupsServer 返回运行器的 CUPS 配置，未配置时使用本机默认值。
func cupsServer(r Runner) (string, string, bool) {
	if configured, ok := r.(interface{ cupsServer() (string, string, bool) }); ok {
		if address, host, secure := configured.cupsServer(); address != "" {
			return address, host, secure
		}
	}
	return localCUPS, "", false
}

// ErrNotStarted 表示命令没有启动，因而不会提交任务。
var ErrNotStarted = errors.New("command not started")

// Run 执行命令并限制执行时间。
func (CommandRunner) Run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var output bytes.Buffer
	cmd.Stdout = &output
	// 诊断输出不能充当提交凭据，任务编号只从标准输出读取。
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return output.Bytes(), fmt.Errorf("%w: %v", ErrNotStarted, err)
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		return output.Bytes(), ctx.Err()
	}
	return output.Bytes(), err
}

// Media 描述驱动中的纸张。
type Media struct {
	ID       string            `json:"id"`
	Label    string            `json:"label"`
	WidthMM  float64           `json:"widthMM"`
	HeightMM float64           `json:"heightMM"`
	Options  map[string]string `json:"options"`
}

// Mode 描述颜色或双面模式。
type Mode struct {
	ID      string            `json:"id"`
	Label   string            `json:"label"`
	Options map[string]string `json:"options"`
}

// Constraint 描述禁止的设置组合。
type Constraint struct {
	MediaID   string `json:"mediaId"`
	ColorMode string `json:"colorMode"`
	Duplex    string `json:"duplex"`
}

// Profile 保存真实驱动提供的配置。
type Profile struct {
	Queue       string       `json:"queue"`
	Media       []Media      `json:"media"`
	ColorModes  []Mode       `json:"colorModes"`
	DuplexModes []Mode       `json:"duplexModes"`
	Constraints []Constraint `json:"constraints"`
}

var safeOption = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)

// Load 读取并验证打印配置。
func Load(path string) (*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Profile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&p); err != nil {
		return nil, err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, errors.New("extra JSON")
	}
	if err = p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate 检查配置的标识、尺寸、选项和约束。
func (p *Profile) Validate() error {
	if p.Queue != Queue || len(p.Media) == 0 || len(p.ColorModes) == 0 || len(p.DuplexModes) == 0 {
		return errors.New("invalid profile")
	}
	media := map[string]bool{}
	colors := map[string]bool{}
	duplex := map[string]bool{}
	validOptions := func(m map[string]string) bool {
		for k, v := range m {
			if !safeOption.MatchString(k) || !safeOption.MatchString(v) || strings.EqualFold(k, "number-up") || strings.EqualFold(k, "page-ranges") || strings.EqualFold(k, "fit-to-page") || strings.EqualFold(k, "collate") {
				return false
			}
		}
		return true
	}
	for _, m := range p.Media {
		if !safeOption.MatchString(m.ID) || media[m.ID] || m.Label == "" || math.IsNaN(m.WidthMM) || math.IsInf(m.WidthMM, 0) || m.WidthMM <= 0 || math.IsNaN(m.HeightMM) || math.IsInf(m.HeightMM, 0) || m.HeightMM <= 0 || !validOptions(m.Options) {
			return errors.New("invalid media")
		}
		media[m.ID] = true
	}
	for _, m := range p.ColorModes {
		if (m.ID != "color" && m.ID != "mono") || colors[m.ID] || m.Label == "" || !validOptions(m.Options) {
			return errors.New("invalid color")
		}
		colors[m.ID] = true
	}
	for _, m := range p.DuplexModes {
		if (m.ID != "single" && m.ID != "long" && m.ID != "short") || duplex[m.ID] || m.Label == "" || !validOptions(m.Options) {
			return errors.New("invalid duplex")
		}
		duplex[m.ID] = true
	}
	if !duplex["single"] {
		return errors.New("single missing")
	}
	for _, c := range p.Constraints {
		if !media[c.MediaID] || !colors[c.ColorMode] || !duplex[c.Duplex] {
			return errors.New("invalid constraint")
		}
	}
	return nil
}

// Options 验证组合并合并驱动选项。
func (p *Profile) Options(mediaID, color, duplex string) ([]string, error) {
	var sets []map[string]string
	for _, m := range p.Media {
		if m.ID == mediaID {
			sets = append(sets, m.Options)
		}
	}
	for _, m := range p.ColorModes {
		if m.ID == color {
			sets = append(sets, m.Options)
		}
	}
	for _, m := range p.DuplexModes {
		if m.ID == duplex {
			sets = append(sets, m.Options)
		}
	}
	if len(sets) != 3 {
		return nil, errors.New("invalid selection")
	}
	for _, c := range p.Constraints {
		if c.MediaID == mediaID && c.ColorMode == color && c.Duplex == duplex {
			return nil, errors.New("forbidden combination")
		}
	}
	merged := map[string]string{}
	for _, set := range sets {
		for k, v := range set {
			if old, ok := merged[k]; ok && old != v {
				return nil, errors.New("conflicting options")
			}
			merged[k] = v
		}
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var result []string
	for _, k := range keys {
		result = append(result, "-o", k+"="+merged[k])
	}
	return result, nil
}

// Available 检查队列是否存在且接受任务。
func Available(ctx context.Context, r Runner) (bool, string) {
	address, _, _ := cupsServer(r)
	out, err := r.Run(ctx, 15*time.Second, "/usr/bin/ipptool", "-t", "-v", address+"/printers/"+Queue, "/usr/share/cups/ipptool/get-printer-attributes.test")
	if err != nil {
		return false, "无法查询打印队列，请检查 CUPS 和打印机配置。"
	}
	accept, state := "", ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "printer-is-accepting-jobs (boolean) = ") {
			if accept != "" {
				return false, "无法确认打印队列状态，请检查系统打印设置。"
			}
			accept = strings.TrimSpace(strings.TrimPrefix(line, "printer-is-accepting-jobs (boolean) = "))
		}
		if strings.HasPrefix(line, "printer-state (enum) = ") {
			if state != "" {
				return false, "无法确认打印队列状态，请检查系统打印设置。"
			}
			state = strings.TrimSpace(strings.TrimPrefix(line, "printer-state (enum) = "))
		}
	}
	if accept != "true" || (state != "idle" && state != "processing") {
		return false, "打印队列未接受任务或已暂停，请检查系统打印设置。"
	}
	return true, ""
}

var jobPattern = regexp.MustCompile(`^(?:request id is (` + regexp.QuoteMeta(Queue) + `-[0-9]+) \([0-9]+ file\(s\)\)|请求id是(` + regexp.QuoteMeta(Queue) + `-[0-9]+)（[0-9]+个文件）)$`)

// Submit 将 PDF 提交到固定队列并校验返回的任务编号。
func Submit(ctx context.Context, r Runner, path string, copies int, ranges string, opts []string) (string, error) {
	args := []string{"-d", Queue, "-n", strconv.Itoa(copies), "-o", "collate=true", "-o", "fit-to-page", "-o", "number-up=1", "-o", "page-ranges=" + ranges}
	_, host, secure := cupsServer(r)
	if host != "" {
		args = append([]string{"-h", host}, args...)
		if secure {
			args = append([]string{"-E"}, args...)
		}
	}
	args = append(args, opts...)
	args = append(args, path)
	out, err := r.Run(ctx, 15*time.Second, "/usr/bin/lp", args...)
	if matches := jobPattern.FindStringSubmatch(strings.TrimSpace(string(out))); matches != nil {
		if matches[1] != "" {
			return matches[1], nil
		}
		return matches[2], nil
	}
	if errors.Is(err, ErrNotStarted) {
		return "", err
	}
	return "", ErrUncertain
}

// ErrUncertain 表示无法确定打印任务是否已提交。
var ErrUncertain = errors.New("submission uncertain")

// JobStatus 查询已提交任务；消失时不推断完成。
func JobStatus(ctx context.Context, r Runner, id string) (string, string) {
	unknown := "无法确认任务状态，请检查系统打印队列。"
	prefix := Queue + "-"
	if !strings.HasPrefix(id, prefix) {
		return "unknown", unknown
	}
	number := strings.TrimPrefix(id, prefix)
	if number == "" {
		return "unknown", unknown
	}
	for _, c := range number {
		if c < '0' || c > '9' {
			return "unknown", unknown
		}
	}
	address, _, _ := cupsServer(r)
	out, err := r.Run(ctx, 15*time.Second, "/usr/bin/ipptool", "-t", "-v", address+"/jobs/"+number, "/usr/share/cups/ipptool/get-job-attributes.test")
	if err != nil {
		return "unknown", unknown
	}
	jobID, state := "", ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "job-id (integer) = ") {
			if jobID != "" {
				return "unknown", unknown
			}
			jobID = strings.TrimSpace(strings.TrimPrefix(line, "job-id (integer) = "))
		}
		if strings.HasPrefix(line, "job-state (enum) = ") {
			if state != "" {
				return "unknown", unknown
			}
			state = strings.TrimSpace(strings.TrimPrefix(line, "job-state (enum) = "))
		}
	}
	if jobID != number {
		return "unknown", unknown
	}
	switch state {
	case "pending", "pending-held", "3", "4":
		return "waiting", ""
	case "processing", "5":
		return "processing", ""
	case "canceled", "aborted", "7", "8":
		return "failed", "打印任务失败，请检查系统打印队列。"
	default:
		return "unknown", unknown
	}
}

// Sheets 计算逐份打印所需的纸张数量。
func Sheets(pages, copies int, duplex string) int {
	if duplex != "single" {
		pages = (pages + 1) / 2
	}
	return pages * copies
}

// FormatError 返回不会暴露内部细节的配置错误。
func FormatError(err error) string {
	return fmt.Sprintf("打印配置不可用，请检查 config/printer.json（%T）。", err)
}
