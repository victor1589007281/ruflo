// v1.3 新增 handlers:
//   - 多源日志 /api/logs/sources (list) + /api/logs/stream?source=...
//   - 团队 LLM 诊断 /api/teams/:name/diagnose
//   - LLM 统计 /api/llm/stats (读 metrics/llm.jsonl)
//   - 群体智能 simulate 动作 (action queue: swarm.simulate)
//   - cron CRUD: cron.create / cron.update / cron.delete 动作
//   - 团队黑板写入 /api/teams/:name/blackboard (POST)
package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// =====================================================================
// /api/logs/sources -> 列出可用日志来源
// =====================================================================

// LogSourceDTO 前端日志频道描述。
type LogSourceDTO struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path"`
	Module      string `json:"module,omitempty"`
	SizeBytes   int64  `json:"sizeBytes"`
	ModTime     string `json:"modTime,omitempty"`
	Exists      bool   `json:"exists"`
}

// handleLogsSources 枚举 dashboard / feishu / client / mcp 以及 logs 目录下全部模块日志。
func (s *Server) handleLogsSources(w http.ResponseWriter, r *http.Request) {
	out := s.collectLogSources()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) collectLogSources() []LogSourceDTO {
	root := s.cfg.StateDir
	homeLogsDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		homeLogsDir = filepath.Join(home, ".claude-go", "logs")
	}
	logsDir := filepath.Join(root, "logs")

	seen := map[string]bool{}
	add := func(list *[]LogSourceDTO, id, title, desc, path, module string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()
		sz := int64(0)
		mt := ""
		if exists {
			sz = info.Size()
			mt = info.ModTime().Format(time.RFC3339)
		}
		*list = append(*list, LogSourceDTO{
			ID: id, Title: title, Description: desc, Path: path,
			Module: module, SizeBytes: sz, ModTime: mt, Exists: exists,
		})
	}

	var list []LogSourceDTO
	// 1) Dashboard 自身
	add(&list, "dashboard", "Dashboard", "dashboard 前台服务日志",
		filepath.Join(root, ".dashboard", "dashboard.log"), "dashboard")
	// 2) Dashboard 后台 daemon
	add(&list, "daemon", "Daemon", "claude-go daemon 日志",
		filepath.Join(root, "daemon.log"), "daemon")
	// 3) 各模块日志 (logging.For 模块) — 优先项目内, 其次 $HOME/.claude-go/logs
	for _, name := range []struct{ id, title, desc, module string }{
		{"client", "Client", "claude-go 客户端聊天日志", "client"},
		{"feishu", "Feishu Bot", "飞书机器人守护进程", "wiki"},
		{"mcp", "MCP", "MCP 动态管理器 (dynmcp)", "dynmcp"},
		{"hooks", "Hooks", "钩子执行日志", "hooks"},
		{"teams", "Teams", "团队编排日志", "teams"},
		{"agent-pool", "Agent Pool", "Agent 池调度日志", "agent-pool"},
		{"swarm", "Swarm", "群体智能 swarm_intel", "swarm_intel"},
	} {
		p1 := filepath.Join(logsDir, name.module+".log")
		p2 := filepath.Join(homeLogsDir, name.module+".log")
		// 挑一个存在的 (存在优先)
		preferred := p1
		if info, err := os.Stat(p1); err != nil || info.IsDir() {
			if info2, err2 := os.Stat(p2); err2 == nil && !info2.IsDir() {
				preferred = p2
			}
		}
		add(&list, name.id, name.title, name.desc, preferred, name.module)
	}
	// 4) 全局 claude-go.log
	add(&list, "global", "claude-go.log", "全局汇总日志",
		filepath.Join(logsDir, "claude-go.log"), "all")
	if homeLogsDir != "" {
		add(&list, "global-home", "claude-go.log (home)", "~/.claude-go/logs 全局日志",
			filepath.Join(homeLogsDir, "claude-go.log"), "all")
	}
	// 5) 发现 logs 目录里的其它 .log 文件 (项目优先, 再扫 home)
	for _, base := range []string{logsDir, homeLogsDir} {
		if base == "" {
			continue
		}
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			if !strings.HasSuffix(n, ".log") {
				continue
			}
			module := strings.TrimSuffix(n, ".log")
			id := "mod-" + module
			if strings.HasPrefix(base, homeLogsDir) && homeLogsDir != "" && base != logsDir {
				id = "home-" + module
			}
			title := "logs/" + n
			add(&list, id, title, "模块日志", filepath.Join(base, n), module)
		}
	}
	// 6) 历史 jsonl
	if homeLogsDir != "" {
		home := filepath.Dir(homeLogsDir)
		add(&list, "history", "History", "claude-go CLI 历史记录",
			filepath.Join(home, "history.jsonl"), "history")
		add(&list, "debug", "Debug", "CLI debug 日志",
			filepath.Join(home, "debug.log"), "debug")
	}

	// 稳定顺序: 先按"已知顺序", 其它按字母表
	preferredOrder := []string{
		"dashboard", "daemon", "client", "feishu", "mcp", "hooks",
		"teams", "agent-pool", "swarm", "global", "global-home",
		"history", "debug",
	}
	orderMap := map[string]int{}
	for i, id := range preferredOrder {
		orderMap[id] = i
	}
	sort.SliceStable(list, func(i, j int) bool {
		pi, iok := orderMap[list[i].ID]
		pj, jok := orderMap[list[j].ID]
		if iok && jok {
			return pi < pj
		}
		if iok {
			return true
		}
		if jok {
			return false
		}
		return list[i].ID < list[j].ID
	})
	return list
}

