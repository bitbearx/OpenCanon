// Package document 识别并转换上传的可打印文档。
package document

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/bitbearx/opencanon/internal/printer"
	"github.com/bitbearx/opencanon/internal/store"
)

const MaxFile = 20 << 20

// ParseError 是可以安全返回给客户端的文档解析错误。
type ParseError string

// Error 返回不包含内部路径的错误说明。
func (e ParseError) Error() string { return string(e) }

// InputError 表示读取上传请求体时发生的错误。
type InputError struct{ Err error }

// Error 返回输入读取错误。
func (e *InputError) Error() string { return e.Err.Error() }

// Unwrap 保留底层请求体错误以识别大小限制。
func (e *InputError) Unwrap() error { return e.Err }

// inputReader 为请求体读取错误添加来源信息。
type inputReader struct{ io.Reader }

// Read 区分客户端读取失败和服务端文件写入失败。
func (r inputReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && err != io.EOF {
		return n, &InputError{Err: err}
	}
	return n, err
}

// Processor 限制同时运行的图像转换并验证 PDF。
type Processor struct {
	Runner  printer.Runner
	PDFInfo string
	slots   chan struct{}
}

// New 创建文档处理器。
func New(r printer.Runner, pdfinfo string) *Processor {
	return &Processor{Runner: r, PDFInfo: pdfinfo, slots: make(chan struct{}, 2)}
}

var pagesPattern = regexp.MustCompile(`(?m)^Pages:\s*([0-9]+)\s*$`)
var encryptedPattern = regexp.MustCompile(`(?mi)^Encrypted:\s*(.*)$`)

