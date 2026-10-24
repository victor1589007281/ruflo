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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/graph"
	"github.com/anthropic/claude-go/pkg/logging"
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

// ---------------------------------------------------------------------------
// 版本守卫 (21.8.12 P3): 把"改写"从不可撤销变成可发现的
// ---------------------------------------------------------------------------
//
// 改造前 RegisterWorkflow 是**创建与改写共用的一个口**: 同名再来一次就静默覆盖,
// 双方都不知情。两件事的失败后果完全不同 ——
//
//	创建: 重名是调用方写错了 (或并发创建), 报错即可
//	改写: 覆盖掉的是**别人可能正在引用/正在回退的那一版**, 而覆盖是不可逆的
//
// 所以拆成两个口: RegisterWorkflow (创建) 与 UpdateWorkflow (改写, 必须带期望版本)。
// 这与本仓既有的 TaskRecord.Revision / TransitionCAS 是同一套语义, 不新造机制:
// 外改者携带它读到的 Revision, 不匹配即拒, 而不是"最后一次写入赢"。

// ErrWorkflowRevisionConflict 更新被版本守卫拒绝 (期望版本与现值不符)。
//
// 单独导出成哨兵错误: 调用方 (dashboard / 上层平台) 要能把"版本冲突"与"定义非法"
// 分开处理 —— 前者该提示"请刷新后重试", 后者该提示"你的定义哪里写错了"。
// 混成一句话会让用户去改一份本来没写错的定义。
var ErrWorkflowRevisionConflict = errors.New("workflow: 版本冲突")

// RevisionConflictError 一次被拒的更新, 带上两个数 —— 光说"冲突了"用户无从下手。
//
// 与 pkg/agent.taskservice 的 ErrStateConflict 同类, 这里额外带上数值:
// 工作流的更新方是一个**人** (webapp 表单), 它需要看到"我手上是 3, 服务端是 5"
// 才知道该重新拉一次。任务那侧是程序在重试, 报不报数无所谓 —— 差别在这里, 不在语义。
type RevisionConflictError struct {
	Name     string
	Expected int64
	Actual   int64
}

// Error 渲染版本冲突的人话说明; Expected<=0 表示本次提交根本没带版本, 措辞与"版本过旧"分开。
func (e *RevisionConflictError) Error() string {
	// Expected==0 是"没带版本"这一种, 不是"手上有 r0"。分开措辞, 因为两种的
	// 下一步动作不同: 没带 → 先 GET 拿版本再发; 带旧的 → 重新读取 (有人改过)。
	if e.Expected <= 0 {
		return fmt.Sprintf("工作流 %q 已存在, 本次提交未携带 revision (服务端是 r%d); 改写必须带版本, 先 GET 取回 revision 再提交", e.Name, e.Actual)
	}
	return fmt.Sprintf("工作流 %q 已被他处改写 (你手上是 r%d, 服务端是 r%d); 请重新读取后再提交", e.Name, e.Expected, e.Actual)
}

// Unwrap 让调用方能用 errors.Is 判定哨兵错误 ErrWorkflowRevisionConflict。
func (e *RevisionConflictError) Unwrap() error { return ErrWorkflowRevisionConflict }

// normalizeRevision 把「写在版本守卫之前的文件」的 0 归一到 1。
//
// 归一发生在**读**而不是**写**: 40 份在野文件一个字节都不用改, 而 GET 出去的
// Revision 从第一次读取起就恒 ≥1 —— 客户端拿到的是一个可回传的期望值。
// 若只在写时归一, 老文件第一次 GET 会拿到 0, 而 0 恰好是"没带版本"的取值,
// 于是"没带"与"带了个合法的老版本"在服务端分不开。
func normalizeRevision(rev int64) int64 {
	if rev < 1 {
		return 1
	}
	return rev
}

// RegisterWorkflow 校验并注册一个**新**的动态工作流到自定义注册表 (带锁, 防与团队创建 goroutine 的并发读 race)。
//
// 重名时的行为是**拒绝**, 不是覆盖 —— 覆盖请走 UpdateWorkflow 并带上期望版本。
// 这条区分是 21.8.12 P3 的核心: 没有它, "注册"与"改写"在调用方看来是同一件事,
// 而它们的失败后果完全不同 (见本文件上方的版本守卫说明)。
func RegisterWorkflow(def *WorkflowDef, roles *RoleRegistry) error {
	return registerWorkflowLocked(def, roles, 0)
}

