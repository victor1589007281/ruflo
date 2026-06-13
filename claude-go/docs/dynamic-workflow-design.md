# 动态工作流(Dynamic Workflow)设计方案

> 2026-06-13 · claude-go · **这是方案(plan),非实现**。结合源码调研给出"如何让工作流可在运行时由配置/自然语言定义、而非硬编码"的落地路线与关键风险。
> 末尾给出最小可用(Phase 1)的具体改动清单,可按需实现。

---

## 1. 现状:好消息与硬约束

### 1.1 好消息——`WorkflowDef`/`StageDef` 已经是纯数据

`pkg/agent/workflow.go`(约 224-238):

```go
type WorkflowDef struct {
    Name        string
    Description string
    Mode        string     // pipeline / fanout / adversarial / adversarial_dev / orchestrated / creative_media / ...
    Stages      []StageDef
    Rounds      int        // 对抗模式轮数
}
type StageDef struct {
    Name      string
    Role      string       // → RoleRegistry 解析 system prompt
    Prompt    string       // 内联模板, 支持 {objective}/{prev_result}/{user_feedback}/{adversarial_feedback}
    DependsOn []string
    Parallel  bool
}
```

**关键结论**:这两个结构**无 Go 闭包/函数指针,可直接 JSON/YAML 序列化反序列化**。执行引擎 `WorkflowExecutor.Execute` 按 `wf.Mode` 分派,只消费 `WorkflowDef` 数据。所以"动态构造一个 WorkflowDef → 现有引擎直接能跑"在数据层面成立。这是动态化最大的有利前提。

### 1.2 已有的三条"动态"路径(可复用,不必重造)

| 机制 | 位置 | 动态性 | 复用价值 |
|---|---|---|---|
| **swarm 模式** | `executeSwarm`/`SwarmOrchestrator` | LLM 把目标动态拆成 SubTask DAG(数量/依赖不预定义)→ 拓扑分层并行执行 | 已是"自然语言→动态任务图",可作为 NL→workflow 的现成后端 |
| **orchestrated 模式** | `pkg/orchestrator/` DAG 引擎 + `workflow_orchestrated.go buildGraph` | WorkflowDef.Stages → Graph.Tasks,背压/并发/对抗循环通用 | 通用 DAG 执行器,动态 def 的理想运行时 |
| **RoleRegistry** | `pkg/agent/roles.go RegisterCustom` + `loadCustomRoles(.claude/agents/*.json)` | 角色**已可运行时注册 + 从磁盘 JSON 加载** | 工作流注册表照抄这套设计即可;自定义角色 prompt 已通路 |

### 1.3 两套编排系统 & 动态工作流跑在哪一套 & 与现有 workflow 的区别

**claude-go 现在确实有两套编排(调度)系统**(见 `pkg/agent/workflow_orchestrated.go` 文件头):

| | 系统 A | 系统 B |
|---|---|---|
| 实现 | `pkg/agent/coordinator.go` + `runPipelineWithRecovery` + 各 `executeXxx`(`workflow.go`) | `pkg/orchestrator/engine.go`(`executeOrchestrated` 桥接) |
| 量级 | 轻量: 拓扑排序 + 并发组 + 检查点 | 重量: K8s 风格三阶段 `Filter→Score→Dispatch` + 背压(RPM 令牌桶) + 错误分治(瞬态/永久/致命) |
| 适用 | 线性/分支 pipeline(顺序阶段 + 偶发并行) | 复杂 DAG、对抗循环、运行时裂变(`LLMExpander`) |
| 触发 | mode = `pipeline`/`fanout`/`adversarial`/`adversarial_dev` 等 | mode = `orchestrated` |
| 路由 | `Coordinator.RunWithRecovery()` 默认分支 | `RunWithRecovery()` 见到 `orchestrated` → `executeOrchestrated()` |

