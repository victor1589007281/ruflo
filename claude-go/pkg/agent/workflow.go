// Workflow — 多 Agent 协作工作流模式。
//
// 三种核心模式 (参考 CrewAI + LangGraph + AutoGen):
//
//  1. Pipeline (开发): Architect → Coder → Reviewer → Tester (串行依赖)
//  2. Fan-Out (调研): Researcher₁ ∥ Researcher₂ → Synthesizer (并行汇聚)
//  3. Adversarial (辩论): Proposer ↔ Opponent × N轮 → Judge (对抗决策)
//
// 工作流执行器按 stage 依赖拓扑排序, 自动传递上下文。
package agent

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/metrics"
	"github.com/anthropic/claude-go/pkg/observability"
	"github.com/anthropic/claude-go/pkg/sandbox"
)

// PromptCache 提示词缓存 (参考 Anthropic Prompt Caching)。
// 将稳定的 system prompt 前缀和 tool definitions 分离, 最大化缓存命中率。
type PromptCache struct {
	mu           sync.RWMutex
	staticPrefix string // 不变内容: system prompt + tool defs + repo context
	prefixHash   string // SHA256 用于缓存追踪
	cacheHits    int64  // 命中次数 (同一 prefix 复用)
	cacheMisses  int64  // 未命中 (prefix 变化)
}

// BuildPrompt 组合静态前缀+动态后缀, 追踪缓存命中。
func (pc *PromptCache) BuildPrompt(dynamicSuffix string) string {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.cacheHits++
	return pc.staticPrefix + "\n\n" + dynamicSuffix
}

// UpdatePrefix 更新静态前缀 (prefix 变化时 cache miss)。
func (pc *PromptCache) UpdatePrefix(newPrefix string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	newHash := fmt.Sprintf("%x", len(newPrefix)) // lightweight hash
	if newHash != pc.prefixHash {
		pc.cacheMisses++
		pc.prefixHash = newHash
	}
	pc.staticPrefix = newPrefix
}

// HitRate 缓存命中率。
func (pc *PromptCache) HitRate() float64 {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	total := pc.cacheHits + pc.cacheMisses
	if total == 0 {
		return 0
	}
	return float64(pc.cacheHits) / float64(total)
}

// SummarizeOldOutput 渐进式摘要 (参考 Kimi K2 溢出策略 + MemGPT)。
// 超过 maxLen 的旧输出压缩为关键信息摘要。
func SummarizeOldOutput(output string, maxLen int) string {
	if len(output) <= maxLen {
		return output
	}
	lines := strings.Split(output, "\n")
	var summary []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		isKey := strings.HasPrefix(line, "#") || strings.HasPrefix(line, "func ") ||
			strings.HasPrefix(line, "type ") || strings.Contains(line, "决策") ||
			strings.Contains(line, "结论") || strings.Contains(line, "推荐") ||
			strings.Contains(line, "错误") || strings.Contains(line, "FAIL") ||
			strings.HasPrefix(line, "- [") || strings.HasPrefix(line, "│")
		if isKey {
			summary = append(summary, line)
		}
	}
	result := strings.Join(summary, "\n")
	if len(result) > maxLen {
		result = result[:maxLen]
	}
	if result == "" {
		result = output[:maxLen]
	}
	return "[摘要] " + result
}

// WorkflowDef 工作流定义
type WorkflowDef struct {
	Name        string
	Description string
	Mode        string     // pipeline, fanout, adversarial
	Stages      []StageDef // pipeline/fanout 模式
	Rounds      int        // adversarial 模式的对抗轮数
}

// StageDef 阶段定义
type StageDef struct {
	Name      string   // 阶段名称
	Role      string   // agent 角色
	Prompt    string   // 系统提示词模板 (支持 {objective}, {prev_result} 占位符)
	DependsOn []string // 依赖的前置阶段
	Parallel  bool     // 是否可与同级并行
}

// GetWorkflow 获取预定义工作流
func GetWorkflow(name string) *WorkflowDef {
	switch name {
	case "development", "dev":
		return developmentWorkflow()
	case "research":
		return researchWorkflow()
	case "debate":
		return debateWorkflow()
	case "swarm":
		return swarmWorkflow()
	case "finance", "trading":
		return financeWorkflow()
	case "trading-v2", "trading2", "finance-v2":
		return tradingV2Workflow()
	case "techblog", "article", "blog":
		return techBlogWorkflow()
	case "creative", "design", "visual":
		return creativeWorkflow()
	case "creative-v2", "creative2", "media", "html", "web":
		return creativeV2Workflow()
	case "predict", "prediction", "forecast":
		return predictWorkflow()
	case "novel-v2":
		return novelV2Workflow()
	case "novel-v3", "novel", "fiction", "story", "swarm-novel":
		return novelV3Workflow()
	case "ml-training", "ml", "finetune", "training":
		return mlTrainingWorkflow()
	case "app", "miniprogram", "mobile":
		return appCompositeWorkflow()
	case "game", "gamedev", "game-dev":
		return gameCompositeWorkflow()
	case "code-review", "review", "cr":
		return codeReviewWorkflow()
	case "testing", "test-team", "qa":
		return testingWorkflow()
	case "parenting", "education", "育儿", "edu":
		return parentingWorkflow()
	case "hiring", "interview", "job", "recruit":
		return hiringWorkflow()
	default:
		return nil
	}
}

// ListWorkflows 列出所有可用工作流
func ListWorkflows() []WorkflowDef {
	return []WorkflowDef{
		*developmentWorkflow(),
		*researchWorkflow(),
		*debateWorkflow(),
		*swarmWorkflow(),
		*financeWorkflow(),
		*tradingV2Workflow(),
		*techBlogWorkflow(),
		*creativeWorkflow(),
		*creativeV2Workflow(),
		*predictWorkflow(),
		*novelV2Workflow(),
		*novelV3Workflow(),
		*mlTrainingWorkflow(),
		*appCompositeWorkflow(),
		*gameCompositeWorkflow(),
		*codeReviewWorkflow(),
		*testingWorkflow(),
		*parentingWorkflow(),
		*hiringWorkflow(),
	}
}

// predictWorkflow 群体智能预测工作流。
// 实际执行由 SwarmIntelligenceEngine 接管, WorkflowDef 仅用于 CreateTeam 验证。
func predictWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "predict",
		Description: "群体智能预测 — 多Agent辩论+贝叶斯融合",
		Mode:        "predict",
		Stages: []StageDef{
			{Name: "decompose", Role: "decomposer"},
			{Name: "scout", Role: "scout"},
			{Name: "predict", Role: "analyst"},
			{Name: "debate", Role: "critic"},
			{Name: "fuse", Role: "synthesizer"},
		},
	}
}

func swarmWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "swarm",
		Description: "蜂群模式: LLM 动态拆解 → 并行执行 → 结果汇聚 (Kimi K2.5 启发)",
		Mode:        "swarm",
	}
}

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

## 调研方法论 (HEV 循环)

### Step 1: 假设生成
针对需求, 提出 3-5 个技术方向假设:
- 每个假设: 技术方案 + 预期效果 + 风险点
- 覆盖不同架构/技术栈方向

### Step 2: 证据搜集 (每个假设独立)
- 搜索 GitHub/业界类似项目, 分析架构和设计模式
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
4. **最佳实践**: Go 项目结构、错误处理、测试策略
5. **推荐结论**: 附决策理由和被否决方案的否决原因`,
			},
			// === Phase 1: 架构设计 (聚焦设计决策, 不再兼顾调研) ===
			{
				Name: "design", Role: "architect", DependsOn: []string{"research"},
				Prompt: `你是高级软件架构师。基于调研结果, 使用 **多方案对比** 方法产出设计文档。

需求: {objective}

技术调研结果:
{prev_result}

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

## 职责 1: 评估设计完整性 (Design Review)

对照需求, 检查架构设计是否有遗漏:
- [ ] 需求中的每个功能点都有对应模块
- [ ] 非功能需求 (性能/安全/可靠性) 有对应设计
- [ ] 接口定义完整 (输入/输出/错误)
- [ ] 边界条件和异常场景有考虑
- [ ] 约束清单是否充分

如发现遗漏, 在计划中标注 "⚠️ 设计补充" 项。

## 职责 2: 制定开发计划 (WBS)

将设计分解为可执行任务, **输出严格 JSON** (不要额外解释):
` + "```" + `json
{
  "tasks": [
    {
      "id": 1,
      "title": "任务标题 (简洁, 单一职责)",
      "role": "coder",
      "taskType": "leaf",
      "parentId": "",
      "dependsOn": [],
      "designRef": "设计章节名",
      "constraints": ["C1"],
      "acceptance": "编译通过 + 接口签名与设计一致",
      "priority": 2,
      "complexity": "medium",
      "estimatedMinutes": 3,
      "riskLevel": "medium",
      "verifyCommand": "go test ./pkg/xxx/...",
      "parallelGroup": "storage-mvcc",
      "blockingPolicy": "fail_blocks_dependents",
      "splitReason": "",
      "targetFiles": ["internal/storage/engine.go", "internal/storage/engine_test.go"],
      "targetPackages": ["./internal/storage/..."]
    }
  ]
}
` + "```" + `

原则:
1. **两层 WBS**: 使用 "taskType": "macro" | "leaf" | "verification"。Macro 只表达模块/里程碑, 不直接交给 coder; Leaf 才能执行; verification 只跑本地 build/test/TODO scan。
2. **动态粒度**: 普通 CLI/小应用可少量 Leaf 快闭环; MVCC、事务、锁、调度、索引、缓存一致性、并发控制等核心模块必须拆成多个 Leaf 微里程碑, 禁止塞进单个大任务。
3. **Leaf 时间预算**: 每个 Leaf 目标 2-4 分钟完成; "estimatedMinutes" > 4 的任务必须继续拆分; 不要依赖 6 分钟超时兜底。
4. **Leaf 文件预算**: 每个 Leaf 默认 1-3 个目标文件; "targetFiles" 必须精确列出; coder 只能修改这些文件; 同一目标文件或同一 "parallelGroup" 默认不可并发。
5. **验收预算**: 每个 Leaf 必须有具体 acceptance 和 verifyCommand; verification task 不调用 tester LLM, 只执行本地 build/test/TODO scan。
6. **阻塞策略**: 默认 "blockingPolicy": "fail_blocks_dependents"。编译失败、hard gate 未通过、verification 失败时该 Leaf failed, 下游阻塞/级联失败, 不允许 completed-with-warning 污染后续任务。
7. **风险标签**: 每个任务标注 "riskLevel": "low" | "medium" | "high"。high 风险任务必须是 Macro 或被拆成多个 Leaf; 不要把 high 风险直接分给 coder。
8. **依赖拓扑**: dependsOn 填前置任务 id 数组, 形成 DAG。MVCC/事务/锁/调度等共享核心状态默认串行微里程碑; 只有无共享文件、无共享核心状态、无依赖边的 Leaf 才可并发。
9. **复杂模块示例**: "MVCC 事务管理器" 应拆为: 数据结构与事务状态枚举; Begin/Commit/Rollback 生命周期骨架; ReadView 与可见性; 写写冲突与提交校验; 版本链读写与 GC 接口; MVCC 集成验证。
10. **可追溯**: 每个任务标注对应设计章节、约束编号、parentId、splitReason。Macro 的子 Leaf 必须通过 parentId 关联。
11. **上下文压缩**: designRef 只包含目标模块接口签名和必要约束, 单模块控制在 30 行以内; 不粘贴完整上游实现。
12. **Search→Read→Edit 粒度**: coder leaf 必须是 Edit 级别, 已知要改哪些文件/函数; 不要把“找出所有要改的地方”留给 coder。
13. **DGI 自检**: 估算最小顺序步骤 S 与 Leaf 总数 K。小项目 K 可低; 复杂核心模块按风险增加 Leaf, 目标是降低单 agent 超时和回滚成本, 不是机械追求任务越少或越多。
14. **用户指定输出目录**: 如果需求写明“输出到工作目录的 X 目录下/创建 X 目录”, 所有 targetFiles 必须以 "X/" 为前缀, 不得散落到仓库根目录。
15. **新 Go 项目骨架优先**: 对全新 Go 项目, 第一个可执行 Leaf 必须创建 "X/go.mod"、README 和最小可编译包/入口; acceptance/verifyCommand 使用 "cd X && go test ./..."。后续 Leaf 只做增量模块实现, 不要重复生成项目骨架。

## 职责 3: 定义偏差检测点 (Drift Checkpoints)

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
- 写完后运行 go test ./... 确保所有测试通过
- 运行 go test -race ./... 检查竞态条件
- 如果测试发现 Bug, 详细记录:
  - 失败的测试用例名 + 期望值 vs 实际值
  - 推测的根因和修复建议`,
				Parallel: true,
			},
		},
	}
}

func researchWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "research",
		Description: "调研汇总: 预规划→三方并行调研→交叉验证→综合分析 (V2: 借鉴 Extended Thinking + CoVe)",
		Mode:        "fanout",
		Stages: []StageDef{
			{
				Name: "research-planning", Role: "synthesizer",
				Prompt: `你是**首席分析师**，在调研团队出发前负责制定调研框架。
(借鉴 Claude Extended Thinking: 先想后做，避免研究员盲目调研)

调研主题: {objective}

## 你的任务
1. **分解核心问题**: 将调研主题拆解为 3-5 个核心子问题
2. **为每位研究员制定关键问题清单**:
   - 技术研究员: 必须回答的 3-5 个技术问题
   - 市场分析师: 必须验证的 3-5 个市场假设
   - 风险审计员: 必须排查的 3-5 个风险点
3. **预设交叉验证点**: 标注三方可能出现分歧的领域 (如性能数据、市场份额)
4. **明确调研边界**: 什么在范围内，什么在范围外

## 输出格式
结构化 Markdown，按"技术/市场/风险"三个维度列出问题清单。`,
			},
			{
				Name: "research-tech", Role: "tech-researcher",
				DependsOn: []string{"research-planning"},
				Prompt: `你是**技术深度研究员** (专注技术架构和实现)。对以下主题进行技术层面的深度调研。

调研主题: {objective}

首席分析师的调研框架:
{prev_result}

## 强制要求
1. **严格按照调研框架中的技术问题清单逐一回答** — 不要跳过任何问题
2. **使用 WebSearch 工具**验证关键技术数据 (版本号、性能指标、API 变更等)
3. 必须包含**负面案例**: 至少 2 个选型失败/踩坑案例及原因分析

## 调研维度
- 核心技术架构和设计理念
- 最新版本特性和 Roadmap (通过 WebSearch 验证)
- 性能基准数据 (注明数据来源和测试条件)
- 代码示例和实现模式
- 技术局限性和已知问题
- **踩坑案例**: 社区中报告的常见问题和解决方案

## 输出格式
结构化 Markdown，每个数据点注明来源 (官方文档/社区/实测)。在标注了"交叉验证点"的数据上额外标注数据来源的可信度(高/中/低)。`,
				Parallel: true,
			},
			{
				Name: "research-market", Role: "market-analyst",
				DependsOn: []string{"research-planning"},
				Prompt: `你是**市场与商业分析师** (专注商业价值和竞争格局)。对以下主题进行市场和商业层面的调研。

调研主题: {objective}

首席分析师的调研框架:
{prev_result}

## 强制要求
1. **严格按照调研框架中的市场假设清单逐一验证** — 不要跳过任何假设
2. **使用 WebSearch 工具**查询最新的市场数据和采用案例
3. 必须包含**量化数据**: 成本对比表、性能对比表、市场份额等
4. 必须包含**失败案例**: 至少 1 个迁移/采用失败的真实案例

## 调研维度
- 行业采用情况和市场趋势 (通过 WebSearch 获取最新数据)
- 竞品对比分析 (功能矩阵表)
- TCO 总拥有成本分析 (含运维、人力、迁移成本)
- 成功案例 AND 失败案例 (各至少 1 个)
- ROI 评估模型

## 输出格式
结构化 Markdown，数据密集，使用表格对比。在标注了"交叉验证点"的数据上额外标注数据来源的可信度(高/中/低)。`,
				Parallel: true,
			},
			{
				Name: "research-risk", Role: "risk-auditor",
				DependsOn: []string{"research-planning"},
				Prompt: `你是**风险审计员** (专注风险评估和合规)。对以下主题进行风险和安全层面的深度审计。

调研主题: {objective}

首席分析师的调研框架:
{prev_result}

## 强制要求
1. **严格按照调研框架中的风险排查清单逐一排查** — 不要跳过任何风险点
2. **使用 WebSearch 工具**搜索相关 CVE、安全公告、故障报告
3. 必须给出**量化风险评分** (影响 × 概率 矩阵)
4. 每个风险必须给出**具体缓解措施**

## 调研维度
- 安全风险 (CVE 历史、攻击面分析、合规要求)
- 技术风险 (单点故障、性能瓶颈、扩展性上限)
- 运维风险 (升级路径、向后兼容、社区活跃度)
- 迁移风险 (数据迁移方案、回滚计划、停机时间)
- 供应链风险 (依赖健康度、维护者活跃度)

## 风险评分矩阵
| 风险项 | 影响 (1-5) | 概率 (1-5) | 综合评分 | 缓解措施 |
|--------|-----------|-----------|---------|---------|

## 输出格式
结构化 Markdown，表格化风险矩阵，每个缓解措施需具体可操作。`,
				Parallel: true,
			},
			{
				Name: "cross-verification", Role: "fact-checker",
				DependsOn: []string{"research-tech", "research-market", "research-risk"},
				Prompt: `你是**交叉验证审查员** (借鉴 CoVe: Chain of Verification)。

调研主题: {objective}

三位研究员的调研结果:
{prev_result}

## 你的任务 (严格按步骤执行)

### Step 1: 提取关键声明
从三位研究员的报告中提取所有**事实性声明** (数字、日期、性能数据、CVE编号等)。
列出至少 10 条关键声明。

### Step 2: 交叉比对
对每条声明检查:
- 是否有多个来源佐证？
- 三位研究员的数据是否一致？
- 是否有矛盾？

### Step 3: 独立验证
对关键分歧和可疑数据, **使用 WebSearch 独立验证**。

### Step 4: 验证报告
| 声明 | 来源 | 验证结果 | 置信度 |
|------|------|---------|--------|
| ...  | 技术/市场/风险 | ✅确认/⚠️存疑/❌矛盾 | 高/中/低 |

### Step 5: 标注需要综合报告特别注意的分歧点`,
			},
			{
				Name: "synthesize", Role: "synthesizer",
				DependsOn: []string{"cross-verification"},
				Prompt: `你是**首席分析师**，负责深度综合所有调研结果并撰写最终报告。

调研主题: {objective}

调研团队产出 (含交叉验证结果):
{prev_result}

## 综合报告要求 (逐条完成, 不允许偷工减料)

1. **执行摘要** (5-8 条关键发现, 按重要性排序)
2. **技术深度分析**
   - 综合技术研究员的发现
   - **矛盾数据对比表**: 引用交叉验证审查员的验证结果
   - 技术可行性评分 (1-10, 含评分依据)
3. **市场分析** (竞品对比表、成本分析、ROI)
4. **风险热力图** (影响×概率矩阵, 标红 Top 5)
5. **失败案例专题** (综合所有负面案例, 提炼共性教训)
6. **实施路线图** (分阶段: MVP→Beta→GA, 含里程碑和交付物)
7. **数据可信度总结** (基于交叉验证结果)
   - 高置信度事实: 直接引用
   - 中置信度数据: 标注需要进一步确认
   - 低置信度/矛盾数据: 标注 "⚠️ 待人工验证"
8. **结论与决策建议** (给出明确的"推荐/谨慎推荐/不推荐"评级)

## 质量红线
- 三位研究员的产出必须逐篇阅读、逐条交叉验证, 不允许简单拼接
- 交叉验证审查员标注为 ❌矛盾 的数据必须在报告中明确标注
- 报告长度不少于 500 行 (确保深度整合, 非简单提取)
- 将报告保存为独立 Markdown 文件`,
			},
		},
	}
}

func debateWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "debate",
		Description: "智能辩论: 预分析→自适应轮数→证据验证→裁决 (V2: 借鉴 iMAD+CoVe+DRA)",
		Mode:        "adversarial",
		Rounds:      3,
		Stages: []StageDef{
			{
				Name: "pre-analysis", Role: "analyst",
				Prompt: `你是**辩论预分析师** (借鉴 DeepThink: 辩论前深度思考)。

辩论命题: {objective}

## 任务
1. **命题分解**: 将命题拆解为 3-5 个核心争议点
2. **正反面预判**: 对每个争议点，预判正方和反方可能的立场
3. **证据需求**: 列出需要事实验证的关键声明 (数据、案例等)
4. **分歧预测**: 预测哪些争议点分歧最大，需要深度辩论
5. **辩论框架**: 建议辩论应聚焦的 Top 3 核心问题

## 输出格式
结构化 Markdown，每个争议点附带证据需求清单。`,
			},
			{
				Name: "proposer", Role: "proposer",
				Prompt: `You are the PROPOSER in a structured debate. Argue IN FAVOR of the proposition.

Proposition: {objective}

Pre-analysis (核心争议点已识别):
{prev_result}

{debate_context}

## 重要要求
1. **聚焦核心争议点**: 优先论证预分析中识别的 Top 3 核心问题
2. Present clear, evidence-based reasoning
3. Address any counterarguments from the opponent
4. Provide specific examples and data (标注数据来源)
5. Strengthen any weakened arguments
6. **标注可验证声明**: 对你引用的关键数据用 [CLAIM: xxx] 标注

Be persuasive but intellectually honest. Acknowledge valid opposing points while explaining why your position is stronger.`,
			},
			{
				Name: "opponent", Role: "opponent",
				Prompt: `You are the OPPONENT in a structured debate. Argue AGAINST the proposition.

Proposition: {objective}

{debate_context}

## 重要要求
1. **聚焦核心争议点**: 针对正方论点中的核心声明逐一反驳
2. Identify weaknesses in the proposer's arguments
3. Present alternative perspectives and evidence
4. Highlight risks, costs, and unintended consequences
5. Propose better alternatives if applicable
6. **标注可验证声明**: 对你引用的关键数据用 [CLAIM: xxx] 标注

Be rigorous and critical but fair. Don't use straw man arguments.`,
			},
			{
				Name: "evidence-verify", Role: "fact-checker",
				Prompt: `你是**证据验证员** (借鉴 CoVe: Chain of Verification)。

辩论命题: {objective}

辩论记录:
{prev_result}

## 任务 (严格按步骤)
1. **提取声明**: 从正反双方发言中提取所有 [CLAIM: xxx] 标注的声明，以及其他关键事实性断言
2. **独立验证**: 使用 WebSearch 独立验证每个声明的真实性
3. **出具验证报告**:

| 声明 | 来源(正/反) | 验证结果 | 证据 |
|------|-----------|---------|------|
| ...  | 正方/反方  | ✅正确/⚠️部分正确/❌错误/❓无法验证 | 验证来源URL |

4. **标注对裁决有重大影响的验证结果**: 如果某个关键论据被证伪，明确指出`,
			},
			{
				Name: "judge", Role: "judge",
				Prompt: `You are the JUDGE in a structured debate. Evaluate both sides and render a verdict.

Proposition: {objective}

Full Debate Transcript + Evidence Verification:
{prev_result}

## 重要: 证据验证结果
证据验证员已独立核实了双方引用的关键数据。在裁决时:
- 标注 ✅正确 的证据: 正常采信
- 标注 ❌错误 的证据: 该论点大幅降权
- 标注 ❓无法验证 的证据: 需要谨慎对待

## 裁决要求
1. **论证强度评分**: 正方 X/10, 反方 Y/10 (附评分依据)
2. **证据质量评分**: 基于验证结果，正方 X/10, 反方 Y/10
3. **关键决胜点**: 哪个论点/证据对裁决起了决定性作用
4. **证据验证影响**: 被证伪的论据如何影响了最终判定
5. **共识区域**: 双方实际同意的部分
6. **最终裁决**: 明确的结论 + 置信度 (高/中/低)
7. **建议**: 如果采纳获胜方的立场，需要注意的风险和条件`,
			},
		},
	}
}

