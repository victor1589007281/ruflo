// Package evidence 定义排障 Agent 的证据模型与门控。
//
// 设计约束（见 ops-rca-agent-design.md §3）：
//   - 事实由采集器产出，模型只做解释；结论必须绑定 evidence_id。
//   - 证据分级 E0–E4，根因结论最低门槛是 E3（多元融合）。
//   - 探针失效（非零退出/空输出/缺权限）不得当作“值为 0/不存在”，
//     必须显式标 ProbeValid=false。
package evidence

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Grade 证据等级。
type Grade string

const (
	// E0 原始信号：未经解析的字节/行，不能单独支持结论。
	GradeE0 Grade = "E0"
	// E1 背景上下文：版本、配置、拓扑、历史，只用于解释。
	GradeE1 Grade = "E1"
	// E2 单点观测：一次查询/一条日志，能被解释但不能定因。
	GradeE2 Grade = "E2"
	// E3 多元融合：两个以上独立来源相互印证。
	GradeE3 Grade = "E3"
	// E4 直接因果：证据本身直接给出因果（如死锁报告点名阻塞事务）。
	GradeE4 Grade = "E4"
)

// Valid 报告等级是否合法。
func (g Grade) Valid() bool {
	switch g {
	case GradeE0, GradeE1, GradeE2, GradeE3, GradeE4:
		return true
	}
	return false
}

// Rank 返回等级序号，便于比较。非法等级返回 -1。
func (g Grade) Rank() int {
	switch g {
	case GradeE0:
		return 0
	case GradeE1:
		return 1
	case GradeE2:
		return 2
	case GradeE3:
		return 3
	case GradeE4:
		return 4
	}
	return -1
}

// AtLeast 报告 g 是否不低于 min。
func (g Grade) AtLeast(min Grade) bool { return g.Rank() >= min.Rank() }

// Type 证据类型。
type Type string

const (
	TypeSignal   Type = "signal"
	TypeMetric   Type = "metric"
	TypeLog      Type = "log"
	TypeTrace    Type = "trace"
	TypeEvent    Type = "event"
	TypeChange   Type = "change"
	TypeTopology Type = "topology"
	TypeCode     Type = "code"
	TypeConfig   Type = "config"
	TypeHistory  Type = "history"
)

// Valid 报告类型是否合法。
func (t Type) Valid() bool {
	switch t {
	case TypeSignal, TypeMetric, TypeLog, TypeTrace, TypeEvent,
		TypeChange, TypeTopology, TypeCode, TypeConfig, TypeHistory:
		return true
	}
	return false
}

// CausalRole 证据在假设中的角色。
type CausalRole string

const (
	RoleSupport    CausalRole = "support"
	RoleContradict CausalRole = "contradict"
	RoleNeutral    CausalRole = "neutral"
	RoleRootCause  CausalRole = "root_cause"
)

// Valid 报告角色是否合法；空串等价于 neutral。
func (r CausalRole) Valid() bool {
	switch r {
	case "", RoleSupport, RoleContradict, RoleNeutral, RoleRootCause:
		return true
	}
	return false
}

// Subject 证据指向的对象。
type Subject struct {
	Kind      string `json:"kind"` // instance|database|table|pod|node|process|service|repo
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Instance  string `json:"instance,omitempty"`
}

// Signal 证据里的结构化读数。
type Signal struct {
	Name      string `json:"name"`
	Value     any    `json:"value,omitempty"`
	Unit      string `json:"unit,omitempty"`
	Baseline  any    `json:"baseline,omitempty"`
	Delta     any    `json:"delta,omitempty"`
	Threshold string `json:"threshold,omitempty"`
}

