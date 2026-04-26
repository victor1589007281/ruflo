package dashboard

// v1.4: 新增 dashboard 能力 (按用户 6 项需求)
//   1. /api/llm/rate    - QPS (1s/60s/300s) + 错误率 + 时长滚动窗口
//   2. /api/llm/guard   - 限流器 / 熔断器 实时快照 (复用 Shared LLM Client)
//   3. /api/metrics/catalog    - 全量指标目录 (中英文 + 单位 + 采集类型 + 面板)
//   4. /api/metrics/coverage   - 指标可视化审计: catalog × 实际采集 × 可视化
//   5. /api/teams/:name/dag    - 团队运行时 DAG (stages + dependsOn + 状态)
//   6. /api/teams/:name/checkpoints - 读 checkpoints.json
//   7. /api/teams/:name/logs   - 按 team tag 过滤全局日志

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/metrics"
)

// ────────────────────────────────────────────────────────────────────────
// /api/llm/rate  — 基于 metrics/llm.jsonl 的滑动窗口统计
// ────────────────────────────────────────────────────────────────────────

type llmRateBucket struct {
	WindowSec    int     `json:"windowSec"`              // 窗口秒数
	Calls        int     `json:"calls"`                  // 窗口内 LLM 调用总数
	Success      int     `json:"success"`                // 成功数
	Errors       int     `json:"errors"`                 // 失败数
	RateLimited  int     `json:"rateLimited"`            // 被限流次数 (HTTP 429)
	Overloaded   int     `json:"overloaded"`             // 过载 (503/529) 次数
	Timeouts     int     `json:"timeouts"`               // 超时次数
	Retries      int     `json:"retries"`                // 重试次数
	AIMDCuts     int     `json:"aimdCuts"`               // AIMD 降并发次数
	CircuitTrips int     `json:"circuitTrips"`           // 窗口内熔断触发数
	CircuitBlock int     `json:"circuitBlocked"`         // 被已熔断直接拒绝数
	QPS          float64 `json:"qps"`                    // Calls / WindowSec
	SuccessRate  float64 `json:"successRate"`            // Success / Calls
	ErrorRate    float64 `json:"errorRate"`              // Errors / Calls
	AvgDuration  float64 `json:"avgDurationSec"`         // 平均耗时
	P95Duration  float64 `json:"p95DurationSec"`         // P95 耗时
	AvgWaitSec   float64 `json:"avgGuardWaitSec"`        // 平均准入等待
	TotalTokens  int64   `json:"totalTokens"`            // 总 tokens (in + out)
	// ByModel 窗口内每个模型的调用数, 便于前端区分
	ByModel map[string]int `json:"byModel,omitempty"`
}

type llmRateResp struct {
	Source  string          `json:"source"`
	Now     time.Time       `json:"now"`
	Buckets []llmRateBucket `json:"buckets"`
}