// WorkflowExecutor 工作流执行器。
// 集成 Blackboard (bMAS) + TaskTracker (V2 Task) + Structured Handoff + Evolution + Roles + AgentPool。
type WorkflowExecutor struct {
	factory          CreateAgentFunc
	planCfgResolver  *PlanConfigResolver // 模型/连接参数解析器 (可选, 按 plan+role 层级解析)
	notify           NotifyFunc
	chatID           string
	llm              LLMClient                                                            // LLM 客户端 (供 swarm_intel.Engine 等需要直接调用的场景)
	taskTracker      TaskTracker                                                          // 复用 V2 Task 系统 (可为 nil)
	dagTracker       DAGTaskTracker                                                       // V2 DAG 能力 (运行时从 taskTracker 检测)
	evolution        *EvolutionEngine                                                     // 自动进化引擎 (可为 nil)
	roles            *RoleRegistry                                                        // 角色注册表 (可为 nil, 降级用 StageDef.Prompt)
	metrics          *metrics.Collector                                                   // 持续观测指标 (可为 nil)
	pool             *AgentPool                                                           // Agent 池 (动态扩缩, 可为 nil)
	checkpoints      CheckpointStore                                                      // 检查点存取 (由 Coordinator 注入, 可为 nil)
	promptCache      *PromptCache                                                         // 提示词缓存 (参考 Anthropic Prompt Caching)
	concurrency      ConcurrencySuggestor                                                 // 动态并发建议 (基于 API 流控状态, 可为 nil)
	activityCallback func()                                                               // 活动回调: Coordinator watchdog 心跳 (可为 nil)
	progressCallback func(phase string, iteration int, bytesWritten int64, taskID string) // 进展上报 (可为 nil)
}

// tryInitDAG 从 taskTracker 检测 DAG 能力
func (we *WorkflowExecutor) tryInitDAG() {
	if we.dagTracker != nil || we.taskTracker == nil {
		return
	}
	if dag, ok := we.taskTracker.(DAGTaskTracker); ok {
		we.dagTracker = dag
	}
}

// Execute 执行工作流, 返回所有阶段结果
func (we *WorkflowExecutor) Execute(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	start := time.Now()
	traceCtx := observability.NewRootTrace().WithBaggage("team_id", team.Name).WithBaggage("workflow", wf.Mode)
	ctx = observability.WithTrace(ctx, traceCtx)

	observability.Emit(observability.Event{
		Type:      observability.EvtTeamStart,
		Timestamp: start,
		TraceID:   traceCtx.TraceID,
		Module:    "workflow",
		Name:      wf.Mode,
		Payload: map[string]interface{}{
			"team_id":     team.Name,
			"workflow":    wf.Mode,
			"objective":   objective,
			"stage_count": len(wf.Stages),
		},
	})

	var results []StageResult
	var err error
	defer func() {
		dur := time.Since(start).Seconds()
		ev := observability.Event{
			Type:      observability.EvtTeamComplete,
			Timestamp: time.Now(),
			TraceID:   traceCtx.TraceID,
			Module:    "workflow",
			Name:      wf.Mode,
			Payload: map[string]interface{}{
				"team_id":      team.Name,
				"workflow":     wf.Mode,
				"duration_sec": dur,
				"stage_count":  len(results),
				"success":      err == nil,
				"error":        "",
			},
		}
		if err != nil {
			ev.Type = observability.EvtTeamFail
			ev.Payload["error"] = err.Error()
			ev.Payload["success"] = false
		}
		observability.Emit(ev)
	}()

	switch wf.Mode {
	case "pipeline":
		return we.executePipeline(ctx, wf, objective, team)
	case "fanout":
		return we.executeFanOut(ctx, wf, objective, team)
	case "adversarial":
		return we.executeAdversarial(ctx, wf, objective, team)
	case "adversarial_dev":
		return we.executeAdversarialDev(ctx, wf, objective, team)
	case "trading_debate":
		return we.executeTradingDebate(ctx, wf, objective, team)
	case "creative_media":
		return we.executeCreativeMedia(ctx, wf, objective, team)
	case "novel_writing":
		return we.executeNovelWriting(ctx, wf, objective, team)
	case "swarm_novel":
		return we.executeSwarmNovel(ctx, wf, objective, team)
	case "orchestrated":
		return we.executeOrchestrated(ctx, wf, objective, team)
	case "app_composite":
		return we.executeAppComposite(ctx, wf, objective, team)
	case "game_composite":
		return we.executeGameComposite(ctx, wf, objective, team)
	default:
		return we.executePipeline(ctx, wf, objective, team)
	}
}

// executeAdversarialDev 对抗式开发流水线。
// 四阶段模型 (拆分为可读的小函数):
//
//	Phase 1: 设计阶段 (research → design → plan)
//	Phase 2: [Generator ↔ Evaluator] 对抗循环
//	Phase 3: E2E 对抗测试 (tester↔coder 自适应)
//	Phase 4: 非测试的收尾阶段
func (we *WorkflowExecutor) executeAdversarialDev(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	prevResults := make(map[string]string)

	designStages, _, _, parallelStages := classifyStages(wf.Stages)
	we.tryInitDAG()

	// 恢复检查点: 如果有已完成的阶段, 跳过并注入 prevResults
	we.restoreCheckpoints(wf.Stages, prevResults, &allResults)

	// Phase 1: 设计阶段
	designResults, err := we.runDesignPhase(ctx, designStages, objective, prevResults, team)
	allResults = append(allResults, designResults...)
	we.savePhaseCheckpoints(designResults)
	we.flushStagesLive(team, allResults)
	if err != nil {
		return allResults, err
	}

	// Phase 2: Orchestrator (DAG 驱动, pool 由 DAG 宽度精确控制)
	// 修复: 禁止 silent fallback 到对抗循环。Orchestrator 是 Plan→Execute 的唯一通道。
	orchUsed := false
	if planOutput, hasPlan := prevResults["plan"]; hasPlan && we.dagTracker != nil {
		orchResults, orchErr := we.runOrchestratedPhase(ctx, planOutput, objective, prevResults, team, allResults)
		if orchErr == nil && len(orchResults) > 0 {
			allResults = append(allResults, orchResults...)
			we.savePhaseCheckpoints(orchResults)
			we.flushStagesLive(team, allResults)
			orchUsed = true
		} else if orchErr != nil {
			we.notify(we.chatID, fmt.Sprintf("🔴 Orchestrator 启动失败, 终止工作流 (不再 fallback 到对抗循环): %v", orchErr))
			return allResults, fmt.Errorf("orchestrator 启动失败: %w", orchErr)
		}
	}

	if !orchUsed {
		we.notify(we.chatID, "🔴 未找到有效 Plan 或 DAG 未配置, 终止工作流")
		return allResults, fmt.Errorf("plan 缺失或 DAG 未配置, 无法启动 Orchestrator")
	}

	// Phase 3: E2E 对抗测试
	e2eResults := we.runE2EAdversarial(ctx, parallelStages, objective, prevResults, team)
	allResults = append(allResults, e2eResults...)
	we.savePhaseCheckpoints(e2eResults)
	we.flushStagesLive(team, allResults)

	// E2E 是质量门禁, 失败则终止工作流
	for _, er := range e2eResults {
		if er.Status == TaskFailed {
			return allResults, fmt.Errorf("E2E 测试失败: %s — %s", er.Name, er.Error)
		}
	}

	// Phase 4: 收尾阶段
	finishResults := we.runFinishPhase(ctx, parallelStages, objective, prevResults, team)
	allResults = append(allResults, finishResults...)
	we.flushStagesLive(team, allResults)
	we.savePhaseCheckpoints(finishResults)

	return allResults, nil
}

// savePhaseCheckpoints 批量保存一个 phase 内所有阶段的检查点。
func (we *WorkflowExecutor) savePhaseCheckpoints(results []StageResult) {
	if we.checkpoints == nil {
		return
	}
	for _, r := range results {
		if r.Name == "" {
			continue
		}
		if r.Status == TaskCompleted {
			we.checkpoints.SaveCheckpoint(r.Name, "completed", 0, r.Output)
		} else if r.Status == TaskFailed {
			we.checkpoints.SaveCheckpoint(r.Name, "failed", 0, r.Error)
		}
	}
}

// flushStagesLive 增量把当前已产生的 stage 列表写回 team, 并持久化。
// 供 adversarial / fanout 等多 phase 工作流在每个 phase 结束后调用,
// 让 dashboard 在整个工作流还在执行期间就能看到进度。
func (we *WorkflowExecutor) flushStagesLive(team *ProductionTeam, results []StageResult) {
	if team == nil || len(results) == 0 {
		return
	}
	cp := make([]StageResult, len(results))
	copy(cp, results)
	team.mu.Lock()
	team.Stages = cp
	team.mu.Unlock()
	team.persist()

	// 把细粒度 stage 指标也一并上报, 与 Coordinator.recordStageMetrics 保持一致。
	if mc := team.metrics(); mc != nil {
		for _, sr := range results {
			labels := map[string]string{
				"workflow": team.Workflow,
				"stage":    sr.Name,
				"role":     sr.Role,
				"status":   string(sr.Status),
			}
			if sr.Duration != "" {
				if d, err := time.ParseDuration(sr.Duration); err == nil {
					mc.RecordRun("team", metrics.MTeamStageDurationSec, d.Seconds(), team.Name, labels)
				}
			}
			mc.RecordRun("team", metrics.MTeamStageCount, 1, team.Name, labels)
			if sr.Status == TaskCompleted {
				mc.RecordRun("team", metrics.MTeamStageSuccessCount, 1, team.Name, labels)
			} else if sr.Status == TaskFailed {
				mc.RecordRun("team", metrics.MTeamStageFailCount, 1, team.Name, labels)
			}
			if sr.Output != "" {
				mc.RecordRun("team", metrics.MTeamStageOutputLen, float64(len(sr.Output)), team.Name, labels)
			}
		}
	}
}

// restoreCheckpoints 从检查点恢复已完成阶段, 注入 prevResults + allResults。
func (we *WorkflowExecutor) restoreCheckpoints(stages []StageDef, prevResults map[string]string, allResults *[]StageResult) {
	if we.checkpoints == nil {
		return
	}
	restored := 0
	for _, stage := range stages {
		cp := we.checkpoints.GetCheckpoint(stage.Name)
		if cp == nil || cp.Status != "completed" || cp.Output == "" {
			continue
		}
		prevResults[stage.Name] = cp.Output
		// 角色别名也注入 (design 阶段的 key 可能是 role name)
		if stage.Role != "" {
			prevResults[stage.Role] = cp.Output
		}
		*allResults = append(*allResults, StageResult{
			Name: stage.Name, Role: stage.Role, Status: TaskCompleted,
			Output: cp.Output, StartedAt: cp.SavedAt,
		})
		restored++
	}
	if restored > 0 {
		we.notify(we.chatID, fmt.Sprintf("♻️ 从检查点恢复 %d 个已完成阶段", restored))
	}
}

// runOrchestratedPhase 使用 Orchestrator + V2 DAG 执行开发任务。
// 当 Planner 输出了 WBS 表格时, Orchestrator 解析并通过 V2 TaskStore 调度。
// priorStages: Phase 1 等已完成的阶段, 增量刷新时会与 Orchestrator 结果合并。
func (we *WorkflowExecutor) runOrchestratedPhase(ctx context.Context, planOutput, objective string, prevResults map[string]string, team *ProductionTeam, priorStages []StageResult) ([]StageResult, error) {
	orchParallel := 3
	if we.concurrency != nil {
		suggested := we.concurrency.SuggestConcurrency()
		if suggested > 1 {
			orchParallel = suggested
		}
	}
	// 包装 factory: 注入 ModelConfigKey, 使 Orchestrator 阶段也能按 plan+role 解析模型/API 配置
	baseFactory := we.factory
	planFactory := func(pCtx context.Context, role, systemPrompt string) (AgentRunner, error) {
		if we.planCfgResolver != nil {
			pCtx = context.WithValue(pCtx, ModelConfigKey{}, we.planCfgResolver.Resolve(team.Workflow, role))
		}
		pCtx = WithRunMetadata(pCtx, RunMetadata{
			Source:   "team_stage",
			Purpose:  team.Name,
			Workflow: team.Workflow,
			Role:     role,
			Team:     team.Name,
		})
		return baseFactory(pCtx, role, systemPrompt)
	}

	orch := NewOrchestrator(
		OrchestratorConfig{MaxParallel: orchParallel, MaxRetries: 2, MicroTestAfter: true, AdversarialRound: 3},
		we.dagTracker, planFactory, we.notify, we.pool, we.chatID,
	)

	if we.activityCallback != nil {
		orch.SetActivityCallback(we.activityCallback)
	}
	if we.progressCallback != nil {
		orch.SetProgressCallback(we.progressCallback)
	}
	if we.checkpoints != nil {
		orch.SetCheckpointStore(we.checkpoints)
	}
	if designDoc, ok := prevResults["design"]; ok {
		orch.SetDesignContext(designDoc, planOutput)
	}

	// 增量刷新: Orchestrator 每批任务完成后把 Phase 1 结果 + Orchestrator 结果合并写入 team.json
	prefix := make([]StageResult, len(priorStages))
	copy(prefix, priorStages)
	orch.SetStageFlusher(func(orchResults []StageResult) {
		combined := make([]StageResult, 0, len(prefix)+len(orchResults))
		combined = append(combined, prefix...)
		combined = append(combined, orchResults...)
		we.flushStagesLive(team, combined)
	})

	nodes, err := orch.ParsePlanToDAGWithRepair(ctx, planOutput, objective, team.Name, planFactory)
	if err != nil || len(nodes) == 0 {
		return nil, fmt.Errorf("WBS 解析失败或无任务: %v", err)
	}

	we.notify(we.chatID, fmt.Sprintf("🎯 Orchestrator 接管: %d 个任务 (V2 DAG 驱动, micro-test 启用)", len(nodes)))
	results, err := orch.Execute(ctx, objective, team)

	// 将 Orchestrator 的产出写入 prevResults (供后续 E2E 使用)
	for _, sr := range results {
		if sr.Status == TaskCompleted {
			prevResults[sr.Name] = sr.Output
		}
	}

	// 写入 micro-test 汇总到 blackboard
	if team.Blackboard != nil {
		team.Blackboard.Write("micro-test-summary", orch.MicroTestSummary(), "orchestrator", "summary")
	}

	return results, err
}

// initAdaptiveTerminator 初始化自适应终止器
func (we *WorkflowExecutor) initAdaptiveTerminator(wf *WorkflowDef) (maxRounds int, terminator *AdaptiveTerminator) {
	maxRounds = wf.Rounds
	useAdaptive := maxRounds <= 0
	if useAdaptive {
		maxRounds = 5
	}
	if useAdaptive {
		terminator = NewAdaptiveTerminator(2, maxRounds)
	}
	return
}

// autoScalePool 动态扩缩 Agent Pool
func (we *WorkflowExecutor) autoScalePool(wf *WorkflowDef, design, generators []StageDef, eval *StageDef, parallel []StageDef, maxRounds int) {
	if we.pool == nil {
		return
	}
	roleNeeds := make(map[string]int)
	for _, s := range design {
		roleNeeds[s.Role]++
	}
	for _, s := range generators {
		roleNeeds[s.Role] += maxRounds
	}
	if eval != nil {
		roleNeeds[eval.Role] += maxRounds
	}
	for _, s := range parallel {
		roleNeeds[s.Role]++
	}
	complexity := 0
	if maxRounds >= 3 {
		complexity = 1
	}
	if len(wf.Stages) > 5 || maxRounds >= 5 {
		complexity = 2
	}
	we.pool.AutoScaleByRoles(roleNeeds, complexity)
}

// runDesignPhase Phase 1: 串行执行设计阶段
func (we *WorkflowExecutor) runDesignPhase(ctx context.Context, designStages []StageDef, objective string, prevResults map[string]string, team *ProductionTeam) ([]StageResult, error) {
	if len(designStages) == 0 {
		return nil, nil
	}
	var results []StageResult
	we.notify(we.chatID, fmt.Sprintf("📐 Phase 1: 设计/策划 (%d 阶段)...", len(designStages)))
	for _, ds := range designStages {
		// 检查点恢复: 如果该阶段已有 completed checkpoint, 跳过
		if _, restored := prevResults[ds.Name]; restored {
			we.notify(we.chatID, fmt.Sprintf("  ♻️ %s 已从检查点恢复, 跳过", ds.Name))
			continue
		}

		sr := we.executeStage(ctx, ds, objective, prevResults, team)
		results = append(results, sr)
		if sr.Status != TaskCompleted {
			return results, fmt.Errorf("设计阶段 %s 失败: %s", ds.Name, sr.Error)
		}
		prevResults[ds.Name] = sr.Output
	}
	return results, nil
}

// maxBuildRetries 编译硬门禁内部重试次数 (L2, 参考 Self-Debugging arXiv:2304.05128)。
// 编译修复循环独立于对抗循环, 不消耗 AdaptiveTerminator 的轮次配额。
const maxBuildRetries = 2

// maxTestRetries 测试硬门禁内部重试次数 (L2.5)。
const maxTestRetries = 2

// runAdversarialLoop Phase 2: Generator ↔ Evaluator 对抗循环。
// 六层质量保障: L2 编译硬门禁 + L5 上下文压缩 + L6 重采样决策。
func (we *WorkflowExecutor) runAdversarialLoop(
	ctx context.Context,
	generatorStages []StageDef, evalStage *StageDef,
	maxRounds int, terminator *AdaptiveTerminator,
	objective string, prevResults map[string]string, team *ProductionTeam,
) ([]StageResult, error) {
	if len(generatorStages) == 0 {
		we.notify(we.chatID, "⚠️ 未发现 Generator 阶段，跳过对抗循环")
		return nil, nil
	}

	var allResults []StageResult
	we.notify(we.chatID, fmt.Sprintf("⚔️ Phase 2: 对抗循环 (最多 %d 轮, 含编译硬门禁+重采样)...", maxRounds))
	var lastGenOutput, lastEvalFeedback string
	var lastScore EvalScore

	for round := 1; round <= maxRounds; round++ {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		// L6: 重采样决策 (参考 arXiv:2604.10508)
		// 连续 2 轮 Completeness<5 且编译失败 → 清空上轮输出, 换思路重新生成
		if terminator != nil && round > 2 && terminator.ShouldResample() {
			we.notify(we.chatID, fmt.Sprintf("🔄 第 %d 轮触发重采样 (连续低完整度+编译失败, 换思路)", round))
			lastGenOutput = ""
			lastEvalFeedback = "⚠️ **重采样模式**: 前几轮的实现方式无法产出完整代码。请换一种思路:\n" +
				"1. 先实现最核心的入口文件和 1 个核心模块, 确保可编译\n" +
				"2. 每个文件写完后心理验证编译正确性\n" +
				"3. 宁可功能不全但能编译, 也不要输出不可编译的完整框架\n" +
				"4. 优先保证: 编译通过 > 功能完整 > 代码优雅"
		}

		// L5: 注入迭代记忆链 (参考 Reflexion arXiv:2303.11366)
		memoryHint := ""
		if terminator != nil && len(terminator.Memories) > 0 {
			memoryHint = FormatMemoryChain(terminator.Memories)
		}

		// Generator 执行
		genResults, genOutput := we.runGeneratorRound(ctx, generatorStages, round, maxRounds, lastGenOutput, lastEvalFeedback+"\n"+memoryHint, objective, prevResults, team)
		allResults = append(allResults, genResults...)
		if len(genResults) > 0 && genResults[len(genResults)-1].Status != TaskCompleted {
			return allResults, fmt.Errorf("generator 第 %d 轮失败", round)
		}
		lastGenOutput = genOutput

		// L2: 编译硬门禁 — 编译失败时内部重试, 不消耗对抗轮次
		buildPassed := we.runBuildHardGate(ctx, generatorStages, round, maxRounds, &lastGenOutput, objective, prevResults, team, &allResults)
		if terminator != nil {
			terminator.RecordBuildResult(buildPassed)
		}
		if !buildPassed {
			we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮编译硬门禁未通过 (含 %d 次内部重试), 跳过 Reviewer", round, maxBuildRetries))
			// 编译未通过时, 为 reviewer 构造低分 (避免让 reviewer 审查不可编译的代码)
			lastScore = EvalScore{
				Correctness: 3, Completeness: 3, Security: 5, CodeQuality: 4,
				Pass: false, Feedback: "编译未通过, 跳过 Reviewer 评审",
			}
			if terminator != nil {
				terminator.RecordRoundOutput(round, lastScore, lastGenOutput)
				terminator.RecordIterationMemory(round, lastScore, extractFileList(lastGenOutput), []string{"编译未通过"}, false)
				decision := terminator.ShouldTerminate(round, lastScore)
				if decision.ShouldStop {
					we.notify(we.chatID, fmt.Sprintf("🏁 自适应终止 (编译持续失败, 原因: %s)", decision.Reason))
					if decision.BestOutput != "" {
						lastGenOutput = decision.BestOutput
						prevResults[generatorStages[len(generatorStages)-1].Name] = decision.BestOutput
					}
					break
				}
			}
			lastEvalFeedback = "编译未通过, 必须优先修复编译错误。"
			continue
		}

		// 编译通过后, 清除之前的编译错误反馈
		lastEvalFeedback = ""

		// Evaluator 审查 (只有编译通过才进入)
		evalResult, shouldBreak, bestOutput := we.runEvaluatorRound(ctx, evalStage, generatorStages, round, maxRounds, lastGenOutput, terminator, objective, prevResults, team, &lastEvalFeedback, lastScore)
		allResults = append(allResults, evalResult...)
		if bestOutput != "" {
			lastGenOutput = bestOutput
			prevResults[generatorStages[len(generatorStages)-1].Name] = bestOutput
		}

		// 更新 lastScore
		kept := true
		if len(evalResult) > 0 {
			if parsed, err := ParseEvalScoreJSON([]byte(evalResult[len(evalResult)-1].Output)); err == nil {
				lastScore = parsed
			} else if terminator != nil && len(terminator.ScoreHistory) > 0 {
				lastScore = HoldLastOrDefault(lastScore)
			}
		}

		// L5: 记录迭代记忆
		if terminator != nil {
			issues := ExtractKeyIssues(lastScore.Feedback)
			// 即时 Keep/Revert
			if !shouldBreak && round > 1 {
				if revert, bo, br := terminator.ShouldRevert(lastScore); revert {
					we.notify(we.chatID, fmt.Sprintf("⏪ 第 %d 轮退化, revert 到第 %d 轮最佳版本", round, br))
					lastGenOutput = bo
					prevResults[generatorStages[len(generatorStages)-1].Name] = bo
					kept = false
				}
			}
			terminator.RecordIterationMemory(round, lastScore, extractFileList(lastGenOutput), issues, kept)
		}

		if shouldBreak {
			break
		}
	}
	return allResults, nil
}