// PDFPages 使用 pdfinfo 检查页数和打印权限。
func (p *Processor) PDFPages(ctx context.Context, path string) (int, error) {
	if p.PDFInfo == "" {
		return 0, ParseError("PDF 解析程序不可用，请安装 pdfinfo。")
	}
	out, err := p.Runner.Run(ctx, 30*time.Second, p.PDFInfo, path)
	if err != nil {
		return 0, ParseError("文档损坏、受密码保护或无法解析。")
	}
	m := pagesPattern.FindSubmatch(out)
	if m == nil {
		return 0, ParseError("无法读取 PDF 页数。")
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil || n < 1 {
		return 0, ParseError("PDF 页数无效。")
	}
	e := encryptedPattern.FindSubmatch(out)
	if e == nil {
		return 0, ParseError("无法确认 PDF 打印权限。")
	}
	enc := strings.ToLower(string(e[1]))
	if !strings.HasPrefix(enc, "no") && (!strings.HasPrefix(enc, "yes") || !strings.Contains(enc, "print:yes")) {
		return 0, ParseError("PDF 受保护或禁止打印。")
	}
	return n, nil
}

// detect 根据文件签名识别允许的静态文档。
func detect(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := make([]byte, 32)
	n, _ := io.ReadFull(f, h)
	h = h[:n]
	if bytes.HasPrefix(h, []byte("%PDF-")) {
		return "pdf", nil
	}
	if len(h) >= 3 && bytes.Equal(h[:3], []byte{0xff, 0xd8, 0xff}) {
		return "image", nil
	}
	if len(h) >= 8 && bytes.Equal(h[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		if animatedPNG(f) {
			return "", ParseError("不支持动画图片。")
		}
		return "image", nil
	}
	if len(h) >= 12 && string(h[:4]) == "RIFF" && string(h[8:12]) == "WEBP" {
		if animatedWebP(f) {
			return "", ParseError("不支持动画图片。")
		}
		return "image", nil
	}
	if len(h) >= 12 && string(h[4:8]) == "ftyp" {
		return detectHEIF(f)
	}
	return "", ParseError("不支持该文件格式。")
}

// detectHEIF 按 ftyp 长度解析完整品牌并排除动画序列。
func detectHEIF(f *os.File) (string, error) {
	invalid := ParseError("不支持该文件格式或动画图片。")
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	var header [16]byte
	if _, err := io.ReadFull(f, header[:8]); err != nil {
		return "", invalid
	}
	size := uint64(binary.BigEndian.Uint32(header[:4]))
	headerSize := uint64(8)
	if size == 1 {
		if _, err := io.ReadFull(f, header[8:16]); err != nil {
			return "", invalid
		}
		size = binary.BigEndian.Uint64(header[8:16])
		headerSize = 16
	}
	if size < headerSize+8 || size > MaxFile || (size-headerSize-8)%4 != 0 {
		return "", invalid
	}
	brands := make([]byte, int(size-headerSize))
	if _, err := io.ReadFull(f, brands); err != nil {
		return "", invalid
	}
	accepted := false
	for i := 0; i < len(brands); i += 4 {
		if i == 4 {
			continue
		} // minor_version 不是品牌。
		switch string(brands[i : i+4]) {
		case "hevc", "hevx", "msf1", "avis":
			return "", invalid
		case "heic", "heix", "mif1":
			accepted = true
		}
	}
	if !accepted {
		return "", invalid
	}
	return "image", nil
}

// animatedPNG 检测 PNG 中的 APNG 动画控制块。
func animatedPNG(f *os.File) bool {
	_, _ = f.Seek(8, io.SeekStart)
	for {
		var h [8]byte
		if _, err := io.ReadFull(f, h[:]); err != nil {
			return false
		}
		size := binary.BigEndian.Uint32(h[:4])
		if string(h[4:]) == "acTL" {
			return true
		}
		if string(h[4:]) == "IDAT" || size > MaxFile {
			return false
		}
		if _, err := f.Seek(int64(size)+4, io.SeekCurrent); err != nil {
			return false
		}
	}
}

// animatedWebP 检测 WebP 动画块。
func animatedWebP(f *os.File) bool {
	_, _ = f.Seek(12, io.SeekStart)
	for {
		var h [8]byte
		if _, err := io.ReadFull(f, h[:]); err != nil {
			return false
		}
		size := binary.LittleEndian.Uint32(h[4:])
		if string(h[:4]) == "ANIM" || string(h[:4]) == "ANMF" {
			return true
		}
		if string(h[:4]) == "VP8X" && size >= 1 {
			var b [1]byte
			if _, err := io.ReadFull(f, b[:]); err != nil {
				return false
			}
			if b[0]&2 != 0 {
				return true
			}
			_, _ = f.Seek(-1, io.SeekCurrent)
		}
		if size > MaxFile {
			return false
		}
		if _, err := f.Seek(int64(size+(size&1)), io.SeekCurrent); err != nil {
			return false
		}
	}
}

// Process 将受限输入写入私有目录并验证其内容。
func (p *Processor) Process(ctx context.Context, d *store.Document, src io.Reader) error {
	original := filepath.Join(d.Dir, "source")
	f, err := os.OpenFile(original, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, io.LimitReader(inputReader{src}, MaxFile+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n > MaxFile {
		return ErrTooLarge
	}
	kind, err := detect(original)
	if err != nil {
		return err
	}
	d.Kind = kind
	if kind == "pdf" {
		d.Path = original
		d.PageCount, err = p.PDFPages(ctx, original)
		return err
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	pdf := filepath.Join(d.Dir, "print.pdf")
	preview := filepath.Join(d.Dir, "preview.jpg")
	switch runtime.GOOS {
	case "darwin":
		if _, err = p.Runner.Run(ctx, 30*time.Second, "/usr/bin/sips", "-s", "format", "pdf", original, "--out", pdf); err != nil {
			return ParseError("图片无法转换为 PDF。")
		}
	case "linux":
		// Debian bookworm 使用 ImageMagick 生成预览，再用 img2pdf 封装为单页 PDF。
		if _, err = p.Runner.Run(ctx, 30*time.Second, "/usr/bin/convert", original+"[0]", "-auto-orient", "-background", "white", "-alpha", "remove", "-alpha", "off", preview); err != nil {
			return ParseError("图片预览生成失败。")
		}
		if _, err = p.Runner.Run(ctx, 30*time.Second, "/usr/bin/img2pdf", "--output", pdf, preview); err != nil {
			return ParseError("图片无法转换为 PDF。")
		}
	default:
		return ParseError("当前系统不支持图片转换。")
	}
	pages, err := p.PDFPages(ctx, pdf)
	if err != nil {
		return err
	}
	if pages != 1 {
		return ParseError("图片转换结果不是单页。")
	}
	if runtime.GOOS == "darwin" {
		if _, err = p.Runner.Run(ctx, 30*time.Second, "/usr/bin/sips", "-s", "format", "jpeg", original, "--out", preview); err != nil {
			return ParseError("图片预览生成失败。")
		}
	}
	d.Path = pdf
	d.Preview = preview
	d.PageCount = pages
	return nil
}

// ErrTooLarge 表示单个上传文件超过限制。
var ErrTooLarge = fmt.Errorf("文件超过 20 MiB 限制")