// resolveLogBySource 根据 ?source=xxx 参数, 返回文件路径。fallback 使用已有 resolveLogPath。
func (s *Server) resolveLogBySource(r *http.Request) string {
	src := r.URL.Query().Get("source")
	if src == "" {
		return s.resolveLogPath(r)
	}
	for _, ls := range s.collectLogSources() {
		if ls.ID == src {
			if ls.Exists {
				return ls.Path
			}
			return ""
		}
	}
	return s.resolveLogPath(r)
}

// =====================================================================
// /api/teams/:name/diagnose -> LLM 驱动的团队运行诊断
// =====================================================================

type teamDiagnosisResp struct {
	Team    string     `json:"team"`
	LLM     LLMProfile `json:"llm"`
	Summary string     `json:"summary,omitempty"`
	Error   string     `json:"error,omitempty"`
	Prompt  string     `json:"-"`
}

func (s *Server) handleTeamDiagnose(w http.ResponseWriter, r *http.Request, name string) {
	detail, err := s.provider.GetTeam(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	bb, _ := s.provider.Blackboard(name)
	bbMap := map[string]string{}
	if bb != nil {
		bbMap = bb.Map
	}

	payload := map[string]interface{}{
		"name":        detail.Name,
		"workflow":    detail.Workflow,
		"status":      detail.Status,
		"objective":   truncateString(detail.Objective, 1500),
		"error":       detail.Error,
		"durationSec": detail.DurationSec,
		"stagesTotal": detail.StagesTotal,
		"stagesDone":  detail.StagesDone,
		"stagesFail":  detail.StagesFail,
	}
	stages := []map[string]interface{}{}
	for _, st := range detail.Stages {
		stages = append(stages, map[string]interface{}{
			"name":     st.Name,
			"role":     st.Role,
			"status":   st.Status,
			"duration": st.DurationSec,
			"error":    st.Error,
			"output":   truncateString(st.Output, 600),
		})
	}
	payload["stages"] = stages
	payload["rounds"] = detail.AdversaryRounds
	if len(bbMap) > 0 {
		// 黑板 value 可能很大, 统一截断
		truncBB := map[string]string{}
		for k, v := range bbMap {
			truncBB[k] = truncateString(v, 400)
		}
		payload["blackboard"] = truncBB
	}
	if len(detail.RunMetrics) > 0 {
		// 只保留最近几项
		summary := map[string]interface{}{}
		for _, m := range detail.RunMetrics {
			n := len(m.Points)
			if n == 0 {
				continue
			}
			last := m.Points[n-1].Value
			summary[m.Name] = last
		}
		payload["latestMetrics"] = summary
	}

	b, _ := json.MarshalIndent(payload, "", "  ")
	sys := `你是一位资深的多智能体团队评审专家。根据给定的团队运行快照 (JSON), 输出中文诊断:
1. 运行过程是否符合该 workflow 的预期阶段顺序?
2. 输出质量如何? 每个阶段产出是否与目标 (objective) 匹配?
3. 有哪些风险信号 (报错/失败阶段/低分数/黑板关键字)?
4. 给出 3 条可执行的改进建议, 按优先级排序。
请用简洁的 Markdown, 3 段内完成, 每段不超过 6 行, 避免复读 JSON。`
	user := "团队运行快照:\n```json\n" + string(b) + "\n```"

	resp := teamDiagnosisResp{Team: name}
	summary, profile, err := LLMComplete(r.Context(), sys, user, 45*time.Second)
	resp.LLM = profile
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Summary = summary
	writeJSON(w, http.StatusOK, resp)
}

// =====================================================================
// /api/teams/:name/blackboard (POST) -> 向团队黑板写入一条 key-value
// (dashboard 提供的可控写入点, 主进程 blackboard 的持久化由 agent.Blackboard.flushLoop 完成;
//  这里只是把覆盖写入 blackboard.json, 兼容主进程下一次 Read 时的加载)
// =====================================================================

type blackboardWriteReq struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

func (s *Server) handleTeamBlackboardWrite(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST/PUT required"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req blackboardWriteReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("key required"))
		return
	}

	if !safeName(name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid team name"))
		return
	}
	teamDir := s.provider.pathIn("teams", name)
	if info, err := os.Stat(teamDir); err != nil || !info.IsDir() {
		writeError(w, http.StatusNotFound, fmt.Errorf("team %q not found", name))
		return
	}

	bbPath := filepath.Join(teamDir, "blackboard.json")
	// 读取为 BoardEntry 数组 (主进程写入的真实格式), 兼容旧的 map 格式。
	var entries []BoardEntryDTO
	if b, err := os.ReadFile(bbPath); err == nil && len(b) > 0 {
		trimmed := strings.TrimSpace(string(b))
		if strings.HasPrefix(trimmed, "[") {
			_ = json.Unmarshal(b, &entries)
		} else if strings.HasPrefix(trimmed, "{") {
			var m map[string]interface{}
			if err := json.Unmarshal(b, &m); err == nil {
				for k, v := range m {
					sv := ""
					switch val := v.(type) {
					case string:
						sv = val
					default:
						if vb, err := json.Marshal(val); err == nil {
							sv = string(vb)
						}
					}
					entries = append(entries, BoardEntryDTO{
						Key: k, Value: sv,
					})
				}
			}
		}
	}
	var valueStr string
	if len(req.Value) == 0 {
		valueStr = ""
	} else {
		// 尝试还原 Value: 优先作为字符串, 其他结构序列化成 JSON string
		var raw interface{}
		if err := json.Unmarshal(req.Value, &raw); err == nil {
			switch vv := raw.(type) {
			case string:
				valueStr = vv
			default:
				if b, err := json.Marshal(raw); err == nil {
					valueStr = string(b)
				}
			}
		} else {
			valueStr = string(req.Value)
		}
	}
	// 覆盖相同 key
	updated := false
	now := time.Now()
	for i, e := range entries {
		if e.Key == req.Key {
			entries[i].Value = valueStr
			entries[i].Author = "dashboard"
			entries[i].Category = "dashboard_write"
			entries[i].Timestamp = now
			updated = true
			break
		}
	}
	if !updated {
		entries = append(entries, BoardEntryDTO{
			Key:       req.Key,
			Value:     valueStr,
			Author:    "dashboard",
			Category:  "dashboard_write",
			Timestamp: now,
		})
	}

	nb, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.WriteFile(bbPath, nb, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	value := valueStr
	// 同时排队一条 action, 方便主进程订阅黑板变更触发下一步 (pull 模式)
	queueDir := filepath.Join(s.cfg.StateDir, ".dashboard", "actions")
	_ = os.MkdirAll(queueDir, 0o755)
	actionID := fmt.Sprintf("blackboard-%s-%d", name, time.Now().UnixMilli())
	rec := map[string]interface{}{
		"id":        actionID,
		"kind":      "team",
		"action":    "blackboard.write",
		"target":    name,
		"status":    "pending",
		"requested": time.Now().Format(time.RFC3339),
		"source":    "dashboard",
		"payload": map[string]interface{}{
			"key":   req.Key,
			"value": value,
		},
	}
	ab, _ := json.MarshalIndent(rec, "", "  ")
	_ = os.WriteFile(filepath.Join(queueDir, actionID+".json"), ab, 0o644)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"path":     bbPath,
		"actionId": actionID,
		"entries":  len(entries),
	})
}