// registerWorkflowLocked rev<=0 时按新建处理 (Revision=1), rev>0 时原样落盘。
//
// rev>0 这条分支是给 LoadWorkflowsFromDir 用的: 从磁盘装载**不是一次改写**,
// 它是在恢复这份文件自己上一次的版本。若装载也走 +1, 每重启一次 revision 就涨一格,
// 客户端手里那个"刚 GET 到的值"会在一次无人改动的重启之后失效 —— 版本号于是不再
// 表示"内容变的次数", 而表示"进程起过几次", 守卫就退化成随机拒绝了。
func registerWorkflowLocked(def *WorkflowDef, roles *RoleRegistry, rev int64) error {
	if def == nil {
		return fmt.Errorf("workflow 定义为空")
	}
	if _, builtin := workflowRegistry[def.Name]; builtin {
		return fmt.Errorf("工作流名 %q 与内置工作流冲突", def.Name)
	}
	customMu.RLock()
	_, already := customWFs[def.Name]
	customMu.RUnlock()
	if already && rev <= 0 {
		// 这一条就是"同名 re-POST"从**静默覆盖**变成**可发现拒绝**的那一行。
		// 报错里必须写清楚下一步 (带上 revision 重发), 否则用户只知道"失败了",
		// 而它看起来与他刚刚成功过的那一次一模一样。
		return &RevisionConflictError{Name: def.Name, Expected: 0, Actual: workflowRevision(def.Name)}
	}
	// 校验分两级 (21.8.12 W3): 硬错拒绝, 降级告警放行但留痕。
	//
	// 装载路径 (rev>0) 的告警用 "装载" 前缀: 它与 "注册" 的告警内容一样, 但**排查
	// 方向完全不同** —— 前者要去看那个文件是谁写的, 后者要看这次请求是谁发的。
	warnings, err := def.ValidateAll(roles)
	if err != nil {
		return err
	}
	kind := "注册"
	if rev > 0 {
		kind = "装载"
	}
	recordValidationWarnings(kind, def.Name, warnings)
	cp := *def
	cp.Custom = true
	cp.Stages = append([]StageDef(nil), def.Stages...)
	cp.Graph = def.Graph.Clone()
	if rev > 0 {
		cp.Revision = rev
	} else {
		cp.Revision = 1
	}
	customMu.Lock()
	customWFs[def.Name] = &cp
	customMu.Unlock()

	// 图能力: 工作流定义自己声明的 (21.8.12 P5/X3) 优先于 env 文件里的同名条目。
	// 声明了才注册 —— 不声明时**必须不写这张表**, 否则一条 &{} 空声明会遮蔽
	// env 里那份 (遮蔽告警会响, 而它本该毫无冲突)。
	if cp.Graph != nil {
		RegisterGraphOverride(cp.Name, cp.Graph.toOverride())
	}

	// 同步注册成**可被 subgraph 节点引用**的图 (21.8.12 P1/D4)。
	//
	// 不注册的后果不是报错而是**不可达**: pkg/graph 的 NodeKindSubgraph 与
	// RegisterGraph 都齐备, 但 RegisterGraph 全仓零调用 ⇒ 一张图永远引用不到任何东西。
	// 这是本仓库反复出现的"声明了但没人读"里最隐蔽的一种: 组件都在, 只差一根线。
	//
	// 失败**不阻断工作流注册**: 有些 mode 的图来源是专属模板, 或本就不适合被引用,
	// 让它们的注册失败会是一次纯回退。但也不能静默 —— 见 registerWorkflowGraph 的注释。
	registerWorkflowGraph(&cp)
	return nil
}