**动态工作流用哪一套?——两套都用,且不引入第三套。** 关键点:两套系统**都只消费 `WorkflowDef` 这一份纯数据**,选哪套**完全由 `WorkflowDef.Mode` 字段决定**。所以动态工作流不需要"挑选/新建"编排组件:

- 动态定义 `mode: "pipeline"`(或 fanout/adversarial)→ 跑在**系统 A**;
- 动态定义 `mode: "orchestrated"`→ 跑在**系统 B**(K8s 式 DAG 引擎)。

即"动态工作流"是**定义来源**的动态化,**复用**现有两套执行引擎;用户在 JSON 里写 `mode` 就等于选了 A 还是 B。这也是为什么 §3.2 把动态可用模式限定在 `{pipeline,fanout,adversarial,adversarial_dev,orchestrated}`——这几个恰好是"纯按 WorkflowDef 数据驱动、能安全跑在 A/B 上"的模式。

**dynamic workflow 跟现在的 workflow 有什么区别?** 本质上**只差"定义从哪来"这一点**,执行引擎、数据结构完全相同:

| 维度 | 现有(静态) workflow | 动态 workflow |
|---|---|---|
| 定义来源 | 硬编码 Go 工厂函数(`workflowRegistry` 里的 `developmentWorkflow()` 等),**编译期固定** | 运行时来自 JSON/YAML 配置文件 或 LLM 生成,**免编译** |
| 数据结构 | `WorkflowDef`/`StageDef`(纯数据) | **同一个** `WorkflowDef`/`StageDef` |
| 执行引擎 | 系统 A / B(按 mode) | **同样的**系统 A / B(按 mode) |
| 增改方式 | 改 Go 代码 → 重新 `go build` → 重新部署 | 放一个 `~/.claude-go/workflows/x.json` 或让 LLM 生成,即时生效 |
| 正确性保证 | 编译期 + 人工(Go 代码可信) | **运行时 `Validate()`**(数据不可信)+ 声明式门禁字段 + 模式白名单(见 §3) |
| 角色 prompt | 角色注册表(已支持 `.claude/agents/*.json` 自定义) | 同左,可引用自定义角色或内联 prompt |

> 易混淆点:还有第三种"动态",但它不是"动态 workflow"——**swarm 模式**是运行时让 LLM 把单个目标**裂解成一次性 SubTask DAG**(无预定义阶段、不可复用);**系统 B 的 `LLMExpander`** 是在 orchestrated 执行中让某个阶段**运行时再裂变**出子任务。二者是"动态**任务图**";本文的"动态 **workflow**"指的是**可复用的工作流模板定义**的动态化。三者正交,可组合(例如:一个动态定义的 `orchestrated` 工作流,其中某阶段用 `LLMExpander` 运行时裂变)。

### 1.4 硬约束——动态化的两个"地雷"(源码层面,必须先拆)

这两点是本方案相对"简单加个 JSON 加载器"的核心增量,**不解决就会埋静默 bug**:

#### 地雷 A:质量门禁按"工作流名字"判定 → 动态工作流会**静默丢失门禁**

`pkg/agent/teams.go workflowProducesCode()` 和 `pkg/agent/content_gate.go contentQualityGated()` 都是 **`switch workflow-name`** 白名单:

```go
func workflowProducesCode(workflow string) bool {
    switch workflow { case "development","app","game","ml-training","testing","adversarial-dev": return true; default: return false }
}
func contentQualityGated(workflow string) bool {
    switch workflow { case "techblog","research": return true; default: return false }
}
```

一个用户自定义命名的工作流(如 `my-coding-flow`)既不在代码门禁白名单、也不在内容门禁白名单 → **编译/测试门禁和内容质量门禁全部失效,且无任何报错**。这与上一轮 `--stage`/`workflowProducesCode` 同类的"按名字 switch"陷阱同源。

