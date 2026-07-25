package agent

// 动态工作流(Dynamic Workflow): 让工作流可在运行时由配置文件/API 定义并注册, 复用现有执行引擎,
// 无需改 Go 代码重新编译。设计见 docs/dynamic-workflow-design.md。
//
// 与现有(静态)工作流的唯一本质差异是"定义来源": 静态=硬编码 Go 工厂(workflowRegistry);
// 动态=运行时 JSON。数据结构(WorkflowDef)与执行引擎(系统A Coordinator / 系统B orchestrator,
// 由 Mode 决定)完全相同。因为运行时数据不可信, 增加了 Validate() + 声明式门禁字段 + 模式白名单。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// dynamicModes 允许"动态定义"的工作流模式: 纯按 WorkflowDef 数据驱动、无伴生硬编码逻辑。
// creative_media / app_composite / game_composite / novel_* 等有与具体阶段名/格式列表绑定的
// 伴生代码, 不允许动态定义(见 docs §1.4 地雷 B)。
var dynamicModes = map[string]bool{
	"pipeline":        true,
	"fanout":          true,
	"adversarial":     true,
	"adversarial_dev": true,
	"orchestrated":    true,
	"graph":           true, // design/01 M1: 纯数据图模式, 无伴生硬编码, 天然可动态定义
}

var (
	customMu  sync.RWMutex
	customWFs = map[string]*WorkflowDef{} // 运行时注册的动态工作流, 与内置 workflowRegistry 分离
)

// RegisterWorkflow 校验并注册一个动态工作流到自定义注册表 (带锁, 防与团队创建 goroutine 的并发读 race)。
func RegisterWorkflow(def *WorkflowDef, roles *RoleRegistry) error {
	if def == nil {
		return fmt.Errorf("workflow 定义为空")
	}
	if _, builtin := workflowRegistry[def.Name]; builtin {
		return fmt.Errorf("工作流名 %q 与内置工作流冲突", def.Name)
	}
	if err := def.Validate(roles); err != nil {
		return err
	}
	cp := *def
	cp.Custom = true
	cp.Stages = append([]StageDef(nil), def.Stages...)
	customMu.Lock()
	customWFs[def.Name] = &cp
	customMu.Unlock()
	return nil
}

// UnregisterWorkflow 删除一个动态工作流 (内置工作流不可删)。
func UnregisterWorkflow(name string) error {
	customMu.Lock()
	_, ok := customWFs[name]
	if ok {
		delete(customWFs, name)
	}
	customMu.Unlock()
	if !ok {
		if _, builtin := workflowRegistry[name]; builtin {
			return fmt.Errorf("内置工作流 %q 不可删除", name)
		}
		return fmt.Errorf("动态工作流 %q 不存在", name)
	}
	return nil
}

// getCustomWorkflow 返回自定义工作流的副本 (避免共享引用); 不存在返回 nil。
func getCustomWorkflow(name string) *WorkflowDef {
	customMu.RLock()
	d, ok := customWFs[name]
	customMu.RUnlock()
	if !ok {
		return nil
	}
	cp := *d
	cp.Stages = append([]StageDef(nil), d.Stages...)
	return &cp
}

// customWorkflowFlags 仅读取自定义工作流的门禁声明字段 (门禁热路径用, 不复制 stages)。
func customWorkflowFlags(name string) (producesCode bool, qualityGate string, isCustom bool) {
	customMu.RLock()
	defer customMu.RUnlock()
	d, ok := customWFs[name]
	if !ok {
		return false, "", false
	}
	return d.ProducesCode, d.QualityGate, true
}

// listCustomWorkflows 返回所有自定义工作流的副本。
func listCustomWorkflows() []WorkflowDef {
	customMu.RLock()
	defer customMu.RUnlock()
	out := make([]WorkflowDef, 0, len(customWFs))
	for _, d := range customWFs {
		cp := *d
		cp.Stages = append([]StageDef(nil), d.Stages...)
		out = append(out, cp)
	}
	return out
}