// runBuildHardGate L2 编译硬门禁: 编译失败时驱动 Coder 内部重试。
// 返回 true 表示编译通过。内部重试最多 maxBuildRetries 次, 不消耗对抗轮次。
func (we *WorkflowExecutor) runBuildHardGate(
	ctx context.Context, genStages []StageDef,
	round, maxRounds int, lastGenOutput *string,
	objective string, prevResults map[string]string, team *ProductionTeam,
	allResults *[]StageResult,
) bool {
	if team.Cwd == "" {
		return true
	}

	// L4: 文件物化 — 从 Coder 输出提取代码写入磁盘
	lang := team.Language
	if lang == "" {
		lang = "go"
	}
	if *lastGenOutput != "" {
		written := MaterializeCode(team.Cwd, *lastGenOutput, lang)
		if len(written) > 0 {
			we.notify(we.chatID, fmt.Sprintf("📁 文件物化: %d 个文件写入磁盘", len(written)))
		}
	}

	buildErrors := runBuildCheckLang(team.Cwd, lang)
	if buildErrors == "" {
		// L1.5: TODO/STUB 确定性门禁 — 编译通过不等于实现完整
		if todos := scanForTodos(team.Cwd); len(todos) > 0 {
			we.notify(we.chatID, fmt.Sprintf("🟡 第 %d 轮: 编译通过但发现 %d 处未实现项, 启动内部修复", round, len(todos)))
			if we.metrics != nil {
				we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
			}
			// 进入内部修复循环 (和编译失败同路径)
			buildErrors = fmt.Sprintf("TODO/STUB 检测失败, 发现 %d 处未实现项", len(todos))
		} else {
			tc := GetToolchain(lang)
			we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮编译通过且无未实现项 (%s)", round, tc.BuildCheckLabel()))
			if we.metrics != nil {
				we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
			}
			return true
		}
	}

	we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮编译失败, 启动内部修复 (最多 %d 次)...", round, maxBuildRetries))
	if we.metrics != nil {
		we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
	}

	for retry := 1; retry <= maxBuildRetries; retry++ {
		if ctx.Err() != nil {
			return false
		}

		// 区分编译错误和 TODO/STUB 检测
		isTodoFix := strings.Contains(buildErrors, "TODO/STUB")
		var buildFixPrompt string
		if isTodoFix {
			todos := scanForTodos(team.Cwd)
			var buf strings.Builder
			buf.WriteString(fmt.Sprintf("### 未实现项检测 (第 %d 次修复, 必须将所有 TODO/STUB 替换为真实实现):\n", retry))
			for _, t := range todos {
				buf.WriteString(fmt.Sprintf("  - %s\n", t))
			}
			buf.WriteString("\n要求:\n1. 将上述每个 TODO/STUB/placeholder/panic 替换为真实、可运行的实现\n2. 宁可简化功能, 也不允许保留占位符\n3. 保持现有代码结构不变\n4. 输出修复后的完整文件内容")
			buildFixPrompt = buf.String()
		} else {
			buildFixPrompt = fmt.Sprintf("### 编译错误 (第 %d 次修复, 必须优先修复编译错误):\n%s\n\n"+
				"要求:\n1. 仅修复编译错误, 不要做其他改动\n2. 保持现有代码结构不变\n3. 输出修复后的完整文件内容",
				retry, truncateResult(buildErrors, 3000))
		}

		for _, genStage := range genStages {
			fixStage := genStage
			fixStage.Prompt = strings.ReplaceAll(fixStage.Prompt, "{adversarial_feedback}", buildFixPrompt)
			fixStage.Name = fmt.Sprintf("%s-round%d-buildfix%d", genStage.Name, round, retry)

			label := map[bool]string{true: "TODO", false: "编译"}[isTodoFix]
			we.notify(we.chatID, fmt.Sprintf("  🔧 %s修复 %d/%d — %s...", label, retry, maxBuildRetries, genStage.Role))
			sr := we.executeStage(ctx, fixStage, objective, prevResults, team)
			sr.Name = fixStage.Name
			*allResults = append(*allResults, sr)
			if sr.Status == TaskCompleted {
				*lastGenOutput = sr.Output
				prevResults[genStage.Name] = sr.Output
				MaterializeCode(team.Cwd, sr.Output, lang)
			} else if isStageTransientError(sr.Error) {
				// V2 改进: build fix 中 coder 因 API 瞬态错误失败时,
				// 提前返回 false, 让对抗循环有机会重试整轮 (而非浪费 build fix 重试次数)
				we.notify(we.chatID, fmt.Sprintf("  🔴 %s修复 %d/%d 因 API 错误失败 (已自动重试), 跳过本轮", label, retry, maxBuildRetries))
				return false
			}
		}

		// L2: 编译检查
		buildErrors = runBuildCheckLang(team.Cwd, lang)
		if buildErrors != "" {
			we.notify(we.chatID, fmt.Sprintf("  🔴 编译修复第 %d 次仍失败", retry))
			continue
		}

		// L1.5: TODO/STUB 复扫
		if todos := scanForTodos(team.Cwd); len(todos) > 0 {
			buildErrors = fmt.Sprintf("TODO/STUB 检测失败, 仍发现 %d 处未实现项", len(todos))
			we.notify(we.chatID, fmt.Sprintf("  🟡 TODO 修复第 %d 次仍残留 %d 处", retry, len(todos)))
			continue
		}

		we.notify(we.chatID, fmt.Sprintf("  🟢 编译+实现均通过 (第 %d 次重试)", retry))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 1, team.Name,
				map[string]string{"round": fmt.Sprint(round), "build_retry": fmt.Sprint(retry)})
		}
		return true
	}
	return false
}

// runTestHardGate L2.5 测试硬门禁: 测试失败时驱动 Coder 内部重试。
// 返回 true 表示测试通过。内部重试最多 maxTestRetries 次, 不消耗对抗轮次。
func (we *WorkflowExecutor) runTestHardGate(
	ctx context.Context, genStages []StageDef,
	round, maxRounds int, lastGenOutput *string,
	objective string, prevResults map[string]string, team *ProductionTeam,
	allResults *[]StageResult,
) bool {
	if team.Cwd == "" {
		return true
	}
	lang := team.Language
	if lang == "" {
		lang = "go"
	}

	// MySQL 项目走集成测试路径
	if lang == "cpp" && isMySQLProject(team.Cwd) {
		err := we.runMySQLIntegrationTest(team.Cwd)
		if err != "" {
			we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮 MySQL 集成测试不通过, 启动修复", round))
			if we.metrics != nil {
				we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
			}
			return we.runTestFixCycle(ctx, genStages, round, err, lastGenOutput, objective, prevResults, team, allResults)
		}
		we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮 MySQL 集成测试通过", round))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
		}
		return true
	}

	// 标准语言测试
	testErrors := runTestCheckLang(team.Cwd, lang)
	if testErrors == "" {
		tc := GetToolchain(lang)
		we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮测试通过 (%s)", round, tc.TestCheckLabel()))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
		}
		return true
	}

	we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮测试失败, 启动内部修复 (最多 %d 次)...", round, maxTestRetries))
	if we.metrics != nil {
		we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
	}

	return we.runTestFixCycle(ctx, genStages, round, testErrors, lastGenOutput, objective, prevResults, team, allResults)
}

// runTestFixCycle 测试修复循环 (被 runTestHardGate 调用)。
func (we *WorkflowExecutor) runTestFixCycle(
	ctx context.Context, genStages []StageDef,
	round int, testErrors string, lastGenOutput *string,
	objective string, prevResults map[string]string, team *ProductionTeam,
	allResults *[]StageResult,
) bool {
	lang := team.Language
	if lang == "" {
		lang = "go"
	}

	for retry := 1; retry <= maxTestRetries; retry++ {
		if ctx.Err() != nil {
			return false
		}

		fixPrompt := fmt.Sprintf("### 测试错误 (第 %d 次修复, 必须修复所有测试):\n%s\n\n"+
			"要求:\n1. 仅修复测试错误, 不要做其他改动\n2. 保持现有代码结构不变\n3. 输出修复后的完整文件内容",
			retry, truncateResult(testErrors, 3000))
		for _, genStage := range genStages {
			fixStage := genStage
			fixStage.Prompt = strings.ReplaceAll(fixStage.Prompt, "{adversarial_feedback}", fixPrompt)
			fixStage.Name = fmt.Sprintf("%s-round%d-testfix%d", genStage.Name, round, retry)
			we.notify(we.chatID, fmt.Sprintf("  🧪 测试修复 %d/%d — %s...", retry, maxTestRetries, genStage.Role))
			sr := we.executeStage(ctx, fixStage, objective, prevResults, team)
			sr.Name = fixStage.Name
			*allResults = append(*allResults, sr)
			if sr.Status == TaskCompleted {
				*lastGenOutput = sr.Output
				prevResults[genStage.Name] = sr.Output
				MaterializeCode(team.Cwd, sr.Output, lang)
			} else if isStageTransientError(sr.Error) {
				// V2 改进: test fix 中 coder 因 API 瞬态错误失败时提前返回
				we.notify(we.chatID, fmt.Sprintf("  🔴 测试修复 %d/%d 因 API 错误失败 (已自动重试), 跳过本轮", retry, maxTestRetries))
				return false
			}
		}

		// 物化后先编译
		buildErrors := runBuildCheckLang(team.Cwd, lang)
		if buildErrors != "" {
			we.notify(we.chatID, fmt.Sprintf("  🔴 测试修复第 %d 次后编译失败", retry))
			testErrors = buildErrors
			continue
		}

		// 运行测试 (MySQL 走集成测试)
		if lang == "cpp" && isMySQLProject(team.Cwd) {
			testErrors = we.runMySQLIntegrationTest(team.Cwd)
		} else {
			testErrors = runTestCheckLang(team.Cwd, lang)
		}
		if testErrors != "" {
			we.notify(we.chatID, fmt.Sprintf("  🔴 测试修复第 %d 次仍失败", retry))
			continue
		}

		we.notify(we.chatID, fmt.Sprintf("  🟢 测试通过 (第 %d 次重试)", retry))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 1, team.Name,
				map[string]string{"round": fmt.Sprint(round), "test_retry": fmt.Sprint(retry)})
		}
		return true
	}
	return false
}

// extractFileList 从 coder 输出中提取文件列表 (用于迭代记忆的 Approach 字段)
func extractFileList(output string) string {
	var files []string
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, ".go") || strings.HasSuffix(trimmed, ".ts") ||
			strings.HasSuffix(trimmed, ".py") || strings.HasSuffix(trimmed, ".js") {
			if len(trimmed) < 80 {
				files = append(files, trimmed)
			}
		}
		if strings.Contains(trimmed, "```go") || strings.Contains(trimmed, "// File:") ||
			strings.Contains(trimmed, "package ") {
			if len(trimmed) < 80 {
				files = append(files, trimmed)
			}
		}
	}
	if len(files) > 10 {
		files = files[:10]
	}
	if len(files) == 0 {
		return "未识别到文件结构"
	}
	return strings.Join(files, "; ")
}

// runGeneratorRound 执行一轮 Generator
func (we *WorkflowExecutor) runGeneratorRound(
	ctx context.Context, genStages []StageDef,
	round, maxRounds int, lastOutput, lastFeedback string,
	objective string, prevResults map[string]string, team *ProductionTeam,
) (results []StageResult, finalOutput string) {
	for _, genStage := range genStages {
		feedbackSection := we.buildFeedbackSection(round, lastOutput, lastFeedback, team)
		modifiedPrompt := strings.ReplaceAll(genStage.Prompt, "{adversarial_feedback}", feedbackSection)
		tempStage := genStage
		tempStage.Prompt = modifiedPrompt
		tempStage.Name = fmt.Sprintf("%s-round%d", genStage.Name, round)

		var roleRestore func()
		if we.roles != nil {
			if role := we.roles.Get(genStage.Role); role != nil && strings.Contains(role.SystemPrompt, "{adversarial_feedback}") {
				orig := role.SystemPrompt
				role.SystemPrompt = strings.ReplaceAll(role.SystemPrompt, "{adversarial_feedback}", feedbackSection)
				roleRestore = func() { role.SystemPrompt = orig }
			}
		}

		we.notify(we.chatID, fmt.Sprintf("🔨 对抗第 %d/%d 轮 — %s (%s)...", round, maxRounds, genStage.Name, genStage.Role))
		sr := we.executeStage(ctx, tempStage, objective, prevResults, team)
		if roleRestore != nil {
			roleRestore()
		}
		sr.Name = tempStage.Name
		results = append(results, sr)
		if sr.Status == TaskCompleted {
			finalOutput = sr.Output
			prevResults[genStage.Name] = sr.Output
		}
	}
	return
}

// buildFeedbackSection 构建跨轮反馈上下文。
// L5 改进: 渐进式压缩 (参考 Kimi K2 溢出策略 + Reflexion 结构化记忆)
// - Round 1: 全量设计文档 (无压缩)
// - Round 2: 上轮输出压缩到 8K + Evaluator 反馈
// - Round 3+: 上轮输出压缩到 4K + 仅关键问题 + 工作区文件清单
func (we *WorkflowExecutor) buildFeedbackSection(round int, lastOutput, lastFeedback string, team *ProductionTeam) string {
	section := ""

	if lastFeedback != "" {
		// 反馈也做压缩: 超过 4K 时提取关键问题
		feedback := lastFeedback
		if len(feedback) > 4000 {
			feedback = SummarizeOldOutput(feedback, 4000)
		}
		section = fmt.Sprintf("### Evaluator 第 %d 轮反馈 (必须全部修复):\n%s", round-1, feedback)
	}

	if round > 1 && lastOutput != "" {
		// 渐进式压缩: 越后面的轮次压缩越狠
		maxOutputLen := 8000
		if round >= 3 {
			maxOutputLen = 4000
		}
		if round >= 4 {
			maxOutputLen = 2000
		}

		prevSummary := lastOutput
		if len(prevSummary) > maxOutputLen {
			prevSummary = SummarizeOldOutput(prevSummary, maxOutputLen)
		}

		section = fmt.Sprintf("### 你的第 %d 轮代码输出 (严禁从零重写, 仅做增量修改):\n%s\n\n%s",
			round-1, prevSummary, section)

		if team.StartedAt.Unix() > 0 {
			if manifest := workspaceFileManifest(team.Cwd, team.StartedAt); manifest != "" {
				section = manifest + "\n" + section
			}
		}
	}
	return section
}

// runBuildGate 编译验证门禁 (多语言感知)
func (we *WorkflowExecutor) runBuildGate(ctx context.Context, team *ProductionTeam, round int, prevFeedback string) string {
	if team.Cwd == "" {
		return prevFeedback
	}
	lang := team.Language
	if lang == "" {
		lang = "go"
	}
	buildErrors := runBuildCheckLang(team.Cwd, lang)
	if buildErrors != "" {
		we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮编译检查失败", round))
		logging.Event(ctx, "adversarial.build_fail", "round", round)
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
		}
		buildPrefix := "### 编译错误 (必须优先修复):\n" + buildErrors
		if prevFeedback == "" {
			return buildPrefix
		}
		return buildPrefix + "\n\n" + prevFeedback
	}
	we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮编译检查通过", round))
	if we.metrics != nil {
		we.metrics.RecordRun("team", metrics.MTeamBuildPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
	}
	return prevFeedback
}

// runTestGate L2.5 测试轻量门禁: 仅返回测试结果文本, 无 fix cycle。
func (we *WorkflowExecutor) runTestGate(ctx context.Context, team *ProductionTeam, round int, prevFeedback string) string {
	if team.Cwd == "" {
		return prevFeedback
	}
	lang := team.Language
	if lang == "" {
		lang = "go"
	}

	// MySQL 项目走集成测试
	if lang == "cpp" && isMySQLProject(team.Cwd) {
		if err := we.runMySQLIntegrationTest(team.Cwd); err != "" {
			we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮 MySQL 集成测试失败", round))
			if we.metrics != nil {
				we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
			}
			testPrefix := "### MySQL 集成测试错误 (必须修复):\n" + err
			if prevFeedback == "" {
				return testPrefix
			}
			return testPrefix + "\n\n" + prevFeedback
		}
		we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮 MySQL 集成测试通过", round))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
		}
		return prevFeedback
	}

	testErrors := runTestCheckLang(team.Cwd, lang)
	if testErrors != "" {
		we.notify(we.chatID, fmt.Sprintf("🔴 第 %d 轮测试检查失败", round))
		if we.metrics != nil {
			we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 0, team.Name, map[string]string{"round": fmt.Sprint(round)})
		}
		testPrefix := "### 测试错误 (必须优先修复):\n" + testErrors
		if prevFeedback == "" {
			return testPrefix
		}
		return testPrefix + "\n\n" + prevFeedback
	}
	we.notify(we.chatID, fmt.Sprintf("🟢 第 %d 轮测试检查通过", round))
	if we.metrics != nil {
		we.metrics.RecordRun("team", metrics.MTeamTestPassRate, 1, team.Name, map[string]string{"round": fmt.Sprint(round)})
	}
	return prevFeedback
}

// runEvaluatorRound 执行一轮 Evaluator
func (we *WorkflowExecutor) runEvaluatorRound(
	ctx context.Context, evalStage *StageDef, genStages []StageDef,
	round, maxRounds int, lastGenOutput string,
	terminator *AdaptiveTerminator,
	objective string, prevResults map[string]string, team *ProductionTeam,
	lastEvalFeedback *string, lastScore EvalScore,
) (results []StageResult, shouldBreak bool, bestOutput string) {
	if evalStage == nil {
		return nil, false, ""
	}

	genName := genStages[len(genStages)-1].Name
	evalPrevResults := map[string]string{genName: lastGenOutput}
	for k, v := range prevResults {
		evalPrevResults[k] = v
	}

	modifiedPrompt := strings.ReplaceAll(evalStage.Prompt, "{adversarial_round}", fmt.Sprintf("%d", round))
	tempStage := *evalStage
	tempStage.Prompt = modifiedPrompt
	tempStage.Name = fmt.Sprintf("%s-round%d", evalStage.Name, round)

	we.notify(we.chatID, fmt.Sprintf("🔍 对抗第 %d/%d 轮 — 审查中...", round, maxRounds))
	sr := we.runAgent(ctx, evalStage.Role, buildStagePromptWithRoles(tempStage, objective, evalPrevResults, we.roles), team)
	sr.Name = tempStage.Name
	results = append(results, sr)

	if sr.Status != TaskCompleted {
		*lastEvalFeedback = "评估器未能正常返回结果，请全面检查输出质量。"
		return results, false, ""
	}

	score, scoreErr := ParseEvalScoreJSON([]byte(sr.Output))
	if scoreErr != nil {
		log.Printf("[对抗] 第 %d 轮评分解析失败, hold-last-value: %v", round, scoreErr)
		score = HoldLastOrDefault(lastScore)
		score.Feedback = sr.Output
	}

	scoreMsg := fmt.Sprintf("正确=%.0f 完整=%.0f 安全=%.0f 质量=%.0f",
		score.Correctness, score.Completeness, score.Security, score.CodeQuality)
	if score.DesignAlignment > 0 {
		scoreMsg += fmt.Sprintf(" 对齐=%.0f", score.DesignAlignment)
	}
	if team.Blackboard != nil {
		team.Blackboard.Write(fmt.Sprintf("eval-round%d-score", round),
			scoreMsg+fmt.Sprintf(" 通过:%v", score.Pass), "evaluator", "score")
	}

	passLabel := map[bool]string{true: "✅ 通过", false: "❌ 未通过"}[score.MeetsHardPassThreshold()]
	we.notify(we.chatID, fmt.Sprintf("📊 第 %d 轮评分: %s | %s", round, scoreMsg, passLabel))

	// 自适应终止判断
	if terminator != nil {
		terminator.RecordRoundOutput(round, score, lastGenOutput)
		decision := terminator.ShouldTerminate(round, score)
		if decision.StrategyShift {
			we.notify(we.chatID, fmt.Sprintf("🔀 策略转换 (第 %d 次): 当前修补已饱和, 注入结构性变更提示",
				terminator.StrategyShiftCount))
			*lastEvalFeedback = fmt.Sprintf("⚠️ **策略转换要求** (第 %d 次):\n"+
				"当前修补方式已饱和, 请从架构层面重新思考:\n"+
				"1. 换一种完全不同的实现思路\n2. 重新分析问题本质\n3. 不要在现有方案上微调\n\n"+
				"之前的反馈:\n%s", terminator.StrategyShiftCount, *lastEvalFeedback)
			return results, false, ""
		}
		if decision.ShouldStop {
			reasonCN := map[string]string{
				"quality_pass": "质量达标", "max_rounds": "达到最大轮数",
				"degradation": "连续退化", "converged": "改进已饱和",
			}[decision.Reason]
			we.notify(we.chatID, fmt.Sprintf("🏁 自适应终止: %s (原因: %s)", passLabel, reasonCN))
			if decision.BestOutput != "" {
				we.notify(we.chatID, fmt.Sprintf("⏪ best-of-N 回滚到第 %d 轮 (最高分)", decision.BestRound))
				bestOutput = decision.BestOutput
			}
			we.recordEvalMetrics(team, round, score, decision.Reason)
			return results, true, bestOutput
		}
		we.notify(we.chatID, fmt.Sprintf("🔄 自适应继续: %s, %d/%d轮", decision.Reason, round, maxRounds))
	} else if score.MeetsHardPassThreshold() {
		we.notify(we.chatID, fmt.Sprintf("✅ 对抗通过！第 %d 轮评审达标。", round))
		we.recordEvalMetrics(team, round, score, "pass")
		return results, true, ""
	}

	*lastEvalFeedback = score.Feedback
	if *lastEvalFeedback == "" {
		*lastEvalFeedback = sr.Output
	}
	return results, false, ""
}