// =====================================================================
// /api/llm/stats -> LLM 调用综合指标 (近 24h / 按模型聚合 / 错误分布)
// 数据源: {stateDir}/metrics/llm.jsonl
// =====================================================================

type llmModelStats struct {
	Model        string  `json:"model"`
	Calls        int     `json:"calls"`
	Success      int     `json:"success"`
	Errors       int     `json:"errors"`
	AvgDuration  float64 `json:"avgDuration"`
	P95Duration  float64 `json:"p95Duration"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CacheRead    int64   `json:"cacheReadTokens"`
	CacheCreate  int64   `json:"cacheCreationTokens"`
	SuccessRate  float64 `json:"successRate"`
	SuccessCost  float64 `json:"successCostSec"`  // 成功调用总耗时
	ErrorCost    float64 `json:"errorCostSec"`    // 失败调用总耗时
	Retries      int     `json:"retries"`
}

type llmErrorBucket struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// llmSourceStats 按调用来源 (cli / feishu / team / dashboard / unknown) 聚合。
// 帮助运维快速定位 "哪条业务路径在消耗 token / 触发错误"。
type llmSourceStats struct {
	Source       string  `json:"source"`
	Calls        int     `json:"calls"`
	Success      int     `json:"success"`
	Errors       int     `json:"errors"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	AvgDuration  float64 `json:"avgDuration"`
	SuccessRate  float64 `json:"successRate"`
}

