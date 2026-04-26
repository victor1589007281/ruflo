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
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/metrics"
)

// Provider 封装对 .claude-go/ 数据目录的只读访问, 内置 TTL 缓存。
type Provider struct {
	stateDir string
	ttl      time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	value     interface{}
	expiresAt time.Time
}

// NewProvider 创建 Provider, ttl<=0 表示禁用缓存。
func NewProvider(stateDir string, ttl time.Duration) *Provider {
	return &Provider{
		stateDir: stateDir,
		ttl:      ttl,
		cache:    make(map[string]cacheEntry),
	}
}

// StateDir 返回数据根目录。
func (p *Provider) StateDir() string { return p.stateDir }

// Exists 检查数据目录是否可用。
func (p *Provider) Exists() bool {
	if p.stateDir == "" {
		return false
	}
	info, err := os.Stat(p.stateDir)
	return err == nil && info.IsDir()
}

func (p *Provider) load(key string, fn func() (interface{}, error)) (interface{}, error) {
	if p.ttl > 0 {
		p.mu.RLock()
		if e, ok := p.cache[key]; ok && time.Now().Before(e.expiresAt) {
			p.mu.RUnlock()
			return e.value, nil
		}
		p.mu.RUnlock()
	}
	v, err := fn()
	if err != nil {
		return nil, err
	}
	if p.ttl > 0 {
		p.mu.Lock()
		p.cache[key] = cacheEntry{value: v, expiresAt: time.Now().Add(p.ttl)}
		p.mu.Unlock()
	}
	return v, nil
}

func (p *Provider) pathIn(sub ...string) string {
	parts := append([]string{p.stateDir}, sub...)
	return filepath.Join(parts...)
}

// ============== Teams ==============

// rawTeam 对应 teams/<name>/team.json 序列化结构 (与 agent.ProductionTeam 对齐)。
type rawTeam struct {
	Name       string                 `json:"name"`
	Workflow   string                 `json:"workflow"`
	Objective  string                 `json:"objective"`
	ChatID     string                 `json:"chatId"`
	Status     string                 `json:"status"`
	Agents     map[string]*rawAgent   `json:"agents"`
	Stages     []rawStage             `json:"stages"`
	TaskIDs    map[string]string      `json:"taskIds,omitempty"`
	CreatedAt  time.Time              `json:"createdAt"`
	StartedAt  time.Time              `json:"startedAt,omitempty"`
	FinishedAt time.Time              `json:"finishedAt,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Cwd        string                 `json:"cwd,omitempty"`
	// 忽略 Mailbox, Blackboard 等
}

type rawAgent struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type rawStage struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Status    string    `json:"status"`
	Input     string    `json:"input,omitempty"`
	Output    string    `json:"output,omitempty"`
	Error     string    `json:"error,omitempty"`
	V2TaskID  string    `json:"v2TaskId,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	Duration  string    `json:"duration,omitempty"`
}

// ListTeams 读取 teams/ 目录, 返回轻量摘要列表, 按 CreatedAt 倒序。
func (p *Provider) ListTeams() ([]TeamSummary, error) {
	v, err := p.load("teams:list", func() (interface{}, error) {
		root := p.pathIn("teams")
		entries, err := os.ReadDir(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return []TeamSummary{}, nil
			}
			return nil, err
		}
		var list []TeamSummary
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			t, err := p.readTeamFile(e.Name())
			if err != nil {
				continue
			}
			list = append(list, toSummary(t))
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		})
		return list, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]TeamSummary), nil
}

