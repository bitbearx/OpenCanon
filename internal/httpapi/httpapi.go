// Package httpapi 提供同源保护的文档和打印接口。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bitbearx/opencanon/internal/discovery"
	"github.com/bitbearx/opencanon/internal/document"
	"github.com/bitbearx/opencanon/internal/printer"
	"github.com/bitbearx/opencanon/internal/store"
	"github.com/bitbearx/opencanon/internal/web"
)

// Server 持有 HTTP 接口所需的运行时依赖。
type Server struct {
	Store        *store.Store
	Processor    *document.Processor
	Runner       printer.Runner
	Profile      *printer.Profile
	ProfileError string
	Discovery    *discovery.Service
}

// Handler 创建包含静态资源和 API 的处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/printer", s.printer)
	mux.HandleFunc("/api/printer/scan", s.scanPrinter)
	mux.HandleFunc("/api/documents", s.documents)
	mux.HandleFunc("/api/documents/", s.preview)
	mux.HandleFunc("/api/print-jobs", s.jobs)
	mux.HandleFunc("/api/print-jobs/", s.job)
	mux.Handle("/", http.FileServer(http.FS(web.FS)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(r, origin) {
				fail(w, 403, "forbidden", "请求来源不受允许。 ")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// sameOrigin 比较浏览器来源与实际请求的协议和主机。
func sameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return u.Scheme == scheme && u.Host == r.Host
}

// respond 输出 JSON 响应。
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail 输出统一的公开错误。
func fail(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// method 限制接口允许的方法。
func method(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method != want {
		w.Header().Set("Allow", want)
		fail(w, 405, "method_not_allowed", "请求方法不受支持。")
		return false
	}
	return true
}

// printer 返回经过验证的配置和队列状态。
func (s *Server) printer(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	state := discovery.State{}
	if s.Discovery != nil {
		state = s.Discovery.Snapshot()
	}
	result := map[string]any{"name": printer.Queue, "address": state.Address, "discovery": state, "available": false, "stateReason": s.ProfileError, "media": []printer.Media{}, "colorModes": []printer.Mode{}, "duplexModes": []printer.Mode{}, "constraints": []printer.Constraint{}}
	if s.Profile != nil {
		result["media"] = s.Profile.Media
		result["colorModes"] = s.Profile.ColorModes
		result["duplexModes"] = s.Profile.DuplexModes
		result["constraints"] = s.Profile.Constraints
		if s.Processor.PDFInfo == "" {
			result["stateReason"] = "请安装 pdfinfo 并重启服务。"
		} else {
			ok, reason := printer.Available(r.Context(), s.Runner)
			result["available"] = ok
			result["stateReason"] = reason
		}
	}
	respond(w, 200, result)
}

// scanPrinter 仅允许无参数的同源请求扫描固定队列。
func (s *Server) scanPrinter(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	if r.Header.Get("X-OpenCanon-Request") != "1" {
		fail(w, 403, "forbidden", "请求缺少必要的安全标识。 ")
		return
	}
	if r.URL.RawQuery != "" || r.ContentLength != 0 {
		fail(w, 400, "invalid_request", "扫描请求不能包含参数或请求体。 ")
		return
	}
	if s.Discovery == nil {
		fail(w, 503, "discovery_unavailable", "打印机发现服务不可用，请检查数据目录并重启服务。 ")
		return
	}
	state, err := s.Discovery.Scan(r.Context())
	if errors.Is(err, discovery.ErrScanInProgress) {
		fail(w, 409, "scan_in_progress", "打印机扫描正在进行。 ")
		return
	}
	if err != nil {
		fail(w, 503, "discovery_unavailable", err.Error())
		return
	}
	respond(w, 200, map[string]any{"discovery": state})
}

// documents 接收单个文件，成功解析后才公开文档 ID。
func (s *Server) documents(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	if r.ContentLength > 21<<20 {
		fail(w, 413, "too_large", "上传文件超过大小限制。")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 21<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, "invalid_multipart", "请上传单个文件。")
		return
	}
	part, err := mr.NextPart()
	if err != nil {
		multipartError(w, err)
		return
	}
	if part.FormName() != "file" || part.FileName() == "" {
		fail(w, 400, "invalid_multipart", "只允许上传一个 file 文件。")
		return
	}
	d, err := s.Store.Create(part.FileName())
	if err != nil {
		fail(w, 500, "storage_error", "无法保存上传文件。")
		return
	}
	keep := false
	defer func() {
		if !keep {
			_ = s.Store.RemoveFiles(d)
		}
	}()
	err = s.Processor.Process(r.Context(), d, part)
	if err != nil {
		uploadError(w, err)
		return
	}
	if _, err = mr.NextPart(); err != io.EOF {
		if err != nil {
			multipartError(w, err)
		} else {
			fail(w, 400, "invalid_multipart", "只允许上传一个 file 文件。")
		}
		return
	}
	if _, err = io.Copy(io.Discard, r.Body); err != nil {
		multipartError(w, err)
		return
	}
	s.Store.Add(d)
	keep = true
	var preview any
	if d.Kind == "image" {
		preview = "/api/documents/" + d.ID + "/preview"
	}
	respond(w, 201, map[string]any{"id": d.ID, "name": d.Name, "kind": d.Kind, "pageCount": d.PageCount, "expiresAt": d.ExpiresAt, "previewUrl": preview})
}

// multipartError 将请求体语法错误映射为客户端错误。
func multipartError(w http.ResponseWriter, err error) {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		fail(w, 413, "too_large", "上传文件超过大小限制。 ")
		return
	}
	fail(w, 400, "invalid_multipart", "上传请求格式无效或已截断。")
}

// uploadError 分类上传错误并避免暴露内部路径。
func uploadError(w http.ResponseWriter, err error) {
	var limit *http.MaxBytesError
	if errors.Is(err, document.ErrTooLarge) || errors.As(err, &limit) {
		fail(w, 413, "too_large", "上传文件超过大小限制。")
		return
	}
	if err == io.EOF {
		fail(w, 400, "invalid_multipart", "请上传单个文件。")
		return
	}
	var input *document.InputError
	if errors.As(err, &input) {
		multipartError(w, err)
		return
	}
	var parse document.ParseError
	if errors.As(err, &parse) {
		fail(w, 422, "invalid_document", parse.Error())
		return
	}
	fail(w, 500, "storage_error", "无法处理上传文件，请检查服务状态。")
}

// preview 返回未过期图像的 JPEG 预览。
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/documents/")
	if !strings.HasSuffix(path, "/preview") {
		fail(w, 404, "not_found", "文档不存在或已过期。")
		return
	}
	id := strings.TrimSuffix(path, "/preview")
	if strings.Contains(id, "/") {
		fail(w, 404, "not_found", "文档不存在或已过期。")
		return
	}
	d := s.Store.Get(id)
	if d == nil {
		fail(w, 404, "not_found", "文档不存在或已过期。")
		return
	}
	d.Mu.Lock()
	defer d.Mu.Unlock()
	if time.Now().After(d.ExpiresAt) || d.Kind != "image" || d.Preview == "" || d.State == "submitted" {
		fail(w, 404, "not_found", "预览不存在或已过期。")
		return
	}
	f, err := os.Open(d.Preview)
	if err != nil {
		fail(w, 404, "not_found", "预览不存在或已过期。")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, f)
}

