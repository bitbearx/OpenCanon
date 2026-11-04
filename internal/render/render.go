// Package render 将选定 PDF 页面转换为精确 E560 型号的设备流。
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxRasterBytes int64 = 64 << 20
	maxJobBytes    int64 = 1 << 30
)

// Renderer 指定独立 Gutenprint 辅助程序与 Poppler 可执行文件。
type Renderer struct {
	Helper   string
	PDFToPPM string
}

// Job 描述已选页面、逐份份数、介质与打印方向。
type Job struct {
	Pages                               []int
	Copies                              int
	Media, MediaType, ColorMode, Duplex string
	WidthMM, HeightMM                   float64
}

// paperSize 保存用户纸张标识的真实毫米尺寸。
type paperSize struct{ width, height float64 }

var paperSizes = map[string]paperSize{
	"A4": {210, 297}, "A5": {148, 210}, "JIS-B5": {182, 257},
	"Letter": {215.9, 279.4}, "Legal": {215.9, 355.6},
	"4x6": {101.6, 152.4}, "5x7": {127, 177.8}, "8x10": {203.2, 254},
	"L": {89, 127}, "Postcard": {100, 148}, "DoublePostcard": {200, 148},
	"Env10": {104.8, 241.3}, "EnvDL": {110, 220}, "EnvYou4": {105, 235}, "EnvYou6": {98, 190},
}

// validate 拒绝无效参数及未经确认的自动双面介质组合。
func (j Job) validate() error {
	if len(j.Pages) == 0 || j.Copies < 1 || j.Copies > 99 {
		return errors.New("render: pages must not be empty and copies must be 1..99")
	}
	last := 0
	for _, page := range j.Pages {
		if page <= last {
			return errors.New("render: page numbers must be positive and strictly ascending")
		}
		last = page
	}
	if j.ColorMode != "color" && j.ColorMode != "mono" {
		return errors.New("render: invalid color mode")
	}
	if j.Duplex != "single" && j.Duplex != "long" && j.Duplex != "short" {
		return errors.New("render: invalid duplex mode")
	}
	switch j.MediaType {
	case "plain", "photo", "postcard", "envelope":
	default:
		return errors.New("render: invalid media type")
	}
	size, ok := paperSizes[j.Media]
	if !ok || math.IsNaN(j.WidthMM) || math.IsNaN(j.HeightMM) || math.IsInf(j.WidthMM, 0) || math.IsInf(j.HeightMM, 0) || math.Abs(j.WidthMM-size.width) > 0.2 || math.Abs(j.HeightMM-size.height) > 0.2 {
		return errors.New("render: unknown paper or mismatched dimensions")
	}
	if j.Duplex != "single" && (j.MediaType != "plain" || (j.Media != "A4" && j.Media != "Letter")) {
		return errors.New("render: E560 automatic duplex requires A4 or Letter plain paper")
	}
	return nil
}

// binaries 为未配置的工具提供标准安装名称。
func (r Renderer) binaries() (string, string) {
	helper, ppm := r.Helper, r.PDFToPPM
	if helper == "" {
		helper = "opencanon-render"
	}
	if ppm == "" {
		ppm = "pdftoppm"
	}
	return helper, ppm
}

// limitedLog 仅保留诊断前缀，避免外部程序耗尽服务内存。
type limitedLog struct{ bytes.Buffer }

// Write 丢弃超出诊断上限的字节，同时继续排空子进程管道。
func (b *limitedLog) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 8192 - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

// run 通过 context 管理单一进程；helper 的 exec 保留相同 PID 和资源限制。
func run(ctx context.Context, output io.Writer, dir, binary string, args ...string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = output
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TMPDIR="+dir)
	cmd.WaitDelay = 2 * time.Second
	var diagnostic limitedLog
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("render: %s: %w", filepath.Base(binary), ctx.Err())
		}
		return fmt.Errorf("render: %s: %w: %s", filepath.Base(binary), err, strings.TrimSpace(diagnostic.String()))
	}
	return nil
}

// Check 确认精确型号及必需参数可用，并检查 Poppler 可执行文件。
func (r Renderer) Check(ctx context.Context) error {
	helper, ppm := r.binaries()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := exec.LookPath(ppm); err != nil {
		return fmt.Errorf("render: pdftoppm: %w", err)
	}
	if err := run(ctx, io.Discard, os.TempDir(), helper, "--check"); err != nil {
		return err
	}
	return run(ctx, io.Discard, os.TempDir(), helper, "--rasterize", ppm, "-v")
}

// Render 逐页生成有限大小 PPM，再让同一个 Gutenprint 作业输出全部逐份页面。
func (r Renderer) Render(ctx context.Context, input string, job Job, dir string) (result string, err error) {
	if err = job.validate(); err != nil {
		return "", err
	}
	input, err = filepath.Abs(input)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(input)
	if err != nil {
		return "", fmt.Errorf("render: input: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 100<<20 {
		return "", errors.New("render: input must be a nonempty regular PDF of at most 100 MiB")
	}
	source, err := os.Open(input)
	if err != nil {
		return "", err
	}
	var magic [5]byte
	_, readErr := io.ReadFull(source, magic[:])
	closeErr := source.Close()
	if readErr != nil || closeErr != nil || string(magic[:]) != "%PDF-" {
		return "", errors.New("render: input is not a PDF")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(dir, "raster-")
	if err != nil {
		return "", fmt.Errorf("render: workspace: %w", err)
	}
	defer os.RemoveAll(work)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if err = r.Check(ctx); err != nil {
		return "", err
	}
	helper, ppm := r.binaries()
	pages := make([]string, 0, len(job.Pages))
	var rasterBytes int64
	for _, page := range job.Pages {
		path := filepath.Join(work, strconv.Itoa(page)+".ppm")
		out, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if createErr != nil {
			return "", createErr
		}
		pageCtx, pageCancel := context.WithTimeout(ctx, 90*time.Second)
		// 省略输出前缀让 pdftoppm 写到 stdout，RLIMIT_FSIZE 因此也约束栅格文件。
		runErr := run(pageCtx, out, work, helper, "--rasterize", ppm, "-f", strconv.Itoa(page), "-l", strconv.Itoa(page), "-singlefile", "-r", "300", "-scale-to", "4200", input)
		pageCancel()
		closeErr := out.Close()
		if runErr != nil {
			return "", runErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return "", statErr
		}
		rasterBytes += info.Size()
		if info.Size() == 0 || info.Size() > maxRasterBytes || rasterBytes > maxJobBytes {
			return "", errors.New("render: raster resource limit exceeded or missing selected page")
		}
		pages = append(pages, path)
	}
	out, err := os.CreateTemp(dir, "canon-*.prn")
	if err != nil {
		return "", err
	}
	path := out.Name()
	defer func() {
		if result == "" {
			_ = os.Remove(path)
		}
	}()
	args := []string{"--render", "--media", job.Media, "--media-type", job.MediaType, "--color", job.ColorMode, "--duplex", job.Duplex, "--copies", strconv.Itoa(job.Copies), "--width-mm", strconv.FormatFloat(job.WidthMM, 'f', -1, 64), "--height-mm", strconv.FormatFloat(job.HeightMM, 'f', -1, 64), "--"}
	args = append(args, pages...)
	err = run(ctx, out, work, helper, args...)
	closeErr = out.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	info, err = os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() == 0 || info.Size() > maxJobBytes {
		return "", errors.New("render: invalid device output size")
	}
	return path, nil
}
