// Package metrics 中的 LLM 指标采集器。
//
// 对于 claude-go 各处 (主进程 / feishu bot / dashboard 内部诊断) 发起的 LLM 调用,
// 通过 api.SetGlobalLLMMetricsHook 把每次调用的 token / 延迟 / 错误码 / 重试等
// 数据以 MetricEvent 形式 append 到 {stateDir}/metrics/llm.jsonl, 方便 dashboard
// 统一展示:
//   - llm_call_count / llm_success_count / llm_error_count
//   - llm_duration_sec (单次耗时), llm_*tokens, llm_retry_count
//   - 按错误原因细分的计数: llm_rate_limit_count / llm_overload_count / ...
package metrics

import (
	"log"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
)

var (
	llmGlobalMu sync.Mutex
	// llmRestoreBG 追踪回放历史事件的后台 goroutine。
	// 它是 fire-and-forget(不拖慢启动), 但那让采集器**不可 join** —— 测试里
	// t.TempDir() 的清理与它竞态, 表现为 "TempDir RemoveAll cleanup: directory not
	// empty" 的间歇性失败, 而报错来自 testing 框架、与被测逻辑看起来毫无关系。
	// 这是本仓记过的**第三类假测试(非密闭)**, 同一形态在 pkg/dreaming 已修过一次。
	llmRestoreBG     sync.WaitGroup
	llmGlobal        *Collector
	llmGlobalDataDir string // 用于检测 stateDir 变化时重新挂载采集器
	aliasResolver    func(string) string
)

// SetAliasResolver 设置模型名到完整 alias 的解析函数。
// 当 resolver 非 nil 时, recordLLMCall 会用它将原始 model 名解析为 provider:model 格式 alias,
// 否则 fallback 到原始 model 名。
func SetAliasResolver(fn func(string) string) {
	llmGlobalMu.Lock()
	defer llmGlobalMu.Unlock()
	aliasResolver = fn
}

// InitGlobalLLMCollector 在指定 stateDir 上启动全局 LLM 指标采集。
//
// 行为:
//   - 首次调用: 创建新的 Collector, 安装全局 hook, 返回。
//   - 相同 stateDir 再次调用: no-op, 返回已有 Collector。
//   - 不同 stateDir 再次调用 (重定位): 关闭旧 Collector 的文件句柄,
//     用新 stateDir 重建 Collector 并替换全局 hook。这对应 CLI 场景:
//     rootCmd.PersistentPreRun 用 os.Getwd() 先占位, 子命令 (dashboard/feishu)
//     解析到真实配置 stateDir 后, 会以精确路径再次调用, 此时应以新路径为准,
//     避免指标写错目录。
//
// 返回当前生效的 Collector, 方便其它模块复用。
func InitGlobalLLMCollector(stateDir string) *Collector {
	// dataDir 以 NewCollector 的拼接方式 (stateDir/metrics) 作为标识,
	// 保证幂等比较时走同一个范式, 不受传入 stateDir 是否带尾部斜杠影响。
	wantDir := filepath.Join(stateDir, "metrics")

	llmGlobalMu.Lock()
	defer llmGlobalMu.Unlock()

	if llmGlobal != nil && llmGlobalDataDir == wantDir {
		return llmGlobal
	}

	relocating := llmGlobal != nil
	oldCollector := llmGlobal

	// 先准备好新 collector, 再原子替换全局指针 + hook,
	// 最后再关闭旧 collector, 避免 in-flight LLM 调用命中已关闭的 fd。
	c := NewCollector(stateDir)
	llmGlobal = c
	llmGlobalDataDir = wantDir
	llmPath := filepath.Join(stateDir, "metrics", "llm.jsonl")
	if relocating {
		log.Printf("[metrics] LLM 指标采集重定位 -> %s", llmPath)
	} else {
		log.Printf("[metrics] LLM 指标采集已初始化: %s", llmPath)
	}
	api.SetGlobalLLMMetricsHook(func(rec api.LLMCallRecord) {
		recordLLMCall(c, rec)
	})

	// 关闭旧 collector 的文件句柄 (只影响旧目录, 新目录已由新 collector 接管)。
	if oldCollector != nil {
		oldCollector.Close()
	}

	// 首次启动时回放 JSONL 历史事件到 Prometheus, 使重启后 counter/histogram 保留累计值。
	if !relocating {
		llmRestoreBG.Add(1)
		go func() {
			defer llmRestoreBG.Done()
			_ = RestorePromFromJSONL(stateDir, 20000)
		}()
	}

	return c
}

