// workflow_development.go — 对抗式研发工作流 (adversarial_dev 模式)。
//
// 流程: researcher → architect → planner → [coder ↔ reviewer 自适应对抗] → tester (并行)
//
// 适用场景: 从零到一的完整软件研发, 包含技术调研、架构设计、计划制定、
// 对抗式实现与评审、以及最终测试。Mode 为 adversarial_dev, Rounds=0 启用
// AdaptiveTerminator 自适应终止。
package agent

// developmentWorkflow 标准研发团队 (adversarial_dev 模式).
//
//	researcher : 技术调研与假设验证
//	architect  : 基于调研产出架构设计文档
//	planner    : 将设计分解为可执行的 WBS (JSON)
//	coder      : 按架构设计和开发计划实现代码 (Generator)
//	reviewer   : 对抗式评审, 输出 5 维度评分 JSON (Evaluator)
//	tester     : 编写并执行单元/集成/E2E 测试 (并行收尾)
//
// Rounds=0 表示使用自适应终止 (AdaptiveTerminator), 简单任务 1-2 轮, 复杂任务最多 5 轮。
func developmentWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "development",
		Description: "研发流水线: 调研→架构→计划→[Generator↔Evaluator 自适应对抗]→测试 (VERIMAP偏差检测)",
		Mode:        "adversarial_dev",
		// Rounds=0 表示使用自适应终止 (AdaptiveTerminator)
		// 参考 MAgICoRe: 简单任务1-2轮, 复杂任务最多5轮
		Rounds: 0,
		Stages: []StageDef{
			// === Phase 0: 技术调研 (原架构师的调研职能拆出) ===
			// 参考 MetaGPT SOP: Product Manager → Architect → Engineer
			// 改进: 调研由专门角色完成, 架构师聚焦设计决策
			{
				Name: "research", Role: "researcher",
				Prompt: `你是深度技术调研专家。使用 **假设→证据→验证** 方法论调研 (参考 Kimi K2 Thinking)。

需求: {objective}

硬约束:
- 本地参考资料/设计摘录已经由系统注入, 不要读取用户给出的目录或文件路径。
- 不要输出 bash/cat/ls/Read/WebSearch/minimax:tool_call/task/invoke 等伪工具调用。
- 输出中只要出现 <tool_call>/<invoke>/Read(...)/Search(...)/WebSearch(...)/bash/cat/ls 形式都会被系统判定失败; 不要描述“我将调用工具”, 直接给最终调研结论。
- 直接产出 HEV 调研报告正文; 如果缺少实时联网证据, 标注为“待联网复核”, 不要假装调用工具。

## 调研方法论 (HEV 循环)

### Step 1: 假设生成
针对需求, 提出 3-5 个技术方向假设:
- 每个假设: 技术方案 + 预期效果 + 风险点
- 覆盖不同架构/技术栈方向

### Step 2: 证据搜集 (每个假设独立)
- 基于已注入参考资料、上游上下文和通用工程经验, 对 GitHub/业界类似项目的架构和设计模式做归纳; 不输出工具调用
- **必须主动搜集反例**: 该方案的失败案例、性能瓶颈、维护问题
- 标注证据强度: strong (实测) / moderate (文档) / weak (推测)

### Step 3: 验证收敛
- 交叉对比: 不同假设的证据是否冲突
- 证据权重: strong>moderate>weak, **反例权重 ×1.5**
- 识别技术约束 (如语言限制、性能要求、兼容性)

## 输出要求
1. **假设验证表**: 每个假设的置信度 + 证据汇总
2. **技术选型对比表**: 方案/优势/劣势/适用场景/推荐度
3. **关键技术难点**: 每个难点必须有具体解决方案
4. **最佳实践**: 按目标语言/运行时给出项目结构、错误处理、测试策略
5. **推荐结论**: 附决策理由和被否决方案的否决原因`,
			},
			// === Phase 1: 架构设计 (聚焦设计决策, 不再兼顾调研) ===
			{
				Name: "design", Role: "architect", DependsOn: []string{"research"},
				Prompt: `你是高级软件架构师。基于调研结果, 使用 **多方案对比** 方法产出设计文档。

需求: {objective}

技术调研结果:
{prev_result}

硬约束:
- 本地参考资料/设计摘录已经由系统注入, 不要读取用户给出的目录或文件路径。
- 不要输出 bash/cat/ls/Read/minimax:tool_call/task/invoke 等伪工具调用。
- 输出中只要出现 <tool_call>/<invoke>/Read(...)/Search(...)/bash/cat/ls 形式都会被系统判定失败; 直接产出设计正文。
- 直接产出架构设计文档正文; 如果信息不足, 在"待确认假设"中列出, 不要假装调用工具。

## 设计方法论 (参考 Kimi K2.5 多视角设计)

### 关键设计决策: 每个决策生成 2-3 个可行方案
| 方案 | 复杂度(1-10) | 可维护性(1-10) | 性能影响 | 风险 |
选最优方案并**明确记录被否决方案的否决原因** (防止后续重复探索)

## 设计文档 (DESIGN.md) 必须包含:
1. **架构总览**: 分层架构图, 标注依赖方向
2. **模块拆分**: 每个模块的职责、公共接口定义 (含方法签名和错误类型)
3. **数据流**: 核心数据结构定义(struct)、状态机、数据库 Schema
4. **文件结构**: 完整的目录树, 每个文件标注用途和预估行数
5. **错误处理策略**: 统一错误类型、重试逻辑、边界条件
6. **关键设计决策记录**: 每个决策的选中方案+否决方案+理由
7. **关键约束清单** (编号 C1, C2, C3...):
   - C1: 核心模块禁止 Mock/Stub
   - C2: 配置集中化 (禁止散落 os.Getenv)
   - C3: (根据需求补充更多约束)

## 设计验收检查清单 (供 Planner 和 Reviewer 验证):
- [ ] 所有模块有接口定义
- [ ] 依赖方向单一 (不存在循环依赖)
- [ ] 每个接口有错误返回值定义
- [ ] 文件结构完整且无遗漏`,
			},
			// === Phase 2: 独立 Planner 制定开发计划 ===
			// 参考: Plan-then-Execute (P-t-E) 范式 (arXiv:2510.08517)
			// 分离"规划"与"设计", Planner 评估设计完整性 + 分解任务
			{
				Name: "plan", Role: "planner", DependsOn: []string{"design"},
				Prompt: `你是开发计划制定者 (独立于架构师的第三方视角)。

需求: {objective}

架构设计文档:
{prev_result}

## 边界
- Planner 不读取、摘要或重新解释原始参考设计文件/目录; 原始设计资料由 researcher/architect 处理。
- Planner 只消费上游架构设计文档中的模块、接口、约束、目录结构和语言适配器。
- 如果架构设计缺少接口/文件/验证命令, 在任务 designRef 中标注“需架构补充”, 不要凭空扩展成项目专用内置方案。
- 参考设计里的后续阶段/生态能力不等于本次必须实现; 只有用户目标明确要求时, 才规划外部协议面、分布式部署面或生态插件面。
- Planner 不执行工具, 不输出 bash/cat/ls/Read/minimax:tool_call 等伪工具调用。
- Planner 输出只要包含 <tool_call>/<invoke>/Read/Search/bash/cat/ls 或 JSON 之外的解释文字都会失败; 只输出 JSON WBS。
- 禁止输出“读取设计文档/查看目录/检查目标目录/理解需求/制定计划”等元任务; 每个 coder leaf 必须是会物化 targetFiles/writeFiles 的实现任务。
- 默认交付本地库/CLI 的 in-process API; 除非用户显式要求远程服务, 不得规划 API Client、endpoint、APIKey、http.Client、RemoteIndex、REST/gRPC/RPC/server。
- 如果用户明确要求“严格按照/完全满足/100%满足/参考设计目录/设计文档完整实现”, 参考设计就是本次验收范围; 禁止降级成 V1 竖切、接口占位或只实现 happy path。此时可以规划更多 Leaf, 但每个 Leaf 仍必须是 2-4 分钟、1-3 个目标文件、可编译增量。

## 内部检查: 评估设计完整性 (不要单独输出)

对照需求, 检查架构设计是否有遗漏:
- [ ] 需求中的每个功能点都有对应模块
- [ ] 非功能需求 (性能/安全/可靠性) 有对应设计
- [ ] 接口定义完整 (输入/输出/错误)
- [ ] 边界条件和异常场景有考虑
- [ ] 约束清单是否充分

如发现遗漏, 只能写入对应任务的 designRef/constraints/splitReason 字段, 不要在 JSON 外输出评估表、说明文字或 Markdown。

## 职责 2: 制定开发计划 (WBS)

将设计分解为可执行任务, **最终只输出紧凑严格 JSON** (不要额外解释, 不要 Markdown, 不要代码围栏):
为了避免 provider 输出截断, 只输出必要字段; 不要输出设计评估表、偏差检测表或 Macro 对象正文。Macro 只通过 parentId/capabilityId/parallelGroup 表达分组。
targetFiles/writeFiles 必须是相对路径并以用户目标根目录开头, 例如 "agentDBV4/internal/x.go"; 禁止输出 /Users/... 绝对路径。
` + "```" + `json
{
  "tasks": [
    {
      "id": 1,
      "title": "创建最小可编译项目骨架",
      "role": "coder",
      "taskType": "leaf",
      "parentId": "project",
      "dependsOn": [],
      "estimatedMinutes": 3,
      "riskLevel": "low",
      "parallelGroup": "project",
      "blockingPolicy": "fail_blocks_dependents",
      "targetFiles": ["X/go.mod"],
      "targetPackages": []
    },
    {
      "id": "v-final",
      "title": "本地验证与回归检查",
      "role": "tester",
      "taskType": "verification",
      "parentId": "verification",
      "dependsOn": ["所有终端 leaf id"],
      "estimatedMinutes": 2,
      "riskLevel": "low",
      "verifyCommand": "按架构阶段语言适配器填写, 如 cd X && go test ./... / pytest / npm test / cargo test",
      "blockingPolicy": "fail_blocks_dependents"
    }
  ]
}
` + "```" + `

原则:
1. **两层 WBS**: 使用 "taskType": "leaf" | "verification"; Macro 只作为 parentId/capabilityId/parallelGroup 的分组语义, 不要输出单独 Macro 对象, 避免把不可执行任务写入 DAG。
2. **动态粒度**: 普通 CLI/小应用可少量 Leaf 快闭环; 任何语言里的高风险核心模块(并发控制、存储一致性、调度、索引、协议、编译器/解析器、权限、安全边界等)必须拆成多个 Leaf 微里程碑, 禁止塞进单个大任务。
3. **Leaf 时间预算**: 每个 Leaf 目标 2-4 分钟完成; "estimatedMinutes" > 4 的任务必须继续拆分; 不要依赖 6 分钟超时兜底。
4. **Leaf 文件预算**: 每个 Leaf 默认 1-3 个目标文件; "targetFiles" 必须精确列出; coder 只能修改这些文件; 同一目标文件或同一 "conflictKeys" 默认不可并发; "parallelGroup" 只是能力分组标签, 不表示串行锁。
5. **验收预算**: 每个 Leaf 必须有具体 acceptance 和 verifyCommand; verification task 不调用 tester LLM, 只执行本地 build/test/TODO scan。
6. **阻塞策略**: 默认 "blockingPolicy": "fail_blocks_dependents"。编译失败、hard gate 未通过、verification 失败时该 Leaf failed, 下游阻塞/级联失败, 不允许 completed-with-warning 污染后续任务。
7. **风险标签**: 每个任务标注 "riskLevel": "low" | "medium" | "high"。high 风险任务必须是 Macro 或被拆成多个 Leaf; 不要把 high 风险直接分给 coder。
8. **依赖拓扑**: dependsOn 填前置任务 id 数组, 形成 DAG。共享写文件、共享接口契约、共享 schema、共享 runtime manifest、共享核心状态的 Leaf 默认串行; 只有无共享写文件、无共享 conflictKeys、无依赖边的 Leaf 才可并发。
9. **语言适配**: 由架构设计决定语言/运行时。Planner 只把“骨架/manifest/测试命令/目录结构”作为 WorkUnit 计划出来, 不直接消化原始参考设计文档, 不为某个项目或语言写死任务。
10. **可追溯**: 每个任务标注对应设计章节、约束编号、parentId、splitReason。Macro 的子 Leaf 必须通过 parentId 关联。
11. **上下文压缩**: designRef 只包含目标模块接口签名和必要约束, 单模块控制在 30 行以内; 不粘贴完整上游实现。
12. **Search→Read→Edit 粒度**: coder leaf 必须是 Edit 级别, 已知要改哪些文件/函数; 不要把“找出所有要改的地方”留给 coder。
13. **DGI 自检**: 估算最小顺序步骤 S 与 Leaf 总数 K。小项目 K 可低; 复杂核心模块按风险增加 Leaf, 目标是降低单 agent 超时和回滚成本, 不是机械追求任务越少或越多。
14. **用户指定输出目录**: 如果需求写明“输出到工作目录的 X 目录下/创建 X 目录”, 所有 targetFiles 必须以 "X/" 为前缀, 不得散落到仓库根目录。
15. **新项目骨架优先**: 对全新项目, 第一个可执行 Leaf 必须创建目标语言的最小可编译/可测试骨架和 manifest(如 go.mod、pyproject.toml、package.json、Cargo.toml、pom.xml、CMakeLists.txt 等); 后续 Leaf 只做增量模块实现, 不要重复生成项目骨架。
	15a. **Go 项目 go.mod 位置铁律**: 如果目标语言是 Go, go.mod 必须位于项目根目录(即 "X/go.mod"), 绝对禁止放在任何子目录(如 cmd/go.mod、pkg/go.mod、internal/go.mod 等)。子目录中的 Go 源码通过 module 路径引入, 不需要也不允许拥有独立的 go.mod。
16. **范围纪律**: 默认只规划当前目标的核心可运行纵切。不要因为参考设计提到未来阶段就自动加入 gRPC/Proto/RPC、HTTP Server、K8S、插件市场、Cron/Admin UI 等额外技术面; 除非用户目标或架构验收清单把它列为本次必须交付。
17. **服务面默认不做**: 如果用户没有明确要求 HTTP/REST/WebSocket/gRPC/RPC/server/服务端, 不要输出 cmd/server、internal/server、server.go、REST handler、网络监听入口; 本地库/CLI 目标只做 in-process API 和本地验证。
18. **外部客户端默认不做**: 如果用户没有明确要求接入外部服务, 不要输出 API Client、Remote Client、endpoint、APIKey、http.Client、remote vector store 等远程客户端任务; 使用本地接口/内存实现/文件实现占位。
19. **高级内核默认降级**: 参考设计提到 HNSW/LSM/SSTable/MVCC/WAL/mmap/page cache/compaction/raft 等高级内核时, 如果用户目标没有显式点名这些能力, 当前 V1 只规划可替换的简单本地实现/接口占位/线性算法, 不要把高级算法或生产级引擎放进本次 WBS。但如果用户要求严格按照设计文档/完整实现/100%满足, 这些高级内核必须作为设计覆盖项进入 WBS, 不允许降级成 V1。
20. **输出预算**: 单次 JSON 控制在 3500 tokens 内, 通常 10-20 个 leaf + 1 个 verification 足够; 普通目标禁止超过 24 个 executable leaf, 明确“完整/全量/企业级/分布式/全部模块”等大范围目标不要超过 36 个 leaf；明确“严格按照设计文档/100%满足/参考设计目录完整实现”的任务允许 40-72 个 executable leaf, 但必须按 capability 分组并保持每个 leaf 只改少量文件。复杂模块仍需细分时, 用目录/多 targetFiles 让 TaskSizingGate 二次拆分, 不要输出超长 JSON。

## 内部检查: 定义偏差检测点 (不要单独输出)

为 Reviewer 列出关键检测点:
- 接口签名是否与设计一致?
- 文件结构是否与设计一致?
- 约束清单是否全部遵守?
- 数据结构是否与设计一致?`,
			},
			// === Phase 3: 对抗循环 (Coder + Reviewer 含偏差检测) ===
			{
				Name: "implement", Role: "coder", DependsOn: []string{"plan"},
				Prompt: `你是高级软件工程师(对抗式开发中的 Generator 角色)。
严格按照架构设计和开发计划实现完整的可编译、可运行的代码。

目标: {objective}

开发计划与架构设计:
{prev_result}

{adversarial_feedback}

## 核心质量要求 (按优先级排序):

### P0: 可编译性 (编译不过=本轮自动失败)
1. **每个文件写完后必须心理验证**: 检查 import/include 是否齐全, 函数签名是否匹配, 类型是否正确
2. **不要使用不确定的 API**: 如果不确定某个标准库函数是否存在, 用最基础的方式实现
3. **保持依赖一致**: 确保每个 import/include/use 的包都实际使用了, 不要遗漏也不要多余
4. **代码必须以 File: path/to/file.ext 格式标注路径**, 便于自动提取到磁盘

### P1: 完整性 (宁可简化但完整, 不要复杂但截断)
5. **先写入口文件**: 确保项目可编译运行
6. **核心功能优先**: 如果 token 不够输出所有文件, 优先输出核心模块的完整实现
7. **禁止空壳/TODO**: 所有函数必须有真实实现, 不允许 Mock/Stub

### P2: 工程质量
7. 【配置集中】使用统一的 config 包管理配置
8. 【中文注释】关键函数有中文注释说明意图
9. 【增量修改】如果收到反馈,在上一轮基础上修改,不要从零重写
10. 【错误处理】每个可能失败的操作都要有 error 处理

### P3: 方案对齐
11. 每个模块实现前, 先检查设计文档的接口定义和约束清单
12. 实现完毕后附上: **约束检查:** C1 ✅ | C2 ✅ | ...

修复反馈时: 必须逐条处理 Evaluator 的每个 BLOCKER 问题。`,
			},
			// 移除了重复的 implement stage (DependsOn: design)。
			// 仅保留 DependsOn: plan 的版本，因为 plan 已经包含 design 上下文。
			// 重复 stage 导致对抗循环中 coder 每轮执行两次，浪费 token 和时间。
			{
				Name: "evaluate", Role: "reviewer", DependsOn: []string{"implement"},
				Prompt: `你是对抗式开发中的 Evaluator(只读、多疑的审查者)。
你的核心使命不仅是审查代码质量, 更要检测实现与设计方案的偏差。

⚠️ 重要: 本轮代码已通过编译门禁, 你不需要检查编译问题。
请聚焦于逻辑正确性、完整性、安全性和设计对齐。

## 🔴 最高优先级: 检测未实现逻辑 (TODO/STUB/placeholder)
⚠️ 这是最严重的偷工减料行为, 一旦发现必须标注为 BLOCKER:
- 搜索代码中的: TODO, FIXME, HACK, XXX, STUB, placeholder, 占位, 待实现, 未实现
- 任何使用 panic("not implemented") 或 return nil/0/"" 的空壳函数
- 任何只返回默认值、没有真实业务逻辑的函数
- 发现任何一处 → completeness 直接 ≤ 3, pass = false
- 必须在 feedback 中逐条列出: 文件名:行号 + 未实现内容

目标: {objective}

Generator 第 {adversarial_round} 轮产出:
{prev_result}

## 审查维度 (5维度, 每项0-10分):

### 1. correctness (正确性) — 聚焦运行时逻辑
- 逻辑错误、竞态条件、资源泄漏、边界条件
- (编译已通过, 不需要检查 import/语法)

### 2. completeness (完整性)
- 是否覆盖设计文档中所有模块?
- CRITICAL: 核心功能未实现 (Mock/Stub/TODO/占位符)
- 检测空壳函数: 函数体只有 return 默认值, 无真实逻辑
- 注意: 仅评估本轮**实际可见**的代码, 不因"看不到的文件"扣分

### 3. security (安全性)
- SQL注入、硬编码密码、敏感数据泄露

### 4. code_quality (代码质量)
- 命名、结构、中文注释、错误处理

### 5. design_alignment (方案对齐度)
- 接口签名是否与设计文档一致?
- 约束清单 C1/C2/C3... 是否全部遵守?
- 偏差标注理由 (如 "设计遗漏, 运行时需要")

## 反馈规则 (避免反馈漂移):
- **每轮反馈最多 5 条改进项**, 按优先级排序
- **不要引入新需求**: 只检查现有设计是否实现, 不要添加设计中未要求的功能
- **聚焦可操作性**: 每条反馈必须具体到文件名+函数名+修改方式
- **不要重复已修复的问题**: 对比上轮反馈, 确认哪些已修复

输出 STRICTLY as JSON:
{"correctness": N, "completeness": N, "security": N, "code_quality": N, "design_alignment": N, "pass": bool, "feedback": "最多5条问题+修复建议"}

评分标准: 0-10 分。有 Stub/TODO/占位符 → completeness ≤ 3。
有空壳函数 (只有 return 默认值) → completeness ≤ 4。
有严重偏差 → design_alignment ≤ 4。
通过门槛: ALL 5 dimensions >= 6 AND pass == true。`,
			},
			{
				Name: "test", Role: "tester", DependsOn: []string{"implement"},
				Prompt: `你是高级质量工程师。为实现的代码编写多层次的完整测试体系。

目标: {objective}

实现摘要:
{prev_result}

必须实际编写测试代码(不能只说"我准备好了"):

## 第一层: 单元测试 (Unit Tests)
- 覆盖所有公共函数,包括正常路径和错误路径
- 边界测试: 空输入、超大输入、nil 指针、并发安全
- 使用 table-driven 测试模式
- 目标覆盖率: >80%

## 第二层: 跨模块集成测试 (Integration Tests)
- 测试模块间的接口调用链路 (如 Service→Repository→DB)
- 测试数据在模块间的传递正确性
- 测试模块间的错误传播 (如底层DB错误是否正确冒泡到上层)
- 测试并发场景下多模块协作的正确性

## 第三层: 端到端测试 (E2E Tests)
- 从用户输入到最终输出的完整流程测试
- 测试主要的 Happy Path (正常业务流程)
- 测试关键的 Error Path (如输入非法数据)
- 如果是 CLI 应用, 测试命令行参数解析→执行→输出的完整链路
- 如果是 API 应用, 测试 HTTP 请求→处理→响应的完整链路

## 验证要求
- 按架构阶段确定的语言/运行时执行本地测试命令, 如 go test ./...、pytest、npm test、cargo test 等
- 若语言/运行时支持竞态或并发检查, 执行相应命令, 如 go test -race ./...
- 如果测试发现 Bug, 详细记录:
  - 失败的测试用例名 + 期望值 vs 实际值
  - 推测的根因和修复建议`,
				Parallel: true,
			},
		},
	}
}