// TimeWindow 证据覆盖的时间窗。
type TimeWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Evidence 一条可复核证据。
type Evidence struct {
	ID          string      `json:"id"`
	IncidentID  string      `json:"incident_id,omitempty"`
	Type        Type        `json:"type"`
	Source      string      `json:"source"` // 采集器/查询/文件，必须能复跑
	CollectedAt time.Time   `json:"collected_at"`
	Window      *TimeWindow `json:"window,omitempty"`
	Subject     Subject     `json:"subject"`
	Signal      Signal      `json:"signal,omitempty"`
	Grade       Grade       `json:"grade"`
	Reliability float64     `json:"reliability"` // 0–1
	Summary     string      `json:"summary"`
	RawRef      string      `json:"raw_ref,omitempty"`
	RawPreview  string      `json:"raw_preview,omitempty"`
	Links       []string    `json:"links,omitempty"` // 关联 evidence_id
	Role        CausalRole  `json:"role,omitempty"`
	ProbeValid  bool        `json:"probe_valid"`
	ProbeNote   string      `json:"probe_note,omitempty"`
}

// Validate 校验一条证据的最低结构要求。
func (e Evidence) Validate() error {
	var problems []string
	if strings.TrimSpace(e.ID) == "" {
		problems = append(problems, "id 为空")
	}
	if !e.Type.Valid() {
		problems = append(problems, fmt.Sprintf("type 非法: %q", e.Type))
	}
	if strings.TrimSpace(e.Source) == "" {
		problems = append(problems, "source 为空")
	}
	if strings.TrimSpace(e.Subject.Kind) == "" || strings.TrimSpace(e.Subject.Name) == "" {
		problems = append(problems, "subject.kind/name 为空")
	}
	if !e.Grade.Valid() {
		problems = append(problems, fmt.Sprintf("grade 非法: %q", e.Grade))
	}
	if e.Reliability < 0 || e.Reliability > 1 {
		problems = append(problems, fmt.Sprintf("reliability 越界: %v", e.Reliability))
	}
	if !e.Role.Valid() {
		problems = append(problems, fmt.Sprintf("role 非法: %q", e.Role))
	}
	if strings.TrimSpace(e.Summary) == "" {
		problems = append(problems, "summary 为空")
	}
	if len(problems) > 0 {
		return fmt.Errorf("evidence %q: %s", e.ID, strings.Join(problems, "; "))
	}
	return nil
}

// HypothesisStatus 假设状态。
type HypothesisStatus string

const (
	HypothesisProposed  HypothesisStatus = "proposed"
	HypothesisTesting   HypothesisStatus = "testing"
	HypothesisSupported HypothesisStatus = "supported"
	HypothesisRefuted   HypothesisStatus = "refuted"
	HypothesisInconcl   HypothesisStatus = "inconclusive"
)

// Valid 报告状态是否合法。
func (s HypothesisStatus) Valid() bool {
	switch s {
	case HypothesisProposed, HypothesisTesting, HypothesisSupported, HypothesisRefuted, HypothesisInconcl:
		return true
	}
	return false
}

// Hypothesis 可证伪假设。
type Hypothesis struct {
	ID          string           `json:"id"`
	Statement   string           `json:"statement"`
	Mechanism   string           `json:"mechanism,omitempty"`
	Predicts    []string         `json:"predicts"`
	Disconfirms []string         `json:"disconfirms"`
	Support     []string         `json:"support,omitempty"`
	Contradict  []string         `json:"contradict,omitempty"`
	Status      HypothesisStatus `json:"status"`
	Confidence  float64          `json:"confidence"`
	TestPlan    []string         `json:"test_plan,omitempty"`
	RootCause   bool             `json:"root_cause,omitempty"`
}