// Validate 校验工作流可被安全执行 (动态定义注册前必过)。
func (wf *WorkflowDef) Validate(roles *RoleRegistry) error {
	if strings.TrimSpace(wf.Name) == "" {
		return fmt.Errorf("workflow.name 不能为空")
	}
	if len(wf.Stages) == 0 {
		return fmt.Errorf("workflow %q 至少需要一个 stage", wf.Name)
	}
	if !dynamicModes[wf.Mode] {
		return fmt.Errorf("mode %q 不支持动态定义(含伴生硬编码); 可选: pipeline/fanout/graph/adversarial/adversarial_dev/orchestrated", wf.Mode)
	}
	names := make(map[string]bool, len(wf.Stages))
	for _, s := range wf.Stages {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("存在未命名的 stage")
		}
		if names[s.Name] {
			return fmt.Errorf("stage 名重复: %q", s.Name)
		}
		names[s.Name] = true
		// 角色必须能解析(注册表里有), 或提供内联 Prompt(否则 agent 没有 system prompt)
		roleOK := roles != nil && roles.Get(s.Role) != nil
		if !roleOK && strings.TrimSpace(s.Prompt) == "" {
			return fmt.Errorf("stage %q: 角色 %q 未注册且未提供内联 prompt", s.Name, s.Role)
		}
	}
	for _, s := range wf.Stages {
		for _, dep := range s.DependsOn {
			if !names[dep] {
				return fmt.Errorf("stage %q 依赖了不存在的阶段 %q", s.Name, dep)
			}
		}
	}
	return validateNoCycle(wf.Stages)
}

// validateNoCycle 用 Kahn 拓扑排序检测依赖环。
func validateNoCycle(stages []StageDef) error {
	indeg := make(map[string]int, len(stages))
	adj := make(map[string][]string, len(stages))
	for _, s := range stages {
		if _, ok := indeg[s.Name]; !ok {
			indeg[s.Name] = 0
		}
	}
	for _, s := range stages {
		for _, dep := range s.DependsOn {
			adj[dep] = append(adj[dep], s.Name)
			indeg[s.Name]++
		}
	}
	queue := make([]string, 0, len(stages))
	for n, d := range indeg {
		if d == 0 {
			queue = append(queue, n)
		}
	}
	seen := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		seen++
		for _, m := range adj[n] {
			indeg[m]--
			if indeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}
	if seen != len(stages) {
		return fmt.Errorf("工作流存在循环依赖")
	}
	return nil
}

// LoadWorkflowsFromDir 从目录加载并注册所有 *.json 动态工作流。
// 返回成功数、失败数、失败明细; 目录不存在视为 0(非错误)。
func LoadWorkflowsFromDir(dir string, roles *RoleRegistry) (loaded, failed int, details []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			failed++
			details = append(details, e.Name()+": "+err.Error())
			continue
		}
		var def WorkflowDef
		if err := json.Unmarshal(data, &def); err != nil {
			failed++
			details = append(details, e.Name()+": JSON 解析失败: "+err.Error())
			continue
		}
		if err := RegisterWorkflow(&def, roles); err != nil {
			failed++
			details = append(details, e.Name()+": "+err.Error())
			continue
		}
		loaded++
	}
	return loaded, failed, details
}

// SaveWorkflowToDir 把动态工作流持久化到 dir/<name>.json (供 dashboard API 注册后落盘, 重启可恢复)。
func SaveWorkflowToDir(dir string, def *WorkflowDef) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(def, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sanitizeWorkflowFileName(def.Name)+".json"), data, 0o644)
}

func sanitizeWorkflowFileName(name string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", "..", "_", " ", "-")
	return r.Replace(name)
}

// ============================================================================
// 三种动态工作流的共同脊柱: "LLM 从目标生成编排" 是同一种操作, 区别只在生命周期:
//   - 自定义(Custom): 人手写 WorkflowDef
//   - LLM 生成+审核: GenerateWorkflowDef → 用户审核/编辑 → 注册 (可复用)
//   - 蜂群(Swarm): decompose → 即时执行 (其 DecompositionPlan 可经 PlanToWorkflowDef 转成
//     同一 WorkflowDef 形状做可视化, 或"另存为工作流"晋升成自定义)
// 三者最终都落到同一个 WorkflowDef + 同一套执行引擎(系统A/B, 由 Mode 决定)。
// ============================================================================

