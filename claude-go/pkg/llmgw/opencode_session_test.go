package llmgw

// opencode_session_test.go — 钉住 llm-gateway 的出站 opencode 会话头 (K8s 集成阻塞项)。
//
// # 这个文件防的是哪一类错
//
// 不是"逻辑写错了", 而是**"逻辑存在但从未被执行"**。缺陷形态是:
//
//	cmd/anthropic-gateway 有完整的 x-opencode-session 注入;
//	pkg/llmgw (部署里 claude-go-gateway 真正跑的那个子命令) 一行都没有。
//
// 于是一次 K8s 集成测试卡在 400 MissingSessionID 上, 而排查时 grep "x-opencode-session"
// 明明能 grep 到三处 —— 代码在, 只是不在**流量真正经过的那个进程**里。
//
// 所以这里的判据不能是"仓库里有这个字符串", 必须是**假上游真的收到了这个头**。
// 只有站在上游一侧观测, 才能同时覆盖: 判定函数命中、头名拼写正确、注入位置在
// 出站请求上、且没有被后面的 Header.Set 覆盖掉。
//
// # 每条用例都配一个反方向断言
//
// "必须发" 单独存在时, 一个无条件给所有 provider 都发会话头的实现同样能通过。
// 那会污染 kimi/ollama 等非 opencode 上游 (无谓的外部输入进入出站请求)。
// 所以每条正向用例都配一条"非 opencode 上游不得发"的对照。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/api"
)

// recordingUpstream 记录每次请求收到的 opencode 会话头。
//
// saw 保护: httptest handler 在独立 goroutine 上跑, 而断言在主 goroutine ——
// 不加锁时本地 -race 会报数据竞争, 而竞争表现在 CI 上是"偶发"的假失败。
type recordingUpstream struct {
	mu   sync.Mutex
	seen []string
}

func (r *recordingUpstream) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return ""
	}
	return r.seen[len(r.seen)-1]
}