// GlobalLLMCollector 返回已初始化的全局采集器 (可能为 nil)。
func GlobalLLMCollector() *Collector {
	llmGlobalMu.Lock()
	defer llmGlobalMu.Unlock()
	return llmGlobal
}

func recordLLMCall(c *Collector, rec api.LLMCallRecord) {
	modelAlias := firstNonEmptyStr(rec.Model, "unknown")
	if aliasResolver != nil {
		if resolved := aliasResolver(rec.Model); resolved != "" {
			modelAlias = resolved
		}
	}
	labels := map[string]string{
		"model":       firstNonEmptyStr(rec.Model, "unknown"),
		"model_alias": modelAlias,
		"status":      rec.Status,
		"source":      firstNonEmptyStr(rec.Source, "unknown"),
		"request":     firstNonEmptyStr(rec.Request, "messages"),
	}
	if rec.Purpose != "" {
		labels["purpose"] = rec.Purpose
	}
	if rec.Workflow != "" {
		labels["workflow"] = rec.Workflow
	}
	if rec.Role != "" {
		labels["role"] = rec.Role
	}
	if rec.StopReason != "" {
		labels["stop_reason"] = rec.StopReason
	}
	if rec.ErrorKind != "" {
		labels["error_kind"] = rec.ErrorKind
	}
	if rec.Stream {
		labels["stream"] = "1"
	}
	if rec.HTTPStatus != 0 {
		labels["http_status"] = intStr(rec.HTTPStatus)
	}
	if rec.InputEstimated {
		// design/02 §1.4 的另一半: 估算值与网关真回的值进的是同一个 llm_input_tokens,
		// 不打标签的话"总量不偏小是靠字符数估算撑起来的"这件事完全不可见 —— 而有人
		// 会拿这个数去和账单对账。只在为 true 时加 (与上面各条同风格), 于是没有估算
		// 发生的部署标签集逐字节不变、既有 Grafana 查询不受影响; 取值只有一个 ("1"),
		// 基数增量上限是 ×2 而不是无界。
		labels["input_estimated"] = "1"
	}

	// 同一次 LLM 调用的所有指标共享同一个时间戳,
	// 确保 dashboard 的 handleLLMStats 能正确按 (ts, model) 分组聚合。
	ts := rec.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	// rec 落 JSONL 时带上 run_id (design/03 §1.3 E0 验收项「llm.jsonl 可按 run_id
	// 聚合」)。**只进事件不进标签** —— run_id 是无界基数, 做成 Prometheus 标签会把
	// llm_* 炸成每次运行一条时间序列, 理由详见 RecordRunAtTime 的注释。
	// rec.RunID 为空 (非团队路径 / 上游未注入) 时 omitempty 生效, llm.jsonl 逐字节
	// 与改造前一致。
	rt := func(name string, value float64) {
		c.RecordRunAtTime("llm", name, value, rec.RunID, labels, ts)
	}

	rt(MLLMCallCount, 1)
	rt(MLLMDurationSec, rec.DurationSec)
	// token 双边记账 (design/02 §1.4): input **不再用 >0 守卫跳过**。
	//
	// ⚠️ 改造前这行注释就已经写着"不再用 >0 守卫", 而守卫仍在 —— 注释与代码相反,
	// 且 design/02 §1.4 早已把它记为"设计承诺没做"。现在真的去掉了。
	//
	// 为什么必须显式记 0 而不是跳过: 跳过会让 llm_input_tokens 的样本数**少于**
	// 调用数, 于是"这次调用的 input 是 0"与"这次调用没有 input 数据"在指标上不可
	// 区分, 而按样本数算的均值会被**系统性抬高**(只有拿到数的那些被计入)。Kimi 类
	// 网关经常不回 input_tokens, 这条路径是常态而非边角。
	//
	// output/cache/total/retry 四项**保留守卫**且这不是不一致: 它们为 0 时是"这次
	// 调用确实没有该项"(没走缓存 / 没重试), 显式记 0 只会给每次调用凭空多出四条
	// 恒零样本, 不带任何信息。input 为 0 才是**信息缺失**这一独立事实。
	rt(MLLMInputTokens, float64(rec.InputTokens))
	if rec.OutputTokens > 0 {
		rt(MLLMOutputTokens, float64(rec.OutputTokens))
	}
	if rec.CacheReadTokens > 0 {
		rt(MLLMCacheReadTokens, float64(rec.CacheReadTokens))
	}
	if rec.CacheCreationTokens > 0 {
		rt(MLLMCacheCreateTokens, float64(rec.CacheCreationTokens))
	}
	if rec.TotalTokens > 0 {
		rt(MLLMTotalTokens, float64(rec.TotalTokens))
	}
	if rec.Retries > 0 {
		rt(MLLMRetryCount, float64(rec.Retries))
	}
	recordPromptComponentMetrics(c, rec, labels, ts)

	switch rec.Status {
	case "success", "retry_success":
		rt(MLLMSuccessCount, 1)
	case "error":
		rt(MLLMErrorCount, 1)
	}

	switch rec.ErrorKind {
	case "rate_limit":
		rt(MLLMRateLimitCount, 1)
	case "overloaded":
		rt(MLLMOverloadCount, 1)
	case "timeout":
		rt(MLLMTimeoutCount, 1)
	case "refusal":
		rt(MLLMRefusalCount, 1)
	case "prompt_too_long":
		rt(MLLMPromptTooLong, 1)
	}

	// 限流 / 熔断器事件
	if rec.GuardWaitSec > 0 {
		rt(MLLMGuardWaitSec, rec.GuardWaitSec)
	}
	if rec.CircuitOpened {
		rt(MLLMCircuitTrips, 1)
	}
	if rec.CircuitBlocked {
		blockedLabels := copyLabels(labels)
		blockedLabels["blocked"] = "1"
		// 这一条用的是另一份 labels, 所以走不了上面的 rt 闭包; run_id 仍要带上 ——
		// 一次调用的事件里有的带 run_id 有的不带, 比全都不带更难排障 (按 run_id 过滤
		// 会**静默漏掉**恰好是熔断那几条, 而那几条正是最该被查到的)。
		c.RecordRunAtTime("llm", MLLMCircuitOpenGauge, 1, rec.RunID, blockedLabels, ts)
	}
}