type llmTimePoint struct {
	Timestamp    time.Time `json:"ts"`
	Calls        int       `json:"calls"`
	Success      int       `json:"success"`
	Errors       int       `json:"errors"`
	TotalTokens  int64     `json:"totalTokens"`
	AvgDuration  float64   `json:"avgDuration"`
}

// llmCacheStats 提示词缓存 (prompt cache) 效果统计。
//   - HitRate: cacheRead / (cacheRead + input) 命中率: 在 "应当作为输入读入" 的 token 中,
//     有多少是从缓存直接拿的 (不需要重新计费)。
//   - SavedTokens: 等于 cacheRead (被直接当作输入的缓存命中 token)
//   - CacheCreateTokens: 缓存写入的 token (首次/过期 时需要写一次, 有少量附加成本)
//   - CallsWithCache: 该窗口内至少命中过一次缓存的调用数
//   - CacheCoverage: CallsWithCache / TotalCalls 覆盖率
type llmCacheStats struct {
	HitRate           float64            `json:"hitRate"`
	SavedTokens       int64              `json:"savedTokens"`
	CacheReadTokens   int64              `json:"cacheReadTokens"`
	CacheCreateTokens int64              `json:"cacheCreateTokens"`
	InputTokens       int64              `json:"inputTokens"`
	CallsWithCache    int                `json:"callsWithCache"`
	CacheCoverage     float64            `json:"cacheCoverage"`
	ByModel           []llmCacheByModel  `json:"byModel"`
	Timeseries        []llmCacheTimePt   `json:"timeseries"`
}

