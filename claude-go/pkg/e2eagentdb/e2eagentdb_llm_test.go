// 三真回环测试: 真实 claude-go 二进制 + 真实 agentDB serve + 真实 LLM (ollama)。
//
// 与 TestAgentDBDeployedE2E (库级直连) 的本质区别: 本测试让**部署后的真实
// claude-go 二进制**经 --agentdb-url 装配 AgentDBSemanticInjectHook, 走真实引擎
// 的 hook 链 (engine.go 只认 preRequestResult.SystemPrompt 替换), 在 claude-go 与
// ollama 之间插一个**记录代理**捕获真实 HTTP 请求体, 证明:
//   1. 真实引擎在首个回合 (TurnCount==0) 确实经 serve 检索到 agentDB 记忆/文件;
//   2. 注入块 <memory_context source="agentdb"> 真实出现在发往 LLM 的请求体里
//      (HTTP 层可观测, 而非库级构造 HookContext 的单元断言);
//   3. 真实 LLM (lfm2.5) 消费了注入的记忆, 最终回答引用其中的独特令牌。
//
// 前置 (未设置则跳过):
//   export AGENTDB_E2E_BIN=/tmp/agentdb-e2e/agentdb
//   export CLAUDE_GO_E2E_BIN=/tmp/agentdb-e2e/claude-go
//   export OLLAMA_BASE_URL=http://127.0.0.1:11434/v1   (可选, 默认值即此)
package e2eagentdb_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agentdbsemantic"
)

const (
	claudeGoBinEnv = "CLAUDE_GO_E2E_BIN"
	ollamaBaseEnv  = "OLLAMA_BASE_URL"
	defaultOllama  = "http://127.0.0.1:11434/v1"
	// 独特令牌: 只在注入记忆/文件里出现, 绝不可能是模型编造。
	memToken = "AUTH-7719-KEY-OPEN"
	fileToken = "OPS-8842-FILE"
	// 请求体是 JSON, Go 序列化把 < > 写成 < >, " 写成 \"。
	// 注入块在 wire 上的形态即 <memory_context source=, 按此匹配。
	wireMemCtxMarker = `\u003cmemory_context source=`
)

// recordProxy 是 claude-go → ollama 之间的透明记录代理: 记录每个请求体,
// 原样转发 (SSE 流式透传), 供测试断言"注入块真实出现在发往 LLM 的请求里"。
type recordProxy struct {
	srv     *httptest.Server
	target  string // ollama 基址, 如 http://127.0.0.1:11434/v1
	mu      sync.Mutex
	bodies  [][]byte
}