func recordPromptComponentMetrics(c *Collector, rec api.LLMCallRecord, labels map[string]string, ts time.Time) {
	parts := []struct {
		name  string
		value int
	}{
		{"system_chars", rec.PromptComponents.SystemChars},
		{"tools_schema_chars", rec.PromptComponents.ToolsSchemaChars},
		{"mcp_tools_chars", rec.PromptComponents.MCPToolsChars},
		{"skill_listing_chars", rec.PromptComponents.SkillListingChars},
		{"role_skills_chars", rec.PromptComponents.RoleSkillsChars},
		{"memory_chars", rec.PromptComponents.MemoryChars},
		{"blackboard_chars", rec.PromptComponents.BlackboardChars},
		{"prev_result_chars", rec.PromptComponents.PrevResultChars},
		{"messages_chars", rec.PromptComponents.MessagesChars},
	}
	for _, part := range parts {
		componentLabels := copyLabels(labels)
		componentLabels["component"] = part.name
		// 同带 run_id: 提示词构成是"这次运行为什么烧了这么多 token"的直接证据,
		// 恰恰是最需要按 run_id 归因的一路 (design/03 §1.3)。
		c.RecordRunAtTime("llm", MLLMPromptComponentChars, float64(part.value), rec.RunID, componentLabels, ts)
		c.RecordRunAtTime("llm", MLLMPromptComponentTokens, float64(part.value)/4.0, rec.RunID, componentLabels, ts)
	}

	// F10 表面定价: 启发式分量 (上面两条) 只回答"构成大概长什么样", 回答不了
	// "这次调用里 skill 清单吃掉多少 token"。锚定分量把真实 InputTokens 按
	// 组件启发式占比再分配, 是 llm_prompt_component_tokens 的 usage 锚定孪生
	// (dsh token-meter 的 estimate/usage 双轨)。没有锚点 (InputTokens<=0 且
	// 无估算兜底) 或零组件时不发 —— 拿启发式冒充总量是 dsh projection.ts
	// 明文禁止的呈现方式。
	anchor := rec.InputTokens
	if anchor > 0 {
		meas := api.AllocatePromptSurface(rec.PromptComponents, anchor, rec.InputEstimated)
		if len(meas.Surfaces) > 0 {
			// logRevision (规格 F10): 每条定价事件都携带规则版本, 历史样本与新样本
			// 混在同一文件里时口径可辨。它不进 Prometheus 标签 (不在 llmComponentNeed
			// 里, fillLabels 会滤掉) —— series 身份保持与启发式孪生一致, 版本只随
			// JSONL 事件走。
			surfaceLabels := copyLabels(labels)
			surfaceLabels["pricing_rev"] = strconv.FormatInt(api.TokenPricingRevision, 10)
			for _, node := range meas.Surfaces {
				sl := copyLabels(surfaceLabels)
				sl["component"] = node.Component
				c.RecordRunAtTime("llm", MLLMPromptSurfaceTokens, float64(node.Tokens), rec.RunID, sl, ts)
			}
		}
	}
}