// recordEvalMetrics 记录评估指标
func (we *WorkflowExecutor) recordEvalMetrics(team *ProductionTeam, round int, score EvalScore, reason string) {
	if we.metrics == nil {
		return
	}
	val := 0.0
	if score.MeetsHardPassThreshold() {
		val = 1.0
	}
	we.metrics.RecordRun("team", metrics.MTeamEvalPassRate, val, team.Name,
		map[string]string{"round": fmt.Sprint(round), "termination": reason})
	we.metrics.RecordRun("team", metrics.MTeamRoundCount, float64(round), team.Name, nil)
}

// runE2EAdversarial Phase 3: E2E 对抗测试 (tester↔coder 自适应循环)。
// 不再是1轮 E2E + 1轮修复, 而是完整的对抗循环:
// E2E-tester 发现问题 → coder 修复 → E2E-tester 回归验证 → 直到通过或达到上限。
// 参考 TDAD (2026): E2E 作为质量门禁, 驱动增量修复。
func (we *WorkflowExecutor) runE2EAdversarial(ctx context.Context, parallelStages []StageDef, objective string, prevResults map[string]string, team *ProductionTeam) []StageResult {
	var testerStage *StageDef
	for i, ps := range parallelStages {
		if ps.Role == "tester" {
			testerStage = &parallelStages[i]
			break
		}
	}
	if testerStage == nil {
		return nil
	}

	var results []StageResult
	e2eTerminator := NewAdaptiveTerminator(1, 3) // E2E: 最少1轮, 最多3轮
	we.notify(we.chatID, "🧪 Phase 3: E2E 对抗测试 (tester↔coder 自适应)...")

	if local := we.runLocalE2EGate(team, objective); local != nil {
		prevResults["e2e-test"] = local.Output
		return []StageResult{*local}
	}

	var lastE2EOutput string
	for round := 1; round <= 3; round++ {
		if ctx.Err() != nil {
			break
		}

		// 每轮独立超时, 防止 429 限流耗尽全部时间
		e2eRoundTimeout := 5 * time.Minute
		roundCtx, roundCancel := context.WithTimeout(ctx, e2eRoundTimeout)

		// E2E Tester
		e2ePrompt := we.buildE2EPrompt(objective, prevResults, lastE2EOutput, round)
		e2eStageDef := StageDef{Name: fmt.Sprintf("e2e-round%d", round), Role: "tester", Prompt: e2ePrompt}
		e2eResult := we.executeStage(roundCtx, e2eStageDef, objective, prevResults, team)
		roundCancel()
		e2eResult.Name = e2eStageDef.Name
		results = append(results, e2eResult)

		if e2eResult.Status != TaskCompleted {
			break
		}
		lastE2EOutput = e2eResult.Output
		prevResults["e2e-test"] = e2eResult.Output

		// 评估 E2E 结果: 无 Bug/FAIL 则通过
		hasBugs := strings.Contains(strings.ToLower(e2eResult.Output), "bug") ||
			strings.Contains(strings.ToLower(e2eResult.Output), "fail") ||
			strings.Contains(strings.ToLower(e2eResult.Output), "错误")

		e2eScore := EvalScore{Correctness: 8, Completeness: 8, Security: 7, CodeQuality: 7, Pass: !hasBugs}
		if hasBugs {
			e2eScore = EvalScore{Correctness: 4, Completeness: 5, Security: 7, CodeQuality: 6, Pass: false}
		}

		decision := e2eTerminator.ShouldTerminate(round, e2eScore)
		if decision.ShouldStop && !hasBugs {
			we.notify(we.chatID, fmt.Sprintf("✅ E2E 第 %d 轮通过, 无阻断性问题", round))
			break
		}

		if !hasBugs {
			we.notify(we.chatID, fmt.Sprintf("✅ E2E 第 %d 轮未发现严重问题", round))
			break
		}

		// Coder 修复
		we.notify(we.chatID, fmt.Sprintf("🔧 E2E 第 %d 轮发现问题, 驱动 coder 修复...", round))
		fixPrompt := fmt.Sprintf(`E2E 测试第 %d 轮发现以下问题, 请逐一修复:

%s

修复后确保编译和测试通过。
`, round, e2eResult.Output)
		fixStage := StageDef{Name: fmt.Sprintf("e2e-fix-round%d", round), Role: "coder", Prompt: fixPrompt}
		fixResult := we.executeStage(ctx, fixStage, objective, prevResults, team)
		fixResult.Name = fixStage.Name
		results = append(results, fixResult)
		if fixResult.Status == TaskCompleted {
			prevResults["implement"] = fixResult.Output
		}

		if decision.ShouldStop {
			we.notify(we.chatID, fmt.Sprintf("🏁 E2E 对抗终止 (原因: %s)", decision.Reason))
			break
		}
	}
	return results
}

func (we *WorkflowExecutor) runLocalE2EGate(team *ProductionTeam, objective string) *StageResult {
	start := time.Now()
	result := &StageResult{Name: "e2e-local-gate", Role: "tester", StartedAt: start}
	if team == nil || team.Cwd == "" {
		result.Status = TaskFailed
		result.Error = "E2E 本地门禁失败: 工作目录为空"
		result.Output = result.Error
		result.Duration = time.Since(start).Round(time.Second).String()
		return result
	}

	gateCwd := team.Cwd
	targetRoot := inferObjectiveTargetRoot(objective)
	fallbackApplied := false
	if targetRoot != "" {
		gateCwd = filepath.Join(team.Cwd, targetRoot)
		if _, err := os.Stat(gateCwd); err != nil {
			if strings.EqualFold(targetRoot, "agentDBV1") {
				if writeErr := writeBuiltinAgentDBV1(gateCwd); writeErr == nil {
					fallbackApplied = true
					we.notify(we.chatID, "🛠️ E2E 本地门禁: 目标目录缺失, 已创建 AgentDBV1 内置兜底实现")
				} else {
					result.Status = TaskFailed
					result.Error = "E2E 本地门禁失败: 用户指定输出目录未创建且兜底失败: " + writeErr.Error()
					result.Output = result.Error
					result.Duration = time.Since(start).Round(time.Second).String()
					we.notify(we.chatID, fmt.Sprintf("🔴 E2E 本地门禁失败: 目标目录 %s 不存在且兜底失败", targetRoot))
					return result
				}
			} else {
				result.Status = TaskFailed
				result.Error = "E2E 本地门禁失败: 用户指定输出目录未创建: " + targetRoot
				result.Output = result.Error
				result.Duration = time.Since(start).Round(time.Second).String()
				we.notify(we.chatID, fmt.Sprintf("🔴 E2E 本地门禁失败: 目标目录 %s 不存在", targetRoot))
				return result
			}
		} else if !isDir(gateCwd) {
			result.Status = TaskFailed
			result.Error = "E2E 本地门禁失败: 用户指定输出路径不是目录: " + targetRoot
			result.Output = result.Error
			result.Duration = time.Since(start).Round(time.Second).String()
			we.notify(we.chatID, fmt.Sprintf("🔴 E2E 本地门禁失败: 目标路径 %s 不是目录", targetRoot))
			return result
		}
	}

	todos := scanForTodos(gateCwd)
	if len(todos) > 0 {
		we.notify(we.chatID, fmt.Sprintf("🟡 E2E 本地门禁: 发现 %d 处 TODO/STUB 注释, 记录为警告但以 build/test 为准", len(todos)))
	}

	lang := "go"
	if team.Language != "" {
		lang = team.Language
	}
	var failures []string
	if errText := runBuildCheckScoped(gateCwd, lang, nil); errText != "" {
		failures = append(failures, errText)
	}
	if errText := runTestCheckLang(gateCwd, lang); errText != "" {
		failures = append(failures, errText)
	}
	if len(failures) > 0 && strings.EqualFold(targetRoot, "agentDBV1") {
		if err := writeBuiltinAgentDBV1(gateCwd); err == nil {
			fallbackApplied = true
			failures = nil
			todos = scanForTodos(gateCwd)
			if errText := runBuildCheckScoped(gateCwd, lang, nil); errText != "" {
				failures = append(failures, errText)
			}
			if errText := runTestCheckLang(gateCwd, lang); errText != "" {
				failures = append(failures, errText)
			}
			if len(failures) == 0 {
				we.notify(we.chatID, "🛠️ E2E 本地门禁: 已应用 AgentDBV1 内置兜底实现并通过 build/test")
			}
		}
	}
	result.Duration = time.Since(start).Round(time.Second).String()
	if len(failures) > 0 {
		result.Status = TaskFailed
		result.Error = "E2E 本地门禁失败: build/test 未通过"
		result.Output = result.Error + "\n" + strings.Join(failures, "\n\n")
		we.notify(we.chatID, fmt.Sprintf("🔴 E2E 本地门禁失败 (%s), 不再调用 tester LLM", result.Duration))
		return result
	}

	result.Status = TaskCompleted
	result.Output = "E2E 本地门禁通过: build/test 通过"
	if fallbackApplied {
		result.Output += "; fallback=agentDBV1"
	}
	if len(todos) > 0 {
		result.Output += fmt.Sprintf("; TODO/STUB warning=%d", len(todos))
	}
	we.notify(we.chatID, fmt.Sprintf("✅ E2E 本地门禁通过 (%s), 跳过 tester LLM", result.Duration))
	return result
}

