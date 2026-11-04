package printer

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"time"
)

// Runner 执行文档检查和图片转换工具，不承担设备通信。
type Runner interface {
	Run(context.Context, time.Duration, string, ...string) ([]byte, error)
}

// CommandRunner 按参数数组和超时限制执行文档工具。
type CommandRunner struct{}

// Run 返回工具标准输出，不使用 shell 解释用户输入。
func (CommandRunner) Run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.Bytes(), ctx.Err()
	}
	return out.Bytes(), err
}