// PlanToWorkflowDef 把蜂群 LLM 动态生成的 DecompositionPlan 转成统一的 WorkflowDef 形状
// (供 dashboard 可视化, 以及"把这次蜂群编排另存为可复用工作流")。
func PlanToWorkflowDef(plan *DecompositionPlan, name string) *WorkflowDef {
	wf := &WorkflowDef{Name: name, Mode: "pipeline", Description: plan.Rationale}
	for i, st := range plan.SubTasks {
		sname := strings.TrimSpace(st.ID)
		if sname == "" {
			sname = fmt.Sprintf("task-%d", i+1)
		}
		wf.Stages = append(wf.Stages, StageDef{
			Name:      sname,
			Role:      st.Role,
			Prompt:    st.Description,
			DependsOn: append([]string(nil), st.DependsOn...),
			Parallel:  len(st.DependsOn) == 0 && plan.Strategy == "parallel",
		})
	}
	return wf
}

// GenerateWorkflowDef 用 LLM 从目标 + 可用角色生成一个动态工作流定义 (返回但不注册, 供审核)。
// 与 swarm 的 decompose 同源(都是 LLM 从目标生成编排), 但产出可复用、可审核的 WorkflowDef。
//
// 迭代生成: 当 current!=nil && instruction!="" 时为"在现有编排上按指令调整"(多轮交互), 否则首次生成。
func GenerateWorkflowDef(ctx context.Context, llm LLMClient, objective string, roleNames []string, current *WorkflowDef, instruction string) (*WorkflowDef, error) {
	if llm == nil {
		return nil, fmt.Errorf("LLM 不可用")
	}
	sort.Strings(roleNames)
	roleList := strings.Join(roleNames, ", ")
	sys := "你是多智能体工作流编排专家。把用户目标拆解成一个可执行的工作流(有依赖关系的若干阶段)。只输出严格 JSON, 不要任何解释或代码块标记。"

	schema := `只输出如下结构的 JSON:
{"name":"英文短横线命名的唯一工作流名","description":"一句话描述","mode":"pipeline|fanout|adversarial|orchestrated","stages":[
  {"name":"阶段英文名(唯一)","role":"上面列表里的角色名(可留空)","prompt":"该阶段做什么; 支持 {objective} {prev_result} {user_feedback} 占位符; role 留空时必填","dependsOn":["前置阶段名"]}
]}

硬性要求: mode 只能是列出的四种之一; stage 名互不相同; dependsOn 只能引用已定义的 stage; 不能有循环依赖; 阶段数 3-7 个。`

	var user string
	if current != nil && strings.TrimSpace(instruction) != "" {
		// 迭代: 在现有编排上按调整指令修改
		curJSON, _ := json.Marshal(current)
		user = fmt.Sprintf(`这是当前的工作流编排:
%s

可用角色: %s

请按下面的调整指令修改并重新输出**完整**工作流(保留未涉及的部分):
调整指令: %s

%s`, string(curJSON), roleList, truncateResult(instruction, 1000), schema)
	} else {
		if strings.TrimSpace(objective) == "" {
			return nil, fmt.Errorf("objective 不能为空")
		}
		user = fmt.Sprintf(`目标: %s

可用角色(尽量从中给每个阶段选一个 role; 找不到合适角色时, 把该阶段要做的事写进 prompt 字段): %s

%s`, truncateResult(objective, 2000), roleList, schema)
	}

	out, err := llm.SimpleComplete(ctx, sys, user)
	if err != nil {
		return nil, err
	}
	js := extractJSONObject(out)
	if js == "" {
		return nil, fmt.Errorf("LLM 未返回可解析的 JSON")
	}
	var def WorkflowDef
	if err := json.Unmarshal([]byte(js), &def); err != nil {
		return nil, fmt.Errorf("解析 LLM 输出失败: %w", err)
	}
	if strings.TrimSpace(def.Mode) == "" {
		def.Mode = "pipeline"
	}
	return &def, nil
}

// mergedWorkflowList 合并内置 + 自定义工作流, 按名称排序 (供 ListWorkflows)。
func mergedWorkflowList() []WorkflowDef {
	names := make([]string, 0, len(workflowRegistry))
	for n := range workflowRegistry {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]WorkflowDef, 0, len(names))
	for _, n := range names {
		if wf := workflowRegistry[n](); wf != nil {
			out = append(out, *wf)
		}
	}
	custom := listCustomWorkflows()
	sort.Slice(custom, func(i, j int) bool { return custom[i].Name < custom[j].Name })
	return append(out, custom...)
}