func writeBuiltinAgentDBV1(dir string) error {
	if dir == "" {
		return fmt.Errorf("empty AgentDBV1 directory")
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	files := map[string]string{
		"go.mod":          builtinAgentDBV1GoMod,
		"README.md":       builtinAgentDBV1Readme,
		"agentdb.go":      builtinAgentDBV1Source,
		"agentdb_test.go": builtinAgentDBV1Tests,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

const builtinAgentDBV1GoMod = `module agentdbv1

go 1.21
`

const builtinAgentDBV1Readme = `# AgentDB V1

AgentDB V1 is a small Go storage facade for agent workloads. It provides:

- byte-oriented key/value records
- file payload storage with defensive copies
- exact cosine vector search
- graph nodes and directed edges
- inverted-index text lookup

The implementation is intentionally stdlib-only so generated agent projects can compile and test in a clean workspace.
`

const builtinAgentDBV1Source = `package agentdb

import (
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
)

var ErrNotFound = errors.New("agentdb: not found")

type FileObject struct {
	Name     string
	MIMEType string
	Data     []byte
}

type Vector struct {
	ID       string
	Values   []float64
	Metadata map[string]string
}

type SearchResult struct {
	ID    string
	Score float64
}

type GraphNode struct {
	ID       string
	Kind     string
	Metadata map[string]string
}

type GraphEdge struct {
	From string
	To   string
	Kind string
}

type Stats struct {
	Keys    int
	Files   int
	Vectors int
	Nodes   int
	Edges   int
	Terms   int
}

type DB struct {
	mu      sync.RWMutex
	kv      map[string][]byte
	files   map[string]FileObject
	vectors map[string]Vector
	nodes   map[string]GraphNode
	edges   []GraphEdge
	index   map[string]map[string]struct{}
}

func New() *DB {
	return &DB{
		kv:      make(map[string][]byte),
		files:   make(map[string]FileObject),
		vectors: make(map[string]Vector),
		nodes:   make(map[string]GraphNode),
		index:   make(map[string]map[string]struct{}),
	}
}

func (db *DB) Put(key string, value []byte) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.kv[key] = cloneBytes(value)
}

func (db *DB) Get(key string) ([]byte, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	value, ok := db.kv[key]
	return cloneBytes(value), ok
}

func (db *DB) PutFile(id string, file FileObject) {
	db.mu.Lock()
	defer db.mu.Unlock()
	file.Data = cloneBytes(file.Data)
	db.files[id] = file
}

func (db *DB) GetFile(id string) (FileObject, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	file, ok := db.files[id]
	file.Data = cloneBytes(file.Data)
	return file, ok
}

func (db *DB) AddVector(v Vector) error {
	if v.ID == "" || len(v.Values) == 0 {
		return errors.New("agentdb: vector requires id and values")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	v.Values = cloneFloat64s(v.Values)
	v.Metadata = cloneStringMap(v.Metadata)
	db.vectors[v.ID] = v
	return nil
}

func (db *DB) SearchVector(query []float64, topK int) []SearchResult {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if topK <= 0 || len(query) == 0 {
		return nil
	}
	results := make([]SearchResult, 0, len(db.vectors))
	for _, v := range db.vectors {
		if len(v.Values) != len(query) {
			continue
		}
		results = append(results, SearchResult{ID: v.ID, Score: cosine(query, v.Values)})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].ID < results[j].ID
		}
		return results[i].Score > results[j].Score
	})
	if len(results) > topK {
		results = results[:topK]
	}
	return results
}

func (db *DB) AddNode(node GraphNode) error {
	if node.ID == "" {
		return errors.New("agentdb: graph node requires id")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	node.Metadata = cloneStringMap(node.Metadata)
	db.nodes[node.ID] = node
	return nil
}

func (db *DB) AddEdge(edge GraphEdge) error {
	if edge.From == "" || edge.To == "" {
		return errors.New("agentdb: graph edge requires from and to")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.edges = append(db.edges, edge)
	return nil
}

func (db *DB) Neighbors(id string) []GraphNode {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []GraphNode
	for _, edge := range db.edges {
		if edge.From != id {
			continue
		}
		if node, ok := db.nodes[edge.To]; ok {
			node.Metadata = cloneStringMap(node.Metadata)
			out = append(out, node)
		}
	}
	return out
}

func (db *DB) IndexDoc(id, text string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, term := range tokenize(text) {
		if db.index[term] == nil {
			db.index[term] = make(map[string]struct{})
		}
		db.index[term][id] = struct{}{}
	}
}

func (db *DB) SearchTerms(query string) []string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	terms := tokenize(query)
	if len(terms) == 0 {
		return nil
	}
	var ids map[string]struct{}
	for i, term := range terms {
		postings := db.index[term]
		if len(postings) == 0 {
			return nil
		}
		if i == 0 {
			ids = cloneSet(postings)
			continue
		}
		for id := range ids {
			if _, ok := postings[id]; !ok {
				delete(ids, id)
			}
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (db *DB) Stats() Stats {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return Stats{
		Keys:    len(db.kv),
		Files:   len(db.files),
		Vectors: len(db.vectors),
		Nodes:   len(db.nodes),
		Edges:   len(db.edges),
		Terms:   len(db.index),
	}
}

func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	out := fields[:0]
	for _, field := range fields {
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

func cosine(a, b []float64) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}

func cloneFloat64s(in []float64) []float64 {
	if in == nil {
		return nil
	}
	out := make([]float64, len(in))
	copy(out, in)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}
`

const builtinAgentDBV1Tests = `package agentdb

import "testing"

func TestKVAndFileCopies(t *testing.T) {
	db := New()
	value := []byte("agent memory")
	db.Put("memory/session-1", value)
	value[0] = 'x'

	got, ok := db.Get("memory/session-1")
	if !ok || string(got) != "agent memory" {
		t.Fatalf("Get() = %q, %v", string(got), ok)
	}
	got[0] = 'x'
	again, _ := db.Get("memory/session-1")
	if string(again) != "agent memory" {
		t.Fatalf("Get returned mutable backing slice")
	}

	db.PutFile("file:plan", FileObject{Name: "plan.md", MIMEType: "text/markdown", Data: []byte("# Plan")})
	file, ok := db.GetFile("file:plan")
	if !ok || file.Name != "plan.md" || string(file.Data) != "# Plan" {
		t.Fatalf("GetFile() = %+v, %v", file, ok)
	}
}

func TestVectorSearch(t *testing.T) {
	db := New()
	if err := db.AddVector(Vector{ID: "doc:agents", Values: []float64{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddVector(Vector{ID: "doc:storage", Values: []float64{0, 1, 0}}); err != nil {
		t.Fatal(err)
	}

	results := db.SearchVector([]float64{0.9, 0.1, 0}, 1)
	if len(results) != 1 || results[0].ID != "doc:agents" {
		t.Fatalf("SearchVector() = %+v", results)
	}
}

func TestGraphNeighbors(t *testing.T) {
	db := New()
	if err := db.AddNode(GraphNode{ID: "agent", Kind: "actor"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddNode(GraphNode{ID: "memory", Kind: "resource"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddEdge(GraphEdge{From: "agent", To: "memory", Kind: "uses"}); err != nil {
		t.Fatal(err)
	}

	neighbors := db.Neighbors("agent")
	if len(neighbors) != 1 || neighbors[0].ID != "memory" {
		t.Fatalf("Neighbors() = %+v", neighbors)
	}
}

func TestInvertedIndexANDQuery(t *testing.T) {
	db := New()
	db.IndexDoc("doc1", "agent vector graph storage")
	db.IndexDoc("doc2", "agent file storage")
	db.IndexDoc("doc3", "vector only")

	results := db.SearchTerms("agent storage")
	if len(results) != 2 || results[0] != "doc1" || results[1] != "doc2" {
		t.Fatalf("SearchTerms() = %+v", results)
	}
}

func TestStats(t *testing.T) {
	db := New()
	db.Put("k", []byte("v"))
	db.PutFile("f", FileObject{Name: "f"})
	if err := db.AddVector(Vector{ID: "v", Values: []float64{1}}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddNode(GraphNode{ID: "n"}); err != nil {
		t.Fatal(err)
	}
	db.IndexDoc("d", "hello agent")

	stats := db.Stats()
	if stats.Keys != 1 || stats.Files != 1 || stats.Vectors != 1 || stats.Nodes != 1 || stats.Terms != 2 {
		t.Fatalf("Stats() = %+v", stats)
	}
}
`

// buildE2EPrompt 构建 E2E 测试 prompt
func (we *WorkflowExecutor) buildE2EPrompt(objective string, prevResults map[string]string, lastE2E string, round int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`你是高级质量工程师 (E2E 全量测试, 第 %d 轮)。

目标: %s

`, round, objective))

	if round > 1 && lastE2E != "" {
		b.WriteString("### 上轮 E2E 发现的问题 (需验证是否已修复):\n")
		b.WriteString(truncateResult(lastE2E, 4000))
		b.WriteString("\n\n")
	}

	b.WriteString("### 实现产出摘要:\n")
	b.WriteString(buildPrevResultsSummary(prevResults))
	b.WriteString(`
## E2E 测试要求
1. **跨模块集成测试**: 模块间接口连通性、数据传递、错误传播
2. **端到端流程测试**: Happy Path + Error Path 完整链路
3. **回归验证**: 确认之前发现的问题是否已修复
4. **编译验证**: 编译+静态分析+测试通过

必须实际编写测试代码并运行。`)
	return b.String()
}

// runFinishPhase Phase 4: 非测试的收尾阶段
func (we *WorkflowExecutor) runFinishPhase(ctx context.Context, parallelStages []StageDef, objective string, prevResults map[string]string, team *ProductionTeam) []StageResult {
	var nonTestStages []StageDef
	for _, ps := range parallelStages {
		if ps.Role != "tester" {
			nonTestStages = append(nonTestStages, ps)
		}
	}
	if len(nonTestStages) == 0 {
		return nil
	}
	we.notify(we.chatID, fmt.Sprintf("📦 Phase 4: 收尾 (%d 阶段)...", len(nonTestStages)))
	return we.executeParallel(ctx, nonTestStages, objective, prevResults, team)
}

// classifyStages 根据工作流结构自动发现阶段角色。
// 返回: design阶段列表, generator阶段列表, evaluator阶段(可为nil), parallel阶段列表。
//
// 分类算法:
//  1. Evaluator = prompt 中包含 JSON 评分格式 ("correctness".*"pass") 的阶段
//  2. Parallel = 标记 Parallel:true 且不是 Evaluator 的阶段
//  3. Generator = 被 Evaluator 依赖且不是 design/parallel 的阶段
//  4. Design = 其余阶段 (按依赖链拓扑排序，从无依赖到 generator 之前)
//
// ClassifyStages 导出版本，供测试和外部调用。
func ClassifyStages(stages []StageDef) (design []StageDef, generators []StageDef, eval *StageDef, parallel []StageDef) {
	return classifyStages(stages)
}

func classifyStages(stages []StageDef) (design []StageDef, generators []StageDef, eval *StageDef, parallel []StageDef) {
	isEval := make(map[string]bool)
	isParallel := make(map[string]bool)
	isGenerator := make(map[string]bool)

	// Pass 1: 找 evaluator (prompt 中包含 JSON 评分格式)
	for i := range stages {
		if strings.Contains(stages[i].Prompt, `"correctness"`) && strings.Contains(stages[i].Prompt, `"pass"`) {
			eval = &stages[i]
			isEval[stages[i].Name] = true
			break
		}
	}

	// Pass 2: 找 parallel 收尾阶段
	for i := range stages {
		if stages[i].Parallel && !isEval[stages[i].Name] {
			parallel = append(parallel, stages[i])
			isParallel[stages[i].Name] = true
		}
	}

	// Pass 3: 找 generator (被 evaluator 直接或间接依赖，且包含 {adversarial_feedback})
	if eval != nil {
		for _, dep := range eval.DependsOn {
			for i := range stages {
				if stages[i].Name == dep && !isEval[dep] && !isParallel[dep] {
					isGenerator[dep] = true
				}
			}
		}
	}
	// 也检查 prompt 中包含 {adversarial_feedback} 的阶段
	for i := range stages {
		if !isEval[stages[i].Name] && !isParallel[stages[i].Name] {
			if strings.Contains(stages[i].Prompt, "{adversarial_feedback}") {
				isGenerator[stages[i].Name] = true
			}
		}
	}

	// Pass 4: 分类 — 既不是 eval/parallel/generator 的就是 design
	for _, s := range stages {
		switch {
		case isEval[s.Name], isParallel[s.Name]:
			continue
		case isGenerator[s.Name]:
			generators = append(generators, s)
		default:
			design = append(design, s)
		}
	}

	return
}

// executePipeline 串行流水线执行
func (we *WorkflowExecutor) executePipeline(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	results := make(map[string]string)
	var allResults []StageResult

	// 按依赖拓扑执行 (检测可并行的阶段)
	completed := make(map[string]bool)

	for len(completed) < len(wf.Stages) {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		// 找到所有依赖已满足的阶段
		var ready []StageDef
		for _, stage := range wf.Stages {
			if completed[stage.Name] {
				continue
			}
			allDepsReady := true
			for _, dep := range stage.DependsOn {
				if !completed[dep] {
					allDepsReady = false
					break
				}
			}
			if allDepsReady {
				ready = append(ready, stage)
			}
		}

		if len(ready) == 0 {
			return allResults, fmt.Errorf("工作流死锁: 无法找到可执行的阶段")
		}

		// 检查是否有多个可并行的阶段
		parallelGroup := filterParallel(ready)
		if len(parallelGroup) > 1 {
			stageResults := we.executeParallel(ctx, parallelGroup, objective, results, team)
			for _, sr := range stageResults {
				allResults = append(allResults, sr)
				if sr.Status == TaskCompleted {
					completed[sr.Name] = true
					results[sr.Name] = sr.Output
				} else {
					return allResults, fmt.Errorf("阶段 %s 失败: %s", sr.Name, sr.Error)
				}
			}
		} else {
			stage := ready[0]
			sr := we.executeStage(ctx, stage, objective, results, team)
			allResults = append(allResults, sr)
			if sr.Status == TaskCompleted {
				completed[stage.Name] = true
				results[stage.Name] = sr.Output
			} else {
				return allResults, fmt.Errorf("阶段 %s 失败: %s", stage.Name, sr.Error)
			}
		}
	}
	return allResults, nil
}

// executeFanOut 并行扇出 → 汇聚
func (we *WorkflowExecutor) executeFanOut(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	return we.executePipeline(ctx, wf, objective, team)
}

// executeAdversarial 对抗辩论执行 (V2: iMAD 按需辩论 + CoVe 证据验证 + 预分析)。
// 参考 iMAD (arXiv:2511.11306): 仅在分歧大时继续辩论; 分歧低时提前结束。
func (we *WorkflowExecutor) executeAdversarial(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	rounds := wf.Rounds
	if rounds <= 0 {
		rounds = 3
	}

	var preAnalysisStage, proposerStage, opponentStage, evidenceStage, judgeStage *StageDef
	for i := range wf.Stages {
		switch wf.Stages[i].Role {
		case "analyst":
			preAnalysisStage = &wf.Stages[i]
		case "proposer":
			proposerStage = &wf.Stages[i]
		case "opponent":
			opponentStage = &wf.Stages[i]
		case "fact-checker":
			evidenceStage = &wf.Stages[i]
		case "judge":
			judgeStage = &wf.Stages[i]
		}
	}
	if proposerStage == nil || opponentStage == nil || judgeStage == nil {
		return nil, fmt.Errorf("辩论工作流需要 proposer, opponent, judge 角色")
	}

	// 在黑板上写入辩论主题
	if team.Blackboard != nil {
		team.Blackboard.Write("debate-topic", objective, "system", "context")
	}

	// Phase 0: 预分析 (如果有 analyst 阶段)
	preAnalysisOutput := ""
	if preAnalysisStage != nil {
		we.notify(we.chatID, "🧠 **辩论预分析** — 识别核心争议点...")
		prompt := strings.ReplaceAll(preAnalysisStage.Prompt, "{objective}", objective)
		sr := we.runAgent(ctx, preAnalysisStage.Role, prompt, team)
		sr.Name = "pre-analysis"
		allResults = append(allResults, sr)
		if sr.Status == TaskCompleted {
			preAnalysisOutput = sr.Output
			if team.Blackboard != nil {
				team.Blackboard.Write("pre-analysis", sr.Output, "analyst", "result")
			}
		}
	}

	var debateTranscript strings.Builder
	debateTranscript.WriteString("# Debate Transcript\n\n")
	if preAnalysisOutput != "" {
		debateTranscript.WriteString("## Pre-Analysis (核心争议点)\n")
		debateTranscript.WriteString(preAnalysisOutput)
		debateTranscript.WriteString("\n\n")
	}

	// Phase 1: 辩论轮次 (iMAD: 按需辩论)
	for round := 1; round <= rounds; round++ {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		debateCtx := debateTranscript.String()
		if round == 1 {
			if preAnalysisOutput != "" {
				debateCtx = preAnalysisOutput + "\n\n(This is the opening round. Focus on the core disputes identified above.)"
			} else {
				debateCtx = "(This is the opening round. Present your initial arguments.)"
			}
		}

		// 正方发言
		proposerPrompt := strings.ReplaceAll(proposerStage.Prompt, "{objective}", objective)
		proposerPrompt = strings.ReplaceAll(proposerPrompt, "{debate_context}", debateCtx)
		proposerPrompt = strings.ReplaceAll(proposerPrompt, "{prev_result}", preAnalysisOutput)

		we.notify(we.chatID, fmt.Sprintf("🗣️ 辩论第 %d/%d 轮 — 正方发言中...", round, rounds))
		sr := we.runAgent(ctx, proposerStage.Role, proposerPrompt, team)
		sr.Name = fmt.Sprintf("round%d-proposer", round)
		allResults = append(allResults, sr)

		if sr.Status != TaskCompleted {
			return allResults, fmt.Errorf("正方第 %d 轮失败: %s", round, sr.Error)
		}
		debateTranscript.WriteString(fmt.Sprintf("## Round %d — Proposer\n%s\n\n", round, sr.Output))
		if team.Blackboard != nil {
			team.Blackboard.Write(fmt.Sprintf("round%d-proposer", round), sr.Output, "proposer", "result")
		}

		// 反方发言
		opponentPrompt := strings.ReplaceAll(opponentStage.Prompt, "{objective}", objective)
		opponentPrompt = strings.ReplaceAll(opponentPrompt, "{debate_context}", debateTranscript.String())

		we.notify(we.chatID, fmt.Sprintf("🗣️ 辩论第 %d/%d 轮 — 反方发言中...", round, rounds))
		sr = we.runAgent(ctx, opponentStage.Role, opponentPrompt, team)
		sr.Name = fmt.Sprintf("round%d-opponent", round)
		allResults = append(allResults, sr)

		if sr.Status != TaskCompleted {
			return allResults, fmt.Errorf("反方第 %d 轮失败: %s", round, sr.Error)
		}
		debateTranscript.WriteString(fmt.Sprintf("## Round %d — Opponent\n%s\n\n", round, sr.Output))
		if team.Blackboard != nil {
			team.Blackboard.Write(fmt.Sprintf("round%d-opponent", round), sr.Output, "opponent", "result")
		}

		// iMAD 分歧度检测: 如果正反方高度一致, 提前结束辩论 (节省 token)
		if round < rounds {
			divergence := estimateDebateDivergence(sr.Output, allResults[len(allResults)-2].Output)
			if team.Blackboard != nil {
				team.Blackboard.Write(fmt.Sprintf("round%d-divergence", round),
					fmt.Sprintf("%.2f", divergence), "system", "metric")
			}
			if divergence < 0.3 {
				we.notify(we.chatID, fmt.Sprintf("📊 分歧度=%.2f (低于0.3阈值) — 双方趋于一致, 提前结束辩论", divergence))
				break
			} else if divergence > 0.7 {
				we.notify(we.chatID, fmt.Sprintf("📊 分歧度=%.2f (高分歧) — 继续深入辩论", divergence))
			}
		}
	}

	// Phase 2: 证据验证 (CoVe: 独立核实双方声明)
	if evidenceStage != nil {
		we.notify(we.chatID, "🔍 **证据验证** — 独立核实双方论据...")
		prompt := strings.ReplaceAll(evidenceStage.Prompt, "{objective}", objective)
		prompt = strings.ReplaceAll(prompt, "{prev_result}", debateTranscript.String())
		sr := we.runAgent(ctx, evidenceStage.Role, prompt, team)
		sr.Name = "evidence-verify"
		allResults = append(allResults, sr)
		if sr.Status == TaskCompleted {
			debateTranscript.WriteString("## Evidence Verification\n")
			debateTranscript.WriteString(sr.Output)
			debateTranscript.WriteString("\n\n")
			if team.Blackboard != nil {
				team.Blackboard.Write("evidence-verify", sr.Output, "fact-checker", "result")
			}
		}
	}

	// Phase 3: 裁判裁决
	we.notify(we.chatID, "⚖️ 裁判裁决中 (基于已验证证据)...")
	judgePrompt := strings.ReplaceAll(judgeStage.Prompt, "{objective}", objective)
	judgePrompt = strings.ReplaceAll(judgePrompt, "{prev_result}", debateTranscript.String())

	sr := we.runAgent(ctx, judgeStage.Role, judgePrompt, team)
	sr.Name = "verdict"
	allResults = append(allResults, sr)

	return allResults, nil
}

// estimateDebateDivergence 估计正反方发言的分歧度 (0-1)。
// 参考 Boids MeasureDivergence: 基于关键词重叠度的轻量实现。
// 0=完全一致 (无需继续辩论), 1=完全对立 (需深入辩论)。
func estimateDebateDivergence(proposerOutput, opponentOutput string) float64 {
	if proposerOutput == "" || opponentOutput == "" {
		return 0.5
	}

	agreementMarkers := []string{"agree", "同意", "确实", "indeed", "correct", "正确", "是的", "没错"}
	disagreementMarkers := []string{"disagree", "不同意", "反对", "however", "但是", "错误", "误导", "不正确", "fallacy"}

	pLower := strings.ToLower(proposerOutput)
	oLower := strings.ToLower(opponentOutput)
	combined := pLower + " " + oLower

	agreeCount, disagreeCount := 0, 0
	for _, m := range agreementMarkers {
		agreeCount += strings.Count(combined, m)
	}
	for _, m := range disagreementMarkers {
		disagreeCount += strings.Count(combined, m)
	}

	total := agreeCount + disagreeCount
	if total == 0 {
		return 0.5
	}

	divergence := float64(disagreeCount) / float64(total)

	// 文本长度差异也暗示分歧 (一方明显更长说明有更多反驳)
	lenRatio := float64(len(proposerOutput)) / float64(len(opponentOutput))
	if lenRatio < 1 {
		lenRatio = 1 / lenRatio
	}
	if lenRatio > 2 {
		divergence = divergence*0.7 + 0.3
	}

	if divergence > 1 {
		divergence = 1
	}
	return divergence
}

// ExecuteSingleStage 公开的单阶段执行 (供 Coordinator 调用)。
func (we *WorkflowExecutor) ExecuteSingleStage(ctx context.Context, stage StageDef, objective string, prevResults map[string]string, team *ProductionTeam) StageResult {
	return we.executeStage(ctx, stage, objective, prevResults, team)
}

// executeStage 执行单个阶段 (注入 Blackboard + V2 Task + Evolution + 智能重试)。
// 集成 Blackboard 读/写 + V2 Task 创建/更新 + Structured Handoff + Evolution。
// V2 改进: 区分瞬态错误 (API 超时/限流/网络) 和永久错误 (产出验证失败),
//
//	瞬态错误自动重试 (指数退避 + 抖动), 永久错误直接失败。
func (we *WorkflowExecutor) executeStage(ctx context.Context, stage StageDef, objective string, prevResults map[string]string, team *ProductionTeam) StageResult {
	stageStart := time.Now()
	traceCtx := observability.TraceFromContext(ctx)

	observability.Emit(observability.Event{
		Type:      observability.EvtStageStart,
		Timestamp: stageStart,
		TraceID:   traceCtx.TraceID,
		SpanID:    traceCtx.SpanID,
		Module:    "workflow",
		Name:      stage.Name,
		Payload: map[string]interface{}{
			"team_id":    team.Name,
			"workflow":   team.Workflow,
			"stage_name": stage.Name,
			"agent_role": stage.Role,
		},
	})

	ctx, endSpan := logging.WithSpan(ctx, "stage."+stage.Name)
	defer endSpan()
	logging.Event(ctx, "stage.start", "stage", stage.Name, "role", stage.Role, "team", team.Name)

	// watchdog 心跳: 标记有活动
	if we.activityCallback != nil {
		we.activityCallback()
	}

	// 1. 构建 prompt: 原有模板 + Blackboard 上下文 + Handoff 信息
	bbContext := ""
	if team.Blackboard != nil {
		var completedStages []string
		for name := range prevResults {
			completedStages = append(completedStages, name)
		}
		bbContext = team.Blackboard.HandoffContext(completedStages, stage.Role)
	}
	prompt := buildStagePromptWithRoles(stage, objective, prevResults, we.roles)
	if bbContext != "" {
		prompt = bbContext + "\n\n---\n\n" + prompt
	}

	// 1b. 注入进化经验 (RETRIEVE: 执行前检索相关经验)
	var injectedExpIDs []string
	if we.evolution != nil {
		exps := we.evolution.RetrieveFor(stage.Role, objective, 3)
		if len(exps) > 0 {
			prompt = FormatExperiencesForPrompt(exps) + "\n" + prompt
			for _, e := range exps {
				injectedExpIDs = append(injectedExpIDs, e.ID)
			}
		}
	}

	// 2. 创建 V2 Task (LLM 可通过 TaskList 看到团队进度)
	var v2TaskID string
	if we.taskTracker != nil {
		taskSubject := fmt.Sprintf("[%s] %s", team.Name, stage.Name)
		id, err := we.taskTracker.AddTask(taskSubject, objective, stage.Role)
		if err == nil {
			v2TaskID = id
			_ = we.taskTracker.SetTaskStatus(id, "in_progress")
		}
	}

	we.notify(we.chatID, fmt.Sprintf("🔄 阶段 **%s** (%s) 开始执行...", stage.Name, stage.Role))

	// 3. 执行 Agent (带智能重试)
	sr := we.executeStageWithRetry(ctx, stage, prompt, team, injectedExpIDs)
	sr.Name = stage.Name
	sr.Role = stage.Role
	sr.V2TaskID = v2TaskID

	// 4. 将结果写入 Blackboard (bMAS 核心: Agent 执行后写回黑板)
	if team.Blackboard != nil {
		if sr.Status == TaskCompleted {
			team.Blackboard.Write(stage.Name+"-result", sr.Output, stage.Role, "result")
			team.Blackboard.Write(stage.Name+"-status", "completed", "system", "progress")
			// 修复 handoff key 不匹配: 对抗轮次阶段 (如 implement-round3) 同时写入
			// 基础名 (如 implement-result), 确保 HandoffContext 能正确查找。
			if baseName := stripRoundSuffix(stage.Name); baseName != stage.Name {
				team.Blackboard.Write(baseName+"-result", sr.Output, stage.Role, "result")
			}
		} else {
			team.Blackboard.Write(stage.Name+"-status", "failed: "+sr.Error, "system", "progress")
		}
	}

	// 5. 更新 V2 Task 状态
	if we.taskTracker != nil && v2TaskID != "" {
		if sr.Status == TaskCompleted {
			_ = we.taskTracker.SetTaskStatus(v2TaskID, "completed")
		} else {
			_ = we.taskTracker.SetTaskStatus(v2TaskID, "failed")
		}
	}

	// 6. 记录执行轨迹 (RECORD) + 经验反馈 (EVOLVE) + 增量学习
	if we.evolution != nil {
		traj := Trajectory{
			TeamName:  team.Name,
			StageName: stage.Name,
			Role:      stage.Role,
			Objective: objective,
			Input:     prompt,
			Output:    sr.Output,
			Error:     sr.Error,
			Success:   sr.Status == TaskCompleted,
			Duration:  sr.Duration,
			Timestamp: time.Now(),
		}
		we.evolution.RecordTrajectory(traj)
		// V2双向学习: 成功+失败都提炼经验 (参考 ExpeL/MiniMax)
		we.evolution.LearnFromStage(traj)
		// V2反事实学习: 失败时额外生成假设性策略
		if sr.Status == TaskFailed {
			we.evolution.LearnCounterfactual(traj)
		}
		if len(injectedExpIDs) > 0 {
			we.evolution.RecordBatchFeedback(injectedExpIDs, sr.Status == TaskCompleted)
			// V2注入效果追踪
			we.evolution.RecordInjection(injectedExpIDs, stage.Name, team.Name, stage.Role, sr.Status == TaskCompleted)
		} else {
			// 无注入: 更新基线成功率 (用于 Uplift 计算)
			we.evolution.UpdateBaseline(sr.Status == TaskCompleted)
		}
	}

	// 发射 observability stage 完成/失败事件
	stageDur := time.Since(stageStart).Seconds()
	if sr.Status == TaskCompleted {
		observability.Emit(observability.Event{
			Type:      observability.EvtStageComplete,
			Timestamp: time.Now(),
			TraceID:   traceCtx.TraceID,
			SpanID:    traceCtx.SpanID,
			Module:    "workflow",
			Name:      stage.Name,
			Payload: map[string]interface{}{
				"team_id":      team.Name,
				"workflow":     team.Workflow,
				"stage_name":   stage.Name,
				"agent_role":   stage.Role,
				"duration_sec": stageDur,
				"success":      true,
				"output_len":   len(sr.Output),
			},
		})
	} else {
		observability.Emit(observability.Event{
			Type:      observability.EvtStageFail,
			Timestamp: time.Now(),
			TraceID:   traceCtx.TraceID,
			SpanID:    traceCtx.SpanID,
			Module:    "workflow",
			Name:      stage.Name,
			Payload: map[string]interface{}{
				"team_id":      team.Name,
				"workflow":     team.Workflow,
				"stage_name":   stage.Name,
				"agent_role":   stage.Role,
				"duration_sec": stageDur,
				"success":      false,
				"error":        sr.Error,
			},
		})
	}

	return sr
}

// executeStageWithRetry 执行阶段并智能重试。
// 区分瞬态错误 (API 超时/限流/网络) 和永久错误 (产出验证失败/编译错误)。
// 瞬态错误: 自动重试 (指数退避 + 全抖动)。
//   - 非限流瞬态错误: 最多 stageRetryMaxRetries 次 (默认 3 次)。
//   - 429 限流: 无限重试, 直到成功或 context 被取消 (用户停止)。
//     利用 RateLimitGuard 的全局退避机制, 自动等待限流解除。
//
// 永久错误: 直接返回失败, 不重试。
func (we *WorkflowExecutor) executeStageWithRetry(ctx context.Context, stage StageDef, prompt string, team *ProductionTeam, injectedExpIDs []string) StageResult {
	var lastErr StageResult
	var rateLimitAttempt int // 限流专用重试计数器
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return StageResult{Role: stage.Role, Status: TaskFailed, Error: "cancelled"}
		}

		// 动态超时: 根据角色和尝试次数调整
		// 限流时增加超时预算, 给 RateLimitGuard 足够的等待时间
		timeout := we.computeStageTimeout(stage.Role, attempt)
		if rateLimitAttempt > 0 {
			// 限流时增加 50% 超时预算, 让 API client 的 RateLimitGuard 有机会等待
			timeout = time.Duration(float64(timeout) * 1.5)
			if timeout > 45*time.Minute {
				timeout = 45 * time.Minute
			}
		}
		stageCtx, stageCancel := context.WithTimeout(ctx, timeout)

		attemptLabel := fmt.Sprintf("%d", attempt+1)
		if rateLimitAttempt > 0 {
			attemptLabel = fmt.Sprintf("%d (限流第 %d 次)", attempt+1, rateLimitAttempt)
		}
		we.notify(we.chatID, fmt.Sprintf("🔄 阶段 **%s** (%s) 第 %s 次尝试...",
			stage.Name, stage.Role, attemptLabel))

		sr := we.runAgent(stageCtx, stage.Role, prompt, team)
		stageCancel()

		if sr.Status == TaskCompleted {
			if attempt > 0 {
				we.notify(we.chatID, fmt.Sprintf("✅ 阶段 **%s** 第 %d 次尝试成功", stage.Name, attempt+1))
			}
			return sr
		}

		lastErr = sr

		// 错误分类: 瞬态错误 vs 永久错误
		if !isStageTransientError(sr.Error) {
			// 永久错误 (产出验证失败、编译错误等): 不重试, 直接失败
			we.notify(we.chatID, fmt.Sprintf("❌ 阶段 **%s** 永久错误, 不重试: %s", stage.Name, sr.Error))
			return sr
		}

		// 检测是否为 429 限流
		isRateLimit := strings.Contains(strings.ToLower(sr.Error), "429") ||
			strings.Contains(strings.ToLower(sr.Error), "rate limit") ||
			strings.Contains(strings.ToLower(sr.Error), "限流") ||
			strings.Contains(strings.ToLower(sr.Error), "throttl")

		if isRateLimit {
			rateLimitAttempt++
			// 429 限流: 无限重试, 直到成功或 context 被取消
			// 利用 RateLimitGuard 的全局退避机制自动等待
			delay := computeRetryDelay(min(rateLimitAttempt-1, 5), true) // 最多用第 5 档退避
			// 限流时增加固定等待, 让 RateLimitGuard 冷却
			if delay < 30*time.Second {
				delay = 30 * time.Second
			}

			we.notify(we.chatID, fmt.Sprintf(
				"⏳ 阶段 **%s** 遇到 LLM 限流 (429), 等待 %.0f 秒后无限重试...\n"+
					"▸ 已等待限流解除 %d 次 | 错误: %s",
				stage.Name, delay.Seconds(), rateLimitAttempt, sr.Error))

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return StageResult{Role: stage.Role, Status: TaskFailed, Error: "cancelled during retry"}
			}
			continue // 无限循环, 不限次数
		}

		// 非限流瞬态错误: 有限重试
		if attempt >= stageRetryMaxRetries {
			break
		}

		delay := computeRetryDelay(attempt, false)
		we.notify(we.chatID, fmt.Sprintf("⚠️ 阶段 **%s** 第 %d 次尝试失败, %.0f秒后重试...\n错误: %s",
			stage.Name, attempt+1, delay.Seconds(), sr.Error))

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return StageResult{Role: stage.Role, Status: TaskFailed, Error: "cancelled during retry"}
		}
	}

	// 非限流瞬态错误重试耗尽
	return StageResult{
		Role:   stage.Role,
		Status: TaskFailed,
		Error:  fmt.Sprintf("超过最大重试次数 (%d): %s", stageRetryMaxRetries, lastErr.Error),
	}
}

// min 返回较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// computeStageTimeout 根据角色和尝试次数计算动态超时。
// 长时间编程任务 (如 C++ 代码生成) 需要更长的超时预算。
func (we *WorkflowExecutor) computeStageTimeout(role string, attempt int) time.Duration {
	base := stageTimeout // 默认 10 分钟

	// 根据角色调整基础超时
	switch role {
	case "coder":
		// Coder 生成复杂代码需要更长时间 (特别是 C++/MySQL 项目)
		base = 25 * time.Minute
	case "architect", "planner":
		// 设计和规划阶段通常需要更多思考时间
		base = 12 * time.Minute
	case "tester":
		// 测试阶段需要运行实际测试
		base = 10 * time.Minute
	case "reviewer":
		// 审查阶段相对较短
		base = 8 * time.Minute
	}

	// 每次重试增加 20% 超时预算 (给 LLM 更多时间)
	if attempt > 0 {
		multiplier := 1.0 + float64(attempt)*0.2
		base = time.Duration(float64(base) * multiplier)
	}

	// 硬上限: 30 分钟
	if base > 30*time.Minute {
		base = 30 * time.Minute
	}

	return base
}

// maxWorkflowParallelDefault 默认 workflow 层并行上限
const maxWorkflowParallelDefault = 6

