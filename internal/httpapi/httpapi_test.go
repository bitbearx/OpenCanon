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
	"github.com/bitbearx/opencanon/internal/store"
)

// fakeRunner 模拟系统工具以验证并发提交行为。
type fakeRunner struct{ calls atomic.Int32 }

// Run 返回队列和提交结果，避免真实打印。
func (r *fakeRunner) Run(_ context.Context, _ time.Duration, name string, args ...string) ([]byte, error) {
	if name == "/usr/bin/ipptool" {
		return []byte("printer-is-accepting-jobs (boolean) = true\nprinter-state (enum) = idle\n"), nil
	}
	if name == "/usr/bin/lp" {
		r.calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		return []byte("request id is OpenCanon_E560-123 (1 file(s))"), nil
	}
	return nil, fmt.Errorf("unexpected command")
}

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
	p := &printer.Profile{Queue: printer.Queue, Media: []printer.Media{{ID: "A4", Label: "A4", WidthMM: 210, HeightMM: 297}}, ColorModes: []printer.Mode{{ID: "color", Label: "彩色"}}, DuplexModes: []printer.Mode{{ID: "single", Label: "单面"}}}
	srv := &Server{Store: st, Runner: runner, Processor: document.New(runner, "pdfinfo"), Profile: p}
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
	if got := runner.calls.Load(); got != 1 {
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
	srv := &Server{Store: st, Runner: runner, Processor: document.New(runner, "pdfinfo")}
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
