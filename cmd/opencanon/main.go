// opencanon 启动局域网打印服务。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bitbearx/opencanon/internal/discovery"
	"github.com/bitbearx/opencanon/internal/document"
	"github.com/bitbearx/opencanon/internal/httpapi"
	"github.com/bitbearx/opencanon/internal/printer"
	"github.com/bitbearx/opencanon/internal/render"
	"github.com/bitbearx/opencanon/internal/store"
)

// findPDFInfo 按参数、PATH 和 Homebrew 路径查找解析程序。
func findPDFInfo(preferred string) string {
	candidates := []string{}
	if preferred != "" && filepath.IsAbs(preferred) {
		candidates = append(candidates, preferred)
	}
	if p, err := exec.LookPath("pdfinfo"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			candidates = append(candidates, abs)
		}
	}
	candidates = append(candidates, "/opt/homebrew/bin/pdfinfo")
	for _, p := range candidates {
		if f, err := os.Stat(p); err == nil && !f.IsDir() && f.Mode()&0111 != 0 {
			return p
		}
	}
	return ""
}

// logURLs 输出可能可供局域网访问的服务地址。
func logURLs(addr string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		log.Printf("访问地址：http://%s", net.JoinHostPort(host, port))
		return
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ip, _, err := net.ParseCIDR(a.String())
		if err == nil && !ip.IsLoopback() && ip.To4() != nil {
			log.Printf("局域网地址：http://%s", net.JoinHostPort(ip.String(), port))
		}
	}
}

// main 初始化依赖并在收到信号时关闭服务。
func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal("无法确定用户主目录")
	}
	listen := flag.String("listen", "0.0.0.0:8080", "监听地址")
	config := flag.String("printer-config", "config/printer.json", "打印配置")
	printerAddress := flag.String("printer-address", "", "打印机主机或主机:端口；留空通过 BJNP 广播发现")
	data := flag.String("data-dir", filepath.Join(home, ".local", "share", "opencanon"), "数据目录")
	helper := flag.String("renderer", "opencanon-render", "Gutenprint E560 辅助程序路径")
	pdftoppm := flag.String("pdftoppm", "pdftoppm", "Poppler 页面渲染程序路径")
	probe := flag.Bool("probe", false, "直接查询设备身份和状态后退出，不发送打印数据")
	pdfinfo := flag.String("pdfinfo", "", "pdfinfo 绝对路径")
	flag.Parse()
	if *pdfinfo != "" && !filepath.IsAbs(*pdfinfo) {
		log.Print("pdfinfo 参数必须为绝对路径，将尝试其他位置")
	}
	runner := printer.CommandRunner{}
	discoveryService, err := discovery.New(*data, *printerAddress)
	if err != nil {
		log.Fatal(err)
	}
	if *probe {
		state, err := discoveryService.Scan(context.Background())
		_ = json.NewEncoder(os.Stdout).Encode(state)
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	st, err := store.New(*data)
	if err != nil {
		log.Fatal("无法初始化数据目录")
	}
	p, err := printer.Load(*config)
	reason := ""
	if err != nil {
		reason = "打印配置不可用，请检查 config/printer.json。"
		log.Print(reason)
	}
	device := printer.NewService(context.Background(), discoveryService.Endpoint, render.Renderer{Helper: *helper, PDFToPPM: *pdftoppm})
	processor := document.New(runner, findPDFInfo(*pdfinfo))
	if processor.PDFInfo == "" {
		log.Print("pdfinfo 不可用，请安装后重启服务")
	}
	api := &httpapi.Server{Store: st, Processor: processor, Printer: device, Profile: p, ProfileError: reason, Discovery: discoveryService}
	// 直接渲染和发送可能持续数分钟；请求取消仍会中止设备通信。
	srv := &http.Server{Addr: *listen, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 30 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	done := make(chan struct{})
	go st.RunCleanup(done)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		close(done)
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logURLs(*listen)
	if err = srv.ListenAndServe(); err != nil && !strings.Contains(err.Error(), "Server closed") {
		log.Fatal(err)
	}
}