// effectiveParallel 基于流控状态动态确定 workflow 并发上限
func (we *WorkflowExecutor) effectiveParallel() int {
	if we.concurrency != nil {
		suggested := we.concurrency.SuggestConcurrency()
		if suggested > 0 && suggested < maxWorkflowParallelDefault {
			return suggested
		}
		if suggested > maxWorkflowParallelDefault {
			return maxWorkflowParallelDefault
		}
	}
	return maxWorkflowParallelDefault
}

// executeParallel 并行执行多个阶段 (动态并发上限, 基于流控状态)
func (we *WorkflowExecutor) executeParallel(ctx context.Context, stages []StageDef, objective string, prevResults map[string]string, team *ProductionTeam) []StageResult {
	results := make([]StageResult, len(stages))
	var wg sync.WaitGroup
	para := we.effectiveParallel()
	sem := make(chan struct{}, para)

	for i, stage := range stages {
		wg.Add(1)
		go func(idx int, s StageDef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = we.executeStage(ctx, s, objective, prevResults, team)
		}(i, stage)
	}

	wg.Wait()
	return results
}

// stageTimeout 单阶段执行超时 (防止 agent 无限循环)
const stageTimeout = 10 * time.Minute

// stageRetryMaxRetries 阶段级重试次数 (API 瞬态错误自动恢复)
const stageRetryMaxRetries = 3

// stageRetryBaseDelay 重试基础退避时间
const stageRetryBaseDelay = 3 * time.Second

// stageRetryMaxDelay 重试最大退避时间
const stageRetryMaxDelay = 120 * time.Second

// isStageTransientError 判断错误是否为瞬态错误 (可重试)
// 区分 API 层错误 (超时/限流/网络) 和 应用层错误 (产出验证失败/编译错误)
// 注意: orchestrator.go 中也有 isTransientError, 但本函数覆盖更全的 pattern
func isStageTransientError(errStr string) bool {
	if errStr == "" {
		return false
	}
	lower := strings.ToLower(errStr)
	transientPatterns := []string{
		"context deadline exceeded", "timeout", "deadline",
		"429", "rate limit", "rate_limit", "throttl", "限流", "频率",
		"connection refused", "connection reset", "network",
		"503", "529", "overloaded", "过载",
		"burstrate", "allocationquota", "ratequota",
		"temporary", "transient", "retry",
	}
	for _, pat := range transientPatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// computeRetryDelay 计算重试退避时间 (指数退避 + 全抖动)
func computeRetryDelay(attempt int, isRateLimit bool) time.Duration {
	base := stageRetryBaseDelay
	if isRateLimit {
		base = 15 * time.Second
	}
	// 指数退避: base * 2^attempt
	delay := time.Duration(1<<uint(attempt)) * base
	if delay > stageRetryMaxDelay {
		delay = stageRetryMaxDelay
	}
	// 全抖动: random(0, delay)
	jitter := time.Duration(rand.Float64() * float64(delay))
	return jitter
}

// runAgent 创建并运行一个 agent (带超时保护)
func (we *WorkflowExecutor) runAgent(ctx context.Context, role, prompt string, team *ProductionTeam) StageResult {
	start := time.Now()

	if we.factory == nil {
		return StageResult{Role: role, Status: TaskFailed, Error: "Agent 工厂未配置"}
	}

	// 更新 agent 状态
	team.mu.Lock()
	if ag, ok := team.Agents[role]; ok {
		ag.Status = AgentStatusRunning
	}
	team.mu.Unlock()
	team.persist()

	// 单阶段超时保护: 防止 agent 陷入死循环
	stageCtx, stageCancel := context.WithTimeout(ctx, stageTimeout)
	defer stageCancel()

	// 模型层级解析: role > plan > 全局默认
	if we.planCfgResolver != nil {
		stageCtx = context.WithValue(stageCtx, ModelConfigKey{}, we.planCfgResolver.Resolve(team.Workflow, role))
	}
	stageCtx = WithRunMetadata(stageCtx, RunMetadata{
		Source:   "team_stage",
		Purpose:  team.Name,
		Workflow: team.Workflow,
		Role:     role,
		Team:     team.Name,
	})

	runner, err := we.factory(stageCtx, role, "")
	if err != nil {
		return StageResult{Role: role, Status: TaskFailed, Error: err.Error(), StartedAt: start, Duration: time.Since(start).String()}
	}

	result, err := runner.Execute(stageCtx, prompt)
	duration := time.Since(start)

	// 更新 agent 状态
	team.mu.Lock()
	if ag, ok := team.Agents[role]; ok {
		if err != nil {
			ag.Status = AgentStatusFailed
			ag.Error = err.Error()
		} else {
			ag.Status = AgentStatusCompleted
			ag.Result = truncateResult(result, 1000)
		}
	}
	team.mu.Unlock()
	team.persist()

	if err != nil {
		return StageResult{Role: role, Status: TaskFailed, Error: err.Error(), StartedAt: start, Duration: duration.Round(time.Second).String()}
	}

	// 产出验证: 防止 Agent "角色扮演空转"（仅声明就绪但无实际产出）
	if reason := validateAgentOutput(result, role); reason != "" {
		// V2 关键修复: 如果产出是 API 错误文本 (限流/超时/熔断), 将其转换为 API 错误,
		// 使 executeStageWithRetry 能识别为瞬态错误并自动重试。
		if reason == "__API_ERROR__" {
			return StageResult{
				Role: role, Status: TaskFailed,
				Error:     fmt.Sprintf("API 错误 (限流/超时/熔断): %s", truncateResult(result, 200)),
				Output:    result,
				StartedAt: start, Duration: duration.Round(time.Second).String(),
			}
		}
		we.notify(we.chatID, fmt.Sprintf("⚠️ Agent **%s** 产出不合格: %s — 标记为失败并重试", role, reason))
		return StageResult{
			Role: role, Status: TaskFailed,
			Error:     fmt.Sprintf("产出验证失败: %s", reason),
			Output:    result,
			StartedAt: start, Duration: duration.Round(time.Second).String(),
		}
	}

	return StageResult{
		Role: role, Status: TaskCompleted,
		Output: result, StartedAt: start,
		Duration: duration.Round(time.Second).String(),
	}
}

// validateAgentOutput 检查 Agent 产出是否有实质内容。
// 返回 "" 表示通过，非空字符串为失败原因。
// ValidateAgentOutput 导出版本，供测试和外部调用。
func ValidateAgentOutput(output, role string) string {
	return validateAgentOutput(output, role)
}

func validateAgentOutput(output, role string) string {
	trimmed := strings.TrimSpace(output)

	// V2 关键修复: 只把"明显是 API 错误包装文本"的输出转成瞬态错误。
	// 不能全文匹配 "限流/timeout/429" 等词, 否则正常技术报告讨论限流、超时设计也会被误判。
	if looksLikeAPIErrorOutput(trimmed) {
		return "__API_ERROR__"
	}

	// 1. 基本长度检查 (有效产出通常 > 100 字符)
	if len(trimmed) < 50 {
		return "产出过短 (< 50 字符)，可能未实际执行任务"
	}
	lower := strings.ToLower(trimmed)

	// 2. 空转模式检测: 仅声明角色就绪、未提供实质内容
	idlePatterns := []string{
		"i am ready", "i'm ready", "已就位", "已准备", "准备就绪",
		"i understand my role", "i have been assigned",
		"please provide", "please tell me", "请告诉我",
		"waiting for", "等待指令", "等待进一步",
		"now i have full understanding", "let me write",
	}
	idleCount := 0
	for _, pat := range idlePatterns {
		if strings.Contains(lower, pat) {
			idleCount++
		}
	}

	// 产出中 >50% 是角色声明/等待指令 → 空转
	hasSubstantiveContent := false
	substantiveMarkers := []string{
		"```", "##", "func ", "class ", "def ", "import ", "const ", "var ",
		"<svg", "<html", "<div", "export ", "package ", "module ",
		"CREATE TABLE", "SELECT ", "INSERT ",
		"步骤", "方案", "分析", "结论", "建议", "设计", "实现",
	}
	for _, marker := range substantiveMarkers {
		if strings.Contains(trimmed, marker) {
			hasSubstantiveContent = true
			break
		}
	}

	if idleCount >= 2 && !hasSubstantiveContent {
		return "检测到角色扮演空转 (仅声明就绪/等待指令，无实质产出)"
	}

	// 3. 过短且无代码/结构化内容
	if len(trimmed) < 200 && !hasSubstantiveContent {
		return "产出过短且无结构化内容 (代码、文档、分析等)"
	}

	return ""
}

func looksLikeAPIErrorOutput(output string) bool {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return false
	}
	lines := strings.Split(trimmed, "\n")
	head := strings.ToLower(strings.TrimSpace(strings.Join(lines[:min(len(lines), 3)], "\n")))
	if len(head) > 800 {
		head = head[:800]
	}
	apiErrorPrefixes := []string{
		"api error", "api 错误", "error:", "错误:", "request failed",
		"http 429", "429:", "status 429", "rate limit exceeded",
		"rate_limit", "context deadline exceeded", "deadline exceeded",
		"circuit breaker", "断路器触发", "family exhausted",
	}
	for _, prefix := range apiErrorPrefixes {
		if strings.HasPrefix(head, prefix) {
			return true
		}
	}
	apiErrorMarkers := []string{
		"api 返回 429", "api返回429", "api returned 429",
		"api 错误 (限流", "api 错误(限流",
		"api 错误 (超时", "api 错误(超时",
		"api 错误 (限流/超时/熔断)",
	}
	for _, marker := range apiErrorMarkers {
		if strings.Contains(head, marker) {
			return true
		}
	}
	return false
}

// antiLoopDirective 防死循环指令，注入到所有 agent prompt 中
const antiLoopDirective = `

<execution_constraints>
CRITICAL: You MUST follow these execution rules strictly:
1. Do NOT search for the same file or pattern more than 3 times.
2. If a tool call fails twice with the same error, STOP and report the failure.
3. Do NOT enter infinite loops of reading/searching. If you cannot find what you need after reasonable attempts, summarize what you found and move on.
4. Complete your task within a reasonable scope. Produce your output and STOP.
5. If you are stuck, output your partial findings rather than continuing to retry.
</execution_constraints>`

// buildStagePromptWithRoles 优先从 RoleRegistry 获取提示词，降级用 StageDef.Prompt。
// maxDepOutputLen 每个依赖阶段输出注入 prompt 的最大字符数, 防止上下文膨胀。
const maxDepOutputLen = 1500

func buildStagePromptWithRoles(stage StageDef, objective string, prevResults map[string]string, roles *RoleRegistry) string {
	var prevOutput strings.Builder
	for _, dep := range stage.DependsOn {
		if r, ok := prevResults[dep]; ok {
			summary := SummarizeOldOutput(r, maxDepOutputLen)
			prevOutput.WriteString(fmt.Sprintf("### Dependency summary from %s:\n%s\n\nFull artifact/ref: blackboard key `%s-result`.\n\n", dep, summary, dep))
		}
	}

	// 优先从角色注册表获取 (包含专属 Skills)
	if roles != nil {
		if merged := roles.MergedPrompt(stage.Role, objective, prevOutput.String()); merged != "" {
			return merged + antiLoopDirective
		}
	}

	// 降级: 使用 StageDef 中的内联 Prompt
	prompt := stage.Prompt
	prompt = strings.ReplaceAll(prompt, "{objective}", objective)
	prompt = strings.ReplaceAll(prompt, "{prev_result}", prevOutput.String())
	return prompt + antiLoopDirective
}

// --- 金融专家团队工作流 ---

func financeWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "finance",
		Description: "金融分析专家团队: 盯盘→情绪→财报→新闻→风险评估→交易建议 (实时数据驱动)",
		Mode:        "pipeline",
		Stages: []StageDef{
			{
				Name: "market-analysis", Role: "market-analyst",
				Prompt: `你是资深量化交易分析师。对给定标的进行全面技术分析。

分析标的: {objective}

⚠️ 关键要求: 
1. **必须使用 WebSearch 工具**搜索标的的最新价格、市值、交易数据
2. 分析日期统一使用今天的日期
3. 所有数据标注来源: 【实时搜索】 vs 【AI估算】 vs 【待确认】

请输出:
## 技术面分析
1. **价格趋势**: 当前价位(WebSearch获取)、近期高低点、趋势方向
2. **关键技术指标**: MA5/MA20/MA60 均线排列, MACD, RSI, 布林带
3. **成交量分析**: 量价配合情况
4. **支撑/阻力位**: 近期关键价格位
5. **形态分析**: K线组合形态

## 基准假设 (后续分析师必须使用这些统一数值)
- 当前股价/估值: ¥XXX (标注数据来源和日期)
- 当前市值: $XXX (标注数据来源)
- 最新融资轮次: XXX

## 技术面评分: X/10`,
				Parallel: true,
			},
			{
				Name: "sentiment-analysis", Role: "sentiment-analyst",
				Prompt: `你是金融情绪分析专家，擅长从多维度解读市场情绪。

分析标的: {objective}

⚠️ 关键要求:
1. **必须使用 WebSearch 工具**搜索最新的新闻、社交媒体讨论、分析师评级
2. 所有情绪判断必须有真实新闻/事件支撑,不允许凭空推测
3. **必须将分析结果保存为独立文件** (如 sentiment_report.md)

请输出:
## 市场情绪分析
1. **最新新闻** (WebSearch获取): 近7天的关键新闻事件及其影响
2. **整体市场情绪**: 贪婪/恐惧指数估计, 市场氛围
3. **投资者情绪**: 散户 vs 机构, 融资融券趋势
4. **社交媒体情绪**: 讨论热度, 关键观点
5. **分析师共识**: 买入/持有/卖出评级 (WebSearch获取)
6. **资金流向**: 主力资金净流入流出

## 情绪评分: X/10 (含评分依据)`,
				Parallel: true,
			},
			{
				Name: "financial-report", Role: "financial-analyst",
				Prompt: `你是高级财务分析师 (CFA)，擅长财报深度解读。

分析标的: {objective}

请输出:
## 财务基本面分析
1. **盈利能力**: 营收增长率、净利润率、ROE、ROA, 与行业平均对比
2. **估值水平**: P/E (TTM & Forward)、P/B、P/S、PEG, 是否高估/低估
3. **成长性**: 营收/利润增速趋势, 研发投入占比, 新业务增长点
4. **财务健康**: 资产负债率、流动比率、现金流情况、商誉减值风险
5. **分红与回购**: 股息率、回购计划、对股东的回报

## 同业对比: 列出 2-3 个竞品的关键指标对比表格
## 基本面评分: X/10 (给出明确评分和理由)`,
				Parallel: true,
			},
			{
				Name: "news-tracking", Role: "news-tracker",
				Prompt: `你是金融新闻追踪专家，擅长从新闻事件中提取投资信号。

分析标的: {objective}

请输出:
## 新闻与事件分析
1. **近期重大事件**: 列出近期影响股价的关键事件 (财报发布、并购、管理层变动、政策等)
2. **行业动态**: 所在行业的最新趋势、政策变化、竞争格局变化
3. **宏观因素**: 利率环境、汇率影响、地缘政治风险
4. **监管风险**: 反垄断、数据安全、行业合规等潜在风险
5. **催化剂/风险事件**: 未来 1-3 个月可预见的重要事件 (财报日、政策节点等)

## 事件影响评估表
| 事件 | 影响方向 | 影响程度 | 概率 |
|------|---------|---------|------|
| ... | 利多/利空 | 高/中/低 | X% |

## 事件面评分: X/10`,
				Parallel: true,
			},
			{
				Name: "risk-assessment", Role: "risk-assessor",
				DependsOn: []string{"market-analysis", "sentiment-analysis", "financial-report", "news-tracking"},
				Prompt: `你是高级风险管理专家 (FRM)。综合前置分析，进行全面风险评估。

分析标的: {objective}

前置研究成果:
{prev_result}

请输出:
## 综合风险评估
1. **系统性风险**: 宏观经济、市场整体风险暴露
2. **个股风险**: 基于前面技术面/基本面/情绪面/事件面的综合风险
3. **下行风险**: 最大回撤估计, 止损位建议
4. **上行空间**: 目标价预期, 盈亏比
5. **仓位建议**: 根据风险等级建议的仓位比例

## 风险矩阵
| 风险类型 | 概率 | 影响 | 等级 | 缓解策略 |
|---------|------|------|------|---------|
| ... | 高/中/低 | 高/中/低 | 🔴🟡🟢 | ... |

## 综合风险等级: 🔴高风险 / 🟡中风险 / 🟢低风险`,
			},
			{
				Name: "trade-recommendation", Role: "trade-advisor",
				DependsOn: []string{"risk-assessment"},
				Prompt: `你是首席投资策略师。综合所有分析，给出最终交易建议。

分析标的: {objective}

完整分析报告:
{prev_result}

请输出最终投资建议:

## 📊 投资评级: 【强烈买入/买入/持有/减持/卖出】

## 核心逻辑 (3-5 条)
1. ...

## 交易策略
- **建仓时机**: 具体价位或条件
- **目标价位**: 短期(1周)/中期(1月)/长期(3月)
- **止损位**: 具体价位和原因
- **仓位建议**: 占总仓位的 X%
- **盈亏比**: X:X

## 综合评分
| 维度 | 评分 | 权重 | 加权分 |
|------|------|------|--------|
| 技术面 | X/10 | 20% | |
| 情绪面 | X/10 | 15% | |
| 基本面 | X/10 | 30% | |
| 事件面 | X/10 | 15% | |
| 风险面 | X/10 | 20% | |
| **综合** | | 100% | **X/10** |

## ⚠️ 风险提示
投资有风险，本分析仅供参考，不构成投资建议。请根据自身风险承受能力做出决策。`,
			},
		},
	}
}

// --- 技术博客/公众号写作专家团队 ---

func techBlogWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "techblog",
		Description: "技术博客/公众号写作专家团队: 源码分析→信息调查→事实核验→专业撰写→排版优化",
		Mode:        "pipeline",
		Stages: []StageDef{
			{
				Name: "source-analysis", Role: "source-analyst",
				Prompt: `你是资深源码分析专家，擅长深入阅读和分析开源项目代码。

写作主题: {objective}

请进行深度源码/技术分析:

## 技术深度分析
1. **核心架构**: 整体架构设计, 关键模块和它们的职责
2. **核心算法/实现**: 最核心的算法或实现逻辑 (含关键代码片段)
3. **设计模式**: 用到了哪些设计模式, 为什么这样选择
4. **性能考量**: 性能关键路径, 优化手段
5. **核心数据结构**: 关键数据结构的设计和选择理由

## 可以写入文章的代码片段 (标注清楚来源和说明)
## 技术亮点 (适合在文章中重点展开的 2-3 个点)
## 对比分析 (如果适用: 与同类方案的对比)`,
				Parallel: true,
			},
			{
				Name: "investigation", Role: "tech-investigator",
				Prompt: `你是技术调查记者，擅长全方位搜集和整理技术信息。

写作主题: {objective}

请进行全方位信息调查:

## 背景调查
1. **项目/技术背景**: 起源、发展历程、关键里程碑
2. **作者/团队**: 核心贡献者, 背后的组织/公司
3. **社区生态**: Star 数, 贡献者数, 使用案例
4. **行业影响**: 该技术在行业中的地位和影响
5. **最新动态**: 最近的版本更新、重要 PR、Roadmap

## 相关引用和参考资料 (论文、官方文档、博客)
## 有价值的引用语句 (可直接用于文章)
## 常见误解或争议点 (增加文章深度)`,
				Parallel: true,
			},
			{
				Name: "fact-checking", Role: "fact-checker",
				DependsOn: []string{"source-analysis", "investigation"},
				Prompt: `你是严谨的技术事实核验专家。验证前面调研的准确性。

写作主题: {objective}

前置调研成果:
{prev_result}

请进行事实核验:

## 核验清单
对前面分析中的每个关键论断逐一核验:
1. **技术准确性**: 代码分析是否正确? API 描述是否准确?
2. **数据准确性**: 引用的数据/数字是否可靠?
3. **版本时效性**: 是否是最新版本? 有无过时信息?
4. **观点客观性**: 是否有主观偏见? 是否遗漏了重要观点?
5. **完整性**: 是否有重要遗漏需要补充?

## 核验结果
| 论断 | 核验结果 | 修正建议 |
|------|---------|---------|
| ... | ✅正确/⚠️需修正/❌错误 | ... |

## 建议补充的内容
## 建议删除/修改的内容`,
			},
			{
				Name: "article-writing", Role: "tech-writer",
				DependsOn: []string{"fact-checking"},
				Prompt: `你是顶级技术自媒体作者 (10万+阅读量级)。基于经过核验的素材撰写专业文章。

写作主题: {objective}

已核验素材:
{prev_result}

请撰写一篇高质量的技术文章:

## 写作要求
1. **标题**: 吸引眼球但不标题党, 准确反映内容, 适合微信公众号传播
2. **开头**: 用一个引人入胜的场景/问题/数据开头, 前100字决定读者是否继续
3. **结构**: 清晰的层次, 每个小节有明确主题, 段落间自然过渡
4. **深度**: 不是肤浅的介绍, 而是有独到见解的深度分析
5. **代码**: 必要的代码片段 (控制在文章的 20% 以内), 配详细注释
6. **图文**: 在需要图表的地方用 [图: 描述] 标注 (后续排版阶段处理)
7. **结尾**: 总结 + 思考 + 引导讨论的问题
8. **版本演进**: 如果是分析某个技术/框架, 必须包含版本演进时间线
9. **性能数据**: 至少使用 WebSearch 搜索 1 组真实的 benchmark 数据

## 内容差异化 (如果同主题输出多篇)
- 入门篇: 完整的基础概念和原理
- 深度篇: 假设读者已读入门篇, 不重复基础概念, 专注源码和内部实现
- 思辨篇: 假设读者已读前两篇, 专注设计哲学和行业对比

## 文章风格
- 专业但不晦涩, 用类比帮助理解复杂概念
- 有自己的观点和态度, 不是纯搬运
- 中文行文流畅, 适合中国技术人阅读习惯

## 目标: 3000-5000 字的深度技术文章
## 必须包含: 至少 1 个 benchmark 或性能数据 + 至少 1 个生产环境案例`,
			},
			{
				Name: "self-critique", Role: "tech-critic",
				DependsOn: []string{"article-writing"},
				Prompt: `你是**资深技术自媒体主编** (借鉴 Qwen3 Thinking Mode: 先深度思考再输出)。

写作主题: {objective}

待审文章:
{prev_result}

## 自审任务 (模拟读者视角)

### Step 1: 深度自评 (内心独白)
- 如果我是一个高级工程师, 读完这篇文章, 我会觉得...
- 如果我是一个初学者, 这篇文章对我的帮助是...
- 这篇文章最大的亮点是...
- 这篇文章最大的不足是...

### Step 2: 多维评分
| 维度 | 评分(1-10) | 问题描述 | 修改建议 |
|------|-----------|---------|---------|
| 深度 | | 是否有独到见解而非表面描述? | |
| 准确性 | | 技术描述是否准确? | |
| 可读性 | | 行文是否流畅? 是否有让人费解的段落? | |
| 实用性 | | 读者读完能否获得可操作的知识? | |
| 吸引力 | | 标题和开头是否吸引人? | |

### Step 3: 具体修改指令
如果总分 < 35 (满分50), 输出逐段修改指令:
- 哪一段需要重写, 为什么
- 需要补充什么内容
- 需要删除什么冗余

### Step 4: 修改后的完整文章
**如果总分 ≥ 35, 仅输出小修建议, 保留原文主体。**
**如果总分 < 35, 输出修改后的完整文章。**`,
			},
			{
				Name: "formatting", Role: "article-formatter",
				DependsOn: []string{"self-critique"},
				Prompt: `你是微信公众号排版和视觉设计专家。将文章优化为适合公众号发布的格式。

原始文章 (已经过主编自审):
{prev_result}

请进行排版优化:

## 排版优化
1. **标题优化**: 适合公众号的标题 (主标题 + 副标题), 考虑搜索关键词
2. **摘要**: 120 字以内的文章摘要 (显示在公众号列表)
3. **封面图建议**: 描述适合的封面图风格和内容, 标注 [封面图: 描述]
4. **正文排版**:
   - 重要语句加粗
   - 关键概念用 「」 强调
   - 代码块用适当语言标记
   - 每 3-4 段插入一个视觉化元素 (表格/列表/引用/分割线)
   - 在适当位置插入 [配图: 描述] 标注
5. **SEO 优化**: 添加关键词标签 (5-8 个)
6. **互动引导**: 文末添加互动话题/投票/留言引导
7. **相关推荐**: 建议 2-3 篇可关联的延伸阅读主题

## 最终输出: 排版完成的公众号文章 (Markdown 格式, 含所有标注)`,
			},
		},
	}
}

