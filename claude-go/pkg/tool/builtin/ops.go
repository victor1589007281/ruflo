package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/domain/k8s"
	"github.com/anthropic/claude-go/pkg/ops/domain/linux"
	"github.com/anthropic/claude-go/pkg/ops/domain/mysql"
	"github.com/anthropic/claude-go/pkg/ops/evidence"
	"github.com/anthropic/claude-go/pkg/ops/guard"
	opsreport "github.com/anthropic/claude-go/pkg/ops/report"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// OpsInvestigateTool 是排障 Agent 的只读取数入口（MySQL / Kubernetes / Linux）。
//
// 为什么不是 Bash + mysql/kubectl：Bash 在本仓是主机进程直通，会把“只读”完全
// 交给模型自觉；而这个工具把采集语句/命令、只读账号、超时、证据归一化都固定在
// 代码里。模型能选的只有目标（白名单实例 / kubeconfig context / 本机）和窗口，
// 不能选采集内容，也不能传任意 SQL/argv。
//
// MySQL DSN 只从状态目录/环境变量读取，永远不出现在工具入参或结果里；kubectl
// 只接受 kubeconfig 中已存在的 context 和合法 namespace。目标是让模型即使被
// 注入，也拿不到凭证、改不了目标集群/主机。
type OpsInvestigateTool struct {
	// Now 可注入，便于测试。
	Now func() time.Time
}

// NewOpsInvestigateTool 构造工具。
func NewOpsInvestigateTool() *OpsInvestigateTool { return &OpsInvestigateTool{Now: time.Now} }

// Name 返回工具名。
func (t *OpsInvestigateTool) Name() string { return "OpsInvestigate" }

// IsReadOnly 对目标系统只读；落盘只写本地证据档案。
func (t *OpsInvestigateTool) IsReadOnly(_ json.RawMessage) bool { return true }

// IsConcurrencySafe 不同实例可并行采集。
func (t *OpsInvestigateTool) IsConcurrencySafe(_ json.RawMessage) bool { return true }

// CheckPermissions 工具自身不设门槛，权限交由上层策略与只读账号裁决。
func (t *OpsInvestigateTool) CheckPermissions(_ json.RawMessage, _ *tool.ToolContext) *types.PermissionResult {
	return nil
}

// Description 返回给模型的说明（含铁律，避免把 E0/E2 当根因）。
func (t *OpsInvestigateTool) Description() string {
	return `Collect read-only evidence from an allow-listed target and run deterministic analysis.
Domains:
- mysql: instance = allow-listed datasource name; fixed status/processlist/InnoDB snapshot.
- k8s: context = existing kubeconfig context (required); namespace optional; fixed read-only
  kubectl get for nodes/pods/events/PVC/workloads.
- linux: this host; fixed ps/df/systemctl/journalctl/ss plus /proc sampling.

Returns a compact reasoning view plus a local artifact path. The view retains every
evidence ID/grade/caliber, hypothesis, contradiction and missing item; raw previews
and point-by-point timelines stay in the full artifact so you do not need to repeat
a second full report. Explicit format=json returns the full bundle for debugging.
Collection capabilities distinguish tool-supported collection from manual follow-up.
Facts come from fixed collectors, not from you. Rules:
- Only cite evidence IDs that appear in the result; never invent IDs or values.
- A root cause requires a supported hypothesis with E3+ evidence and no unresolved
  high-severity contradiction. If the tool reports no eligible root cause, say so.
- Empty output / probe_valid=false means the probe failed — it does NOT mean the value is zero.
- Optional "sql" (MySQL only) runs one read-only statement and returns raw rows graded E0;
  it cannot by itself support a conclusion. There is no arbitrary host/DSN/argv input.`
}