// registerWorkflowGraph 把工作流翻译成图并登记进 pkg/graph 的注册表 (供 subgraph 引用)。
//
// **同名冲突不覆盖**: 先 UnregisterGraph 再注册, 因为这里换掉的是"别人引用的那张图",
// 与 RegisterWorkflow 换掉自己的定义是两件事。清理这一步是必要的 —— 不清理则第二次
// 注册同名工作流会永远失败 (RegisterGraph 拒绝重名), 而失败只进日志。
//
// 失败**必须可见**: 一张没能注册成图的工作流, 从外部看与"它本来就不该被引用"完全一样。
// 这里用 Warn 而不是 Debug, 并在错误里带上工作流名 —— 否则"我引用了它怎么起不来"
// 要一路查到 pkg/graph 的校验错误里才知道根因是注册这一步。
func registerWorkflowGraph(wf *WorkflowDef) {
	if wf == nil {
		return
	}
	spec, err := graphSpecForWorkflow(wf)
	if err != nil {
		logging.For("workflow").Warn("工作流未能注册成可引用的图 (subgraph 节点引用它时会解析失败)",
			"workflow", wf.Name, "err", err)
		return
	}
	spec.Name = wf.Name // 引用键恒等于工作流名, 不吃模板自带的 spec.Name
	if _, exists := graph.LookupGraph(wf.Name); exists {
		graph.UnregisterGraph(wf.Name)
	}
	if err := graph.RegisterGraph(spec); err != nil {
		logging.For("workflow").Warn("工作流注册成图失败",
			"workflow", wf.Name, "err", err)
	}
}

// UnregisterWorkflow 删除一个动态工作流 (内置工作流不可删)。
func UnregisterWorkflow(name string) error {
	customMu.Lock()
	_, ok := customWFs[name]
	if ok {
		delete(customWFs, name)
	}
	customMu.Unlock()
	// 连同它注册的图一起摘掉: 留着会让一个**已删除**的工作流仍能被子图节点引用,
	// 而引用方从注册表拿到的是一份不再属于任何工作流的载荷。
	if ok {
		graph.UnregisterGraph(name)
		// 图能力声明也要摘: 留着的话, 下一次用同名新建一个工作流会**继承**上一版的
		// MaxParallel/Groups —— 一份没人声明过的图表, 而它看起来完全正常。
		UnregisterGraphOverride(name)
	}
	if !ok {
		if _, builtin := workflowRegistry[name]; builtin {
			return fmt.Errorf("内置工作流 %q 不可删除", name)
		}
		return fmt.Errorf("动态工作流 %q 不存在", name)
	}
	return nil
}

// UpdateWorkflow 改写一个**已存在**的动态工作流, 并受版本守卫保护 (21.8.12 P3)。
//
// 契约 (与 taskservice.TransitionCAS 同形):
//   - expectRevision < 1 → 拒绝。**不把 0 当成"强制覆盖"**: 一个可以省略的守卫
//     等于没有守卫, 而"我忘了带"与"我确认要覆盖"在服务端分不开。
//   - expectRevision != 当前值 → RevisionConflictError, 且**什么都不改** (先判后写)。
//   - 通过 → 内容替换, Revision = expectRevision + 1, 阶段深拷贝 (防调用方改切片串台)。
//
// Validate 在守卫**之后**跑, 顺序是刻意的: 先确认"你要改的还是我看到的那一版",
// 再评判"你改出来的这份合不合法"。反过来的话, 一份过期的定义会先拿到一句
// "你定义写错了", 而它其实没错 —— 用户于是会去改一份没坏的东西。
func UpdateWorkflow(def *WorkflowDef, roles *RoleRegistry, expectRevision int64) (*WorkflowDef, error) {
	if def == nil {
		return nil, fmt.Errorf("workflow 定义为空")
	}
	if expectRevision < 1 {
		return nil, &RevisionConflictError{Name: def.Name, Expected: 0, Actual: workflowRevision(def.Name)}
	}
	// 内置不可改写: 它们由 Go 工厂产出, 覆盖掉只会在进程重启后"自己变回去",
	// 而中间这段时间的行为差异没有任何地方记录。要改内置, 请另存为新名字。
	if _, builtin := workflowRegistry[def.Name]; builtin {
		return nil, fmt.Errorf("内置工作流 %q 不可改写; 请换个名字另存", def.Name)
	}
	customMu.RLock()
	cur, ok := customWFs[def.Name]
	var actual int64
	if ok {
		actual = normalizeRevision(cur.Revision)
	}
	customMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("动态工作流 %q 不存在 (版本守卫只用于改写; 新建请走 POST)", def.Name)
	}
	if actual != expectRevision {
		return nil, &RevisionConflictError{Name: def.Name, Expected: expectRevision, Actual: actual}
	}
	warnings, err := def.ValidateAll(roles)
	if err != nil {
		return nil, err
	}
	recordValidationWarnings("改写", def.Name, warnings)
	cp := *def
	cp.Custom = true
	cp.Revision = expectRevision + 1
	cp.Stages = append([]StageDef(nil), def.Stages...)
	cp.Graph = def.Graph.Clone()
	customMu.Lock()
	customWFs[def.Name] = &cp
	customMu.Unlock()
	// 先摘再按需重注册: 这一版可能**去掉了** graph 段, 而上一版注册进覆盖表的声明
	// 会留着 —— 于是"删掉那段声明"看起来什么都没发生 (P4 的改写同步同一形态)。
	UnregisterGraphOverride(cp.Name)
	if cp.Graph != nil {
		RegisterGraphOverride(cp.Name, cp.Graph.toOverride())
	}
	registerWorkflowGraph(&cp)
	out := cp
	out.Stages = append([]StageDef(nil), cp.Stages...)
	return &out, nil
}