// GetTeam 读取某个团队详情。
func (p *Provider) GetTeam(name string) (*TeamDetail, error) {
	// 安全: 避免路径穿越
	if !safeName(name) {
		return nil, errors.New("invalid team name")
	}
	t, err := p.readTeamFile(name)
	if err != nil {
		return nil, err
	}
	detail := &TeamDetail{
		TeamSummary: toSummary(t),
		Cwd:         t.Cwd,
		TaskIDs:     t.TaskIDs,
	}
	for _, ag := range t.Agents {
		if ag == nil {
			continue
		}
		detail.Agents = append(detail.Agents, AgentDTO{
			Name: ag.Name, Role: ag.Role, Status: ag.Status,
			Result: ag.Result, Error: ag.Error,
		})
	}
	sort.Slice(detail.Agents, func(i, j int) bool {
		return detail.Agents[i].Name < detail.Agents[j].Name
	})
	for _, s := range t.Stages {
		dur := parseDurationSec(s.Duration)
		detail.Stages = append(detail.Stages, StageDTO{
			Name: s.Name, Role: s.Role, Status: s.Status,
			Input: s.Input, Output: s.Output, Error: s.Error,
			StartedAt: s.StartedAt, Duration: s.Duration,
			DurationSec: dur,
		})
	}

	if data, err := os.ReadFile(p.pathIn("teams", name, "REPORT.md")); err == nil {
		detail.Report = string(data)
	}

	// Adversary rounds 从 blackboard.json 的 eval-roundN-score 字段解析
	detail.AdversaryRounds = p.extractAdversaryRounds(name)

	// Run metrics: 从 metrics/*.jsonl 按 labels.team=name 过滤
	detail.RunMetrics = p.teamRunMetrics(name)

	return detail, nil
}