// Validate 校验假设的最低结构要求。
func (h Hypothesis) Validate() error {
	var problems []string
	if strings.TrimSpace(h.ID) == "" {
		problems = append(problems, "id 为空")
	}
	if strings.TrimSpace(h.Statement) == "" {
		problems = append(problems, "statement 为空")
	}
	if !h.Status.Valid() {
		problems = append(problems, fmt.Sprintf("status 非法: %q", h.Status))
	}
	if h.Confidence < 0 || h.Confidence > 1 {
		problems = append(problems, fmt.Sprintf("confidence 越界: %v", h.Confidence))
	}
	if len(h.Predicts) == 0 {
		problems = append(problems, "predicts 为空（假设必须可证伪）")
	}
	if len(h.Disconfirms) == 0 {
		problems = append(problems, "disconfirms 为空（没有反证条件的假设不可用）")
	}
	if problems != nil {
		return fmt.Errorf("hypothesis %q: %s", h.ID, strings.Join(problems, "; "))
	}
	return nil
}

// TimelinePoint 时间线上的一个采样点。
type TimelinePoint struct {
	At      time.Time          `json:"at"`
	Metrics map[string]float64 `json:"metrics,omitempty"`
	Labels  map[string]string  `json:"labels,omitempty"`
}

// Contradiction 两个证据口径不一致。
type Contradiction struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	EvidenceA   string `json:"evidence_a"`
	EvidenceB   string `json:"evidence_b"`
	Severity    string `json:"severity"` // low|medium|high
	Resolution  string `json:"resolution,omitempty"`
}

// Missing 缺失证据（显式输出，不用置信度话术掩盖）。
type Missing struct {
	What       string `json:"what"`
	Why        string `json:"why"`
	NextAction string `json:"next_action"`
}

// Sampling 记录真实采样口径。WindowSeconds 是日志/事件的回看窗，
// SpanSeconds 才是状态样本首末时间差；两者不能混称为趋势窗口。
type Sampling struct {
	Samples         int     `json:"samples"`
	IntervalSeconds float64 `json:"interval_seconds"`
	SpanSeconds     float64 `json:"span_seconds"`
	WindowSeconds   float64 `json:"window_seconds"`
	Note            string  `json:"note,omitempty"`
}

// Capability 把“本工具已支持”和“需要操作员在工具外补证”分开。
// Availability 只取 tool|manual|unsupported，报告层据此给出明确标签。
type Capability struct {
	Name         string `json:"name"`
	Availability string `json:"availability"`
	Detail       string `json:"detail"`
}

// CollectionInfo 是采集面的自述，避免模型从参数名推断不存在的能力。
type CollectionInfo struct {
	Sampling     *Sampling    `json:"sampling,omitempty"`
	Capabilities []Capability `json:"capabilities"`
}

// Bundle 一次排障会话的证据包。
type Bundle struct {
	IncidentID     string          `json:"incident_id"`
	Instance       string          `json:"instance"`
	Domain         string          `json:"domain,omitempty"` // mysql|k8s|linux
	GeneratedAt    time.Time       `json:"generated_at"`
	Window         TimeWindow      `json:"window"`
	Collection     CollectionInfo  `json:"collection"`
	Timeline       []TimelinePoint `json:"timeline"`
	Evidences      []Evidence      `json:"evidences"`
	Hypotheses     []Hypothesis    `json:"hypotheses"`
	Contradictions []Contradiction `json:"contradictions"`
	Missing        []Missing       `json:"missing"`
	Summary        string          `json:"summary,omitempty"`
}

// EvidenceByID 按 ID 取证据；不存在返回 nil。
func (b *Bundle) EvidenceByID(id string) *Evidence {
	if b == nil {
		return nil
	}
	for i := range b.Evidences {
		if b.Evidences[i].ID == id {
			return &b.Evidences[i]
		}
	}
	return nil
}