// workflowRevision 取当前版本 (不存在返回 0)。供错误信息与 dashboard 用。
func workflowRevision(name string) int64 {
	customMu.RLock()
	defer customMu.RUnlock()
	if d, ok := customWFs[name]; ok {
		return normalizeRevision(d.Revision)
	}
	return 0
}

// RevisionOf 导出给 dashboard / 上层平台读当前版本的唯一入口。
//
// 存在的理由是防止调用方自己拼 `GetWorkflow(name).Revision`: getCustomWorkflow 返回
// 的是副本, 但**内置工作流没有 Revision 概念**, 那条路会回 0 —— 而 0 在守卫里
// 恰好是"没带版本"这个取值。一个从内置工作流读出来的 0 会被当成"忘了带",
// 于是改写内置的请求会得到一句"未携带 revision"而不是"内置不可改写"。
func RevisionOf(name string) int64 { return workflowRevision(name) }

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
//
// **只报硬错** (会让这次执行失败或语义不成立的那些)。降级类问题 (Agent 未注册
// 但走了内联 prompt) 一律进 ValidateAll 的告警面 —— 见那里的论证: 把降级升级成
// 硬错会让已落盘的 40 条工作流在启动装载时被整份丢掉。
func (wf *WorkflowDef) Validate(roles *RoleRegistry) error {
	_, err := wf.ValidateAll(roles)
	return err
}

// ValidateAll 是唯一的校验实现: 返回 (告警, 硬错)。
//
// 两级而不是一级, 对应两种后果完全不同的东西:
//
//   - **硬错** (err != nil): 这次定义跑不起来。缺 stage 名、依赖不存在、成环、
//     既没注册角色又没内联 prompt (那样 agent 连 system prompt 都没有)。
//   - **告警** (warnings): 跑得起来, 但与你写下的**不是同一件事**。今天只有一条
//     形态 —— stage 的 Agent 不在角色表里, 于是它用的是内联 prompt 而不是那个
//     角色的 prompt。
//
// 告警存在的全部理由: 那 40 条落盘工作流用的角色名是宿主平台自己的
// (storyloom-agent / qa-case-designer ...), 永远不会进 claude-go 的角色表, 而它们
// 每条都自带完整 prompt 且跑得对。**这条降级必须保留**。但同时, "把 coder 打成
// codr" 与它们的症状一模一样 —— 都是内联 prompt 生效, 在看板上都没有形态。
// 所以: 默认告警, 声明了 AllowUnregisteredRole 的 stage 不再告警 (作者明说了
// "我知道它不在表里")。
//
// 返回的顺序是稳定的 (按 stage 声明序), 因为它的消费方之一是日志与面板 ——
// 两次装载给出不同顺序的告警就没法对比"这次比上次多了哪条"。
func (wf *WorkflowDef) ValidateAll(roles *RoleRegistry) (warnings []string, hard error) {
	if strings.TrimSpace(wf.Name) == "" {
		return nil, fmt.Errorf("workflow.name 不能为空")
	}
	if len(wf.Stages) == 0 {
		return nil, fmt.Errorf("workflow %q 至少需要一个 stage", wf.Name)
	}
	if !dynamicModes[wf.Mode] {
		return nil, fmt.Errorf("mode %q 不支持动态定义(含伴生硬编码); 可选: pipeline/fanout/graph/adversarial/adversarial_dev/orchestrated", wf.Mode)
	}
	// stagesAreDAG: 这张 stage 列表就是执行顺序本身 (有环 ⇒ 跑不起来)。
	// 只对**本进程执行**的定义成立 —— 具体见 validateStages 的 graph 参数。
	return wf.validateStages(roles, true)
}