// printRequest 保存客户端允许提交的设置。
type printRequest struct {
	DocumentID string `json:"documentId"`
	MediaID    string `json:"mediaId"`
	PageRanges string `json:"pageRanges"`
	Copies     int    `json:"copies"`
	ColorMode  string `json:"colorMode"`
	Duplex     string `json:"duplex"`
}

// UnmarshalJSON 拒绝缺失、空值、未知字段和错误类型。
func (p *printRequest) UnmarshalJSON(data []byte) error {
	var fields struct {
		DocumentID *string `json:"documentId"`
		MediaID    *string `json:"mediaId"`
		PageRanges *string `json:"pageRanges"`
		Copies     *int    `json:"copies"`
		ColorMode  *string `json:"colorMode"`
		Duplex     *string `json:"duplex"`
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fields); err != nil {
		return err
	}
	if fields.DocumentID == nil || fields.MediaID == nil || fields.PageRanges == nil || fields.Copies == nil || fields.ColorMode == nil || fields.Duplex == nil {
		return errors.New("missing field")
	}
	*p = printRequest{DocumentID: *fields.DocumentID, MediaID: *fields.MediaID, PageRanges: *fields.PageRanges, Copies: *fields.Copies, ColorMode: *fields.ColorMode, Duplex: *fields.Duplex}
	return nil
}