**修法**:门禁判定改为**声明式字段**,挂在 WorkflowDef 上,内置工作流也回填该字段;判定时优先读字段、回退名字白名单(兼容期)。见 §3.1。

#### 地雷 B:只有"纯数据模式"能动态定义;"带伴生代码的模式"会错乱

不是所有 `Mode` 都只吃 WorkflowDef 数据。下列模式有**与具体 stage/role 名、格式列表强绑定的伴生 Go 代码**:

| Mode | 伴生硬编码 | 动态 def 的后果 |
|---|---|---|
| `creative_media` | `workflow_creative_v2.go` + `detectOutputFormats`(按关键词/`<section>`决定出 PNG/MP4/PPTX)+ Phase4 `media-producer` 阶段 | 用户自定义 stages 与媒体渲染阶段对不上,产物错乱 |
| `app_composite`/`game_composite` | 组合工作流,固定子团队/格式 | 同上 |
| `novel_v2`/`novel_v3`/`swarm_novel` | 章节级编排 + 特定角色名 | 章节循环逻辑找不到对应 stage |

而 **`pipeline` / `fanout` / `adversarial` / `adversarial_dev` / `orchestrated`** 是纯按 WorkflowDef 数据跑的(stages+deps+mode+rounds),**安全可动态定义**。

**修法**:动态工作流的 `Mode` 限定在纯数据模式白名单内;其余模式拒绝动态定义(明确报错,而非静默跑歪)。见 §3.2。

---

## 2. 目标分层(4 层,价值/成本递增)

| 层 | 能力 | 谁定义 | 依赖 |
|---|---|---|---|
| **L1 配置驱动** | 用 JSON/YAML 文件定义新工作流,启动时加载 | 人(写文件) | §3 的注册/校验/门禁改造 |
| **L2 运行时编辑** | API/CLI 改已注册工作流的 stages/role/prompt | 人(调 API) | L1 + 编辑接口 |
| **L3 自然语言生成** | "我要一个 X 流程" → LLM 产 WorkflowDef → 校验 → 注册 | LLM | L1 + 生成器 + 校验 |
| **L4 复用蜂群/编排** | 完全不预定义,swarm 动态拆解(已存在) | LLM | 已有,正交组合 |

L4 已经能用(`/go swarm <目标>`)。真正的增量在 **L1(地基)**,L2/L3 是其上的薄层。

---

## 3. 核心改造(L1 地基,所有层共享)

### 3.1 WorkflowDef 增加声明式元数据(拆地雷 A)

```go
type WorkflowDef struct {
    Name, Description, Mode string
    Stages []StageDef
    Rounds int
    // 新增:声明式策略, 取代"按名字 switch"的门禁判定
    ProducesCode bool   `json:"producesCode,omitempty"` // 跑编译/测试门禁
    QualityGate  string `json:"qualityGate,omitempty"`  // ""|"content"|"none" 内容质量门禁策略
    Custom       bool   `json:"-"`                       // 运行时加载的标记(非序列化)
}
```

门禁判定改为优先读字段(内置工作流在其工厂里回填 `ProducesCode/QualityGate`,行为不变):

```go
func workflowProducesCode(name string) bool {
    if def := GetWorkflow(name); def != nil && def.Custom { return def.ProducesCode }
    switch name { /* 既有内置白名单, 兼容 */ }
}
func contentQualityGated(name string) bool {
    if def := GetWorkflow(name); def != nil && def.Custom { return def.QualityGate == "content" }
    switch name { /* 既有内置白名单 */ }
}
```

> 注:`GetWorkflow` 每次返回新实例,这里只读字段,无状态共享问题。

### 3.2 WorkflowDef.Validate()(拆地雷 B + 防"死锁/静默")

注册前必须校验,否则坏 def 会命中 `executePipeline` 的"工作流死锁"或更隐蔽的空跑:

```go
var dynamicModes = map[string]bool{"pipeline":true,"fanout":true,"adversarial":true,"adversarial_dev":true,"orchestrated":true}

func (wf *WorkflowDef) Validate(roles *RoleRegistry) error {
    if wf.Name == "" || len(wf.Stages) == 0 { return errMissing }
    if !dynamicModes[wf.Mode] {            // 地雷 B:动态只允许纯数据模式
        return fmt.Errorf("mode %q 不支持动态定义(含伴生硬编码), 可选: pipeline/fanout/adversarial/adversarial_dev/orchestrated", wf.Mode)
    }
    names := map[string]bool{}
    for _, s := range wf.Stages {
        if s.Name == "" { return errStageName }
        if names[s.Name] { return errDupStage }
        names[s.Name] = true
        // role 必须能解析, 或提供内联 Prompt(否则 agent 无 system prompt)
        if (roles == nil || roles.Get(s.Role) == nil) && strings.TrimSpace(s.Prompt) == "" {
            return fmt.Errorf("stage %q: role %q 未注册且无内联 prompt", s.Name, s.Role)
        }
    }
    for _, s := range wf.Stages {           // DependsOn 必须指向已存在 stage
        for _, d := range s.DependsOn { if !names[d] { return fmt.Errorf("stage %q 依赖不存在的 %q", s.Name, d) } }
    }
    return detectCycle(wf.Stages)            // 无环(拓扑可排)
}
```

### 3.3 运行时注册表 + 并发保护(拆"data race")

现有 `workflowRegistry`(`map[string]func()*WorkflowDef`)被团队创建 goroutine 并发读。动态写入**必须加锁**:

```go
var (
    customMu  sync.RWMutex
    customWFs = map[string]*WorkflowDef{}   // 运行时注册的, 与内置 registry 分离
)

func RegisterWorkflow(def *WorkflowDef, roles *RoleRegistry) error {
    if err := def.Validate(roles); err != nil { return err }
    def.Custom = true
    customMu.Lock(); customWFs[def.Name] = def; customMu.Unlock()
    return nil
}

// GetWorkflow 改为: 先查内置, 再查 custom(读锁); 返回副本避免共享
func GetWorkflow(name string) *WorkflowDef {
    if f, ok := workflowRegistry[name]; ok { return f() }
    customMu.RLock(); d, ok := customWFs[name]; customMu.RUnlock()
    if ok { cp := *d; cp.Stages = append([]StageDef(nil), d.Stages...); return &cp }
    return nil
}
```

> 关键:不把 custom 写进 `workflowRegistry`(那是裸 map,并发写=race)。单独 `customWFs`+`RWMutex`。`ListWorkflows` 同样合并两源(读锁)。

### 3.4 配置加载(L1 入口)

```go
func LoadWorkflowsFromDir(dir string, roles *RoleRegistry) (loaded, failed int) {
    for _, p := range glob(dir, "*.json") {       // 也可支持 *.yaml
        var def WorkflowDef
        if json.Unmarshal(read(p), &def) != nil { failed++; continue }
        if RegisterWorkflow(&def, roles) != nil { failed++; log("skip "+p); continue }
        loaded++
    }
    return
}
```

启动接线(`cmd/claude-go/main.go` 团队管理器初始化后):

```go
LoadWorkflowsFromDir(filepath.Join(stateDir, "workflows"), roleRegistry)
```

目录约定 `~/.claude-go/workflows/*.json`,示例:

```json
{
  "name": "my-doc-pipeline", "description": "调研→撰写→评审",
  "mode": "pipeline", "qualityGate": "content", "producesCode": false,
  "stages": [
    { "name": "research", "role": "researcher", "prompt": "调研: {objective}" },
    { "name": "write", "role": "tech-writer", "dependsOn": ["research"], "prompt": "基于调研撰写: {prev_result}\n{user_feedback}" },
    { "name": "review", "role": "tech-critic", "dependsOn": ["write"] }
  ]
}
```

---