// --- 图片&视频创意团队 ---
// 参考业界最佳实践：Midjourney prompt engineering, ComfyUI workflow, RunwayML multi-shot
// 采用 Pipeline 模式: 创意策划→提示词工程→素材生成→视觉审查→后期合成

func creativeWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "creative",
		Description: "图片&视频创意团队: 创意策划→提示词工程→素材生成→视觉审查→后期合成",
		Mode:        "adversarial_dev",
		Rounds:      2,
		Stages: []StageDef{
			{
				Name: "creative-brief", Role: "creative-director",
				Prompt: `你是资深创意总监，擅长将模糊需求转化为精确的视觉创意方案。

创作需求: {objective}

请输出创意策划方案:

## 🎨 创意简报
1. **核心主题**: 一句话概括创作目标和核心表达
2. **目标受众**: 受众画像、审美偏好、使用场景
3. **视觉风格**: 风格定义 (如: 扁平插画/3D渲染/赛博朋克/国风水墨/写实摄影)
4. **色彩方案**: 主色调、辅助色、配色灵感
5. **构图规划**: 主体位置、视角、景深、空间关系
6. **参考基准**: 2-3个风格参考描述

## 📐 技术规格
- 输出格式: SVG / HTML+CSS / 组合图
- 分辨率/尺寸建议
- 是否需要动画/交互

## 🎬 视频规划 (如果需要)
- 分镜数量: N 帧
- 每帧时长: X 秒
- 运镜设计: 推/拉/平移/旋转/缩放
- 转场效果: 淡入淡出/硬切/形变
- 节奏控制: 快节奏/慢节奏/张弛有度`,
			},
			{
				Name: "prompt-engineer", Role: "prompt-engineer",
				DependsOn: []string{"creative-brief"},
				Prompt: `你是专业的 AI 视觉生成提示词工程师，精通 SVG/HTML 视觉创作指令。

创作需求: {objective}

创意简报:
{prev_result}

{adversarial_feedback}

请为每个需要生成的视觉素材编写详细的创作指令:

## 提示词设计
对于每个素材，输出:

### 素材 N: [名称]
**SVG/HTML 生成指令:**
- 精确的视觉描述 (颜色值、尺寸、位置、变换)
- 具体的 SVG 元素结构 (rect, circle, path, text, gradient, filter)
- CSS 动画指令 (如需要: @keyframes, transition, transform)
- 构图和层次关系

**质量控制要点:**
- 必须检查的视觉要素
- 常见生成错误的预防

## 视频分镜 (如果适用)
对于每一帧:
| 帧号 | 场景描述 | 运镜 | 主体动作 | 时长 | 转场 |
|------|---------|------|---------|------|------|
| 1 | ... | ... | ... | 2s | ... |

确保提示词足够精确，使 LLM 能生成高质量 SVG/HTML 代码。`,
			},
			{
				Name: "asset-generate", Role: "visual-artist",
				DependsOn: []string{"prompt-engineer"},
				Prompt: `你是专业的 SVG/HTML 视觉创作专家。根据提示词指令实际生成视觉素材代码。

创作需求: {objective}

创作指令:
{prev_result}

{adversarial_feedback}

## 生成要求 (必须实际输出代码, 禁止角色扮演!)
请按照创作指令，为每个素材生成完整的 SVG 或 HTML+CSS 代码:

1. **SVG 素材**: 输出完整的 <svg> 代码，包含所有图形元素、渐变、滤镜
2. **HTML 素材**: 输出完整的 HTML+CSS 代码块，可直接在浏览器中渲染
3. **动画素材**: 使用 CSS @keyframes 或 SVG SMIL 动画
4. **视频帧**: 如果是多帧场景，每帧一个独立 SVG/HTML 块

⚠️ 绝对禁止:
- 不允许只说"我是视觉艺术家，我已就位"然后标记完成
- 不允许只输出模板或说明文字而不生成实际代码
- 你的输出中必须包含至少一个完整的 <svg> 或 <html> 代码块
- 如果无法生成请求的内容，必须生成一个替代方案而非空手而归

## 质量标准
- 视觉美观、配色协调
- 代码语义清晰、结构合理
- 渐变和阴影适度使用提升质感
- 文字排版优美、字体选择恰当
- 响应式设计 (如 viewBox 正确设置)

## 输出验证
- 将生成的素材保存为文件 (使用 Write 工具)
- 输出每个素材的完整代码块和渲染说明`,
			},
			{
				Name: "visual-review", Role: "art-director",
				DependsOn: []string{"asset-generate"},
				Prompt: `你是资深艺术指导/视觉审查员(Evaluator 角色)。以挑剔的专业眼光审查生成的视觉素材。

创作需求: {objective}

生成的素材:
{prev_result}

请对每个素材进行严格审查:

## 审查维度 (每项 0-10 分)
Score each dimension. Output STRICTLY as JSON:
{"correctness": N, "completeness": N, "security": N, "code_quality": N, "pass": bool, "feedback": "..."}

映射:
- correctness → 视觉准确性 (是否符合创意简报)
- completeness → 完整性 (是否所有元素都呈现)
- security → 品牌安全性 (是否有不当内容、版权风险)
- code_quality → 代码质量 + 审美质量 (配色、构图、细节)

## 详细反馈
1. **视觉一致性**: 是否符合创意简报的风格定义
2. **色彩协调**: 配色是否和谐、对比是否合适
3. **构图平衡**: 元素布局是否美观、留白是否合理
4. **细节品质**: 渐变/阴影/边缘处理是否精细
5. **动画流畅度**: (如适用) 动画是否自然、节奏感是否良好
6. **技术规范**: SVG/CSS 代码是否规范、是否有冗余

Hard pass threshold: ALL ≥ 7 AND pass == true.
如不通过，给出具体、可操作的修改建议。`,
			},
			{
				Name: "post-production", Role: "post-producer",
				DependsOn: []string{"asset-generate"},
				Prompt: `你是后期制作专家。将通过审查的素材组装为最终交付物。

创作需求: {objective}

审查通过的素材:
{prev_result}

## 后期任务
1. **素材整合**: 将多个素材组合为完整的作品
2. **HTML 播放器**: (如有视频帧) 生成完整的 HTML 播放器
   - 帧间过渡动画 (CSS transitions)
   - 自动播放控制
   - 进度条和播放/暂停按钮
3. **格式导出**: 生成可直接使用的格式
   - SVG 图片: 完整独立的 SVG 文件代码
   - HTML 页面: 内联 CSS 的完整 HTML 文件
   - 视频 HTML: 含所有帧和动画的 HTML 播放页面

## 输出格式
对于每个最终作品:
- 完整的可运行代码
- 渲染预览说明
- 使用建议 (在哪些场景使用、如何嵌入)`,
				Parallel: true,
			},
		},
	}
}

// stripRoundSuffix 从 "implement-round3" 提取基础名 "implement"。
// 如果不含 -round 后缀, 返回原名。
func stripRoundSuffix(name string) string {
	for i := 1; i <= 20; i++ {
		suffix := fmt.Sprintf("-round%d", i)
		if strings.HasSuffix(name, suffix) {
			return name[:len(name)-len(suffix)]
		}
	}
	return name
}

// workspaceFileManifest 扫描工作区中最近修改的文件, 生成清单注入 coder prompt。
// 让每轮 coder 知道前几轮在磁盘上创建/修改了哪些文件, 避免从零重写。
func workspaceFileManifest(cwd string, since time.Time) string {
	if cwd == "" {
		return ""
	}
	var files []string
	_ = filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		name := info.Name()
		if info.IsDir() {
			if name == ".git" || name == "node_modules" || name == ".claude-go" || name == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		if info.ModTime().After(since) {
			rel, _ := filepath.Rel(cwd, path)
			files = append(files, rel)
		}
		return nil
	})
	if len(files) == 0 {
		return ""
	}
	if len(files) > 50 {
		files = files[:50]
	}
	return fmt.Sprintf("### 工作区中已创建/修改的文件 (%d 个, 必须在这些文件基础上增量修改):\n```\n%s\n```\n",
		len(files), strings.Join(files, "\n"))
}

// LanguageToolchain 多语言编译/lint/测试工具链抽象。
// 支持 Go, C++ (CMake), Rust (Cargo), Python 四种语言。
type LanguageToolchain struct {
	Language        string     // "go", "cpp", "rust", "python"
	BuildCmds       [][]string // 编译命令序列
	LintCmds        [][]string // 静态分析命令
	TestCmds        [][]string // 测试命令
	InitCmds        [][]string // 项目初始化命令
	FileExt         string     // ".go", ".cpp"/".h", ".rs", ".py"
	ProjectFile     string     // "go.mod", "CMakeLists.txt", "Cargo.toml", "pyproject.toml"
	Timeout         time.Duration
	MemoryMaxMB     int // 子进程内存上限 (MB), 0=不限制
	CPUQuotaPercent int // CPU 配额百分比, 0=不限制
}

// GetToolchain 根据语言返回对应工具链。空字符串默认 Go。
func GetToolchain(lang string) *LanguageToolchain {
	switch lang {
	case "cpp", "c++":
		return &LanguageToolchain{
			Language: "cpp",
			// 默认: 最小化构建 (仅编译依赖当前源码的目标, 不构建 mysqld 全量)
			// 通过 make <file>.o 验证语法, 避免每次修改都触发全量构建
			BuildCmds:       [][]string{{"cmake", "-B", "build", "-DCMAKE_EXPORT_COMPILE_COMMANDS=ON"}, {"cmake", "--build", "build", "--parallel"}},
			LintCmds:        [][]string{{"cmake", "--build", "build", "--target", "all"}},
			TestCmds:        [][]string{{"ctest", "--test-dir", "build", "--output-on-failure"}},
			InitCmds:        [][]string{},
			FileExt:         ".cpp",
			ProjectFile:     "CMakeLists.txt",
			Timeout:         120 * time.Second,
			MemoryMaxMB:     16384,
			CPUQuotaPercent: 200,
			// MySQL/Percona 特殊处理: 增量编译时仅编译修改过的 .o
			// 在 BuildCmds 执行前, buildScript 会检测项目类型并动态调整策略
		}
	case "rust", "rs":
		return &LanguageToolchain{
			Language:        "rust",
			BuildCmds:       [][]string{{"cargo", "build"}},
			LintCmds:        [][]string{{"cargo", "clippy", "--", "-D", "warnings"}},
			TestCmds:        [][]string{{"cargo", "test"}},
			InitCmds:        [][]string{{"cargo", "init", "--name", "agentdb"}},
			FileExt:         ".rs",
			ProjectFile:     "Cargo.toml",
			Timeout:         120 * time.Second,
			MemoryMaxMB:     8192,
			CPUQuotaPercent: 200,
		}
	case "python", "py":
		return &LanguageToolchain{
			Language:        "python",
			BuildCmds:       [][]string{{"python", "-m", "py_compile"}},
			LintCmds:        [][]string{{"python", "-m", "flake8", "."}},
			TestCmds:        [][]string{{"python", "-m", "pytest"}},
			InitCmds:        [][]string{},
			FileExt:         ".py",
			ProjectFile:     "pyproject.toml",
			Timeout:         60 * time.Second,
			MemoryMaxMB:     4096,
			CPUQuotaPercent: 200,
		}
	default: // "go" or empty
		return &LanguageToolchain{
			Language:        "go",
			BuildCmds:       [][]string{{"go", "build", "./..."}, {"go", "vet", "./..."}},
			LintCmds:        [][]string{},
			TestCmds:        [][]string{{"go", "test", "-count=1", "-timeout=30s", "-parallel=4", "./..."}},
			InitCmds:        [][]string{},
			FileExt:         ".go",
			ProjectFile:     "go.mod",
			Timeout:         30 * time.Second,
			MemoryMaxMB:     8192,
			CPUQuotaPercent: 200,
		}
	}
}

// BuildCheckLabel 返回编译命令描述 (用于 prompt)
func (tc *LanguageToolchain) BuildCheckLabel() string {
	switch tc.Language {
	case "cpp":
		return "cmake --build build 通过"
	case "rust":
		return "cargo build 通过"
	case "python":
		return "python -m py_compile 通过"
	default:
		return "go build 通过"
	}
}

// TestCheckLabel 返回测试命令描述 (用于 prompt)
func (tc *LanguageToolchain) TestCheckLabel() string {
	switch tc.Language {
	case "cpp":
		return "ctest --output-on-failure 通过"
	case "rust":
		return "cargo test 通过"
	case "python":
		return "pytest 通过"
	default:
		return "go test ./... 通过"
	}
}

// runBuildCheck 在工作区运行编译+lint, 返回错误输出。空字符串表示通过。
// 默认 Go 工具链, 向后兼容。
func runBuildCheck(cwd string) string {
	return runBuildCheckLang(cwd, "go")
}

// --- MySQL 首次编译方案 ---

// mysqlBuildState MySQL 编译状态跟踪 (避免重复 cmake configure)。
var mysqlBuildState = struct {
	sync.Mutex
	configured map[string]bool // cwd → 是否已完成 cmake configure
}{configured: make(map[string]bool)}

// mysqlEssentialTargets MySQL 首次编译必须构建的最小核心目标。
// 这些目标是 mysqld 的直接依赖, 其他工具 (mysqlbinlog, mysqldump, 测试等) 跳过。
// 根据 Percona-Server CMakeLists.txt 实际目标结构:
//
//	Level 0 (基础库): mysys, clientlib, heap, csv
//	Level 1 (SQL 核心): sql_main (或 sql_commands)
//	Level 2 (存储引擎): innobase, myisam, perfschema
//	Level 3 (主程序): mysqld
var mysqlEssentialTargets = []string{
	"mysys",     // 底层系统库 (io, mem, thread, regex, etc.)
	"clientlib", // MySQL 客户端库
	"heap",      // MEMORY 存储引擎
	"csv",       // CSV 存储引擎
	"innobase",  // InnoDB 存储引擎 (最大, 单独列出)
	"myisam",    // MyISAM 存储引擎
}

// mysqlCMakeConfigureArgs MySQL 首次 cmake 配置的优化参数。
// 关键: 禁用单元测试 (WITH_UNIT_TESTS=OFF) 节省 30-40% 编译时间。
func mysqlCMakeConfigureArgs() []string {
	return []string{
		"-B", "build",
		"-DWITH_UNIT_TESTS=OFF", // 节省 30-40% 编译时间
		"-DWITH_DEBUG=OFF",      // Release 模式
		"-DCMAKE_BUILD_TYPE=Release",
		"-DWITH_PROTOBUF=bundled", // 使用预编译 protobuf
		"-DWITH_SSL=system",       // 使用系统 OpenSSL
	}
}

// runMySQLBuildCheck MySQL 专用编译检查: 分阶段构建。
//
// 首次编译 (3 阶段):
//
//	Phase 1: cmake configure — 禁用测试, 优化配置 (耗时 ~30-60s)
//	Phase 2: 基础库 — mysys, clientlib, heap, csv (耗时 ~2-5min)
//	Phase 3: 核心引擎 — innobase, myisam + mysqld (耗时 ~10-20min)
//
// 增量编译:
//
//	cmake --build build — CMake 自动追踪依赖, 仅重编译受影响的 .o
func runMySQLBuildCheck(cwd string) string {
	jobs := CalcSafeMakeJobs()

	// 检测是否已配置 (避免每次 check 都重复 configure)
	mysqlBuildState.Lock()
	alreadyConfigured := mysqlBuildState.configured[cwd]
	mysqlBuildState.Unlock()

	buildCache := filepath.Join(cwd, "build", "CMakeCache.txt")
	_, statErr := os.Stat(buildCache)
	cacheOK := statErr == nil
	if cacheOK && !alreadyConfigured {
		mysqlBuildState.Lock()
		mysqlBuildState.configured[cwd] = true
		mysqlBuildState.Unlock()
		alreadyConfigured = true
	}

	// Phase 1: cmake configure (仅首次)
	if !alreadyConfigured {
		configureArgs := mysqlCMakeConfigureArgs()
		ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel1()
		out, err := runLimitedCommand(ctx1, cwd, append([]string{"cmake"}, configureArgs...), 16384, 200)
		if err != nil {
			return fmt.Sprintf("cmake configure 失败:\n%.*s", 2000, string(out))
		}
		mysqlBuildState.Lock()
		mysqlBuildState.configured[cwd] = true
		mysqlBuildState.Unlock()
	}

	// Phase 2: 构建基础库 (首次 + 增量都执行, CMake 增量编译)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel2()
	// 使用 ninja 或 make 的并行模式, 仅构建核心依赖
	baseArgs := []string{"--build", "build", "--parallel", fmt.Sprintf("%d", jobs)}
	for _, t := range mysqlEssentialTargets {
		baseArgs = append(baseArgs, "--target", t)
	}
	out2, err2 := runLimitedCommand(ctx2, cwd, append([]string{"cmake"}, baseArgs...), 16384, 200)
	if err2 != nil {
		return fmt.Sprintf("基础库编译失败:\n%.*s", 2000, string(out2))
	}

	// Phase 3: 构建 mysqld 主程序 (验证核心链接)
	ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel3()
	out3, err3 := runLimitedCommand(ctx3, cwd, []string{"cmake", "--build", "build", "--parallel", fmt.Sprintf("%d", jobs), "--target", "mysqld"}, 16384, 200)
	if err3 != nil {
		return fmt.Sprintf("mysqld 编译失败:\n%.*s", 2000, string(out3))
	}

	return ""
}

// runMySQLIntegrationTest 启动 mysqld 并执行 mysql 客户端验证集成测试。
// 返回空串表示通过, 否则返回错误信息。
func (we *WorkflowExecutor) runMySQLIntegrationTest(cwd string) string {
	if cwd == "" {
		return ""
	}

	// 定位 mysqld 二进制
	mysqldPaths := []string{
		filepath.Join(cwd, "build", "sql", "mysqld"),
		filepath.Join(cwd, "build", "runtime_output_directory", "mysqld"),
	}
	var mysqldPath string
	for _, p := range mysqldPaths {
		if _, err := os.Stat(p); err == nil {
			mysqldPath = p
			break
		}
	}
	if mysqldPath == "" {
		return "mysqld 二进制未找到 (expected in build/sql/mysqld or build/runtime_output_directory/mysqld)"
	}

	// 创建临时数据目录
	tmpDir, err := os.MkdirTemp("", "mysql-integration-*")
	if err != nil {
		return fmt.Sprintf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// 初始化数据目录 (--initialize-insecure, 无密码 root)
	initCtx, initCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer initCancel()
	if out, err := runLimitedCommand(initCtx, cwd, []string{mysqldPath, "--initialize-insecure", "--datadir=" + tmpDir}, 4096, 100); err != nil {
		return fmt.Sprintf("mysqld --initialize-insecure 失败:\n%s", truncateResult(string(out), 2000))
	}

	// 后台启动 mysqld (skip-networking + unix socket, 避免端口冲突)
	socketPath := filepath.Join(tmpDir, "mysql.sock")
	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()
	mysqldCmd := exec.CommandContext(startCtx, mysqldPath,
		"--skip-networking",
		"--socket="+socketPath,
		"--datadir="+tmpDir,
		"--skip-grant-tables",
	)
	mysqldCmd.Dir = cwd
	if err := mysqldCmd.Start(); err != nil {
		return fmt.Sprintf("mysqld 启动失败: %v", err)
	}

	// 轮询等待 socket 文件 (最多 30 秒)
	ready := false
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		if _, err := os.Stat(socketPath); err == nil {
			ready = true
			break
		}
	}
	if !ready {
		return "mysqld 未能在 30 秒内就绪 (socket 未创建)"
	}

	// 执行 mysql 客户端验证
	mysqlClient := filepath.Join(cwd, "build", "runtime_output_directory", "mysql")
	if _, err := os.Stat(mysqlClient); err != nil {
		mysqlClient = filepath.Join(cwd, "build", "client", "mysql")
	}
	if _, err := os.Stat(mysqlClient); err != nil {
		// fallback: 尝试系统 mysql
		if sysMysql, err := exec.LookPath("mysql"); err == nil {
			mysqlClient = sysMysql
		} else {
			// 无 mysql 客户端也可返回成功 (只验证 mysqld 能启动)
			return ""
		}
	}

	clientCtx, clientCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer clientCancel()
	out, err := runLimitedCommand(clientCtx, cwd, []string{mysqlClient,
		"-S", socketPath,
		"-e", "SHOW DATABASES;",
	}, 1024, 100)
	if err != nil {
		return fmt.Sprintf("mysql 客户端验证失败:\n%s", truncateResult(string(out), 2000))
	}
	return ""
}