// jobs 验证设置并在文档锁内完成幂等提交。
func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || typ != "application/json" || r.Header.Get("X-OpenCanon-Request") != "1" {
		fail(w, 415, "invalid_content_type", "请使用 JSON 和指定的请求头。")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req printRequest
	if err = dec.Decode(&req); err != nil {
		fail(w, 400, "invalid_json", "打印请求格式无效。")
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		fail(w, 400, "invalid_json", "打印请求格式无效。")
		return
	}
	d := s.Store.Get(req.DocumentID)
	if d == nil {
		fail(w, 404, "not_found", "文档不存在或已过期。")
		return
	}
	d.Mu.Lock()
	defer d.Mu.Unlock()
	if time.Now().After(d.ExpiresAt) {
		fail(w, 404, "not_found", "文档不存在或已过期。")
		return
	}
	if d.State == "submitted" {
		respond(w, 201, map[string]string{"id": d.JobID, "state": "submitted"})
		return
	}
	if d.State == "uncertain" {
		fail(w, 503, "submission_uncertain", "无法确认任务是否已提交，请检查系统打印队列，勿重复打印。")
		return
	}
	if req.Copies < 1 || req.Copies > 99 {
		fail(w, 422, "invalid_settings", "份数必须为 1 到 99。")
		return
	}
	ranges, pages, err := printer.NormalizeRanges(req.PageRanges, d.PageCount)
	if err != nil {
		fail(w, 422, "invalid_settings", "页码范围无效。")
		return
	}
	if s.Profile == nil || s.Processor.PDFInfo == "" {
		fail(w, 503, "printer_unavailable", "打印配置或 PDF 解析程序不可用，请检查服务配置。")
		return
	}
	opts, err := s.Profile.Options(req.MediaID, req.ColorMode, req.Duplex)
	if err != nil {
		fail(w, 422, "invalid_settings", "打印设置无效或不兼容。")
		return
	}
	_ = printer.Sheets(pages, req.Copies, req.Duplex)
	if ok, reason := printer.Available(r.Context(), s.Runner); !ok {
		fail(w, 503, "printer_unavailable", reason)
		return
	}
	d.State = "submitting"
	id, err := printer.Submit(r.Context(), s.Runner, d.Path, req.Copies, ranges, opts)
	if err != nil {
		if errors.Is(err, printer.ErrUncertain) {
			d.State = "uncertain"
			fail(w, 503, "submission_uncertain", "无法确认任务是否已提交，请检查系统打印队列，勿重复打印。")
		} else {
			d.State = "ready"
			fail(w, 503, "submission_failed", "提交失败，请检查打印队列后重试。")
		}
		return
	}
	d.State = "submitted"
	d.JobID = id
	s.Store.AddJob(d)
	_ = s.Store.RemoveFiles(d)
	respond(w, 201, map[string]string{"id": id, "state": "submitted"})
}

// job 查询本进程已提交任务的状态。
func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/print-jobs/")
	if strings.Contains(id, "/") || !s.Store.HasJob(id) {
		fail(w, 404, "not_found", "任务不存在或已过期。")
		return
	}
	state, message := printer.JobStatus(r.Context(), s.Runner, id)
	respond(w, 200, map[string]string{"id": id, "state": state, "message": message})
}
