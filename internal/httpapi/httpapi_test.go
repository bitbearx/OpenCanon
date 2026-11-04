package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitbearx/opencanon/internal/document"
	"github.com/bitbearx/opencanon/internal/printer"
	"github.com/bitbearx/opencanon/internal/render"
	"github.com/bitbearx/opencanon/internal/store"
)

// fakeRunner 拒绝文档工具调用，避免上传失败测试依赖系统命令。
type fakeRunner struct{}

// Run 返回明确失败，不能把无效文件当作可打印文档。
func (r *fakeRunner) Run(context.Context, time.Duration, string, ...string) ([]byte, error) {
	return nil, fmt.Errorf("unexpected document command")
}

// fakePrinter 控制设备提交结果以验证文档防重状态。
type fakePrinter struct {
	calls atomic.Int32
	err   error
}

// Available 模拟设备已就绪。
func (p *fakePrinter) Available(context.Context) (bool, string) { return true, "" }

// Submit 记录真实提交次数并提供确定或不确定结果。
func (p *fakePrinter) Submit(context.Context, string, string, render.Job) (string, error) {
	p.calls.Add(1)
	return "bjnp-test", p.err
}

// JobStatus 保持物理完成未知，避免模拟出纸成功。
func (p *fakePrinter) JobStatus(context.Context, string) (string, string) { return "unknown", "" }

// TestConcurrentSubmission 验证同一文档并发请求只提交一次。
func TestConcurrentSubmission(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.Create("a.pdf")
	if err != nil {
		t.Fatal(err)
	}
	d.Kind = "pdf"
	d.PageCount = 3
	d.Path = filepath.Join(d.Dir, "source")
	if err = os.WriteFile(d.Path, []byte("pdf"), 0600); err != nil {
		t.Fatal(err)
	}
	st.Add(d)
	runner := &fakeRunner{}
	device := &fakePrinter{}
	p := &printer.Profile{Model: "bjc-E560-series", Media: []printer.Media{{ID: "A4", Label: "A4", WidthMM: 210, HeightMM: 297, Type: "plain"}}, ColorModes: []printer.Mode{{ID: "color", Label: "彩色"}}, DuplexModes: []printer.Mode{{ID: "single", Label: "单面"}}}
	srv := &Server{Store: st, Printer: device, Processor: document.New(runner, "pdfinfo"), Profile: p}
	body, _ := json.Marshal(printRequest{DocumentID: d.ID, MediaID: "A4", Copies: 1, ColorMode: "color", Duplex: "single"})
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/api/print-jobs", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-OpenCanon-Request", "1")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusCreated {
				t.Errorf("status %d: %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	if got := device.calls.Load(); got != 1 {
		t.Fatalf("submitted %d times", got)
	}
}

// TestInvalidUploadCleanup 验证解析失败不会保留文档目录。
func TestInvalidUploadCleanup(t *testing.T) {
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	srv := &Server{Store: st, Processor: document.New(runner, "pdfinfo")}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "bad.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("not a document"))
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/api/documents", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 422 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(root, "documents"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("leftovers: %v %v", entries, err)
	}
	if strings.Contains(w.Body.String(), root) {
		t.Fatal("泄露路径")
	}
}

// TestUploadDeclaredSizeLimit 验证过大的已声明请求体在解析前被拒绝。
func TestUploadDeclaredSizeLimit(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Processor: document.New(&fakeRunner{}, "pdfinfo")}
	req := httptest.NewRequest("POST", "/api/documents", strings.NewReader("x"))
	req.ContentLength = 21<<20 + 1
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 413 {
		t.Fatalf("status %d", w.Code)
	}
}

// TestTruncatedMultipart 验证截断的客户端请求返回 400 并清理文件。
func TestTruncatedMultipart(t *testing.T) {
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Processor: document.New(&fakeRunner{}, "pdfinfo")}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "a.pdf")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("%PDF-truncated"))
	req := httptest.NewRequest("POST", "/api/documents", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(root, "documents"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("leftovers: %v %v", entries, err)
	}
}

// TestUncertainSubmissionLocksDocument 验证设备接收结果不明时更改选项也不能再次打印。
func TestUncertainSubmissionLocksDocument(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.Create("page.pdf")
	if err != nil {
		t.Fatal(err)
	}
	d.Kind, d.PageCount, d.Path = "pdf", 1, filepath.Join(d.Dir, "source")
	if err := os.WriteFile(d.Path, []byte("document"), 0600); err != nil {
		t.Fatal(err)
	}
	st.Add(d)
	device := &fakePrinter{err: printer.ErrUncertain}
	p := &printer.Profile{Model: "bjc-E560-series", Media: []printer.Media{{ID: "A4", Label: "A4", WidthMM: 210, HeightMM: 297, Type: "plain"}}, ColorModes: []printer.Mode{{ID: "mono", Label: "灰度"}}, DuplexModes: []printer.Mode{{ID: "single", Label: "单面"}}}
	handler := (&Server{Store: st, Printer: device, Processor: document.New(&fakeRunner{}, "pdfinfo"), Profile: p}).Handler()
	for _, copies := range []int{1, 2} {
		body, _ := json.Marshal(printRequest{DocumentID: d.ID, MediaID: "A4", Copies: copies, ColorMode: "mono", Duplex: "single"})
		req := httptest.NewRequest("POST", "/api/print-jobs", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-OpenCanon-Request", "1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "submission_uncertain") {
			t.Fatalf("response: %d %s", w.Code, w.Body.String())
		}
	}
	if device.calls.Load() != 1 || d.State != "uncertain" {
		t.Fatalf("calls=%d state=%s", device.calls.Load(), d.State)
	}
	if _, err := os.Stat(d.Path); err != nil {
		t.Fatalf("uncertain document removed: %v", err)
	}
}