// extractAdversaryRounds 解析 blackboard.json 中的对抗评分。
// 支持两种键格式:
//   - "eval-roundN-score"                 (经典对抗循环, 全局轮次)
//   - "eval-task-{title}-roundN-score"    (Orchestrator 任务级轮次)
//
// 已知值格式: "正确=7 完整=3 安全=6 质量=8 对齐=5 通过:false"
func (p *Provider) extractAdversaryRounds(name string) []AdversaryRoundDTO {
	if !safeName(name) {
		return nil
	}
	data, err := os.ReadFile(p.pathIn("teams", name, "blackboard.json"))
	if err != nil {
		return nil
	}
	var items []struct {
		Key   string      `json:"key"`
		Value interface{} `json:"value"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return nil
	}

	// compositeKey = "phase:round" → dto, 区分不同来源以防覆盖
	type dtoKey struct {
		Phase string
		Round int
	}
	byKey := map[dtoKey]*AdversaryRoundDTO{}

	for _, it := range items {
		k := it.Key
		if !strings.HasPrefix(k, "eval-") || !strings.HasSuffix(k, "-score") {
			continue
		}
		raw, ok := it.Value.(string)
		if !ok {
			continue
		}

		inner := strings.TrimSuffix(strings.TrimPrefix(k, "eval-"), "-score")
		// 格式 1: "eval-roundN-score" → inner = "roundN"
		// 格式 2: "eval-task-{title}-roundN-score" → inner = "task-{title}-roundN"
		phase := "global"
		roundStr := ""
		if ri := strings.LastIndex(inner, "round"); ri >= 0 {
			roundStr = inner[ri+len("round"):]
			prefix := inner[:ri]
			if prefix != "" {
				phase = strings.TrimSuffix(prefix, "-")
			}
		}
		if roundStr == "" {
			continue
		}
		round, err := strconvItoa(roundStr)
		if err != nil {
			continue
		}
		r := parseEvalScoreLine(round, raw)
		r.Phase = phase
		byKey[dtoKey{Phase: phase, Round: round}] = &r
	}

	rounds := make([]AdversaryRoundDTO, 0, len(byKey))
	for _, r := range byKey {
		rounds = append(rounds, *r)
	}
	sort.Slice(rounds, func(i, j int) bool {
		if rounds[i].Round != rounds[j].Round {
			return rounds[i].Round < rounds[j].Round
		}
		return rounds[i].Raw < rounds[j].Raw
	})
	return rounds
}

// parseEvalScoreLine 从 "正确=7 完整=3 安全=6 质量=8 对齐=5 通过:false" 解析。
// 同时兼容英文键 (correctness=7 completeness=3 ...)
func parseEvalScoreLine(round int, raw string) AdversaryRoundDTO {
	r := AdversaryRoundDTO{Round: round, Raw: raw}
	lower := strings.ToLower(raw)
	for _, tok := range strings.FieldsFunc(raw, func(c rune) bool { return c == ' ' || c == '\t' || c == '\n' }) {
		sep := strings.IndexAny(tok, "=:")
		if sep < 0 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(tok[:sep]))
		v := strings.TrimSpace(tok[sep+1:])
		switch k {
		case "正确", "correctness":
			r.Correctness = parseFloatSoft(v)
		case "完整", "completeness":
			r.Completeness = parseFloatSoft(v)
		case "安全", "security":
			r.Security = parseFloatSoft(v)
		case "质量", "代码质量", "codequality", "code_quality":
			r.CodeQuality = parseFloatSoft(v)
		case "对齐", "设计对齐", "designalignment", "design_alignment":
			r.DesignAlignment = parseFloatSoft(v)
		case "通过", "pass", "passed":
			lv := strings.ToLower(v)
			r.Passed = lv == "true" || lv == "yes" || lv == "1" || lv == "通过"
		}
	}
	// 某些版本只用"pass"出现在文本里
	if strings.Contains(lower, "通过:true") || strings.Contains(lower, "pass:true") {
		r.Passed = true
	}
	denom := 0.0
	sum := 0.0
	for _, v := range []float64{r.Correctness, r.Completeness, r.Security, r.CodeQuality} {
		if v > 0 {
			sum += v
			denom++
		}
	}
	if r.DesignAlignment > 0 {
		sum += r.DesignAlignment
		denom++
	}
	if denom > 0 {
		r.AvgScore = sum / denom
	}
	return r
}

func parseFloatSoft(s string) float64 {
	s = strings.TrimSpace(s)
	// 去掉末尾可能的标点
	s = strings.TrimRight(s, ",;。")
	if s == "" {
		return 0
	}
	f, err := strconvParseFloat(s)
	if err != nil {
		return 0
	}
	return f
}

// teamRunMetrics 扫描 metrics/ 下所有模块, 按 labels["team"] 或 labels["team_name"] 过滤出属于该团队的事件,
// 分组返回。适用于 team 维度的 run 指标展示。
func (p *Provider) teamRunMetrics(name string) []RunMetricSeries {
	dir := p.pathIn("metrics")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	grouped := map[string]*RunMetricSeries{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		module := strings.TrimSuffix(e.Name(), ".jsonl")
		// 只关心与单 run 相关的模块: team / evolution
		if module != "team" && module != "evolution" && module != "dreaming" && module != "task" {
			continue
		}
		events, err := p.readMetricEvents(module)
		if err != nil {
			continue
		}
		for _, ev := range events {
			if !matchTeam(ev.Labels, name) {
				continue
			}
			key := module + "/" + ev.Name
			if grouped[key] == nil {
				grouped[key] = &RunMetricSeries{Name: key, Labels: ev.Labels}
			}
			grouped[key].Points = append(grouped[key].Points, MetricPoint{Timestamp: ev.Timestamp, Value: ev.Value})
		}
	}
	out := make([]RunMetricSeries, 0, len(grouped))
	for _, s := range grouped {
		sort.Slice(s.Points, func(i, j int) bool { return s.Points[i].Timestamp.Before(s.Points[j].Timestamp) })
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func matchTeam(labels map[string]string, name string) bool {
	if labels == nil {
		return false
	}
	for _, k := range []string{"team", "team_name", "teamName", "run", "run_name"} {
		if v, ok := labels[k]; ok && v == name {
			return true
		}
	}
	return false
}

// BoardEntryDTO 黑板条目 (对外 API 视图)。与 pkg/agent.BoardEntry 字段一致。
type BoardEntryDTO struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Author    string    `json:"author"`
	Category  string    `json:"category"`
	Timestamp time.Time `json:"timestamp"`
}

// BlackboardDTO 黑板完整视图: 同时提供 entries (原始条目数组) 和 map (k→最新值),
// 方便前端按条目列表或按 key 快速查找两种方式展示。
type BlackboardDTO struct {
	Team     string           `json:"team"`
	Entries  []BoardEntryDTO  `json:"entries"`
	Map      map[string]string `json:"map"`
	Count    int              `json:"count"`
	UpdateAt time.Time        `json:"updatedAt,omitempty"`
}

// Blackboard 读取某个团队的黑板 JSON。
// 磁盘上 blackboard.json 由 pkg/agent.Blackboard 写入, 实际形态是 BoardEntry 数组;
// 这里同时兼容旧版 map 形态 (dashboard /api/teams/.../blackboard POST 写入的临时形态)。
func (p *Provider) Blackboard(name string) (*BlackboardDTO, error) {
	if !safeName(name) {
		return nil, errors.New("invalid team name")
	}
	dto := &BlackboardDTO{Team: name, Entries: []BoardEntryDTO{}, Map: map[string]string{}}
	fp := p.pathIn("teams", name, "blackboard.json")
	data, err := os.ReadFile(fp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dto, nil
		}
		return nil, err
	}
	if st, err := os.Stat(fp); err == nil {
		dto.UpdateAt = st.ModTime()
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return dto, nil
	}
	// 优先尝试 BoardEntry 数组形态 (真实线上形态)
	if strings.HasPrefix(trimmed, "[") {
		var entries []BoardEntryDTO
		if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
			return nil, err
		}
		dto.Entries = entries
		for _, e := range entries {
			dto.Map[e.Key] = e.Value
		}
		dto.Count = len(entries)
		return dto, nil
	}
	// 兼容 map 形态 (dashboard 历史写入)
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return nil, err
	}
	for k, v := range m {
		sv := ""
		switch val := v.(type) {
		case string:
			sv = val
		default:
			if b, err := json.Marshal(val); err == nil {
				sv = string(b)
			}
		}
		dto.Map[k] = sv
		dto.Entries = append(dto.Entries, BoardEntryDTO{
			Key:   k,
			Value: sv,
		})
	}
	dto.Count = len(dto.Entries)
	return dto, nil
}

func (p *Provider) readTeamFile(name string) (*rawTeam, error) {
	data, err := os.ReadFile(p.pathIn("teams", name, "team.json"))
	if err != nil {
		return nil, err
	}
	var t rawTeam
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	if t.Name == "" {
		t.Name = name
	}
	return &t, nil
}

func toSummary(t *rawTeam) TeamSummary {
	var total, done, fail int
	total = len(t.Stages)
	for _, s := range t.Stages {
		switch s.Status {
		case "completed":
			done++
		case "failed":
			fail++
		}
	}
	dur := 0.0
	if !t.FinishedAt.IsZero() && !t.StartedAt.IsZero() {
		dur = t.FinishedAt.Sub(t.StartedAt).Seconds()
	} else if !t.StartedAt.IsZero() {
		dur = time.Since(t.StartedAt).Seconds()
	}
	return TeamSummary{
		Name: t.Name, Workflow: t.Workflow, Status: t.Status,
		Objective: t.Objective, ChatID: t.ChatID,
		CreatedAt: t.CreatedAt, StartedAt: t.StartedAt, FinishedAt: t.FinishedAt,
		DurationSec: dur, StagesTotal: total, StagesDone: done, StagesFail: fail,
		AgentsTotal: len(t.Agents), Error: t.Error,
	}
}

// ============== Metrics ==============

type rawMetricEvent struct {
	Timestamp time.Time         `json:"ts"`
	Module    string            `json:"module"`
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Labels    map[string]string `json:"labels,omitempty"`
	RunID     string            `json:"run_id,omitempty"`
}

// ============== Prometheus Query Client ==============

// PrometheusURL dashboard 内置的 Prometheus 查询地址。
// 与 claude-go 内置的 /metrics 端点复用同一进程, 通过 127.0.0.1:7777/metrics 暴露。
// 当 Prometheus 服务器存在时 (外部抓取), 也支持通过外部地址查询。
// 默认回退到读取 JSONL 文件。

// promQueryResponse Prometheus /api/v1/query_range 响应。
type promQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string  `json:"metric"`
			Values [][]interface{}    `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// promInstantResponse Prometheus /api/v1/query 响应。
type promInstantResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// queryPromRange 执行 PromQL range query, 返回时序点。
func queryPromRange(promURL, query string, start, end time.Time, step time.Duration) (*promQueryResponse, error) {
	u := fmt.Sprintf("%s/api/v1/query_range", promURL)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("query", query)
	q.Set("start", start.Format(time.RFC3339))
	q.Set("end", end.Format(time.RFC3339))
	q.Set("step", fmt.Sprintf("%.0fs", step.Seconds()))
	req.URL.RawQuery = q.Encode()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result promQueryResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// queryPromInstant 执行 PromQL instant query。
func queryPromInstant(promURL, query string) (*promInstantResponse, error) {
	u := fmt.Sprintf("%s/api/v1/query", promURL)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("query", query)
	req.URL.RawQuery = q.Encode()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result promInstantResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// AllMetricSummaries 从 Prometheus 查询各模块的最新指标摘要。
// 回退到本地 JSONL 文件 (如果 Prometheus 不可用)。
func (p *Provider) AllMetricSummaries() ([]*ModuleSummaryDTO, error) {
	// 尝试 Prometheus
	summaries := p.summariesFromProm()
	if len(summaries) > 0 {
		return summaries, nil
	}

	// 回退: 扫描 metrics/*.jsonl
	return p.summariesFromJSONL()
}

// summariesFromProm 从 Prometheus 查询各模块的瞬时指标, 构造摘要。
func (p *Provider) summariesFromProm() []*ModuleSummaryDTO {
	promURL := p.resolvePromURL()
	if promURL == "" {
		return nil
	}

	// 获取所有 claude_go_ 开头的指标
	resp, err := queryPromInstant(promURL, `{__name__=~"claude_go_.*"}`)
	if err != nil || resp.Status != "success" || len(resp.Data.Result) == 0 {
		return nil
	}

	// 按 module 分组
	grouped := map[string][]rawMetricEvent{}
	for _, r := range resp.Data.Result {
		mod := r.Metric["__name__"]
		if len(r.Value) < 2 {
			continue
		}
		tsFloat, _ := r.Value[0].(float64)
		valFloat, _ := r.Value[1].(string)
		val, _ := strconv.ParseFloat(valFloat, 64)
		evt := rawMetricEvent{
			Timestamp: time.Unix(int64(tsFloat), 0),
			Module:    mod,
			Name:      mod,
			Value:     val,
			Labels:    r.Metric,
		}
		delete(evt.Labels, "__name__")
		grouped[mod] = append(grouped[mod], evt)
	}

	var out []*ModuleSummaryDTO
	for mod, events := range grouped {
		out = append(out, summarizeModule(mod, events))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	return out
}

// summariesFromJSONL 从本地 JSONL 文件扫描指标摘要 (回退路径)。
func (p *Provider) summariesFromJSONL() ([]*ModuleSummaryDTO, error) {
	v, err := p.load("metrics:summary", func() (interface{}, error) {
		dir := p.pathIn("metrics")
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return []*ModuleSummaryDTO{}, nil
			}
			return nil, err
		}
		var out []*ModuleSummaryDTO
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			module := strings.TrimSuffix(e.Name(), ".jsonl")
			events, err := p.readMetricEvents(module)
			if err != nil || len(events) == 0 {
				continue
			}
			out = append(out, summarizeModule(module, events))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Module < out[j].Module })
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]*ModuleSummaryDTO), nil
}

// resolvePromURL 尝试解析 Prometheus 地址。
// 优先使用 CLAUDE_GO_PROMETHEUS_URL 环境变量, 否则返回空。
func (p *Provider) resolvePromURL() string {
	if u := os.Getenv("CLAUDE_GO_PROMETHEUS_URL"); u != "" {
		return u
	}
	// 默认回退: 尝试同进程的 /metrics (非 API 查询)
	// dashboard 本身不提供 PromQL 查询, 需要外部 Prometheus
	return ""
}

// ModuleMetricSummary 返回某模块摘要。
func (p *Provider) ModuleMetricSummary(module string) (*ModuleSummaryDTO, error) {
	if !safeName(module) {
		return nil, errors.New("invalid module name")
	}

	// 尝试 Prometheus
	promURL := p.resolvePromURL()
	if promURL != "" {
		query := fmt.Sprintf(`{__name__=~"claude_go_.*", job="claude-go"}`)
		resp, err := queryPromInstant(promURL, query)
		if err == nil && resp.Status == "success" && len(resp.Data.Result) > 0 {
			var events []rawMetricEvent
			for _, r := range resp.Data.Result {
				if len(r.Value) < 2 {
					continue
				}
				valFloat, _ := r.Value[1].(string)
				val, _ := strconv.ParseFloat(valFloat, 64)
				tsFloat, _ := r.Value[0].(float64)
				events = append(events, rawMetricEvent{
					Timestamp: time.Unix(int64(tsFloat), 0),
					Name:      module,
					Value:     val,
					Labels:    r.Metric,
				})
			}
			if len(events) > 0 {
				return summarizeModule(module, events), nil
			}
		}
	}

	// 回退: JSONL
	events, err := p.readMetricEvents(module)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return &ModuleSummaryDTO{Module: module, SnapshotTime: time.Now(), Metrics: map[string]*MetricStatDTO{}}, nil
	}
	return summarizeModule(module, events), nil
}