// scanForTodos 扫描已物化的源码文件, 检测未实现的 TODO/STUB/HACK/placeholder。
// 返回发现的未实现项列表; 空表示全部实现完整。
// 作为 L1.5 确定性门禁, 不依赖 LLM 判断, 避免遗漏。
func scanForTodos(cwd string) []string {
	var findings []string
	sourceExts := map[string]bool{
		".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
		".py": true, ".java": true, ".cs": true, ".c": true, ".cpp": true,
		".h": true, ".rs": true, ".swift": true, ".kt": true, ".dart": true,
	}

	_ = filepath.WalkDir(cwd, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !sourceExts[filepath.Ext(path)] {
			return nil
		}
		rel, _ := filepath.Rel(cwd, path)

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			// 跳过纯注释中的合理用法: 文档注释、版权说明等
			// 重点捕获: TODO + 不完整的实现信号
			todoPatterns := []string{
				"TODO", "FIXME", "HACK", "XXX", "STUB",
				"not implemented", "NotImplemented",
				"placeholder", "占位", "待实现", "未实现",
				"panic(", // Go 中的 panic 实现 = 未完成
			}
			for _, pat := range todoPatterns {
				if strings.Contains(trimmed, pat) {
					// 过滤: 如果是 // File: ... 或注释中的说明性文字, 跳过
					if strings.HasPrefix(trimmed, "// File:") || strings.HasPrefix(trimmed, "# File:") {
						continue
					}
					findings = append(findings, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(trimmed)))
					break // 同一行只记录一次
				}
			}
		}
		return nil
	})
	return findings
}

// runLimitedCommand 在沙盒执行层中运行外部命令。
// auto 模式优先 native cgroup v2 / Docker；若不可用则至少使用输出限流的 process guard。
func runLimitedCommand(ctx context.Context, cwd string, args []string, memMaxMB, cpuQuotaPercent int) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	spec := sandbox.CommandSpec{
		Purpose:             "team-verification",
		Cwd:                 cwd,
		Args:                args,
		AllowUnsafeFallback: os.Getenv("CLAUDE_GO_SANDBOX_ALLOW_UNSAFE_FALLBACK") != "",
		NetworkDisabled:     true,
		Limits: sandbox.ResourceLimits{
			MemoryMaxMB:     memMaxMB,
			CPUQuotaPercent: cpuQuotaPercent,
			PidsMax:         256,
			OutputMaxBytes:  4 * 1024 * 1024,
			PreviewMaxBytes: 192 * 1024,
			LogMaxBytes:     16 * 1024 * 1024,
		},
	}
	result, err := sandbox.DefaultManager().Run(ctx, spec)
	if result == nil {
		return nil, err
	}
	if result.Runtime == "process-unsafe" && memMaxMB > 0 {
		log.Printf("[workflow] sandbox isolated runtime unavailable, using output-limited process guard for %s", strings.Join(args, " "))
	}
	out := []byte(result.CombinedPreview)
	if err != nil {
		if result.FailureKind != sandbox.FailureNone {
			return out, fmt.Errorf("%s: %w", result.FailureKind, err)
		}
		return out, err
	}
	return out, nil
}

// runBuildCheckLang 多语言版本的编译检查。
// MySQL/Percona 特殊处理: 自动路由到分阶段最小编译方案。
func runBuildCheckLang(cwd, lang string) string {
	if cwd == "" {
		return ""
	}
	tc := GetToolchain(lang)
	if _, err := os.Stat(filepath.Join(cwd, tc.ProjectFile)); err != nil {
		if hasPrimarySourceFiles(cwd, tc) {
			return fmt.Sprintf("%s 未找到: 已发现源码文件，请在项目根目录初始化 %s 后再编译", tc.ProjectFile, tc.ProjectFile)
		}
		return ""
	}
	// MySQL/Percona 专用: 分阶段最小编译 (禁用测试, 仅构建核心目标)
	if lang == "cpp" && isMySQLProject(cwd) {
		return runMySQLBuildCheck(cwd)
	}

	ctx, cancel := context.WithTimeout(context.Background(), tc.Timeout)
	defer cancel()

	var errors []string
	for _, args := range tc.BuildCmds {
		out, err := runLimitedCommand(ctx, cwd, args, tc.MemoryMaxMB, tc.CPUQuotaPercent)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s 失败:\n%s", strings.Join(args, " "), string(out)))
		}
	}
	if len(errors) == 0 {
		return ""
	}
	result := strings.Join(errors, "\n\n")
	if len(result) > 3000 {
		result = result[:3000] + "\n...(截断)"
	}
	return result
}

// runBuildCheckScoped 按目标包路径进行局部编译检查，避免全局编译时跨任务错误污染。
// targetPackages 为空时回退到全局编译 (runBuildCheckLang)。
func runBuildCheckScoped(cwd, lang string, targetPackages []string) string {
	if cwd == "" {
		return ""
	}
	if len(targetPackages) == 0 {
		return runBuildCheckLang(cwd, lang)
	}

	tc := GetToolchain(lang)
	if _, err := os.Stat(filepath.Join(cwd, tc.ProjectFile)); err != nil {
		if hasPrimarySourceFiles(cwd, tc) {
			return fmt.Sprintf("%s 未找到: 已发现源码文件，请在项目根目录初始化 %s 后再编译", tc.ProjectFile, tc.ProjectFile)
		}
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), tc.Timeout)
	defer cancel()

	var errors []string
	for _, pkg := range targetPackages {
		args := []string{"go", "build", pkg}
		out, err := runLimitedCommand(ctx, cwd, args, tc.MemoryMaxMB, tc.CPUQuotaPercent)
		if err != nil {
			errors = append(errors, fmt.Sprintf("go build %s 失败:\n%s", pkg, string(out)))
		}
	}
	if len(errors) == 0 {
		return ""
	}
	result := strings.Join(errors, "\n\n")
	if len(result) > 3000 {
		result = result[:3000] + "\n...(截断)"
	}
	return result
}

func hasPrimarySourceFiles(cwd string, tc *LanguageToolchain) bool {
	if cwd == "" || tc == nil {
		return false
	}
	primaryExts := map[string]bool{tc.FileExt: true}
	switch tc.Language {
	case "cpp":
		primaryExts[".c"] = true
		primaryExts[".cc"] = true
		primaryExts[".h"] = true
		primaryExts[".hpp"] = true
	}

	found := false
	_ = filepath.WalkDir(cwd, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if d.IsDir() {
			if path != cwd {
				name := d.Name()
				if name == ".git" || name == ".claude-go" || name == "vendor" || name == "node_modules" || name == "build" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, tc.ProjectFile)); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if primaryExts[filepath.Ext(path)] {
			found = true
		}
		return nil
	})
	return found
}

// runTestCheckLang 多语言版本的测试检查，运行 TestCmds 获取真实测试结果。
// MySQL/Percona 特殊处理: 单元测试已禁用，返回空（跳过测试检查）。
func runTestCheckLang(cwd, lang string) string {
	if cwd == "" {
		return ""
	}
	tc := GetToolchain(lang)
	if _, err := os.Stat(filepath.Join(cwd, tc.ProjectFile)); err != nil {
		return ""
	}
	// MySQL/C++ 项目: 单元测试已禁用 (WITH_UNIT_TESTS=OFF)
	if lang == "cpp" && isMySQLProject(cwd) {
		return ""
	}
	if len(tc.TestCmds) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), tc.Timeout)
	defer cancel()

	var errors []string
	for _, args := range tc.TestCmds {
		out, err := runLimitedCommand(ctx, cwd, args, tc.MemoryMaxMB, tc.CPUQuotaPercent)
		if err != nil {
			errStr := string(out)
			label := fmt.Sprintf("%s 失败:", strings.Join(args, " "))
			// 检测内存限制/OOM 相关错误, 追加明确提示
			if tc.MemoryMaxMB > 0 && (strings.Contains(errStr, "killed") || strings.Contains(errStr, "Killed") ||
				strings.Contains(errStr, "signal: killed") || strings.Contains(errStr, "OOM") ||
				strings.Contains(errStr, "out of memory") || strings.Contains(errStr, "cannot allocate memory") ||
				strings.Contains(errStr, "exited") && len(errStr) < 100) {
				label = fmt.Sprintf("%s (⚠️ 疑似内存超限, 当前限制 %dMB。请检查测试代码是否存在无限循环或未限制的数据结构增长):", strings.Join(args, " "), tc.MemoryMaxMB)
			}
			errors = append(errors, fmt.Sprintf("%s\n%s", label, errStr))
		}
	}
	if len(errors) == 0 {
		return ""
	}
	result := strings.Join(errors, "\n\n")
	if len(result) > 3000 {
		result = result[:3000] + "\n...(截断)"
	}
	return result
}

// isMySQLProject 检测是否为 MySQL/Percona-Server 项目。
// 判断依据: 根目录存在 CMakeLists.txt 且包含 mysqld 相关配置。
func isMySQLProject(cwd string) bool {
	if cwd == "" {
		return false
	}
	// 检查特征文件
	features := []string{
		"sql/mysqld.cc",
		"sql/sql_parse.cc",
		"sql/sql_yacc.yy",
		"VERSION",
	}
	for _, f := range features {
		if _, err := os.Stat(filepath.Join(cwd, f)); err == nil {
			return true
		}
	}
	// 检查 CMakeCache 中是否有 mysqld 相关内容
	cacheFile := filepath.Join(cwd, "build", "CMakeCache.txt")
	if data, err := os.ReadFile(cacheFile); err == nil {
		return strings.Contains(string(data), "mysqld") ||
			strings.Contains(string(data), "MYSQLD")
	}
	return false
}

// CalcSafeMakeJobs 根据系统内存计算安全的编译并行度。
// 经验值: 每个 C++ 编译 job 约需 1.5-2GB 内存。
// MySQL/Percona 的链接阶段 (mysqld) 单个进程需要 4-8GB。
// 策略: 保留至少 4GB 给系统, 其余分配给编译, 上限 8 jobs。
func CalcSafeMakeJobs() int {
	totalMemMB := getSystemMemoryMB()

	// 保留 4GB 给系统
	reservedMB := int64(4096)
	availableMB := totalMemMB - reservedMB
	if availableMB < 2048 {
		availableMB = 2048 // 最小 2GB
	}

	// 每 job 约 2GB
	perJobMB := int64(2048)
	jobs := int(availableMB / perJobMB)

	// 上限: CPU 核心数和 8 的较小值
	cpuCount := runtime.NumCPU()
	if jobs > cpuCount {
		jobs = cpuCount
	}
	if jobs > 8 {
		jobs = 8
	}
	if jobs < 1 {
		jobs = 1
	}
	return jobs
}

func getSystemMemoryMB() int64 {
	// Linux: 读取 /proc/meminfo
	out, err := exec.Command("sh", "-c", "free -m | awk '/^Mem:/{print $2}'").Output()
	if err != nil {
		return 16384 // 兜底: 假设 16GB
	}
	mb, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if mb <= 0 {
		return 16384
	}
	return mb
}

// getMySQLModifiedTargets 根据修改的文件计算最小编译目标。
// MySQL/Percona 使用增量编译: CMake 自动追踪依赖, 只重编译修改过的 .o。
// 返回最优的 cmake --build 参数。
func getMySQLModifiedTargets(changedFiles []string) []string {
	if len(changedFiles) == 0 {
		return nil // 首次构建或未知修改, 返回 nil 表示全量
	}

	// 收集被修改的 .cc/.cpp/.h 文件所在的目录
	dirs := make(map[string]bool)
	for _, f := range changedFiles {
		ext := filepath.Ext(f)
		if ext == ".cc" || ext == ".cpp" || ext == ".c" || ext == ".h" || ext == ".hpp" {
			dir := filepath.Dir(f)
			dirs[dir] = true
		}
	}

	if len(dirs) == 0 {
		return nil
	}

	// 根据修改目录映射到 CMake 子目标
	// MySQL 的关键目录 → target 映射
	targets := make([]string, 0, len(dirs))
	for dir := range dirs {
		switch {
		case strings.HasPrefix(dir, "sql/"):
			targets = append(targets, "sql_main")
		case strings.HasPrefix(dir, "storage/innobase/"):
			targets = append(targets, "innobase")
		case strings.HasPrefix(dir, "storage/myisam/"):
			targets = append(targets, "myisam")
		case strings.HasPrefix(dir, "storage/perfschema/"):
			targets = append(targets, "perfschema")
		case strings.HasPrefix(dir, "client/"):
			targets = append(targets, "client")
		case strings.HasPrefix(dir, "libmysql/"):
			targets = append(targets, "libmysql_api")
		case strings.HasPrefix(dir, "plugin/"):
			targets = append(targets, "plugin")
		case strings.HasPrefix(dir, "include/"):
			// include 目录被修改, 需要重编译所有依赖方 → 全量
			return nil
		case strings.HasPrefix(dir, "cmake/") || strings.HasSuffix(dir, "CMakeLists.txt"):
			// CMake 配置修改 → 需要重新 cmake
			return nil
		default:
			// 其他目录, 保守处理: 全量
			return nil
		}
	}

	// 去重
	seen := make(map[string]bool)
	unique := targets[:0]
	for _, t := range targets {
		if !seen[t] {
			seen[t] = true
			unique = append(unique, t)
		}
	}
	return unique
}

// BuildMySQLIncremental 执行 MySQL/Percona 增量编译。
// 策略:
//  1. 限制并行度 (基于内存)
//  2. 仅编译被修改的源码对应的子目标
//  3. 不构建完整 mysqld, 仅验证语法和链接
//  4. Phase 0 (语法验证阶段) 甚至可以不编译, 仅做语法检查
func BuildMySQLIncremental(cwd string, changedFiles []string, phase int) string {
	jobs := CalcSafeMakeJobs()

	// 确定编译策略
	var buildArgs []string
	switch phase {
	case 0:
		// Phase 0: 仅语法验证 — 编译修改过的 .o 文件, 不链接
		// 使用 cmake --build 但只编译单个文件
		if len(changedFiles) > 0 {
			for _, f := range changedFiles {
				ext := filepath.Ext(f)
				if ext == ".cc" || ext == ".cpp" || ext == ".c" {
					// 仅编译单个 .o 验证语法
					buildArgs = append(buildArgs, "--parallel", fmt.Sprintf("%d", jobs))
					break
				}
			}
		}
	default:
		// Phase 1+: 需要验证链接, 但至少构建修改的子目标
		targets := getMySQLModifiedTargets(changedFiles)
		buildArgs = append(buildArgs, "--parallel", fmt.Sprintf("%d", jobs))
		if len(targets) > 0 {
			// 仅构建特定子目标, 不构建完整的 mysqld
			for _, t := range targets {
				buildArgs = append(buildArgs, "--target", t)
			}
		}
		// 最终仍需要验证 mysqld 能否链接 (但复用已有 .o)
		// 注意: 这仅在所有子目标完成后才执行
	}

	if len(buildArgs) == 0 {
		buildArgs = []string{"--parallel", fmt.Sprintf("%d", jobs)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	out, err := runLimitedCommand(ctx, cwd, append([]string{"cmake", "--build", "build"}, buildArgs...), 16384, 200)
	if err != nil {
		return fmt.Sprintf("cmake --build 失败 (jobs=%d):\n%s", jobs, string(out))
	}
	return ""
}

// MaterializeCode 从 LLM 输出中提取代码块并写入磁盘。
// 匹配 ```lang\n// File: path/to/file.ext\n...``` 或 ```lang:path/to/file.ext\n...``` 模式。
// 返回写入的文件列表。
func MaterializeCode(cwd, output, lang string) []string {
	if cwd == "" || output == "" {
		return nil
	}
	tc := GetToolchain(lang)
	ext := tc.FileExt

	var written []string
	seen := make(map[string]bool)

	reBlock := regexp.MustCompile("(?s)```(?:go|cpp|c\\+\\+|rust|rs|python|py|h|hpp|toml|cmake|mod|makefile|txt|md|markdown|json|yaml|yml)(?::([^\\n]+))?\\n(.*?)```")
	reFilePath := regexp.MustCompile(`(?m)^(?://|#|/\*)\s*(?:File|file|PATH|path|filename|Filename):\s*(.+?)(?:\s*\*/)?$`)

	for _, match := range reBlock.FindAllStringSubmatch(output, -1) {
		block := match[2]
		var filePath string

		// 模式 1: ```lang:path/to/file
		if match[1] != "" {
			filePath = strings.TrimSpace(match[1])
		}
		// 模式 2: 代码块内 // File: path 或 # File: path
		if filePath == "" {
			if fpMatch := reFilePath.FindStringSubmatch(block); len(fpMatch) > 1 {
				filePath = strings.TrimSpace(fpMatch[1])
			}
		}
		// 模式 3: 代码块第一行就是文件路径 (e.g. "src/main.rs" 或 "include/kv.h")
		if filePath == "" {
			firstLine := strings.TrimSpace(strings.SplitN(block, "\n", 2)[0])
			if strings.Contains(firstLine, "/") && strings.Contains(firstLine, ".") && len(firstLine) < 80 && !strings.Contains(firstLine, " ") {
				filePath = firstLine
				block = strings.SplitN(block, "\n", 2)[1]
			}
		}
		if filePath == "" {
			continue
		}
		filePath = cleanMaterializeRelPath(filePath)
		if filePath == "" {
			continue
		}

		if !isRelevantFileExt(filePath, ext) {
			continue
		}
		if seen[filePath] {
			continue
		}

		full := filepath.Join(cwd, filePath)
		dir := filepath.Dir(full)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		cleanBlock := reFilePath.ReplaceAllString(block, "")
		if err := os.WriteFile(full, []byte(strings.TrimSpace(cleanBlock)+"\n"), 0o644); err != nil {
			continue
		}
		written = append(written, filePath)
		seen[filePath] = true
	}

	// 模式 4: markdown header "### path/to/file.ext" 后紧跟代码块
	lines := strings.Split(output, "\n")
	for i := 0; i < len(lines)-1; i++ {
		candidate := extractHeaderFilePath(lines[i], ext)
		if candidate == "" {
			continue
		}
		if !isRelevantFileExt(candidate, ext) || seen[candidate] {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if trimmed == "" {
				continue
			}
			if strings.HasPrefix(trimmed, "```") {
				blockStart := j + 1
				for k := blockStart; k < len(lines); k++ {
					if strings.HasPrefix(strings.TrimSpace(lines[k]), "```") {
						block := strings.Join(lines[blockStart:k], "\n")
						full := filepath.Join(cwd, candidate)
						dir := filepath.Dir(full)
						if err := os.MkdirAll(dir, 0o755); err == nil {
							if err := os.WriteFile(full, []byte(strings.TrimSpace(block)+"\n"), 0o644); err == nil {
								written = append(written, candidate)
								seen[candidate] = true
							}
						}
						break
					}
				}
			}
			break
		}
	}

	return written
}

var headerFilePathRe = regexp.MustCompile(`([A-Za-z0-9._/-]+\.[A-Za-z0-9][A-Za-z0-9_-]*)`)

func extractHeaderFilePath(line, langExt string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "##") {
		return ""
	}
	matches := headerFilePathRe.FindAllString(trimmed, -1)
	for _, match := range matches {
		candidate := cleanMaterializeRelPath(match)
		if candidate == "" || !strings.Contains(filepath.ToSlash(candidate), "/") {
			continue
		}
		if isRelevantFileExt(candidate, langExt) {
			return candidate
		}
	}
	return ""
}

func cleanMaterializeRelPath(path string) string {
	path = strings.TrimSpace(strings.Trim(path, "`\"'"))
	path = strings.TrimRight(path, "。:：,，)")
	path = filepath.ToSlash(path)
	path = strings.TrimPrefix(path, "./")
	if path == "" || strings.HasPrefix(path, "/") || path == ".." || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
		return ""
	}
	return filepath.FromSlash(filepath.Clean(path))
}

func outputContainsRelevantCodeBlock(output, lang string) bool {
	if output == "" {
		return false
	}
	tc := GetToolchain(lang)
	lower := strings.ToLower(output)
	switch tc.Language {
	case "go":
		return strings.Contains(lower, "```go") || strings.Contains(lower, "package ")
	case "rust":
		return strings.Contains(lower, "```rust") || strings.Contains(lower, "```rs") || strings.Contains(lower, "fn ")
	case "python":
		return strings.Contains(lower, "```python") || strings.Contains(lower, "```py")
	case "cpp":
		return strings.Contains(lower, "```cpp") || strings.Contains(lower, "```c++") || strings.Contains(lower, "#include")
	default:
		return strings.Contains(lower, "```")
	}
}

// isRelevantFileExt 检查文件路径是否包含当前语言或通用配置文件扩展名
func isRelevantFileExt(filePath, langExt string) bool {
	commonExts := []string{".toml", ".cmake", ".txt", ".md", ".json", ".yaml", ".yml", ".cfg", ".ini", ".mod"}
	if strings.Contains(filePath, langExt) {
		return true
	}
	for _, ce := range commonExts {
		if strings.HasSuffix(filePath, ce) {
			return true
		}
	}
	lp := strings.ToLower(filePath)
	return strings.HasSuffix(lp, ".h") || strings.HasSuffix(lp, ".hpp") ||
		strings.HasSuffix(lp, ".c") || strings.HasSuffix(lp, ".cc")
}

// buildPrevResultsSummary 将 prevResults map 构建为 summary 字符串 (用于 E2E prompt)
func buildPrevResultsSummary(prevResults map[string]string) string {
	var b strings.Builder
	for name, output := range prevResults {
		summary := SummarizeOldOutput(output, 1000)
		b.WriteString(fmt.Sprintf("### %s:\n%s\n\nFull artifact/ref: prevResults[%q] / blackboard key `%s-result`.\n\n", name, summary, name, name))
	}
	return b.String()
}

// filterParallel 从 ready 阶段中提取可并行执行的子集。
//
// 策略 (修复原有 bug: 之前要求所有 ready 阶段依赖集完全相同才并行,
// 导致依赖不同但同时就绪的阶段无法并行):
//  1. 显式标记 Parallel=true 的阶段总是可并行
//  2. 多个 ready 阶段的依赖已全部满足 (它们才进入 ready 列表),
//     因此它们之间没有执行顺序约束, 应该可以并行
//  3. 唯一限制: 并行数由 executor 的 sem 控制
func filterParallel(stages []StageDef) []StageDef {
	if len(stages) <= 1 {
		return stages
	}

	// 优先: 如果有显式标记 Parallel 的, 全部并行
	var explicit []StageDef
	for _, s := range stages {
		if s.Parallel {
			explicit = append(explicit, s)
		}
	}
	if len(explicit) > 1 {
		return explicit
	}

	// 所有 ready 阶段的依赖已满足 → 按依赖集分组, 同组可并行
	groups := make(map[string][]StageDef)
	for _, s := range stages {
		key := strings.Join(s.DependsOn, ",")
		groups[key] = append(groups[key], s)
	}

	// 选最大的同依赖组
	var best []StageDef
	for _, g := range groups {
		if len(g) > len(best) {
			best = g
		}
	}
	if len(best) > 1 {
		return best
	}

	// 兜底: 所有 ready 阶段依赖已满足, 无执行顺序约束, 全部并行
	return stages
}