type llmCacheByModel struct {
	Model             string  `json:"model"`
	Calls             int     `json:"calls"`
	CallsWithCache    int     `json:"callsWithCache"`
	HitRate           float64 `json:"hitRate"`
	CacheReadTokens   int64   `json:"cacheReadTokens"`
	CacheCreateTokens int64   `json:"cacheCreateTokens"`
	InputTokens       int64   `json:"inputTokens"`
}

type llmCacheTimePt struct {
	Timestamp         time.Time `json:"ts"`
	HitRate           float64   `json:"hitRate"`
	CacheReadTokens   int64     `json:"cacheReadTokens"`
	CacheCreateTokens int64     `json:"cacheCreateTokens"`
	InputTokens       int64     `json:"inputTokens"`
}

type llmStatsResp struct {
	Source       string            `json:"source"`     // metrics/llm.jsonl 路径
	Window       string            `json:"window"`
	RawEvents    int               `json:"rawEvents"`  // 原始事件数 (调试用)
	TotalCalls   int               `json:"totalCalls"`
	Success      int               `json:"success"`
	Errors       int               `json:"errors"`
	SuccessRate  float64           `json:"successRate"`
	TotalRetries int               `json:"totalRetries"`
	InputTokens  int64             `json:"inputTokens"`
	OutputTokens int64             `json:"outputTokens"`
	CacheRead    int64             `json:"cacheReadTokens"`
	CacheCreate  int64             `json:"cacheCreationTokens"`
	AvgDuration  float64           `json:"avgDuration"`
	P95Duration  float64           `json:"p95Duration"`
	ByModel      []llmModelStats   `json:"byModel"`
	BySource     []llmSourceStats  `json:"bySource"`
	Errors5xx    []llmErrorBucket  `json:"errorBuckets"`
	Timeseries   []llmTimePoint    `json:"timeseries"`
	Alerts       []string          `json:"alerts,omitempty"`
	Cache        llmCacheStats     `json:"cache"`
}