// ModuleMetricEvents 返回某模块原始事件 (用于绘制时序图), 可按 limit/since 过滤。
func (p *Provider) ModuleMetricEvents(module string, limit int, since time.Time) ([]MetricEventDTO, error) {
	if !safeName(module) {
		return nil, errors.New("invalid module name")
	}

	// 尝试 Prometheus range query
	promURL := p.resolvePromURL()
	if promURL != "" {
		end := time.Now()
		start := end.Add(-24 * time.Hour)
		if !since.IsZero() {
			start = since
		}
		query := fmt.Sprintf(`{__name__=~"claude_go_.*", module="%s"}`, module)
		resp, err := queryPromRange(promURL, query, start, end, 60*time.Second)
		if err == nil && resp.Status == "success" && len(resp.Data.Result) > 0 {
			var events []MetricEventDTO
			for _, r := range resp.Data.Result {
				for _, v := range r.Values {
					if len(v) < 2 {
						continue
					}
					tsFloat, _ := v[0].(float64)
					valFloat, _ := v[1].(string)
					val, _ := strconv.ParseFloat(valFloat, 64)
					events = append(events, MetricEventDTO{
						Timestamp: time.Unix(int64(tsFloat), 0),
						Module:    module,
						Name:      r.Metric["__name__"],
						Value:     val,
						Labels:    r.Metric,
					})
				}
			}
			if len(events) > 0 {
				if limit > 0 && len(events) > limit {
					events = events[len(events)-limit:]
				}
				return events, nil
			}
		}
	}

	// 回退: JSONL
	events, err := p.readMetricEvents(module)
	if err != nil {
		return nil, err
	}
	out := make([]MetricEventDTO, 0, len(events))
	for _, e := range events {
		if !since.IsZero() && e.Timestamp.Before(since) {
			continue
		}
		out = append(out, MetricEventDTO{
			Timestamp: e.Timestamp, Module: e.Module, Name: e.Name, Value: e.Value,
			Labels: e.Labels, RunID: e.RunID,
		})
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (p *Provider) readMetricEvents(module string) ([]rawMetricEvent, error) {
	key := "metrics:events:" + module
	v, err := p.load(key, func() (interface{}, error) {
		path := p.pathIn("metrics", module+".jsonl")
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return []rawMetricEvent{}, nil
			}
			return nil, err
		}
		var out []rawMetricEvent
		for _, line := range splitLines(data) {
			if len(line) == 0 {
				continue
			}
			var evt rawMetricEvent
			if json.Unmarshal(line, &evt) == nil {
				out = append(out, evt)
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]rawMetricEvent), nil
}

func summarizeModule(module string, events []rawMetricEvent) *ModuleSummaryDTO {
	grouped := map[string][]float64{}
	for _, e := range events {
		grouped[e.Name] = append(grouped[e.Name], e.Value)
	}
	out := &ModuleSummaryDTO{
		Module: module, SnapshotTime: time.Now(),
		Metrics:    map[string]*MetricStatDTO{},
		EventCount: len(events),
	}
	for name, vs := range grouped {
		recent := vs
		if len(recent) > 100 {
			recent = recent[len(recent)-100:]
		}
		out.Metrics[name] = computeStat(name, recent)
	}
	// 告警: 使用简单的前半/后半对比
	for name, vs := range grouped {
		if alert := detectTrend(name, vs); alert != "" {
			out.TrendAlerts = append(out.TrendAlerts, alert)
		}
	}
	return out
}

func computeStat(name string, values []float64) *MetricStatDTO {
	n := len(values)
	if n == 0 {
		return &MetricStatDTO{Name: name}
	}
	sum := 0.0
	minV, maxV := values[0], values[0]
	for _, v := range values {
		sum += v
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	avg := sum / float64(n)
	variance := 0.0
	for _, v := range values {
		variance += (v - avg) * (v - avg)
	}
	stdDev := math.Sqrt(variance / float64(n))
	trend := "stable"
	if n >= 4 {
		mid := n / 2
		first := avg2(values[:mid])
		second := avg2(values[mid:])
		delta := (second - first) / (math.Abs(first) + 1e-10)
		if delta > 0.1 {
			trend = "improving"
		} else if delta < -0.1 {
			trend = "degrading"
		}
	}
	return &MetricStatDTO{
		Name: name, Count: n, Last: values[n-1],
		Avg: avg, Min: minV, Max: maxV, StdDev: stdDev, Trend: trend,
	}
}

func avg2(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := 0.0
	for _, v := range vs {
		s += v
	}
	return s / float64(len(vs))
}

// detectTrend 对某些关键指标给出简洁的告警, 仅对 ratio/rate 类指标生效。
func detectTrend(name string, vs []float64) string {
	if len(vs) < 8 {
		return ""
	}
	mid := len(vs) / 2
	first, second := avg2(vs[:mid]), avg2(vs[mid:])
	denom := math.Abs(first) + 1e-10
	delta := (second - first) / denom
	// 下降超过 25% 的 rate/ratio 类指标告警
	if strings.Contains(name, "rate") || strings.Contains(name, "ratio") {
		if delta < -0.25 {
			return name + " 显著下降 (" + sprintf("%.1f", delta*100) + "%)"
		}
	}
	// 错误计数显著上升也告警
	if strings.Contains(name, "error") {
		if second-first > math.Max(1, first*0.5) {
			return name + " 错误明显增加"
		}
	}
	return ""
}

// ============== Swarm Intel & Cron Prometheus Export ==============

// ExportSwarmIntelMetrics 从 swarm_intel 的 runs.jsonl 读取指标并写入 Prometheus。
func (p *Provider) ExportSwarmIntelMetrics() {
	dir := p.pathIn("metrics")
	path := filepath.Join(dir, "runs.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return // 无数据, 静默跳过
	}

	type runMetrics struct {
		RunType        string  `json:"type"`
		TotalLatencyMs int64   `json:"total_latency_ms"`
		LLMCallCount   int     `json:"llm_call_count"`
		Consensus      float64 `json:"consensus"`
		BrierScore     float64 `json:"brier_score"`
		Diversity      float64 `json:"diversity_score"`
		DebateSkipped  bool    `json:"debate_skipped"`
	}

	var totalRuns, totalLLMCalls, debateSkips int
	var sumConsensus, sumBrier, sumDiversity, sumLatency float64

	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var m runMetrics
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		lbl := map[string]string{"run_type": m.RunType}
		totalRuns++
		totalLLMCalls += m.LLMCallCount
		sumConsensus += m.Consensus
		sumBrier += m.BrierScore
		sumDiversity += m.Diversity
		sumLatency += float64(m.TotalLatencyMs)
		if m.DebateSkipped {
			debateSkips++
		}

		metrics.RecordPromMetric("swarm", metrics.MSwarmRunCount, 1, lbl)
		metrics.RecordPromMetric("swarm", metrics.MSwarmConsensus, m.Consensus, lbl)
		metrics.RecordPromMetric("swarm", metrics.MSwarmBrierScore, m.BrierScore, lbl)
		metrics.RecordPromMetric("swarm", metrics.MSwarmDiversity, m.Diversity, lbl)
		metrics.RecordPromMetric("swarm", metrics.MSwarmLatencyMs, float64(m.TotalLatencyMs), lbl)
		metrics.RecordPromMetric("swarm", metrics.MSwarmLLMCalls, float64(m.LLMCallCount), lbl)
	}

	if totalRuns > 0 {
		n := float64(totalRuns)
		metrics.RecordPromMetric("swarm", metrics.MSwarmDebateSkipRate, float64(debateSkips)/n, map[string]string{})
	}
}

// ExportCronMetrics 从 cron_jobs.json 读取指标并写入 Prometheus。
func (p *Provider) ExportCronMetrics() {
	jobs, err := p.ListCronJobs()
	if err != nil {
		return
	}

	var totalEnabled int
	for _, j := range jobs {
		lbl := map[string]string{"job_name": j.Name, "job_type": j.JobType}
		if j.Enabled {
			totalEnabled++
		}
		if j.RunCount > 0 {
			metrics.RecordPromMetric("cron", metrics.MCronRunCount, float64(j.RunCount), lbl)
		}
		metrics.RecordPromMetric("cron", metrics.MCronSuccessCount, float64(j.RunCount-j.FailCount), lbl)
		if j.FailCount > 0 {
			metrics.RecordPromMetric("cron", metrics.MCronFailCount, float64(j.FailCount), lbl)
		}
	}

	metrics.RecordPromMetric("cron", metrics.MCronRunCount, float64(totalEnabled), map[string]string{})
}