// GuardSample RateLimitGuard 的瞬时采样 (由调用方从 api.GuardSnapshot 映射)。
// 避免 metrics 反向依赖 api 包。
type GuardSample struct {
	InFlight         float64
	MaxParallel      float64
	RPMAvailable     float64
	PauseSecondsLeft float64
}

// CircuitSample 熔断器瞬时采样。
type CircuitSample struct {
	Open             bool
	ConsecutiveFails float64
}

// SampleGuardSnapshot 周期性写入 RateLimitGuard + 熔断器的 gauge 快照。
// dashboard / daemon 应每 5-10s 调一次, 让时序图能展现实时并发 / RPM 剩余 / 熔断状态。
// 两个参数任意一个为 nil 时对应字段跳过。
func SampleGuardSnapshot(module string, g *GuardSample, cb *CircuitSample) {
	c := GlobalLLMCollector()
	if c == nil {
		return
	}
	labels := map[string]string{"module": firstNonEmptyStr(module, "unknown"), "kind": "sample"}
	if g != nil {
		c.RecordWithLabels("llm", MLLMGuardInFlight, g.InFlight, labels)
		c.RecordWithLabels("llm", MLLMGuardMaxParallel, g.MaxParallel, labels)
		c.RecordWithLabels("llm", MLLMGuardRPMTokens, g.RPMAvailable, labels)
		c.RecordWithLabels("llm", MLLMGuardPauseSec, g.PauseSecondsLeft, labels)
	}
	if cb != nil {
		open := 0.0
		if cb.Open {
			open = 1
		}
		c.RecordWithLabels("llm", MLLMCircuitOpenGauge, open, labels)
		c.RecordWithLabels("llm", MLLMCircuitFailStreak, cb.ConsecutiveFails, labels)
	}
}

func copyLabels(src map[string]string) map[string]string {
	out := make(map[string]string, len(src)+1)
	for k, v := range src {
		out[k] = v
	}
	return out
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func intStr(v int) string {
	// 避免引入 strconv 包 (已经间接依赖但保持简洁)。
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	buf := [20]byte{}
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// WaitRestore 等待历史事件回放的后台 goroutine 结束。
//
// 生产侧无需调用(进程退出即止); **测试应 defer 它** —— 否则 t.TempDir() 的清理会与
// 回放竞态, 得到一个与被测逻辑毫无关系的间歇性失败。
func WaitRestore() { llmRestoreBG.Wait() }