// handleLLMRate 近 1s/60s/300s 滑动窗口 QPS + 错误率 + 时长 + 熔断/限流 次数
// query: ?windows=1,60,300  (秒, 逗号分隔, 默认 1,60,300)
func (s *Server) handleLLMRate(w http.ResponseWriter, r *http.Request) {
	windowsArg := r.URL.Query().Get("windows")
	if windowsArg == "" {
		windowsArg = "1,60,300"
	}
	windowSecs := parseWindowList(windowsArg)
	if len(windowSecs) == 0 {
		windowSecs = []int{1, 60, 300}
	}
	maxWin := windowSecs[0]
	for _, w := range windowSecs {
		if w > maxWin {
			maxWin = w
		}
	}
	// 滑动窗口覆盖范围
	since := time.Now().Add(-time.Duration(maxWin+10) * time.Second)
	var events []MetricEventDTO
	var source string

	// 尝试从 Prometheus 获取最近数据
	promURL := os.Getenv("CLAUDE_GO_PROMETHEUS_URL")
	if promURL != "" {
		end := time.Now()
		start := end.Add(-time.Duration(maxWin+10) * time.Second)
		step := 15 * time.Second
		query := fmt.Sprintf(`{__name__=~"claude_go_.*", module="llm"}`)
		resp, err := queryPromRange(promURL, query, start, end, step)
		if err == nil && resp != nil && resp.Status == "success" && len(resp.Data.Result) > 0 {
			for _, r := range resp.Data.Result {
				name := strings.TrimPrefix(r.Metric["__name__"], "claude_go_")
				for _, v := range r.Values {
					if len(v) < 2 {
						continue
					}
					tsFloat, _ := v[0].(float64)
					valFloat, _ := v[1].(string)
					val, _ := strconv.ParseFloat(valFloat, 64)
					events = append(events, MetricEventDTO{
						Timestamp: time.Unix(int64(tsFloat), 0),
						Module:    "llm",
						Name:      name,
						Value:     val,
						Labels:    r.Metric,
					})
				}
			}
			source = "prometheus"
		}
	}
	// 回退: Provider (自带 Prometheus->JSONL dual-read)
	if len(events) == 0 {
		evts, err := s.provider.ModuleMetricEvents("llm", 50000, since)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		events = evts
		source = "jsonl"
	}

	// (ts_ms, model) -> aggregated call
	type key struct {
		tsMilli int64
		model   string
	}
	type callAgg struct {
		ts            time.Time
		model         string
		status        string
		errKind       string
		duration      float64
		input         int64
		output        int64
		retries       int
		guardWait     float64
		circuitOpened bool
		circuitBlocked bool
		aimdCut       int
	}
	calls := map[key]*callAgg{}

	bucketFor := func(e MetricEventDTO) *callAgg {
		k := key{tsMilli: e.Timestamp.UnixMilli(), model: e.Labels["model"]}
		c := calls[k]
		if c == nil {
			c = &callAgg{
				ts:      e.Timestamp,
				model:   e.Labels["model"],
				status:  e.Labels["status"],
				errKind: e.Labels["error_kind"],
			}
			calls[k] = c
		}
		if c.status == "" {
			c.status = e.Labels["status"]
		}
		if c.errKind == "" {
			c.errKind = e.Labels["error_kind"]
		}
		if c.model == "" {
			c.model = e.Labels["model"]
		}
		return c
	}

	// 汇总 event
	for _, e := range events {
		// guard/circuit gauge 类事件走单独桶 (不与某次 call 关联)
		switch e.Name {
		case metrics.MLLMGuardAIMDCut:
			c := bucketFor(e)
			c.aimdCut += int(e.Value)
			continue
		case metrics.MLLMCircuitTrips:
			c := bucketFor(e)
			c.circuitOpened = true
			continue
		}
		c := bucketFor(e)
		switch e.Name {
		case "llm_duration_sec":
			c.duration = e.Value
		case "llm_input_tokens":
			c.input += int64(e.Value)
		case "llm_output_tokens":
			c.output += int64(e.Value)
		case "llm_retry_count":
			c.retries += int(e.Value)
		case metrics.MLLMGuardWaitSec:
			c.guardWait = e.Value
		}
		if e.Labels["circuit_blocked"] == "true" {
			c.circuitBlocked = true
		}
	}

	now := time.Now()
	resp := llmRateResp{
		Source: source,
		Now:    now,
	}

	for _, wsec := range windowSecs {
		if wsec <= 0 {
			continue
		}
		cutoff := now.Add(-time.Duration(wsec) * time.Second)
		b := llmRateBucket{WindowSec: wsec, ByModel: map[string]int{}}
		var durations []float64
		var waits []float64
		for _, c := range calls {
			if c.ts.Before(cutoff) {
				continue
			}
			b.Calls++
			if c.model != "" {
				b.ByModel[c.model]++
			}
			switch c.status {
			case "success", "retry_success":
				b.Success++
			case "error":
				b.Errors++
			}
			switch c.errKind {
			case "rate_limited":
				b.RateLimited++
			case "overloaded":
				b.Overloaded++
			case "timeout":
				b.Timeouts++
			}
			b.Retries += c.retries
			b.AIMDCuts += c.aimdCut
			if c.circuitOpened {
				b.CircuitTrips++
			}
			if c.circuitBlocked {
				b.CircuitBlock++
			}
			b.TotalTokens += c.input + c.output
			if c.duration > 0 {
				durations = append(durations, c.duration)
			}
			if c.guardWait > 0 {
				waits = append(waits, c.guardWait)
			}
		}
		if b.Calls > 0 {
			b.QPS = float64(b.Calls) / float64(wsec)
			b.SuccessRate = float64(b.Success) / float64(b.Calls)
			b.ErrorRate = float64(b.Errors) / float64(b.Calls)
		}
		if len(durations) > 0 {
			sort.Float64s(durations)
			sum := 0.0
			for _, d := range durations {
				sum += d
			}
			b.AvgDuration = sum / float64(len(durations))
			idx := int(float64(len(durations)) * 0.95)
			if idx >= len(durations) {
				idx = len(durations) - 1
			}
			b.P95Duration = durations[idx]
		}
		if len(waits) > 0 {
			sum := 0.0
			for _, v := range waits {
				sum += v
			}
			b.AvgWaitSec = sum / float64(len(waits))
		}
		resp.Buckets = append(resp.Buckets, b)
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseWindowList(s string) []int {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err == nil && n > 0 && n <= 24*3600 {
			out = append(out, n)
		}
	}
	return out
}

// ────────────────────────────────────────────────────────────────────────
// /api/llm/guard  — 限流器 + 熔断器 实时快照
// ────────────────────────────────────────────────────────────────────────

type llmGuardResp struct {
	Ready     bool        `json:"ready"`          // 是否拿到了共享 LLM client
	Profile   LLMProfile  `json:"profile"`        // 模型配置
	Error     string      `json:"error,omitempty"`
	Guard     interface{} `json:"guard,omitempty"` // api.GuardSnapshot
	Circuit   interface{} `json:"circuit,omitempty"` // api.CircuitSnapshot
	Timestamp time.Time   `json:"timestamp"`
	// Hint 给前端展示的健康提示
	Hint []string `json:"hints,omitempty"`
}

func (s *Server) handleLLMGuard(w http.ResponseWriter, r *http.Request) {
	client, profile, err := GetSharedLLMClient()
	resp := llmGuardResp{
		Profile:   profile,
		Ready:     err == nil && client != nil,
		Timestamp: time.Now(),
	}
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if client != nil {
		cb := client.GetCircuitSnapshot()
		resp.Circuit = cb
		if client.Guard != nil {
			g := client.Guard.Snapshot()
			resp.Guard = g

			// 同时把这次采样写入 gauge, 让时序图有数据
			metrics.SampleGuardSnapshot("dashboard",
				&metrics.GuardSample{
					InFlight:         float64(g.InFlight),
					MaxParallel:      float64(g.MaxParallel),
					RPMAvailable:     g.RPMAvailable,
					PauseSecondsLeft: g.PauseSecondsLeft,
				},
				&metrics.CircuitSample{
					Open:             cb.Open,
					ConsecutiveFails: float64(cb.ConsecutiveFails),
				},
			)

			// 生成人类可读提示
			if cb.Open {
				resp.Hint = append(resp.Hint, fmt.Sprintf("🔴 熔断已开启, 还剩 %.1fs 恢复", cb.OpenSecondsLeft))
			}
			if g.PauseSecondsLeft > 0 {
				resp.Hint = append(resp.Hint, fmt.Sprintf("🟠 全局退避中, %.1fs 后放行", g.PauseSecondsLeft))
			}
			if g.InFlight >= g.MaxParallel && g.MaxParallel > 0 {
				resp.Hint = append(resp.Hint, fmt.Sprintf("🟡 并发已到上限 (%d/%d), 新请求将等待", g.InFlight, g.MaxParallel))
			}
			if g.RPMAvailable < 1 && g.RPMCapacity > 0 {
				resp.Hint = append(resp.Hint, "🟡 RPM 令牌桶已耗尽")
			}
			if len(resp.Hint) == 0 {
				resp.Hint = append(resp.Hint, "🟢 LLM 调用通道健康")
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ────────────────────────────────────────────────────────────────────────
// /api/metrics/catalog  — 指标目录 (中英文)
// ────────────────────────────────────────────────────────────────────────

type catalogResp struct {
	Total     int                          `json:"total"`
	Modules   []string                     `json:"modules"`
	Catalog   []metrics.MetricDesc         `json:"catalog"`
	ByModule  map[string][]metrics.MetricDesc `json:"byModule"`
}

func (s *Server) handleMetricsCatalog(w http.ResponseWriter, r *http.Request) {
	cat := metrics.Catalog()
	byMod := metrics.CatalogByModule()
	modules := make([]string, 0, len(byMod))
	for m := range byMod {
		modules = append(modules, m)
	}
	sort.Strings(modules)
	writeJSON(w, http.StatusOK, catalogResp{
		Total: len(cat), Modules: modules,
		Catalog: cat, ByModule: byMod,
	})
}

// ────────────────────────────────────────────────────────────────────────
// /api/metrics/coverage — 全量可视化审计
// 对比 catalog 中声明的指标 vs 实际 metrics/<module>.jsonl 里出现过的指标,
// 给出每个指标:
//   - declared  是否在 catalog 里声明 (有中英文描述)
//   - collected 是否实际采集到数据 (至少 1 条 event)
//   - lastValue 最近一次采集值 (帮前端快速判活)
//   - lastSeen  最近一次采集时间
// 供前端生成 "grafana-style" 全量审计表。
// ────────────────────────────────────────────────────────────────────────

type metricCoverageRow struct {
	Module    string    `json:"module"`
	Name      string    `json:"name"`
	ZH        string    `json:"zh,omitempty"`
	EN        string    `json:"en,omitempty"`
	Unit      string    `json:"unit,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Panel     string    `json:"panel,omitempty"`
	Declared  bool      `json:"declared"`    // catalog 里是否有
	Collected bool      `json:"collected"`   // 是否有实际 event
	Samples   int       `json:"samples"`     // 事件数
	LastValue float64   `json:"lastValue"`   // 最近一次采样值
	LastSeen  time.Time `json:"lastSeen"`    // 最近一次采样时间
}

type coverageResp struct {
	Now            time.Time           `json:"now"`
	TotalDeclared  int                 `json:"totalDeclared"`
	TotalCollected int                 `json:"totalCollected"`
	CoverageRate   float64             `json:"coverageRate"`  // collected / declared
	Rows           []metricCoverageRow `json:"rows"`
	// MissingDesc 实际采集到但 catalog 里没声明 (需要补中英文)
	MissingDesc []string `json:"missingDesc,omitempty"`
}

// coverageKey 指标覆盖查询的复合键。
type coverageKey struct{ module, name string }

func (s *Server) handleMetricsCoverage(w http.ResponseWriter, r *http.Request) {
	cat := metrics.Catalog()
	// Step 1: build declared index
	declared := map[coverageKey]metrics.MetricDesc{}
	for _, d := range cat {
		declared[coverageKey{d.Module, d.Name}] = d
	}

	// Step 2: 从 Prometheus 查询实际采集到的指标
	type stat struct {
		samples   int
		lastValue float64
		lastSeen  time.Time
	}
	collected := map[coverageKey]*stat{}

	promURL := os.Getenv("CLAUDE_GO_PROMETHEUS_URL")
	if promURL != "" {
		// 从 Prometheus 获取所有 claude_go_ 指标的当前值
		resp, err := queryPromInstant(promURL, `{__name__=~"claude_go_.*"}`)
		if err == nil && resp != nil && resp.Status == "success" {
			for _, r := range resp.Data.Result {
				if len(r.Value) < 2 {
					continue
				}
				metricName := r.Metric["__name__"]
				// claude_go_llm_call_count -> llm_call_count
				name := strings.TrimPrefix(metricName, "claude_go_")
				if name == metricName {
					continue // 不是我们的指标
				}
				mod := r.Metric["module"]
				if mod == "" {
					// 从指标名推断模块: claude_go_{module}_{rest}
					// 需要查 catalog 反向映射
					mod = inferModuleFromName(name, declared)
				}
				tsFloat, _ := r.Value[0].(float64)
				valFloat, _ := r.Value[1].(string)
				val, _ := strconv.ParseFloat(valFloat, 64)

				k := coverageKey{mod, name}
				st, ok := collected[k]
				if !ok {
					st = &stat{}
					collected[k] = st
				}
				st.samples++
				st.lastValue = val
				st.lastSeen = time.Unix(int64(tsFloat), 0)
			}
		}
	}

	// Prometheus 无数据时, 回退到 JSONL 扫描 (兼容模式)
	if len(collected) == 0 {
		entries, _ := os.ReadDir(s.provider.pathIn("metrics"))
		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
				continue
			}
			module := strings.TrimSuffix(ent.Name(), ".jsonl")
			events, err := s.provider.ModuleMetricEvents(module, 100000, time.Time{})
			if err != nil {
				continue
			}
			for _, e := range events {
				k := coverageKey{module, e.Name}
				st, ok := collected[k]
				if !ok {
					st = &stat{}
					collected[k] = st
				}
				st.samples++
				st.lastValue = e.Value
				if e.Timestamp.After(st.lastSeen) {
					st.lastSeen = e.Timestamp
				}
			}
		}
	}

	// Step 3: 合并成行 (declared ∪ collected)
	var rows []metricCoverageRow
	seen := map[coverageKey]bool{}
	for k, d := range declared {
		row := metricCoverageRow{
			Module: k.module, Name: k.name,
			ZH: d.ZH, EN: d.EN, Unit: d.Unit, Kind: string(d.Kind), Panel: d.Panel,
			Declared: true,
		}
		if c, ok := collected[k]; ok {
			row.Collected = true
			row.Samples = c.samples
			row.LastValue = c.lastValue
			row.LastSeen = c.lastSeen
		}
		rows = append(rows, row)
		seen[k] = true
	}
	var missingDesc []string
	for k, c := range collected {
		if seen[k] {
			continue
		}
		rows = append(rows, metricCoverageRow{
			Module: k.module, Name: k.name,
			Declared: false, Collected: true,
			Samples:   c.samples,
			LastValue: c.lastValue,
			LastSeen:  c.lastSeen,
		})
		missingDesc = append(missingDesc, k.module+"."+k.name)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Module != rows[j].Module {
			return rows[i].Module < rows[j].Module
		}
		return rows[i].Name < rows[j].Name
	})

	totalCollected := 0
	for k := range declared {
		if _, ok := collected[k]; ok {
			totalCollected++
		}
	}
	cov := 0.0
	if len(declared) > 0 {
		cov = float64(totalCollected) / float64(len(declared))
	}

	writeJSON(w, http.StatusOK, coverageResp{
		Now:            time.Now(),
		TotalDeclared:  len(declared),
		TotalCollected: totalCollected,
		CoverageRate:   cov,
		Rows:           rows,
		MissingDesc:    missingDesc,
	})
}

// inferModuleFromName 从指标名推断所属模块 (当 Prometheus 没有 module label 时)。
// 利用 catalog 中声明的 module→name 映射反向查找。
// 例如: "llm_call_count" → "llm", "team_stage_pass_rate" → "team".
func inferModuleFromName(name string, declared map[coverageKey]metrics.MetricDesc) string {
	for k := range declared {
		if k.name == name {
			return k.module
		}
	}
	// 回退: 从下划线分割取第一段
	if idx := strings.Index(name, "_"); idx > 0 {
		prefix := name[:idx]
		// 常见前缀映射
		switch prefix {
		case "llm", "team", "dream", "evo", "task", "cron", "mem", "fact", "amnesia":
			if prefix == "dream" {
				return "dreaming"
			}
			if prefix == "evo" {
				return "evolution"
			}
			if prefix == "mem" || prefix == "fact" || prefix == "amnesia" {
				return "memory"
			}
			return prefix
		}
	}
	return "unknown"
}

// ────────────────────────────────────────────────────────────────────────
// /api/teams/:name/dag  — 团队运行时 DAG
// ────────────────────────────────────────────────────────────────────────

// dagStageDTO v1.5 扩展: 为 DAG 节点补充 assignedAgents / taskContent / checkpoint 详情 /
// adversary (对抗评审) 等信息, 让前端在节点上能直接看到
// "任务内容 / 执行 agent / 当前 checkpoint / 对抗评分" 全貌。
type dagStageDTO struct {
	Name        string    `json:"name"`
	Role        string    `json:"role"`
	Status      string    `json:"status"` // pending / running / completed / failed
	DependsOn   []string  `json:"dependsOn"`
	Parallel    bool      `json:"parallel,omitempty"`
	Attempts    int       `json:"attempts,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	FinishedAt  time.Time `json:"finishedAt,omitempty"`
	DurationSec float64   `json:"durationSec,omitempty"`
	Error       string    `json:"error,omitempty"`
	V2TaskID    string    `json:"v2TaskId,omitempty"`
	OutputPre   string    `json:"outputPreview,omitempty"`

	// v1.5 新增
	// 任务信息 - 用户描述的意图 (通常等于 workflow stage description)
	TaskBrief       string   `json:"taskBrief,omitempty"`
	AssignedAgents  []string `json:"assignedAgents,omitempty"`
	// checkpoint 详情 (来自 checkpoints.json)
	CheckpointStatus string    `json:"checkpointStatus,omitempty"`
	CheckpointSaved  time.Time `json:"checkpointSavedAt,omitempty"`
	CheckpointError  string    `json:"checkpointError,omitempty"`
	// 对抗评审 (若该 stage 是 adversary 相关的)
	AdversaryRound   int     `json:"adversaryRound,omitempty"`
	AdversaryScore   float64 `json:"adversaryScore,omitempty"`
	AdversaryPassed  bool    `json:"adversaryPassed,omitempty"`
}

type dagResp struct {
	Team            string                `json:"team"`
	Workflow        string                `json:"workflow"`
	Status          string                `json:"status"`
	StartedAt       time.Time             `json:"startedAt,omitempty"`
	FinishedAt      time.Time             `json:"finishedAt,omitempty"`
	Stages          []dagStageDTO         `json:"stages"`
	AdversaryRounds []AdversaryRoundDTO   `json:"adversaryRounds,omitempty"`
	Source          string                `json:"source"`
	Error           string                `json:"error,omitempty"`
}

// handleTeamDAG 读取 team.json + workflow 静态定义 + checkpoints.json, 合成实时 DAG
func (s *Server) handleTeamDAG(w http.ResponseWriter, r *http.Request, name string) {
	detail, err := s.provider.GetTeam(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	// 1) 静态 DAG (从 workflow 定义中取 dependsOn / task brief / agent)
	type wfMeta struct {
		DependsOn []string
		Parallel  bool
		Brief     string // 任务简述: 首选 prompt 的首行, 否则 role 描述
		Agents    []string
	}
	wfMap := map[string]wfMeta{}
	if wf := agent.GetWorkflow(detail.Workflow); wf != nil {
		for _, st := range wf.Stages {
			brief := stageBriefFromPrompt(st.Prompt)
			if brief == "" && st.Role != "" {
				brief = "角色: " + st.Role
			}
			agents := []string{}
			if st.Role != "" {
				agents = append(agents, st.Role)
			}
			wfMap[st.Name] = wfMeta{
				DependsOn: append([]string(nil), st.DependsOn...),
				Parallel:  st.Parallel,
				Brief:     brief,
				Agents:    agents,
			}
		}
	}

	// 2) checkpoint 状态 (stageName -> attempt)
	checkpoints := readTeamCheckpoints(s.provider.pathIn("teams", name))
	cpMap := map[string]*agent.Checkpoint{}
	for i := range checkpoints {
		cpMap[checkpoints[i].StageName] = &checkpoints[i]
	}

	// 3) 合并 StageDTO + checkpoint + workflow 依赖
	resp := dagResp{
		Team: name, Workflow: detail.Workflow, Status: detail.Status,
		StartedAt: detail.StartedAt, FinishedAt: detail.FinishedAt,
		Source:          "team.json + checkpoints.json + workflow_def",
		AdversaryRounds: detail.AdversaryRounds,
	}
	// 为快速定位对抗评审回合, 按 stage name 中的 "round-N" 字段匹配
	advByRound := map[int]*AdversaryRoundDTO{}
	for i := range detail.AdversaryRounds {
		advByRound[detail.AdversaryRounds[i].Round] = &detail.AdversaryRounds[i]
	}

	for _, st := range detail.Stages {
		meta := wfMap[st.Name]
		stage := dagStageDTO{
			Name: st.Name, Role: st.Role, Status: st.Status,
			DependsOn: meta.DependsOn,
			Parallel:  meta.Parallel,
			StartedAt: st.StartedAt,
			Error:     st.Error,
			TaskBrief:      meta.Brief,
			AssignedAgents: meta.Agents,
		}
		if !st.StartedAt.IsZero() && st.DurationSec > 0 {
			stage.FinishedAt = st.StartedAt.Add(time.Duration(st.DurationSec * float64(time.Second)))
		}
		stage.DurationSec = st.DurationSec
		if len(st.Output) > 400 {
			stage.OutputPre = st.Output[:400] + "..."
		} else {
			stage.OutputPre = st.Output
		}
		if cp, ok := cpMap[st.Name]; ok {
			stage.Attempts = cp.Attempt
			stage.CheckpointStatus = cp.Status
			stage.CheckpointSaved = cp.SavedAt
			stage.CheckpointError = cp.Error
			// checkpoint 的 status 比 stage.status 更实时 (stage.status 只有跑完才覆盖)
			if cp.Status != "" && stage.Status == "" {
				stage.Status = cp.Status
			}
		}
		// 如果 stage 名称是 "adversary-roundN" / "eval-roundN" 模式, 关联对抗回合
		if r := extractRoundFromStageName(st.Name); r > 0 {
			if adv, ok := advByRound[r]; ok {
				stage.AdversaryRound = adv.Round
				stage.AdversaryScore = adv.AvgScore
				stage.AdversaryPassed = adv.Passed
			}
		}
		resp.Stages = append(resp.Stages, stage)
	}

	// 3.5) 补充 checkpoint-only 阶段: checkpoints.json 中有记录但 team.json 的 stages 里没有的阶段。
	// 这发生在 Orchestrator 运行中——checkpoint 已实时落地, 但 stages 尚未 flush。
	stageNameSet := map[string]bool{}
	for _, st := range resp.Stages {
		stageNameSet[st.Name] = true
	}
	for cpName, cp := range cpMap {
		if stageNameSet[cpName] {
			continue
		}
		meta := wfMap[cpName]
		stage := dagStageDTO{
			Name:             cpName,
			Status:           cp.Status,
			DependsOn:        meta.DependsOn,
			Parallel:         meta.Parallel,
			TaskBrief:        meta.Brief,
			AssignedAgents:   meta.Agents,
			Attempts:         cp.Attempt,
			CheckpointStatus: cp.Status,
			CheckpointSaved:  cp.SavedAt,
			CheckpointError:  cp.Error,
			V2TaskID:         cpName,
		}
		if cp.Status == "completed" && cp.Output != "" {
			if len(cp.Output) > 400 {
				stage.OutputPre = cp.Output[:400] + "..."
			} else {
				stage.OutputPre = cp.Output
			}
		}
		resp.Stages = append(resp.Stages, stage)
	}

	// 4) 如果 team 根本没跑过 stages, 直接回填 workflow 的静态 DAG, 让前端也能渲染结构
	if len(resp.Stages) == 0 {
		if wf := agent.GetWorkflow(detail.Workflow); wf != nil {
			for _, st := range wf.Stages {
				meta := wfMap[st.Name]
				resp.Stages = append(resp.Stages, dagStageDTO{
					Name: st.Name, Role: st.Role, Status: "pending",
					DependsOn:      meta.DependsOn,
					Parallel:       st.Parallel,
					TaskBrief:      meta.Brief,
					AssignedAgents: meta.Agents,
				})
			}
			resp.Source = "workflow_def (未启动)"
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// stageBriefFromPrompt 从 workflow prompt 中抽取一条短摘要 (≤ 160 字符)。
// 很多 stage 的 prompt 开头是 "你是 XXX, 负责 ...", 取首句即可。
func stageBriefFromPrompt(prompt string) string {
	s := strings.TrimSpace(prompt)
	if s == "" {
		return ""
	}
	// 尝试第一行
	if idx := strings.IndexAny(s, "\n\r"); idx > 0 {
		s = s[:idx]
	}
	if n := len([]rune(s)); n > 160 {
		s = string([]rune(s)[:160]) + "…"
	}
	return s
}

// extractRoundFromStageName 从 stage 名称中抽 round 编号, 形如:
//   "adversary-round2" -> 2
//   "eval-round-3"     -> 3
//   "round 1 review"   -> 1
// 没匹配到返回 0。
func extractRoundFromStageName(name string) int {
	lower := strings.ToLower(name)
	keywords := []string{"round", "eval-round", "adversary-round"}
	for _, kw := range keywords {
		idx := strings.Index(lower, kw)
		if idx < 0 {
			continue
		}
		rest := lower[idx+len(kw):]
		n := 0
		started := false
		for _, c := range rest {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
				started = true
			} else if started {
				break
			}
		}
		if n > 0 {
			return n
		}
	}
	return 0
}

// ────────────────────────────────────────────────────────────────────────
// /api/teams/:name/checkpoints — 直接返回 checkpoints.json
// ────────────────────────────────────────────────────────────────────────

type checkpointResp struct {
	Team        string              `json:"team"`
	File        string              `json:"file"`
	Exists      bool                `json:"exists"`
	Count       int                 `json:"count"`
	Checkpoints []agent.Checkpoint  `json:"checkpoints"`
}

func (s *Server) handleTeamCheckpoints(w http.ResponseWriter, r *http.Request, name string) {
	dir := s.provider.pathIn("teams", name)
	file := filepath.Join(dir, "checkpoints.json")
	resp := checkpointResp{Team: name, File: file}
	if _, err := os.Stat(file); err == nil {
		resp.Exists = true
		resp.Checkpoints = readTeamCheckpoints(dir)
		resp.Count = len(resp.Checkpoints)
	}
	writeJSON(w, http.StatusOK, resp)
}

// readTeamCheckpoints 读取 checkpoints.json (存储结构: map[stage]*Checkpoint)
// 并按 SavedAt 排序成 slice。
func readTeamCheckpoints(teamDir string) []agent.Checkpoint {
	data, err := os.ReadFile(filepath.Join(teamDir, "checkpoints.json"))
	if err != nil {
		return nil
	}
	m := map[string]*agent.Checkpoint{}
	if err := json.Unmarshal(data, &m); err != nil {
		// 兼容老格式: 一个 array
		var arr []agent.Checkpoint
		if err2 := json.Unmarshal(data, &arr); err2 == nil {
			return arr
		}
		return nil
	}
	out := make([]agent.Checkpoint, 0, len(m))
	for _, v := range m {
		if v != nil {
			out = append(out, *v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt.Before(out[j].SavedAt) })
	return out
}

// ────────────────────────────────────────────────────────────────────────
// /api/teams/:name/logs — 按 team tag 过滤全局日志
// ────────────────────────────────────────────────────────────────────────

type teamLogResp struct {
	Team   string   `json:"team"`
	Source string   `json:"source"`
	Limit  int      `json:"limit"`
	Lines  []string `json:"lines"`
}

// handleTeamLogs 过滤 logs/claude-go.log 里包含 team=<name> 或 [<name>] 的行
// query: ?limit=200  (默认 500, 最大 5000)
func (s *Server) handleTeamLogs(w http.ResponseWriter, r *http.Request, name string) {
	limit := parseIntQuery(r, "limit", 500)
	if limit > 5000 {
		limit = 5000
	}

	logPath := s.provider.pathIn("logs", "claude-go.log")
	resp := teamLogResp{Team: name, Source: logPath, Limit: limit}

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()

	// 匹配策略 (宽松 OR):
	//   team=<name>
	//   "team":"<name>"
	//   [<name>]
	//   slog 结构化: team=<name>  (JSON handler 会把 attrs 写成 "team":"...")
	needles := []string{
		"team=" + name,
		"\"team\":\"" + name + "\"",
		"[" + name + "]",
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var tail []string
	for scanner.Scan() {
		line := scanner.Text()
		hit := false
		for _, n := range needles {
			if strings.Contains(line, n) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		tail = append(tail, line)
		if len(tail) > limit {
			tail = tail[len(tail)-limit:]
		}
	}
	if err := scanner.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resp.Lines = tail
	writeJSON(w, http.StatusOK, resp)
}
