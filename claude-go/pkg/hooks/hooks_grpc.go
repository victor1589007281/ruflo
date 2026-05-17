//go:build linux || freebsd || darwin
// +build linux freebsd darwin

package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// executeGRPCHook 通过命令行调用 grpcurl 执行 gRPC 调用（零额外依赖方案）。
// 若环境需要原生 gRPC 客户端，可后续引入 google.golang.org/grpc 替换本实现。
//
// 配置: URL = gRPC 服务器地址（如 localhost:50051）,
//       GRPCService = 服务完整名（如 my.hook.PolicyService）,
//       GRPCMethod  = 方法名（如 Evaluate）,
//       Command     = grpcurl 额外参数（可选，如 "-plaintext"）。
func (r *Runner) executeGRPCHook(config types.HookConfig, input types.HookInput) (*types.HookOutput, error) {
	addr := strings.TrimSpace(config.URL)
	if addr == "" {
		return nil, fmt.Errorf("grpc hook: empty address")
	}

	service := strings.TrimSpace(config.GRPCService)
	method := strings.TrimSpace(config.GRPCMethod)
	if service == "" || method == "" {
		return nil, fmt.Errorf("grpc hook: empty service or method")
	}

	timeout := r.timeout
	if config.Timeout > 0 {
		timeout = time.Duration(config.Timeout) * time.Millisecond
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("grpc hook: marshal input: %w", err)
	}

	// 优先尝试 grpcurl；若不可用则尝试 HTTP/JSON 回退
	args := []string{"-d", string(payload)}

	// 解析 Command 中的额外参数（如 -plaintext -insecure）
	cmdExtra := strings.TrimSpace(config.Command)
	if cmdExtra != "" {
		args = append(args, strings.Fields(cmdExtra)...)
	}

	args = append(args, addr, service+"."+method)

	// 尝试 grpcurl
	cmd := exec.CommandContext(ctx, "grpcurl", args...)
	out, err := cmd.Output()
	if err != nil {
		// grpcurl 不可用，尝试 HTTP/JSON 回退
		return r.executeGRPCHookFallback(ctx, config, payload)
	}

	body := bytes.TrimSpace(out)
	if len(body) > 0 && body[0] == '{' {
		var hookOutput types.HookOutput
		if err := json.Unmarshal(body, &hookOutput); err == nil {
			return &hookOutput, nil
		}
	}

	return &types.HookOutput{AdditionalContext: string(body)}, nil
}

// executeGRPCHookFallback 当 grpcurl 不可用时，尝试 HTTP/1.1 JSON 回退
//（适用于支持 gRPC-Web 或 HTTP JSON 转发的服务端）。
func (r *Runner) executeGRPCHookFallback(ctx context.Context, config types.HookConfig, payload []byte) (*types.HookOutput, error) {
	addr := strings.TrimSpace(config.URL)
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}

	service := strings.TrimSpace(config.GRPCService)
	method := strings.TrimSpace(config.GRPCMethod)
	endpoint := addr + "/" + service + "/" + method

	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx2, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("grpc hook fallback: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grpc hook fallback: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("grpc hook fallback: read body: %w", err)
	}

	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '{' {
		var hookOutput types.HookOutput
		if err := json.Unmarshal(body, &hookOutput); err == nil {
			return &hookOutput, nil
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("grpc hook fallback: status %s: %s", resp.Status, string(body))
	}

	return nil, nil
}