func (s *Server) handleLLMStats(w http.ResponseWriter, r *http.Request) {
	windowStr := r.URL.Query().Get("window")
	window := 24 * time.Hour
	if windowStr != "" {
		if d, err := time.ParseDuration(windowStr); err == nil && d > 0 {
			window = d
		}
	}
	since := time.Now().Add(-window)

	events, err := s.provider.ModuleMetricEvents("llm", 20000, since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	resp := llmStatsResp{
		Source:    filepath.Join(s.cfg.StateDir, "metrics", "llm.jsonl"),
		Window:    window.String(),
		RawEvents: len(events),
	}

	type callKey struct {
		ts    time.Time
		model string
	}
	// 按 (ts.Second, model) 关联同一次调用的多条指标 (我们同批写入)
	type callAgg struct {
		model      string
		status     string
		errKind    string
		source     string
		duration   float64
		input      int64
		output     int64
		cacheRead  int64
		cacheCreate int64
		retries    int
		ts         time.Time
	}
	calls := map[callKey]*callAgg{}
	bucketForCall := func(e MetricEventDTO) *callAgg {
		k := callKey{ts: e.Timestamp.Truncate(time.Millisecond), model: e.Labels["model"]}
		c := calls[k]
		if c == nil {
			c = &callAgg{
				model:   e.Labels["model"],
				ts:      e.Timestamp,
				status:  e.Labels["status"],
				errKind: e.Labels["error_kind"],
				source:  e.Labels["source"],
			}
			calls[k] = c
		}
		if c.model == "" {
			c.model = e.Labels["model"]
		}
		if c.status == "" {
			c.status = e.Labels["status"]
		}
		if c.errKind == "" {
			c.errKind = e.Labels["error_kind"]
		}
		if c.source == "" {
			c.source = e.Labels["source"]
		}
		return c
	}

	var durations []float64
	for _, e := range events {
		c := bucketForCall(e)
		switch e.Name {
		case "llm_duration_sec":
			c.duration = e.Value
		case "llm_input_tokens":
			c.input += int64(e.Value)
		case "llm_output_tokens":
			c.output += int64(e.Value)
		case "llm_cache_read_tokens":
			c.cacheRead += int64(e.Value)
		case "llm_cache_create_tokens":
			c.cacheCreate += int64(e.Value)
		case "llm_retry_count":
			c.retries += int(e.Value)
		}
	}

	// 按模型 / 按来源 聚合
	byModel := map[string]*llmModelStats{}
	bySource := map[string]*llmSourceStats{}
	errBuckets := map[string]int{}

	for _, c := range calls {
		m := c.model
		if m == "" {
			m = "unknown"
		}
		src := c.source
		if src == "" {
			src = "unknown"
		}
		ms := byModel[m]
		if ms == nil {
			ms = &llmModelStats{Model: m}
			byModel[m] = ms
		}
		ss := bySource[src]
		if ss == nil {
			ss = &llmSourceStats{Source: src}
			bySource[src] = ss
		}
		ms.Calls++
		ss.Calls++
		resp.TotalCalls++
		ms.InputTokens += c.input
		ms.OutputTokens += c.output
		ms.CacheRead += c.cacheRead
		ms.CacheCreate += c.cacheCreate
		ms.Retries += c.retries
		ss.InputTokens += c.input
		ss.OutputTokens += c.output
		resp.InputTokens += c.input
		resp.OutputTokens += c.output
		resp.CacheRead += c.cacheRead
		resp.CacheCreate += c.cacheCreate
		resp.TotalRetries += c.retries
		ms.AvgDuration += c.duration
		ss.AvgDuration += c.duration
		if c.duration > 0 {
			durations = append(durations, c.duration)
		}
		switch c.status {
		case "success", "retry_success":
			ms.Success++
			ms.SuccessCost += c.duration
			ss.Success++
			resp.Success++
		case "error":
			ms.Errors++
			ms.ErrorCost += c.duration
			ss.Errors++
			resp.Errors++
			if c.errKind != "" {
				errBuckets[c.errKind]++
			}
		}
	}
	for _, ss := range bySource {
		if ss.Calls > 0 {
			ss.AvgDuration = ss.AvgDuration / float64(ss.Calls)
			ss.SuccessRate = float64(ss.Success) / float64(ss.Calls)
		}
	}

	// 每模型平均时长 / 成功率
	for _, ms := range byModel {
		if ms.Calls > 0 {
			ms.AvgDuration = ms.AvgDuration / float64(ms.Calls)
			ms.SuccessRate = float64(ms.Success) / float64(ms.Calls)
		}
	}

	// 总体平均 / P95
	if len(durations) > 0 {
		sort.Float64s(durations)
		sum := 0.0
		for _, d := range durations {
			sum += d
		}
		resp.AvgDuration = sum / float64(len(durations))
		idx := int(math.Ceil(float64(len(durations))*0.95)) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(durations) {
			idx = len(durations) - 1
		}
		resp.P95Duration = durations[idx]
	}
	if resp.TotalCalls > 0 {
		resp.SuccessRate = float64(resp.Success) / float64(resp.TotalCalls)
	}

	// byModel 排序 (按调用数倒序)
	for _, ms := range byModel {
		if ms.Calls > 0 {
			resp.ByModel = append(resp.ByModel, *ms)
		}
	}
	sort.Slice(resp.ByModel, func(i, j int) bool { return resp.ByModel[i].Calls > resp.ByModel[j].Calls })

	// bySource 排序 (按调用数倒序)
	for _, ss := range bySource {
		if ss.Calls > 0 {
			resp.BySource = append(resp.BySource, *ss)
		}
	}
	sort.Slice(resp.BySource, func(i, j int) bool { return resp.BySource[i].Calls > resp.BySource[j].Calls })

	// 错误桶
	for k, v := range errBuckets {
		resp.Errors5xx = append(resp.Errors5xx, llmErrorBucket{Kind: k, Count: v})
	}
	sort.Slice(resp.Errors5xx, func(i, j int) bool { return resp.Errors5xx[i].Count > resp.Errors5xx[j].Count })

	// 时序 (按小时桶)
	type tsBucket struct {
		hour    time.Time
		calls   int
		success int
		errors  int
		tokens  int64
		sumDur  float64
		nDur    int
		// cache 相关
		cacheRead   int64
		cacheCreate int64
		input       int64
	}
	tsAll := map[int64]*tsBucket{}
	for _, c := range calls {
		h := c.ts.Truncate(time.Hour)
		k := h.Unix()
		b := tsAll[k]
		if b == nil {
			b = &tsBucket{hour: h}
			tsAll[k] = b
		}
		b.calls++
		if c.status == "success" || c.status == "retry_success" {
			b.success++
		} else if c.status == "error" {
			b.errors++
		}
		b.tokens += c.input + c.output + c.cacheRead + c.cacheCreate
		b.cacheRead += c.cacheRead
		b.cacheCreate += c.cacheCreate
		b.input += c.input
		if c.duration > 0 {
			b.sumDur += c.duration
			b.nDur++
		}
	}
	keys := make([]int64, 0, len(tsAll))
	for k := range tsAll {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		b := tsAll[k]
		avg := 0.0
		if b.nDur > 0 {
			avg = b.sumDur / float64(b.nDur)
		}
		resp.Timeseries = append(resp.Timeseries, llmTimePoint{
			Timestamp:   b.hour,
			Calls:       b.calls,
			Success:     b.success,
			Errors:      b.errors,
			TotalTokens: b.tokens,
			AvgDuration: avg,
		})
	}

	// -------- Prompt Cache 效果统计 --------
	// Anthropic prompt cache: cacheRead 是 "缓存命中, 按 10% 计费的输入 token",
	//                        cacheCreate 是 "首次写入, 按 125% 计费的输入 token".
	// 综合有效 "应输入" token = input + cacheRead (因为 cacheRead 本质上是替代了 input),
	// 所以 HitRate = cacheRead / (input + cacheRead).
	cacheTotalInput := resp.InputTokens + resp.CacheRead
	if cacheTotalInput > 0 {
		resp.Cache.HitRate = float64(resp.CacheRead) / float64(cacheTotalInput)
	}
	resp.Cache.CacheReadTokens = resp.CacheRead
	resp.Cache.CacheCreateTokens = resp.CacheCreate
	resp.Cache.SavedTokens = resp.CacheRead // 以命中 token 计算节省的原始输入成本
	resp.Cache.InputTokens = resp.InputTokens
	cacheCallsWithHit := 0
	for _, c := range calls {
		if c.cacheRead > 0 {
			cacheCallsWithHit++
		}
	}
	resp.Cache.CallsWithCache = cacheCallsWithHit
	if resp.TotalCalls > 0 {
		resp.Cache.CacheCoverage = float64(cacheCallsWithHit) / float64(resp.TotalCalls)
	}

	for model, ms := range byModel {
		hits := 0
		var cr, cc, in int64
		for _, c := range calls {
			if c.model != model && !(c.model == "" && model == "unknown") {
				continue
			}
			cr += c.cacheRead
			cc += c.cacheCreate
			in += c.input
			if c.cacheRead > 0 {
				hits++
			}
		}
		hr := 0.0
		if total := in + cr; total > 0 {
			hr = float64(cr) / float64(total)
		}
		resp.Cache.ByModel = append(resp.Cache.ByModel, llmCacheByModel{
			Model: ms.Model, Calls: ms.Calls, CallsWithCache: hits,
			HitRate: hr, CacheReadTokens: cr, CacheCreateTokens: cc, InputTokens: in,
		})
	}
	sort.Slice(resp.Cache.ByModel, func(i, j int) bool {
		return resp.Cache.ByModel[i].CacheReadTokens > resp.Cache.ByModel[j].CacheReadTokens
	})

	for _, k := range keys {
		b := tsAll[k]
		hr := 0.0
		if total := b.input + b.cacheRead; total > 0 {
			hr = float64(b.cacheRead) / float64(total)
		}
		resp.Cache.Timeseries = append(resp.Cache.Timeseries, llmCacheTimePt{
			Timestamp:         b.hour,
			HitRate:           hr,
			CacheReadTokens:   b.cacheRead,
			CacheCreateTokens: b.cacheCreate,
			InputTokens:       b.input,
		})
	}

	// 规则告警
	if resp.TotalCalls > 50 && resp.SuccessRate < 0.9 {
		resp.Alerts = append(resp.Alerts,
			fmt.Sprintf("成功率 %.1f%% 低于 90%%, 关注限流/过载错误", resp.SuccessRate*100))
	}
	if resp.Cache.CacheReadTokens == 0 && resp.InputTokens > 100_000 {
		resp.Alerts = append(resp.Alerts,
			"提示词缓存未命中 (cache_read=0). 若大量重复系统提示, 建议启用 prompt_cache_control=ephemeral")
	}
	if resp.Cache.HitRate > 0 && resp.Cache.HitRate < 0.2 && resp.InputTokens > 500_000 {
		resp.Alerts = append(resp.Alerts,
			fmt.Sprintf("提示词缓存命中率仅 %.1f%%, 建议检查系统提示是否稳定, 避免频繁变更", resp.Cache.HitRate*100))
	}
	if resp.P95Duration > 25 {
		resp.Alerts = append(resp.Alerts,
			fmt.Sprintf("P95 单次耗时 %.1fs 偏高, 建议优化 prompt 或切更低 token 模型", resp.P95Duration))
	}
	if resp.TotalCalls == 0 {
		resp.Alerts = append(resp.Alerts,
			"最近窗口内没有 LLM 调用记录。可能原因: 主进程未接入全局钩子 / 近期没有请求 / 指标目录不可写。")
	}

	writeJSON(w, http.StatusOK, resp)
}

// =====================================================================
// /api/actions/swarm/simulate   等 v1.3 action 在 handleAction 已支持 queue,
// 这里提供专门的 cron CRUD 落盘逻辑
// =====================================================================

func (s *Server) cronCreate(payload map[string]interface{}) (string, error) {
	if payload == nil {
		return "", errors.New("payload required for cron.create")
	}
	path := s.provider.pathIn("cron", "cron_jobs.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	var arr []map[string]interface{}
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &arr)
	}
	id, _ := payload["id"].(string)
	if id == "" {
		id = fmt.Sprintf("cron-%d", time.Now().UnixMilli())
	}
	for _, j := range arr {
		if fmt.Sprintf("%v", j["id"]) == id {
			return "", fmt.Errorf("cron id %q already exists", id)
		}
	}
	job := map[string]interface{}{
		"id":        id,
		"createdAt": time.Now().Format(time.RFC3339),
		"enabled":   true,
		"status":    "active",
	}
	for k, v := range payload {
		if k == "id" {
			continue
		}
		job[k] = v
	}
	if _, ok := job["schedule"]; !ok {
		return "", errors.New("schedule required")
	}
	arr = append(arr, job)
	b, _ := json.MarshalIndent(arr, "", "  ")
	return id, os.WriteFile(path, b, 0o644)
}

func (s *Server) cronUpdate(id string, payload map[string]interface{}) error {
	path := s.provider.pathIn("cron", "cron_jobs.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	found := false
	for i, j := range arr {
		if fmt.Sprintf("%v", j["id"]) == id {
			for k, v := range payload {
				if k == "id" {
					continue
				}
				j[k] = v
			}
			j["updatedAt"] = time.Now().Format(time.RFC3339)
			arr[i] = j
			found = true
		}
	}
	if !found {
		return fmt.Errorf("cron %q not found", id)
	}
	nb, _ := json.MarshalIndent(arr, "", "  ")
	return os.WriteFile(path, nb, 0o644)
}

func (s *Server) cronDelete(id string) error {
	path := s.provider.pathIn("cron", "cron_jobs.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	out := arr[:0]
	for _, j := range arr {
		if fmt.Sprintf("%v", j["id"]) != id {
			out = append(out, j)
		}
	}
	if len(out) == len(arr) {
		return fmt.Errorf("cron %q not found", id)
	}
	nb, _ := json.MarshalIndent(out, "", "  ")
	return os.WriteFile(path, nb, 0o644)
}

// ensure strconv always referenced
var _ = strconv.Itoa