// ValidateStagesOnly 是内置自检用的那一档: 阶段名与 agent 解析, **不查依赖关系**。
//
// 拆出来是给 21.8.12 W4 用的。内置不能跑 ValidateAll 的三道闸门, 也不能跑依赖检查,
// 三条都是同一个根因: **部分内置的 stage 列表不是它的 DAG**。
//
//   - `mode ∈ dynamicModes` 那张表的定义就是"除有伴生硬编码执行器的模式之外", 而内置里
//     恰好有 app_composite / creative_media / ... 这些**有**伴生执行器的模式。
//   - `至少一个 stage`: graph-extract-swarm / review-panel / plot-* 的 stage 在图模板里。
//   - `DependsOn 必须指向已声明的 stage`: novel-v3 的 outline-design 依赖
//     best-narrative / second-narrative / evaluation —— 那三个是它自己的 Phase B
//     (swarm_intel 模拟) **注入的数据键**, 不是它声明的阶段; 它的阶段列表被显式标注为
//     "其余由编排器动态驱动"。
//
// 三条合起来实测是 11 条每次启动都打印、且永远不会变的"硬错"。见 AuditBuiltinWorkflows。
func (wf *WorkflowDef) ValidateStagesOnly(roles *RoleRegistry) (warnings []string, hard error) {
	return wf.validateStages(roles, false)
}

// validateStages 是**内置与动态共用**的实现: 阶段名与 agent 解析必查,
// 依赖关系与成环只在 graph=true (这张列表就是执行顺序) 时查。
func (wf *WorkflowDef) validateStages(roles *RoleRegistry, graph bool) (warnings []string, hard error) {
	// registrySize 进告警文案: "角色表里有 40 个" 与 "角色表是空的" 是两种完全
	// 不同的处境, 而它们的告警**逐字相同** —— 后者说明来源接线断了 (21.8.12 W1),
	// 前者说明名字写错了。不报这个数, 这条告警就要人再去查一次才知道往哪修。
	registrySize := 0
	if roles != nil {
		registrySize = roles.Count()
	}
	names := make(map[string]bool, len(wf.Stages))
	for _, s := range wf.Stages {
		if strings.TrimSpace(s.Name) == "" {
			return warnings, fmt.Errorf("存在未命名的 stage")
		}
		if names[s.Name] {
			return warnings, fmt.Errorf("stage 名重复: %q", s.Name)
		}
		names[s.Name] = true
		// Agent 必须能解析(注册表里有), 或提供内联 Prompt(否则 agent 没有 system prompt)
		roleOK := roles != nil && roles.Get(s.Agent) != nil
		if roleOK {
			continue
		}
		if strings.TrimSpace(s.Prompt) == "" {
			return warnings, fmt.Errorf("stage %q: agent %q 未注册且未提供内联 prompt", s.Name, s.Agent)
		}
		if s.AllowUnregisteredRole {
			continue // 作者显式承认: 这个 stage 就是走内联 prompt, 无需再提
		}
		// ★ agent 为空**不告警**: 这条告警要抓的是"把 coder 打成 codr" —— 而那要求
		// 有一个名字。空名字不是拼错, 是"这个 stage 由别的机制产出", 生产上有两个
		// 确切的例子: subgraph 节点的内容来自被引用的图, map/gateway 节点同理 ——
		// 它们的 Prompt 只是声明期占位的说明文字, 运行时一个字都不会被消费
		// (见 subgraph_wiring_test.go 的 subgraphFixture: 那两句 prompt 带着注释
		// "去掉它会让整个声明期校验失败")。
		//
		// 报了会怎样: 文案说"本次将用内联 prompt 执行"—— 对这类节点这是**假话**,
		// 而假话比沉默更糟: 作者会去查一句根本不会被执行的 prompt。
		//
		// 硬错那一档不动: agent 空**且** prompt 空仍然拒绝 (那时连声明都没有)。
		if strings.TrimSpace(s.Agent) == "" {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"stage %q 的 agent %q 不在注册表里 (表内 %d 个), 本次将用内联 prompt 执行: "+
				"如果这个名字本该是某个已注册 agent, 请改正; 如果是有意为之, 在 stage 上声明 allowUnregisteredRole: true 即可消除本条",
			s.Name, s.Agent, registrySize))
	}
	if !graph {
		return warnings, nil
	}
	// 工作流级图能力声明 (21.8.12 P5/X3)。与下面两条同一个判据: 只对"这张 stage 列表
	// 就是全部阶段"的定义成立 —— 组的成员必须是本工作流的阶段, 而内置工作流的阶段
	// 可能在图模板里 (Stages 为空), 拿它去判会造出又一批固定误报。
	//
	// 放在这里 (而不是留到直译期) 是因为这些检查只依赖这份定义自己: 一条
	// maxIterations=0 的组声明若能存下来, POST 会返回 200、GET 一字不差, 直到某次
	// 真跑才炸 —— 而那时报出来的是图引擎的错误, 排查方向完全在别处。
	if err := wf.Graph.Validate(names); err != nil {
		return warnings, err
	}
	for _, s := range wf.Stages {
		for _, dep := range s.DependsOn {
			if !names[dep] {
				return warnings, fmt.Errorf("stage %q 依赖了不存在的阶段 %q", s.Name, dep)
			}
		}
	}
	return warnings, validateNoCycle(wf.Stages)
}