func newRecordProxy(t *testing.T, target string) *recordProxy {
	t.Helper()
	p := &recordProxy{target: strings.TrimRight(target, "/")}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.bodies = append(p.bodies, append([]byte(nil), body...))
		p.mu.Unlock()

		// 透明转发到 ollama, 保持路径与 header (claude-go 发 POST {base}/messages)。
		u := p.target + r.URL.Path
		fwd, err := http.NewRequestWithContext(r.Context(), r.Method, u, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fwd.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(fwd)
		if err != nil {
			http.Error(w, fmt.Sprintf("转发 ollama 失败: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body) // SSE 流式透传
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// captured 返回所有已记录请求体的只读拷贝。
func (p *recordProxy) captured() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.bodies))
	copy(out, p.bodies)
	return out
}

// probeOllama 探测 ollama 是否可达 (Anthropic /v1/messages 端点)。
func probeOllama(base string) error {
	u := strings.TrimRight(base, "/") + "/messages"
	body := `{"model":"lfm2.5:2.6b-q4_k_m","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("ollama 返回 %d", resp.StatusCode)
}

// writeOllamaCfg 生成指向记录代理的 providers 配置 (baseUrl=代理地址, 模型经
// providers 解析保留真实 ollama 冒号名)。真实模型名 `lfm2.5:2.6b-q4_k_m` 自带冒号,
// 故 provider 名取 `ollama` 而非 `lfm2.5`, 避免 splitAlias 首冒号截断。
func writeOllamaCfg(t *testing.T, dir, proxyURL string) string {
	t.Helper()
	cfg := map[string]any{
		"providers": map[string]any{
			"ollama": map[string]any{
				"name":    "ollama",
				"baseUrl": proxyURL,
				"apiKey":  "ollama",
				"models": map[string]any{
					"ollama:lfm2.5:2.6b-q4_k_m": map[string]any{"maxTokens": 8192},
				},
			},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAgentDBLLMRoundtrip 三真回环: 真实 claude-go 二进制 + 真实 serve + 真实 LLM。
func TestAgentDBLLMRoundtrip(t *testing.T) {
	agentdbBin := os.Getenv(e2eBinEnv)
	if agentdbBin == "" {
		t.Skip("AGENTDB_E2E_BIN 未设置, 跳过三真回环 (先 build agentdb 二进制)")
	}
	claudeBin := os.Getenv(claudeGoBinEnv)
	if claudeBin == "" {
		t.Skip("CLAUDE_GO_E2E_BIN 未设置, 跳过三真回环 (先 build claude-go 二进制)")
	}
	ollamaBase := os.Getenv(ollamaBaseEnv)
	if ollamaBase == "" {
		ollamaBase = defaultOllama
	}
	if err := probeOllama(ollamaBase); err != nil {
		t.Skipf("ollama 不可达 (%v), 跳过三真回环; 设置 OLLAMA_BASE_URL 可指定", err)
	}

	ctx := context.Background()

	// 1. 真实 agentDB serve (minilm 真实语义嵌入)
	srv := launchServe(t, agentdbBin, t.TempDir(), freePort(t))
	t.Cleanup(func() { srv.stop(t) })
	sem := agentdbsemantic.New(srv.url)

	// 2. 预置独特记忆 + 文件 (两个独特令牌只存在于 agentDB 中)
	if _, err := sem.StoreMemory(ctx, agentdbsemantic.Memory{
		Content: fmt.Sprintf("部署 agentDB 集群使用的认证令牌是 %s，只有运维团队知晓。", memToken),
		Type:    agentdbsemantic.MemoryProcedural,
	}); err != nil {
		t.Fatalf("StoreMemory: %v", err)
	}
	if _, err := sem.IndexFile(ctx, "deploy-note.md", []byte(fmt.Sprintf(
		"agentDB 集群 部署 负责人 是 %s\n认证 令牌 见 记忆", fileToken))); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}

	// 3. 记录代理: claude-go → 代理(记录) → ollama
	proxy := newRecordProxy(t, ollamaBase)

	// 4. 真实 claude-go 二进制: --agentdb-url 装配语义注入, base-url 指向代理
	dir := t.TempDir()
	cfgPath := writeOllamaCfg(t, dir, proxy.srv.URL)
	prompt := "根据记忆检索：部署 agentDB 集群使用的认证令牌是什么？根据文件检索：部署的负责人是谁？请只回答令牌和负责人，不要调用任何工具。"
	runCtx, cancel := context.WithTimeout(ctx, 240*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, claudeBin, "run", "-p", "--final-only",
		"--config", cfgPath,
		"--agentdb-url", srv.url,
		"--model", "ollama:lfm2.5:2.6b-q4_k_m",
		"--max-turns", "2",
		prompt,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Dir = dir // 子进程 cwd 指向临时目录, 避免 metrics/statestore 污染仓库
	runErr := cmd.Run()
	_ = runErr // 循环被 max-turns 或超时截断也继续断言请求体; 答案缺失单独判定

	// 5. 断言 A: 真实请求体里出现注入块 (HTTP 层可观测, 非库级断言)。
	//    这是三真回环的硬性验收: 真实引擎经 serve 检索到的记忆/文件
	//    必须真实出现在发往 LLM 的请求体里。
	//    注意: 请求体是 JSON, Go 序列化会把 < 写成 < (以及 \", >),
	//    所以匹配 wire 形态 <memory_context source= 而非明文。
	var bodyWithInjection string
	for _, b := range proxy.captured() {
		s := string(b)
		if strings.Contains(s, wireMemCtxMarker) {
			bodyWithInjection = s
			break
		}
	}
	if bodyWithInjection == "" {
		t.Fatalf("发往 LLM 的请求体应包含 <memory_context source=\"agentdb\"> 注入块; 代理共记录 %d 个请求\n--- claude-go 输出 ---\n%s\n--- 捕获请求体 ---\n%s",
			len(proxy.captured()), out.String(), dumpBodies(proxy.captured()))
	}
	if !strings.Contains(bodyWithInjection, memToken) {
		t.Fatalf("注入块应包含记忆独特令牌 %s; 实际注入片段:\n%s", memToken,
			extractMemoryContext(bodyWithInjection))
	}
	if !strings.Contains(bodyWithInjection, fileToken) {
		t.Fatalf("注入块应包含文件独特令牌 %s; 实际注入片段:\n%s", fileToken,
			extractMemoryContext(bodyWithInjection))
	}
	t.Logf("请求体注入验证通过: 记忆 %s + 文件 %s 均进入真实请求", memToken, fileToken)

	// 6. 断言 B: 真实 LLM 消费了注入内容, 回答引用独特令牌。
	//    若循环被 max-turns/超时截断导致无 final, 单独报错 (不掩盖断言 A)。
	answer := out.String()
	if runErr != nil {
		t.Logf("claude-go run 提前结束: %v (请求体断言 A 已通过, 回答断言 B 见下)", runErr)
	}
	if answer == "" {
		t.Fatalf("真实 LLM 无回答输出 (断言 A 已通过): %v", runErr)
	}
	if !strings.Contains(answer, memToken) {
		t.Fatalf("真实 LLM 回答应引用记忆令牌 %s, 实际:\n%s", memToken, answer)
	}
	t.Logf("真实 LLM 消费验证通过: 回答包含记忆令牌 %s", memToken)
}

// dumpBodies 拼接全部捕获请求体便于失败诊断。
func dumpBodies(bodies [][]byte) string {
	var sb strings.Builder
	for i, b := range bodies {
		fmt.Fprintf(&sb, "===== 请求 %d (%d bytes) =====\n%s\n", i, len(b), b)
	}
	return sb.String()
}

// extractMemoryContext 截取请求体中的 <memory_context> 块便于报错展示。
// body 是 JSON wire 形态 (< > 被转义为 < >), 按 wire 标记截取;
// 块内引号仍是 JSON 转义形态, 足够诊断。
func extractMemoryContext(body string) string {
	const wireOpen = `\u003cmemory_context source=`
	const wireClose = `\u003c/memory_context\u003e`
	i := strings.Index(body, wireOpen)
	if i < 0 {
		return "(未找到注入块)"
	}
	j := strings.Index(body[i:], wireClose)
	if j < 0 {
		return body[i:]
	}
	return body[i : i+j+len(wireClose)]
}
