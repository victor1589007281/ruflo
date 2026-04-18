package dashboard

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
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

// extractAdversaryRounds 解析 blackboard.json 中的 eval-roundN-score 字符串。
// 已知格式: "正确=7 完整=3 安全=6 质量=8 对齐=5 通过:false"
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
	byRound := map[int]*AdversaryRoundDTO{}
	for _, it := range items {
		k := it.Key
		if !strings.HasPrefix(k, "eval-round") || !strings.HasSuffix(k, "-score") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(k, "eval-round"), "-score")
		round, err := strconvItoa(numStr)
		if err != nil {
			continue
		}
		raw, ok := it.Value.(string)
		if !ok {
			continue
		}
		r := parseEvalScoreLine(round, raw)
		byRound[round] = &r
	}
	rounds := make([]AdversaryRoundDTO, 0, len(byRound))
	for _, r := range byRound {
		rounds = append(rounds, *r)
	}
	sort.Slice(rounds, func(i, j int) bool { return rounds[i].Round < rounds[j].Round })
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

// Blackboard 读取某个团队的黑板 JSON。
func (p *Provider) Blackboard(name string) (map[string]interface{}, error) {
	if !safeName(name) {
		return nil, errors.New("invalid team name")
	}
	data, err := os.ReadFile(p.pathIn("teams", name, "blackboard.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]interface{}{}, nil
		}
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
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

// AllMetricSummaries 扫描 metrics/*.jsonl, 返回每个模块的摘要。
func (p *Provider) AllMetricSummaries() ([]*ModuleSummaryDTO, error) {
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

// ModuleMetricSummary 返回某模块摘要。
func (p *Provider) ModuleMetricSummary(module string) (*ModuleSummaryDTO, error) {
	if !safeName(module) {
		return nil, errors.New("invalid module name")
	}
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