// InputSchema 声明入参。
func (t *OpsInvestigateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"domain": {"type": "string", "enum": ["mysql", "k8s", "linux"], "description": "Evidence domain (default mysql)."},
			"instance": {"type": "string", "description": "MySQL: allow-listed instance name (from operator datasource config)."},
			"context": {"type": "string", "description": "Kubernetes: kubeconfig context name (required for domain=k8s)."},
			"namespace": {"type": "string", "description": "Kubernetes: optional namespace; empty means all namespaces."},
			"target": {"type": "string", "description": "Linux: optional label for the report (default hostname); does not select a remote host."},
			"window_minutes": {"type": "integer", "minimum": 1, "maximum": 1440, "description": "Evidence window in minutes (default 30)."},
			"samples": {"type": "integer", "minimum": 1, "maximum": 5, "description": "Linux/MySQL sample count (default 3; <3 weakens trend evidence)."},
			"format": {"type": "string", "enum": ["markdown", "json"], "description": "Report format (default markdown)."},
			"sql": {"type": "string", "description": "MySQL only: optional single read-only SQL statement (SELECT/SHOW/EXPLAIN/DESC/DESCRIBE/WITH). Returns raw rows graded E0."}
		}
	}`)
}

type opsInput struct {
	Domain        string `json:"domain"`
	Instance      string `json:"instance"`
	Context       string `json:"context"`
	Namespace     string `json:"namespace"`
	Target        string `json:"target"`
	WindowMinutes int    `json:"window_minutes"`
	Samples       int    `json:"samples"`
	Format        string `json:"format"`
	SQL           string `json:"sql"`
}

// Call 按域采集只读证据并返回报告。
func (t *OpsInvestigateTool) Call(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (*tool.ToolResult, error) {
	var in opsInput
	if err := json.Unmarshal(input, &in); err != nil {
		return opsErrResult("parse error: %v", err), nil
	}
	domain := strings.ToLower(strings.TrimSpace(in.Domain))
	if domain == "" {
		domain = "mysql"
	}
	switch domain {
	case "mysql":
		return t.callMySQL(ctx, in)
	case "k8s":
		return t.callK8s(ctx, in)
	case "linux":
		return t.callLinux(ctx, in)
	default:
		return opsErrResult("domain %q 不支持；可选 mysql|k8s|linux", in.Domain), nil
	}
}

func (t *OpsInvestigateTool) callMySQL(ctx context.Context, in opsInput) (*tool.ToolResult, error) {
	in.Instance = strings.TrimSpace(in.Instance)
	if in.Instance == "" {
		return opsErrResult("instance is required for domain=mysql"), nil
	}

	dsn, err := resolveOpsDSN(in.Instance)
	if err != nil {
		return opsErrResult("%v", err), nil
	}

	cfg := mysql.Config{Instance: in.Instance, DSN: dsn, Now: t.now}
	if in.WindowMinutes > 0 {
		cfg.Window = time.Duration(in.WindowMinutes) * time.Minute
	}
	if in.Samples > 0 {
		cfg.Samples = in.Samples
	}

	col, err := mysql.New(cfg)
	if err != nil {
		// 连接失败同样不能伪装成“实例正常”。错误里已脱敏 DSN。
		return opsErrResult("connect failed: %v（实例不可达时不要推断“无异常”）", err), nil
	}
	defer col.Close()

	callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	if strings.TrimSpace(in.SQL) != "" {
		return t.runSQL(callCtx, col, in.SQL)
	}

	bundle, err := col.Collect(callCtx)
	if err != nil {
		return opsErrResult("collect failed: %v", err), nil
	}
	return t.finish(bundle, in.Format)
}

func (t *OpsInvestigateTool) callK8s(ctx context.Context, in opsInput) (*tool.ToolResult, error) {
	if strings.TrimSpace(in.SQL) != "" {
		return opsErrResult("sql 只支持 domain=mysql；Kubernetes 采集内容由固定只读 kubectl 命令决定"), nil
	}
	cfg := k8s.Config{
		Context:   strings.TrimSpace(in.Context),
		Namespace: strings.TrimSpace(in.Namespace),
		Now:       t.now,
	}
	if in.WindowMinutes > 0 {
		cfg.Window = time.Duration(in.WindowMinutes) * time.Minute
	}
	col, err := k8s.New(cfg)
	if err != nil {
		return opsErrResult("k8s collector: %v（采集不可用不等于集群健康）", err), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	bundle, err := col.Collect(callCtx)
	if err != nil {
		return opsErrResult("collect failed: %v", err), nil
	}
	return t.finish(bundle, in.Format)
}

func (t *OpsInvestigateTool) callLinux(ctx context.Context, in opsInput) (*tool.ToolResult, error) {
	if strings.TrimSpace(in.SQL) != "" {
		return opsErrResult("sql 只支持 domain=mysql；Linux 采集内容由固定只读命令与 /proc 决定"), nil
	}
	cfg := linux.Config{Target: strings.TrimSpace(in.Target), Now: t.now}
	if in.WindowMinutes > 0 {
		cfg.Window = time.Duration(in.WindowMinutes) * time.Minute
	}
	if in.Samples > 0 {
		cfg.Samples = in.Samples
	}
	col, err := linux.New(cfg)
	if err != nil {
		return opsErrResult("linux collector: %v", err), nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	bundle, err := col.Collect(callCtx)
	if err != nil {
		return opsErrResult("collect failed: %v", err), nil
	}
	return t.finish(bundle, in.Format)
}

// finish 统一校验/渲染/落盘。报告格式由 bundle.Domain 决定。
func (t *OpsInvestigateTool) finish(bundle *evidence.Bundle, format string) (*tool.ToolResult, error) {
	if err := bundle.Validate(); err != nil {
		return opsErrResult("evidence bundle invalid: %v", err), nil
	}

	var body string
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		out, err := json.MarshalIndent(bundle, "", "  ")
		if err != nil {
			return opsErrResult("marshal bundle: %v", err), nil
		}
		body = string(out)
	default:
		switch bundle.Domain {
		case "k8s":
			body = k8s.RenderMarkdown(bundle)
		case "linux":
			body = linux.RenderMarkdown(bundle)
		default:
			body = mysql.RenderMarkdown(bundle)
		}
	}

	artifact, artErr := writeIncidentArtifact(bundle, body)
	view := "full"
	if artErr == nil && !strings.EqualFold(strings.TrimSpace(format), "json") {
		body = opsreport.RenderModelView(bundle)
		view = "compact"
	}
	header := fmt.Sprintf("domain=%s subject=%s incident=%s evidence=%d hypotheses=%d contradictions=%d missing=%d view=%s\n",
		bundle.Domain, bundle.Instance, bundle.IncidentID, len(bundle.Evidences), len(bundle.Hypotheses),
		len(bundle.Contradictions), len(bundle.Missing), view)
	if artErr == nil {
		header += "artifact=" + artifact + "\n"
	} else {
		header += "artifact=unavailable (" + artErr.Error() + ")\n"
	}
	return &tool.ToolResult{Content: header + "\n" + body}, nil
}

func (t *OpsInvestigateTool) runSQL(ctx context.Context, col *mysql.Collector, q string) (*tool.ToolResult, error) {
	if err := guard.ValidateReadOnly(q); err != nil {
		return opsErrResult("read-only guard rejected SQL: %v", err), nil
	}
	cols, rows, truncated, err := col.QueryReadOnly(ctx, q, 200)
	if err != nil {
		return opsErrResult("query failed: %v（失败不等于结果为空/为 0）", err), nil
	}
	var sb strings.Builder
	sb.WriteString("grade=E0 (raw result, cannot support a root cause by itself)\n")
	sb.WriteString("probe_valid=true\n")
	sb.WriteString("columns: " + strings.Join(cols, " | ") + "\n")
	for _, r := range rows {
		sb.WriteString(strings.Join(r, " | ") + "\n")
	}
	if truncated {
		sb.WriteString("...(truncated at 200 rows)\n")
	}
	fmt.Fprintf(&sb, "rows=%d\n", len(rows))
	return &tool.ToolResult{Content: sb.String()}, nil
}

func (t *OpsInvestigateTool) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// ---- datasource 解析 --------------------------------------------------------

// opsDatasources 是运维预先写入的只读 DSN 清单。
//
// 路径优先级：CLAUDE_GO_OPS_DATASOURCES > <stateDir>/ops-datasources.json。
// 文件权限应在创建时设为 0600；本工具只读不写。
type opsDatasources struct {
	Instances map[string]opsInstance `json:"instances"`
}

type opsInstance struct {
	DSN         string `json:"dsn"`
	Description string `json:"description,omitempty"`
}

func resolveOpsDSN(instance string) (string, error) {
	// 1) 环境变量：CLAUDE_GO_OPS_DSN_<NORMALIZED>
	envKey := "CLAUDE_GO_OPS_DSN_" + opsEnvSuffix(instance)
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v, nil
	}

	// 2) 状态目录清单
	path := strings.TrimSpace(os.Getenv("CLAUDE_GO_OPS_DATASOURCES"))
	if path == "" {
		state := strings.TrimSpace(os.Getenv("CLAUDE_GO_STATE_DIR"))
		if state == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("无法定位状态目录，请设置 CLAUDE_GO_OPS_DATASOURCES")
			}
			state = filepath.Join(home, ".claude-go")
		}
		path = filepath.Join(state, "ops-datasources.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("实例 %q 未配置（读取 %s 失败: %v）", instance, path, err)
	}
	var cfg opsDatasources
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("datasource 配置解析失败: %v", err)
	}
	got, ok := cfg.Instances[instance]
	if !ok || strings.TrimSpace(got.DSN) == "" {
		names := make([]string, 0, len(cfg.Instances))
		for k := range cfg.Instances {
			names = append(names, k)
		}
		sort.Strings(names)
		return "", fmt.Errorf("实例 %q 不在白名单；可用实例: %s", instance, strings.Join(names, ", "))
	}
	return got.DSN, nil
}

// opsEnvSuffix 把实例名归一成环境变量后缀：大写，非字母数字变下划线。
func opsEnvSuffix(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// writeIncidentArtifact 把证据包落到本地档案，便于人工复核与复跑对比。
// 写失败不影响工具结果 —— 档案是辅助，不是事实来源。
func writeIncidentArtifact(b *evidence.Bundle, report string) (string, error) {
	state := strings.TrimSpace(os.Getenv("CLAUDE_GO_OPS_ARTIFACT_DIR"))
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		state = filepath.Join(home, ".claude-go", "ops", "incidents")
	}
	dir := filepath.Join(state, b.IncidentID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle.json"), raw, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(report), 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

func opsErrResult(format string, args ...any) *tool.ToolResult {
	return &tool.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}