## 4. L2 / L3 / L4(地基之上的薄层)

- **L2 运行时编辑**:在 `customWFs` 上加 `UpdateWorkflowStage(name, stage, patch)` / `AddStage`,每次改完重跑 `Validate`。配 dashboard API `PATCH /api/workflows/{name}`(复用现有 `/api/workflows/` handler)。价值中等(调试/微调用)。
- **L3 自然语言生成**:
  ```go
  func GenerateWorkflowFromNL(ctx, objective string, llm LLMClient, roles *RoleRegistry) (*WorkflowDef, error) {
      // SimpleComplete(系统提示: 输出严格 JSON WorkflowDef, mode 限 pipeline/fanout/adversarial/orchestrated,
      //                role 从给定角色清单里选) → extractJSONObject → Unmarshal → Validate
  }
  ```
  复用上一轮 content_gate.go 里已写的 `extractJSONObject`。把"可选 role 清单"喂给 LLM,避免它编造不存在的 role(否则 Validate 拒)。生成后**先给用户预览再注册**(可挂到 dashboard)。
- **L4 蜂群**:已存在,无需开发。定位:L1-L3 是"结构化、可复用、可审计"的工作流;L4 是"一次性、全自动拆解"。两者正交,按任务确定性选择。

---

## 5. 落地路线图

| 阶段 | 内容 | 工作量 | 价值 | 风险 |
|---|---|---|---|---|
| **P1(地基)** | §3 全部:WorkflowDef 加字段 + Validate + RegisterWorkflow(带锁) + 门禁改声明式 + LoadWorkflowsFromDir + 启动接线 + `/workflow load`、`/workflow list --custom` CLI | 小-中 | 高(开放 JSON 自定义工作流) | 低(纯增量,内置行为不变) |
| **P2** | L2 运行时编辑 + dashboard PATCH | 小 | 中 | 低 |
| **P3** | L3 NL 生成 + 预览确认 | 中 | 高(零代码定义) | 中(prompt 工程 + 必过 Validate) |
| **P4** | 工作流模板库/版本管理/A-B(可选) | 大 | 低-中 | — |

**强烈建议从 P1 起步**:它是其余所有层的地基,且**只新增、不改内置工作流行为**。两个"地雷"(门禁声明式化、模式白名单)必须在 P1 一并解决,否则动态工作流上线即埋静默 bug。

---

## 6. 验收要点(P1 实现时)

1. 放一个 `~/.claude-go/workflows/x.json`(pipeline)→ `/go x <目标>` 能真实跑通,产出与内置 pipeline 一致。
2. 自定义 `producesCode:true` 的工作流 → 编译门禁**确实触发**(验证地雷 A 已拆)。
3. `mode:"creative_media"` 的自定义 def → 注册**被拒并报清晰错误**(验证地雷 B 已拆)。
4. 坏 def(缺 role 且无 prompt / 依赖不存在 / 成环)→ `Validate` 拒,不进注册表。
5. 并发创建多个团队同时读注册表 + 后台注册新工作流 → `go test -race` 无 data race。

---

## 7. 小结

WorkflowDef 已是纯数据、执行引擎已解耦、角色注册/蜂群/编排三条动态路径已就绪——动态工作流的"地基件"大多现成。真正要做的是 **P1 三件套**:① 注册表运行时化(带锁)+ 配置加载;② 把按名字判定的质量门禁改成**声明式字段**(否则动态工作流静默丢门禁);③ 限定动态定义只用**纯数据模式** + `Validate()` 兜底。做完 P1,L2/L3 都是薄封装,L4 直接复用。

> 需要的话我可以直接实现 **P1**(预计集中在 `pkg/agent/workflow.go` 注册/校验/加载 + `teams.go`/`content_gate.go` 门禁改造 + `cmd/claude-go/main.go` 接线 + 一个 `-race` 测试),并保持内置工作流行为零变化。
