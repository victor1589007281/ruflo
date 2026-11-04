// opencode_upstream.go — opencode zen/go 上游的两个共用判定 (协议头与上游识别)。
//
// # 为什么这段逻辑必须共用
//
// 仓里有**两个**会把请求发往 provider 的网关, 它们都要做同一件事:
//
//	cmd/anthropic-gateway  (协议翻译/透传, 把 Anthropic 请求发给上游)
//	pkg/llmgw              (反向代理网关, 按 model 路由后原样透传)
//
// opencode zen/go 对**两者**都强制要求 x-opencode-session, 缺了直接 400
// MissingSessionID。这段判定原先只写在 anthropic-gateway 里 (且判据有误, 见下),
// llmgw 侧完全没有 —— 而部署里 claude-go-gateway 跑的正是 llm-gateway 子命令,
// 于是"控制面 → 网关 → opencode"这条真实链路上**没有任何一跳**补这个头,
// 每一次 LLM 调用都 400。这正是本轮 K8s 集成测试卡住的根因。
//
// 把它放在 pkg/api 而不是复制两份: 两个网关都依赖 pkg/api, 放这里不会引入
// 循环依赖; 而复制一份的代价是"改了一处、另一处忘了", 正是本缺陷的成因。
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// OpencodeSessionHeader 是 opencode zen/go 上游要求的会话头。
//
// 注意它与 SessionHeader (X-Claude-Code-Session-Id) 是**两个头**: 后者是 Claude Code
// 的原生入站头, 前者是 opencode 的出站要求。网关的职责就是把前者映射成后者。
// 两者都叫 "session" 但方向相反, 混用会得到一个看起来对、实际不生效的设置。
const OpencodeSessionHeader = "x-opencode-session"

// IsOpenCodeUpstream 判断某个 provider 是否指向 opencode zen/go 上游。
//
// 判据有两条, 命中任一即是:
//
//	① provider 名是 "opencode" —— 这是**权威判据**, 必须优先;
//	② baseURL 的主机是 opencode.ai (含子域) —— 兜底, 覆盖没写 provider 名的配置。
//
// ① 必须看 **provider 名**而不是模型名。这里踩过一个真实的坑: 调用方传进来的是
// ResolvedConfig.ProviderName, 而那个字段装的是**模型**名 (如
// "deepseek-v4.1-flash"), provider 名在同结构的 Provider 字段里。于是判据恒为假,
// 注入代码存在但从未执行过 —— 探针实测 isOpenCodeUpstream=false 而 baseURL 明明
// 就是 opencode.ai, 才把它挖出来。
//
// 为什么 ① 不能省、只靠 ② 也不行: modelconfig.ApplyGatewayOverride 在进程设了
// CLAUDE_GO_LLM_GATEWAY 时会把 BaseURL **改指网关自己**, 此时主机已经不是
// opencode.ai 了。只有 provider 名还留着真实身份。网关进程通常不设那个变量, 但
// "不设"是部署事实而不是类型不变量 —— 判据不该依赖它。
func IsOpenCodeUpstream(provider, baseURL string) bool {
	if strings.EqualFold(strings.TrimSpace(provider), "opencode") {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}

var (
	opencodeFallbackSeedOnce sync.Once
	opencodeFallbackSeedVal  string
)

// OpencodeSessionID 取本次入站请求对应的 opencode 会话 ID。
//
// 顺序:
//
//  1. 透传入站 X-Claude-Code-Session-Id (Claude Code 原生头, 每对话稳定);
//  2. 否则按 seed 派生**稳定** ID;
//  3. seed 也为空时退化为进程级常量。
//
// 第 2/3 步刻意不是随机 UUID: 会话 ID 在 opencode 侧同时是提示缓存的分片键,
// 每请求换一个等于把缓存命中率钉死在 0。同一段 4.4K token 前缀实测:
// 同会话第二次 cache_read=4224 / input=214, 换会话则 cache_read=0 / input=4438。
// 会话 ID 只影响路由与缓存, 不影响正确性, 所以"内容相同 → 同一会话"严格更优。
func OpencodeSessionID(r *http.Request, seed string) string {
	if r != nil {
		if v := strings.TrimSpace(r.Header.Get(SessionHeader)); v != "" {
			return v
		}
	}
	if seed == "" {
		seed = opencodeFallbackSeed()
	}
	sum := sha256.Sum256([]byte(seed))
	return "gw-" + hex.EncodeToString(sum[:16])
}

// opencodeFallbackSeed 返回进程级稳定种子 (只生成一次)。
func opencodeFallbackSeed() string {
	opencodeFallbackSeedOnce.Do(func() {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			// 拿不到熵时给一个跨进程相同的常量: 会话 ID 退化成"全局一个会话"。
			// 这仍然优于每次请求换 ID (那会让缓存永远不命中), 也不影响正确性。
			opencodeFallbackSeedVal = "opencode-fallback-seed"
			return
		}
		opencodeFallbackSeedVal = hex.EncodeToString(b)
	})
	return opencodeFallbackSeedVal
}
