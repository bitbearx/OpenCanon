package printer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// resultRunner 提供本机命令的可控输出。
type resultRunner struct {
	output string
	err    error
}

// Run 返回指定的命令结果。
func (r resultRunner) Run(context.Context, time.Duration, string, ...string) ([]byte, error) {
	return []byte(r.output), r.err
}

// TestAvailableIPP 验证不受本地化影响的 IPP 队列状态。
func TestAvailableIPP(t *testing.T) {
	tests := []struct {
		name, output string
		err          error
		available    bool
	}{
		{"idle", "printer-is-accepting-jobs (boolean) = true\nprinter-state (enum) = idle\n", nil, true},
		{"processing", "printer-is-accepting-jobs (boolean) = true\nprinter-state (enum) = processing\n", nil, true},
		{"rejected", "printer-is-accepting-jobs (boolean) = false\nprinter-state (enum) = idle\n", nil, false},
		{"stopped", "printer-is-accepting-jobs (boolean) = true\nprinter-state (enum) = stopped\n", nil, false},
		{"missing", "printer-is-accepting-jobs (boolean) = true\n", nil, false},
		{"duplicate", "printer-is-accepting-jobs (boolean) = true\nprinter-is-accepting-jobs (boolean) = false\nprinter-state (enum) = idle\n", nil, false},
		{"failure", "printer-is-accepting-jobs (boolean) = true\nprinter-state (enum) = idle\n", errors.New("failed"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := Available(context.Background(), resultRunner{tt.output, tt.err})
			if got != tt.available {
				t.Fatalf("available=%v", got)
			}
		})
	}
}

// TestSubmitLocalized 验证本地化输出和模糊结果的处理。
func TestSubmitLocalized(t *testing.T) {
	tests := []struct {
		output  string
		success bool
	}{{"请求id是" + Queue + "-123（1个文件）\n", true}, {"request id is " + Queue + "-7 (1 file(s))", true}, {"成功但没有编号", false}, {Queue + "-1234xyz", false}, {Queue + "-12 " + Queue + "-13", false}, {"lp: unable to submit " + Queue + "-99", false}}
	for _, tt := range tests {
		id, err := Submit(context.Background(), resultRunner{output: tt.output}, "/tmp/document.pdf", 1, "1", nil)
		if (err == nil) != tt.success {
			t.Errorf("%q: id=%q err=%v", tt.output, id, err)
		}
		if !tt.success && !errors.Is(err, ErrUncertain) {
			t.Errorf("expected uncertain: %v", err)
		}
	}
}

// TestJobStatusLocalized 根据 IPP 属性判断真实任务状态。
func TestJobStatusLocalized(t *testing.T) {
	id := Queue + "-12"
	tests := []struct{ out, want string }{
		{"job-id (integer) = 12\njob-state (enum) = pending\n", "waiting"},
		{"job-id (integer) = 12\njob-state (enum) = processing\n", "processing"},
		{"job-id (integer) = 12\njob-state (enum) = aborted\n", "failed"},
		{"job-id (integer) = 12\njob-state (enum) = completed\n", "unknown"},
		{"job-id (integer) = 123\njob-state (enum) = processing\n", "unknown"},
		{"job-originating-user-name (nameWithoutLanguage) = processing\n", "unknown"},
	}
	for _, tt := range tests {
		got, _ := JobStatus(context.Background(), resultRunner{output: tt.out}, id)
		if got != tt.want {
			t.Errorf("%s: got %s want %s", strings.TrimSpace(tt.out), got, tt.want)
		}
	}
}

// TestSubmitFailures 验证异常退出不能造成重复提交。
func TestSubmitFailures(t *testing.T) {
	tests := []struct {
		out                string
		failure            error
		success, retryable bool
	}{
		{"request id is " + Queue + "-42 (1 file(s))", errors.New("exit status 1"), true, false},
		{"lp: unable to submit " + Queue + "-42", errors.New("exit status 1"), false, false},
		{"", ErrNotStarted, false, true},
		{"", errors.New("signal killed"), false, false},
		{"", context.DeadlineExceeded, false, false},
	}
	for _, tt := range tests {
		_, err := Submit(context.Background(), resultRunner{tt.out, tt.failure}, "/tmp/document.pdf", 1, "1", nil)
		if (err == nil) != tt.success {
			t.Errorf("err=%v", err)
		}
		if !tt.success && (errors.Is(err, ErrNotStarted) != tt.retryable) {
			t.Errorf("err=%v", err)
		}
		if !tt.success && !tt.retryable && !errors.Is(err, ErrUncertain) {
			t.Errorf("err=%v", err)
		}
	}
}