func (r *recordingUpstream) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func newRecordingUpstream(t *testing.T) (*recordingUpstream, *httptest.Server) {
	t.Helper()
	rec := &recordingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen = append(rec.seen, r.Header.Get(api.OpencodeSessionHeader))
		rec.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m1","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

// post 向网关发一次最小 Messages 请求。
func post(t *testing.T, gwURL, model string, hdr map[string]string) {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"ping"}]}`
	req, err := http.NewRequest(http.MethodPost, gwURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// TestGateway_InjectsOpencodeSessionHeader 是本次修复的核心验收。
//
// opencode 路由的出站请求**必须**带 x-opencode-session; 缺了上游就 400
// MissingSessionID, 与模型/凭据/参数无关。这是部署链路里唯一的一跳, 它不补就没人补。
func TestGateway_InjectsOpencodeSessionHeader(t *testing.T) {
	rec, up := newRecordingUpstream(t)
	srv, err := NewServer([]Route{
		{Provider: "opencode", BaseURL: up.URL, APIKey: "sk-oc", Models: []string{"deepseek-v4.1-flash"}},
	}, "opencode", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	post(t, gw.URL, "opencode:deepseek-v4.1-flash", nil)

	if rec.count() != 1 {
		t.Fatalf("上游应收到 1 次请求, 实际 %d", rec.count())
	}
	if got := rec.last(); got == "" {
		t.Fatalf("opencode 上游未收到 %s —— 缺失会被拒 400 MissingSessionID", api.OpencodeSessionHeader)
	}
}

// TestGateway_PassesThroughInboundClaudeSessionID 钉住**透传优先**。
//
// Claude Code CLI 会发 X-Claude-Code-Session-Id (每对话稳定)。网关应把它当作
// 上游的会话 ID, 而不是另生成一个: 否则同一个对话的请求会落到不同会话分片,
// 提示缓存永远命中不了 (实测同前缀 cache_read 4224 → 0)。
func TestGateway_PassesThroughInboundClaudeSessionID(t *testing.T) {
	rec, up := newRecordingUpstream(t)
	srv, err := NewServer([]Route{
		{Provider: "opencode", BaseURL: up.URL, APIKey: "sk-oc", Models: []string{"m"}},
	}, "opencode", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	post(t, gw.URL, "opencode:m", map[string]string{api.SessionHeader: "sess-abc-123"})

	if got := rec.last(); got != "sess-abc-123" {
		t.Fatalf("应透传入站 %s=%q, 上游实际收到 %q", api.SessionHeader, "sess-abc-123", got)
	}
}

// TestGateway_SessionHeaderStableWithoutInbound 钉住"缺头时也不许每请求换"。
//
// 会话 ID 在 opencode 侧是提示缓存的分片键。随机兜底等于把命中率钉死在 0 ——
// 这是**功能可用但性能归零**的那类缺陷, 没有任何报错, 只能靠断言两次取值相等来钉。
func TestGateway_SessionHeaderStableWithoutInbound(t *testing.T) {
	rec, up := newRecordingUpstream(t)
	srv, err := NewServer([]Route{
		{Provider: "opencode", BaseURL: up.URL, APIKey: "sk-oc", Models: []string{"m"}},
	}, "opencode", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	post(t, gw.URL, "opencode:m", nil)
	first := rec.last()
	post(t, gw.URL, "opencode:m", nil)
	second := rec.last()

	if first == "" || second == "" {
		t.Fatalf("两次请求都应带会话头, 实际 %q / %q", first, second)
	}
	if first != second {
		t.Fatalf("缺入站头时会话 ID 必须稳定 (缓存分片键), 实际 %q vs %q", first, second)
	}
}

// TestGateway_NoSessionHeaderForNonOpencodeUpstream 是反方向对照。
//
// 无条件给所有上游发这个头能通过上面全部用例, 但那会把一个 opencode 专有头
// 带进 kimi/ollama 等无关上游。这里钉住"不该发的没发"。
func TestGateway_NoSessionHeaderForNonOpencodeUpstream(t *testing.T) {
	rec, up := newRecordingUpstream(t)
	srv, err := NewServer([]Route{
		{Provider: "kimi", BaseURL: up.URL, APIKey: "sk-kimi", Models: []string{"k3"}},
	}, "kimi", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	post(t, gw.URL, "kimi:k3", nil)

	if rec.count() != 1 {
		t.Fatalf("上游应收到 1 次请求, 实际 %d", rec.count())
	}
	if got := rec.last(); got != "" {
		t.Fatalf("非 opencode 上游不应收到 %s, 实际 %q", api.OpencodeSessionHeader, got)
	}
}

// TestGateway_OpencodeDetectedByBaseURL 钉住**只写 baseURL、不写 provider 名**的形态。
//
// 真实 config.json 里 provider 名与 baseURL 都有, 所以单靠 provider 名也能过。
// 但把判据写成"只看 provider 名"会在 provider 起别名 (如 "oc") 时静默失效,
// 而 baseURL 是配置里几乎必然会写的那一项。
func TestGateway_OpencodeDetectedByBaseURL(t *testing.T) {
	// 用 127.0.0.1 的 httptest 无法伪装 opencode.ai 主机名, 所以这一条直接测
	// 判定函数本身。它与上游观测缺一不可: 只有判定函数绿时, 注入点仍可能传错
	// 参数 (本轮的原始缺陷正是如此); 只有上游观测时, 无法覆盖主机名分支。
	if !api.IsOpenCodeUpstream("some-other-name", "https://opencode.ai/zen/go/v1") {
		t.Fatal("baseURL 指向 opencode.ai 时应识别为 opencode 上游")
	}
}

// TestGateway_StripsProviderPrefixRegardlessOfJSONSpacing 钉住出站模型名剥壳的**格式无关性**。
//
// # 缺陷形态
//
// 原先的实现是 bytes.Replace(`"model":"<alias>"`, `"model":"<bare>"`) —— 一次字节精确
// 替换。发布方只要写成 `"model": "opencode:x"` (冒号后一个空格, Python json.dumps 的
// 默认输出) 就匹配不上, 替换**静默不发生**, 整个别名被发给上游。
//
// 上游的回复是 "Model is unavailable" —— 读起来像模型下线, 实际是本地字符串没换掉。
// 本轮 K8s 集成测试就是这么卡住的: 同一份请求, 紧凑写法 200, 带空格 400。
//
// 这条测试断言的是**上游收到了什么**, 不是"函数返回值变了": 只有站在上游一侧才能
// 同时覆盖替换逻辑与它在请求构造中的位置。
func TestGateway_StripsProviderPrefixRegardlessOfJSONSpacing(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"紧凑", `{"model":"opencode:m1","messages":[{"role":"user","content":"ping"}]}`},
		{"冒号后有空格", `{"model": "opencode:m1","messages":[{"role":"user","content":"ping"}]}`},
		{"冒号两侧都有空格", `{"model" : "opencode:m1","messages":[{"role":"user","content":"ping"}]}`},
		{"换行缩进", "{\n  \"model\": \"opencode:m1\",\n  \"messages\": [{\"role\": \"user\", \"content\": \"ping\"}]\n}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var mu sync.Mutex
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				var p struct {
					Model string `json:"model"`
				}
				_ = json.Unmarshal(b, &p)
				mu.Lock()
				got = p.Model
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"m1","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer up.Close()

			srv, err := NewServer([]Route{
				{Provider: "opencode", BaseURL: up.URL, APIKey: "sk-oc", Models: []string{"m1"}},
			}, "opencode", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			gw := httptest.NewServer(srv.Handler())
			defer gw.Close()

			resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			mu.Lock()
			seen := got
			mu.Unlock()
			if seen != "m1" {
				t.Fatalf("上游应收到裸模型名 %q, 实际 %q —— 别名泄漏到上游会被拒 "+
					"\"Model is unavailable\" (JSON 格式 %s)", "m1", seen, tc.name)
			}
		})
	}
}