// Validate 校验证据包：ID 唯一、引用闭合、根因门控成立。
func (b *Bundle) Validate() error {
	if b == nil {
		return fmt.Errorf("bundle 为空")
	}
	var problems []string
	for i, c := range b.Collection.Capabilities {
		if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Detail) == "" {
			problems = append(problems, fmt.Sprintf("capabilities[%d]: name/detail 为空", i))
		}
		switch c.Availability {
		case "tool", "manual", "unsupported":
		default:
			problems = append(problems, fmt.Sprintf("capabilities[%d]: availability 非法: %q", i, c.Availability))
		}
	}
	seen := map[string]bool{}
	for i, e := range b.Evidences {
		if err := e.Validate(); err != nil {
			problems = append(problems, fmt.Sprintf("evidences[%d]: %v", i, err))
		}
		if seen[e.ID] {
			problems = append(problems, fmt.Sprintf("evidence id 重复: %q", e.ID))
		}
		seen[e.ID] = true
	}
	for i, h := range b.Hypotheses {
		if err := h.Validate(); err != nil {
			problems = append(problems, fmt.Sprintf("hypotheses[%d]: %v", i, err))
		}
		for _, id := range append(append([]string{}, h.Support...), h.Contradict...) {
			if !seen[id] {
				problems = append(problems, fmt.Sprintf("hypothesis %q 引用了不存在的证据 %q", h.ID, id))
			}
		}
		if h.RootCause {
			ok, why := b.RootCauseEligible(h)
			if !ok {
				problems = append(problems, fmt.Sprintf("hypothesis %q 不满足根因门控: %s", h.ID, strings.Join(why, "; ")))
			}
		}
	}
	for i, c := range b.Contradictions {
		if c.EvidenceA != "" && !seen[c.EvidenceA] {
			problems = append(problems, fmt.Sprintf("contradictions[%d] 引用不存在的证据 %q", i, c.EvidenceA))
		}
		if c.EvidenceB != "" && !seen[c.EvidenceB] {
			problems = append(problems, fmt.Sprintf("contradictions[%d] 引用不存在的证据 %q", i, c.EvidenceB))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("bundle 校验失败: %s", strings.Join(problems, "; "))
	}
	return nil
}

// RootCauseEligible 判断一个假设能否作为根因结论。
//
// 门控（缺一不可）：
//   - 状态为 supported；
//   - 至少一条 support 证据达到 E3；
//   - 没有未解释的 high 级矛盾指向该假设的支持证据。
func (b *Bundle) RootCauseEligible(h Hypothesis) (bool, []string) {
	var why []string
	if h.Status != HypothesisSupported {
		why = append(why, "status 不是 supported")
	}
	hasE3 := false
	for _, id := range h.Support {
		if e := b.EvidenceByID(id); e != nil && e.ProbeValid && e.Grade.AtLeast(GradeE3) {
			hasE3 = true
			break
		}
	}
	if !hasE3 {
		why = append(why, "没有有效的 E3+ 支持证据")
	}
	support := map[string]bool{}
	for _, id := range h.Support {
		support[id] = true
	}
	for _, c := range b.Contradictions {
		if c.Severity != "high" || strings.TrimSpace(c.Resolution) != "" {
			continue
		}
		if support[c.EvidenceA] || support[c.EvidenceB] {
			why = append(why, "存在未解释的 high 级矛盾: "+c.ID)
		}
	}
	return len(why) == 0, why
}

// MarshalJSON 保证 nil 切片序列化为 [] 而不是 null。
//
// 面板与下游脚本都按数组消费；null 会让“没有证据”与“字段缺失”长得一样。
func (b Bundle) MarshalJSON() ([]byte, error) {
	type alias Bundle
	a := alias(b)
	if a.Evidences == nil {
		a.Evidences = []Evidence{}
	}
	if a.Hypotheses == nil {
		a.Hypotheses = []Hypothesis{}
	}
	if a.Timeline == nil {
		a.Timeline = []TimelinePoint{}
	}
	if a.Contradictions == nil {
		a.Contradictions = []Contradiction{}
	}
	if a.Missing == nil {
		a.Missing = []Missing{}
	}
	if a.Collection.Capabilities == nil {
		a.Collection.Capabilities = []Capability{}
	}
	return json.Marshal(a)
}
