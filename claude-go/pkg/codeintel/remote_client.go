// remote_client.go — 把 codeintel 查询转发到远端 MCP 服务。
//
// 为什么需要这一层：内置工具（code_intel_query / code_intel_status）在本进程内
// 通过 `gitnexus` / `graphify` 两个外部 CLI 查询，而 **worker Pod 的镜像里没有这两个
// CLI**（实测 `command -v gitnexus` 为空）。结果是 agent 在 Pod 里调内置工具时，
// gitnexus 这一路必然 `gitnexus CLI not found`，Engine 静默降级到 Native 的
// `rg` 文本搜索 —— 拿到的是"文本命中"，不是调用图。agent 观察到的现象是
// "工具未挂载"，于是在 BRIEF 允许下改走手搓 JSON-RPC，而手搓又把 `symbol`
// 拼成 `query`，服务端当时对空目标不报错，返回全仓 21100 行。
//
// 这一层把"本进程无 CLI"变成"转发到有索引的宿主服务"，与手搓 JSON-RPC 相比：
//   - 参数由结构体序列化，拼不出错键名；
//   - 远端地址来自配置/环境变量，不需要 agent 知道 Pod 网络拓扑。
package codeintel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// RemoteCandidates 返回可尝试的远端 codeintel 地址（按顺序）。
//
// 顺序：显式环境变量 → worker 内的服务名 → 宿主本地。worker Pod 里
// `codeintel-mcp` 由 selector-less Service + EndpointSlice 指向宿主；宿主上该
// DNS 不存在，因此必须留 127.0.0.1 兜底。
func RemoteCandidates() []string {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_CODEINTEL_REMOTE")); v != "" {
		return []string{strings.TrimRight(v, "/")}
	}
	return []string{"http://codeintel-mcp:9235", "http://127.0.0.1:9235"}
}

// RemoteToolCall 调用远端 MCP 的 tools/call，返回 result.content[0].text。
// isError=true 表示远端明确报错（内容仍是可读的错误文本）。
func RemoteToolCall(name string, args map[string]interface{}, timeout time.Duration) (text string, isError bool, err error) {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": name, "arguments": args},
	})
	if err != nil {
		return "", false, err
	}
	client := &http.Client{Timeout: timeout}
	var lastErr error
	for _, base := range RemoteCandidates() {
		req, rerr := http.NewRequest(http.MethodPost, base+"/mcp", bytes.NewReader(body))
		if rerr != nil {
			lastErr = rerr
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, derr := client.Do(req)
		if derr != nil {
			lastErr = fmt.Errorf("%s: %w", base, derr)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("%s: HTTP %d: %s", base, resp.StatusCode, strings.TrimSpace(string(raw))[:min(200, len(raw))])
			continue
		}
		var envelope struct {
			Error  *struct{ Message string `json:"message"` } `json:"error"`
			Result struct {
				Content []struct{ Text string `json:"text"` } `json:"content"`
				IsError bool                                   `json:"isError"`
			} `json:"result"`
		}
		if jerr := json.Unmarshal(raw, &envelope); jerr != nil {
			// SSE 形态：取最后一行 data: 的 JSON。
			if t, ok := extractSSEText(string(raw)); ok {
				return t, false, nil
			}
			lastErr = fmt.Errorf("%s: 非 JSON 响应: %s", base, strings.TrimSpace(string(raw))[:min(200, len(raw))])
			continue
		}
		if envelope.Error != nil {
			return envelope.Error.Message, true, nil
		}
		if len(envelope.Result.Content) > 0 {
			return envelope.Result.Content[0].Text, envelope.Result.IsError, nil
		}
		return "", false, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no remote codeintel endpoint configured")
	}
	return "", false, lastErr
}

// extractSSEText 从 SSE 帧里取最后一个 data: 的 JSON 文本。
func extractSSEText(raw string) (string, bool) {
	var last string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if strings.HasPrefix(payload, "{") {
			last = payload
		}
	}
	if last == "" {
		return "", false
	}
	var envelope struct {
		Result struct {
			Content []struct{ Text string `json:"text"` } `json:"content"`
			IsError bool                                   `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(last), &envelope); err != nil {
		return last, true
	}
	if len(envelope.Result.Content) > 0 {
		return envelope.Result.Content[0].Text, true
	}
	return last, true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