// validationWarnings 记下降级告警 (21.8.12 W3「降级必须可见」)。
//
// 落在**注册时**而不是每次执行时: 一次注册只发生一次, 而一次执行发生很多次 ——
// 按执行打日志会把同一条告警刷成噪声, 那正是 [[steady-error-is-a-filtered-error]]
// 记的失效形态 (每 30 秒必现的 ERROR = 没有 ERROR)。
//
// 装载与 PUT 两条路都会到这里: 前者是 "启动后这份表对不对", 后者是 "这次改动
// 有没有引入降级"。两条都要, 缺一条都会有一半的降级只在某个入口可见。
func recordValidationWarnings(kind, name string, warnings []string) {
	for _, w := range warnings {
		logging.For("workflow").Warn(kind+"存在静默降级",
			"workflow", name, "detail", w)
	}
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

// LoadReport 一次装载的三种结果。
//
// 为什么把 (成功, 失败) 两个数扩成一个结构体 (21.8.12 W3): 改造前这两个数**都**
// 无法从外部读到 —— 调用方拿的是三元组 (loaded, failed, details), 而 details
// 在 cmd/claude-go 与 pkg/feishu 两处都被 `_` 丢掉了, 于是"40 个文件里 3 个装载
// 失败"在生产上表现为一行 "成功 37 失败 3", 而那 3 个是谁、为什么, 只有翻日志。
//
// Deferred 是第三个数, 也是本次新加的: **装载成功但发生了降级**的那些 ——
// 它们的文件读进来了, 名字也在表里, 但某个 stage 正在用内联 prompt 而不是它声明
// 的 agent 的 prompt。这一类此前与"完全正常"逐字节同形。
type LoadReport struct {
	Loaded   int
	Failed   int
	Details  []string
	Deferred []string // 降级明细 (装载成功但有静默降级, 一条一 stage)
}

// Summary 一行读数 (目录全部正常时不产生任何噪声)。
func (r LoadReport) Summary(dir string) string {
	s := fmt.Sprintf("%s: 成功 %d 失败 %d", dir, r.Loaded, r.Failed)
	if n := len(r.Deferred); n > 0 {
		s += fmt.Sprintf(" 静默降级 %d 条 (见 details)", n)
	}
	return s
}

// LoadWorkflowsFromDir 从目录加载并注册所有 *.json 动态工作流。
// 返回成功数、失败数、失败明细; 目录不存在视为 0(非错误)。
//
// 保留这个三元组签名是给既有调用方与测试的兼容入口; 需要降级明细的调用方走
// LoadWorkflowsFromDirReport。两个入口共用同一份实现 —— 各写一遍会让"报了哪些
// 降级"这一条在两条路上漂移, 而漂移的表现是"某入口看到了降级警告、另一个没有"。
func LoadWorkflowsFromDir(dir string, roles *RoleRegistry) (loaded, failed int, details []string) {
	r := LoadWorkflowsFromDirReport(dir, roles)
	return r.Loaded, r.Failed, r.Details
}

// LoadWorkflowsFromDirReport 与 LoadWorkflowsFromDir 同源, 多带回降级明细。
func LoadWorkflowsFromDirReport(dir string, roles *RoleRegistry) LoadReport {
	var rep LoadReport
	entries, err := os.ReadDir(dir)
	if err != nil {
		return rep
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			rep.Failed++
			rep.Details = append(rep.Details, e.Name()+": "+err.Error())
			continue
		}
		var def WorkflowDef
		if err := json.Unmarshal(data, &def); err != nil {
			rep.Failed++
			rep.Details = append(rep.Details, e.Name()+": JSON 解析失败: "+err.Error())
			continue
		}
		// 先自校验一次, 把**降级告警**取出来 —— 走的是与注册路径同一个 ValidateAll,
		// 所以这里列出来的与注册时打进日志的必然一致 (不会出现"日志报了一条、汇总
		// 里没有")。硬错不在这里拦: 紧接着的 registerWorkflowLocked 会再校验一次
		// 并给出**带文件名**的报错, 那才是排查要看的那一条。
		warns, _ := def.ValidateAll(roles)
		for _, w := range warns {
			rep.Deferred = append(rep.Deferred, e.Name()+" → "+w)
		}
		// 装载保留文件里**自己的** revision (归一后 ≥1), 不走新建分支。
		//
		// 见 registerWorkflowLocked 的注释: 装载不是改写, 每一次重启都给所有工作流
		// +1 会让客户端手里的版本号在无人改动时失效 —— 版本号于是不再表示"内容变的
		// 次数", 而表示"进程起过几次", 守卫退化成随机拒绝。
		if err := registerWorkflowLocked(&def, roles, normalizeRevision(def.Revision)); err != nil {
			rep.Failed++
			rep.Details = append(rep.Details, e.Name()+": "+err.Error())
			continue
		}
		rep.Loaded++
	}
	return rep
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

// sanitizeWorkflowFileName 把工作流名里的路径分隔符、".." 与空格替换掉, 防止落盘时穿出目标目录。
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
			Agent:     st.Role,
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
// pseudoWorkflows 是不进注册表、但 CreateTeam 显式放行的"伪工作流"。
// 目前只有 swarm: 蜂群的初始 Agent 阵容由 LLM 动态决定, 没有预定义阶段表。
//
// 注意 predict 不在此列——它在 executeWorkflow 里有特判分支与完整的 runPredict
// 实现, 却既不在注册表也不被 CreateTeam 放行, 因此**任何创建路径都到不了它**
// (全部建团队入口都经 CreateTeam)。这是一处待决的不可达路径, 已登记进
// design/PROGRESS.md 偏差记录; 此处不擅自放行, 因为放行等于恢复一条未经
// 真实 LLM 验证的执行路径。
var pseudoWorkflows = []string{"swarm"}

// AvailableWorkflowNames 返回当前真正可用于建团队的工作流名(内置 + 动态注册 + 伪工作流),
// 已排序去重。
//
// 为什么要有这个函数: CreateTeam 的"未知工作流"报错原先硬编码了一串名字, 结果既
// **漏掉全部动态注册的工作流**(下游平台注册的那些一个都没列), 又**列了 debate 与
// predict 两个实际拿不到的名字**——用户照着报错里的名字重试, 会拿到同一句报错,
// 而报错里还写着它可用。名单必须从真源生成, 否则必然漂移。
func AvailableWorkflowNames() []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(workflowRegistry)+len(pseudoWorkflows))
	for _, wf := range mergedWorkflowList() {
		if !seen[wf.Name] {
			seen[wf.Name] = true
			out = append(out, wf.Name)
		}
	}
	for _, n := range pseudoWorkflows {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// mergedWorkflowList 合并内置与自定义工作流并按名称排序, 供 ListWorkflows 使用。
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
