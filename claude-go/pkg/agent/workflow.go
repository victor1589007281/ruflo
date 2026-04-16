// Workflow — 多 Agent 协作工作流模式。
//
// 三种核心模式 (参考 CrewAI + LangGraph + AutoGen):
//
//	1. Pipeline (开发): Architect → Coder → Reviewer → Tester (串行依赖)
//	2. Fan-Out (调研): Researcher₁ ∥ Researcher₂ → Synthesizer (并行汇聚)
//	3. Adversarial (辩论): Proposer ↔ Opponent × N轮 → Judge (对抗决策)
//
// 工作流执行器按 stage 依赖拓扑排序, 自动传递上下文。
package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/metrics"
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
	case "techblog", "article", "blog":
		return techBlogWorkflow()
	case "creative", "design", "visual":
		return creativeWorkflow()
	case "predict", "prediction", "forecast":
		return predictWorkflow()
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
		*techBlogWorkflow(),
		*creativeWorkflow(),
		*predictWorkflow(),
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
		Name: "development",
		Description: "研发流水线: 调研→架构→计划→[Generator↔Evaluator 自适应对抗]→测试 (VERIMAP偏差检测)",
		Mode: "adversarial_dev",
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
      "title": "任务标题",
      "role": "coder",
      "dependsOn": [],
      "designRef": "设计章节名",
      "constraints": ["C1"],
      "acceptance": "go build 通过 + 接口签名与设计一致",
      "priority": 2
    }
  ]
}
` + "```" + `

原则:
1. **原子性**: 每个任务在一轮内可完成
2. **可追溯**: 每个任务标注对应的设计章节和约束编号
3. **验收标准**: 具体、可执行
4. **依赖拓扑**: dependsOn 填前置任务 id 数组, 形成 DAG
5. role 可选: coder, tester, reviewer, researcher, architect

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

关键质量要求:
1. 【禁止空壳】核心模块必须有真实实现,不允许 Mock/Stub/TODO
2. 【编译通过】每创建/修改一个文件后,立即运行 go build/go vet 验证
3. 【配置集中】使用统一的 config 包管理配置,不允许散落的 os.Getenv
4. 【中文注释】关键函数和算法必须有中文注释说明意图
5. 【增量修改】如果收到 Evaluator 反馈,在上一轮代码基础上修改,不要从零重写
6. 【错误处理】每个可能失败的操作都要有 error 处理,不允许 _ = err
7. 【方案对齐】每个模块实现前, 先检查设计文档的接口定义和约束清单

修复反馈时: 必须逐条处理 Evaluator 的每个 BLOCKER 和 HIGH 问题。
实现完毕后, 在输出末尾附上:
**约束检查:** C1 ✅ | C2 ✅ | ... (逐项确认)`,
			},
			{
				Name: "implement", Role: "coder", DependsOn: []string{"design"},
				Prompt: `你是高级软件工程师(对抗式开发中的 Generator 角色)。
严格按照架构设计实现完整的可编译、可运行的代码。

目标: {objective}

架构设计:
{prev_result}

{adversarial_feedback}

关键质量要求:
1. 【禁止空壳】核心模块必须有真实实现,不允许 Mock/Stub/TODO
2. 【编译通过】每创建/修改一个文件后,立即运行 go build/go vet 验证
3. 【配置集中】使用统一的 config 包管理配置,不允许散落的 os.Getenv
4. 【中文注释】关键函数和算法必须有中文注释说明意图
5. 【增量修改】如果收到 Evaluator 反馈,在上一轮代码基础上修改,不要从零重写
6. 【错误处理】每个可能失败的操作都要有 error 处理,不允许 _ = err

修复反馈时: 必须逐条处理 Evaluator 的每个 BLOCKER 和 HIGH 问题。`,
			},
			{
				Name: "evaluate", Role: "reviewer", DependsOn: []string{"implement"},
				Prompt: `你是对抗式开发中的 Evaluator(只读、多疑的审查者)。
你的核心使命不仅是审查代码质量, 更要检测实现与设计方案的偏差。

目标: {objective}

Generator 第 {adversarial_round} 轮产出:
{prev_result}

## 审查维度 (5维度, 每项0-10分):

### 1. correctness (正确性)
- BLOCKER: 编译错误、缺少 import、语法错误 → 标注文件和行号
- 逻辑错误、竞态条件、资源泄漏

### 2. completeness (完整性)
- 是否覆盖设计文档中所有模块?
- CRITICAL: 核心功能未实现 (Mock/Stub)

### 3. security (安全性)
- SQL注入、硬编码密码、敏感数据泄露

### 4. code_quality (代码质量)
- 命名、结构、中文注释、错误处理

### 5. design_alignment (方案对齐度) ← 关键新增维度
参考 VERIMAP (EACL 2026) 的偏差检测:
- 接口签名是否与设计文档一致? (方法名/参数/返回值)
- 文件结构是否与设计文档一致? (目录树对比)
- 约束清单 C1/C2/C3... 是否全部遵守?
- 数据结构 (struct) 是否与设计一致?
- 依赖方向是否与设计一致? (不存在设计中未声明的依赖)
偏差不一定是错误, 但必须标注并给出理由 (如 "设计遗漏, 运行时需要此依赖")。

输出 STRICTLY as JSON:
{"correctness": N, "completeness": N, "security": N, "code_quality": N, "design_alignment": N, "pass": bool, "feedback": "问题列表+修复建议+偏差说明"}

评分标准: 0-10 分。有 BLOCKER → correctness ≤ 3。有 Stub → completeness ≤ 4。
有严重偏差(如设计中定义的接口未实现/签名不一致) → design_alignment ≤ 4。
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
		Description: "调研汇总: 技术专家+市场分析师+风险审计员 并行调研 → 综合分析",
		Mode:        "fanout",
		Stages: []StageDef{
			{
				Name: "research-tech", Role: "tech-researcher",
				Prompt: `你是**技术深度研究员** (专注技术架构和实现)。对以下主题进行技术层面的深度调研。

调研主题: {objective}

## 强制要求
1. **使用 WebSearch 工具**验证关键技术数据 (版本号、性能指标、API 变更等), 不要仅依赖训练知识
2. 必须包含**负面案例**: 至少 2 个选型失败/踩坑案例及原因分析

## 调研维度
- 核心技术架构和设计理念
- 最新版本特性和 Roadmap (通过 WebSearch 验证)
- 性能基准数据 (注明数据来源和测试条件)
- 代码示例和实现模式
- 技术局限性和已知问题
- **踩坑案例**: 社区中报告的常见问题和解决方案

## 输出格式
结构化 Markdown，每个数据点注明来源 (官方文档/社区/实测)。`,
				Parallel: true,
			},
			{
				Name: "research-market", Role: "market-analyst",
				Prompt: `你是**市场与商业分析师** (专注商业价值和竞争格局)。对以下主题进行市场和商业层面的调研。

调研主题: {objective}

## 强制要求
1. **使用 WebSearch 工具**查询最新的市场数据和采用案例
2. 必须包含**量化数据**: 成本对比表、性能对比表、市场份额等
3. 必须包含**失败案例**: 至少 1 个迁移/采用失败的真实案例

## 调研维度
- 行业采用情况和市场趋势 (通过 WebSearch 获取最新数据)
- 竞品对比分析 (功能矩阵表)
- TCO 总拥有成本分析 (含运维、人力、迁移成本)
- 成功案例 AND 失败案例 (各至少 1 个)
- ROI 评估模型

## 输出格式
结构化 Markdown，数据密集，使用表格对比。`,
				Parallel: true,
			},
			{
				Name: "research-risk", Role: "risk-auditor",
				Prompt: `你是**风险审计员** (专注风险评估和合规)。对以下主题进行风险和安全层面的深度审计。

调研主题: {objective}

## 强制要求
1. **使用 WebSearch 工具**搜索相关 CVE、安全公告、故障报告
2. 必须给出**量化风险评分** (影响 × 概率 矩阵)
3. 每个风险必须给出**具体缓解措施**

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
				Name: "synthesize", Role: "synthesizer",
				DependsOn: []string{"research-tech", "research-market", "research-risk"},
				Prompt: `你是**首席分析师**，负责深度综合所有调研结果并撰写最终报告。

调研主题: {objective}

调研团队产出:
{prev_result}

## 综合报告要求 (逐条完成, 不允许偷工减料)

1. **执行摘要** (5-8 条关键发现, 按重要性排序)
2. **技术深度分析**
   - 综合技术研究员的发现
   - **矛盾数据对比表**: 当不同研究员给出不同数据时,列表对比并分析原因
   - 技术可行性评分 (1-10, 含评分依据)
3. **市场分析** (竞品对比表、成本分析、ROI)
4. **风险热力图** (影响×概率矩阵, 标红 Top 5)
5. **失败案例专题** (综合所有负面案例, 提炼共性教训)
6. **实施路线图** (分阶段: MVP→Beta→GA, 含里程碑和交付物)
7. **CVE/安全验证** 
   - 核查风险审计员引用的每个 CVE 编号
   - 对无法验证的 CVE 标注 "⚠️ 待人工验证(AI推断)"
8. **结论与决策建议** (给出明确的"推荐/谨慎推荐/不推荐"评级)

## 质量红线
- 三位研究员的产出必须逐篇阅读、逐条交叉验证, 不允许简单拼接
- CVE 引用必须标注来源 (NVD URL 或 "AI 推断")
- 性能数据必须标注测试条件 (如有多个来源给出不同数据, 必须注明差异)
- 报告长度不少于 500 行 (确保深度整合, 非简单提取)
- 将报告保存为独立 Markdown 文件`,
			},
		},
	}
}

func debateWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "debate",
		Description: "对抗辩论: 正方 ↔ 反方 × 3轮 → 裁判",
		Mode:        "adversarial",
		Rounds:      3,
		Stages: []StageDef{
			{
				Name: "proposer", Role: "proposer",
				Prompt: `You are the PROPOSER in a structured debate. Argue IN FAVOR of the proposition.

Proposition: {objective}

{debate_context}

Make your strongest arguments:
1. Present clear, evidence-based reasoning
2. Address any counterarguments from the opponent
3. Provide specific examples and data
4. Strengthen any weakened arguments

Be persuasive but intellectually honest. Acknowledge valid opposing points while explaining why your position is stronger.`,
			},
			{
				Name: "opponent", Role: "opponent",
				Prompt: `You are the OPPONENT in a structured debate. Argue AGAINST the proposition.

Proposition: {objective}

{debate_context}

Make your strongest counterarguments:
1. Identify weaknesses in the proposer's arguments
2. Present alternative perspectives and evidence
3. Highlight risks, costs, and unintended consequences
4. Propose better alternatives if applicable

Be rigorous and critical but fair. Don't use straw man arguments.`,
			},
			{
				Name: "judge", Role: "judge",
				Prompt: `You are the JUDGE in a structured debate. Evaluate both sides and render a verdict.

Proposition: {objective}

Full Debate Transcript:
{prev_result}

Evaluate:
1. Strength of arguments from each side
2. Quality of evidence presented
3. How well each side addressed counterarguments
4. Logical consistency and coherence

Render your verdict:
- Which side presented the stronger case and why
- Key deciding factors
- Nuances and areas of agreement
- Final recommendation with caveats`,
			},
		},
	}
}

// WorkflowExecutor 工作流执行器。
// 集成 Blackboard (bMAS) + TaskTracker (V2 Task) + Structured Handoff + Evolution + Roles + AgentPool。
type WorkflowExecutor struct {
	factory     CreateAgentFunc
	notify      NotifyFunc
	chatID      string
	taskTracker TaskTracker        // 复用 V2 Task 系统 (可为 nil)
	dagTracker  DAGTaskTracker     // V2 DAG 能力 (运行时从 taskTracker 检测)
	evolution   *EvolutionEngine   // 自动进化引擎 (可为 nil)
	roles       *RoleRegistry      // 角色注册表 (可为 nil, 降级用 StageDef.Prompt)
	metrics     *metrics.Collector // 持续观测指标 (可为 nil)
	pool        *AgentPool         // Agent 池 (动态扩缩, 可为 nil)
	checkpoints CheckpointStore    // 检查点存取 (由 Coordinator 注入, 可为 nil)
	promptCache *PromptCache       // 提示词缓存 (参考 Anthropic Prompt Caching)
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
	switch wf.Mode {
	case "pipeline":
		return we.executePipeline(ctx, wf, objective, team)
	case "fanout":
		return we.executeFanOut(ctx, wf, objective, team)
	case "adversarial":
		return we.executeAdversarial(ctx, wf, objective, team)
	case "adversarial_dev":
		return we.executeAdversarialDev(ctx, wf, objective, team)
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

	designStages, generatorStages, evalStage, parallelStages := classifyStages(wf.Stages)
	maxRounds, terminator := we.initAdaptiveTerminator(wf)
	we.tryInitDAG()

	// 恢复检查点: 如果有已完成的阶段, 跳过并注入 prevResults
	we.restoreCheckpoints(wf.Stages, prevResults, &allResults)

	// Phase 1: 设计阶段
	designResults, err := we.runDesignPhase(ctx, designStages, objective, prevResults, team)
	allResults = append(allResults, designResults...)
	we.savePhaseCheckpoints(designResults)
	if err != nil {
		return allResults, err
	}

	// Phase 2: Orchestrator (DAG 驱动, pool 由 DAG 宽度精确控制)
	//          fallback: 经典对抗循环 (pool 用粗糙 autoScalePool)
	orchUsed := false
	if planOutput, hasPlan := prevResults["plan"]; hasPlan && we.dagTracker != nil {
		orchResults, orchErr := we.runOrchestratedPhase(ctx, planOutput, objective, prevResults, team)
		if orchErr == nil && len(orchResults) > 0 {
			allResults = append(allResults, orchResults...)
			we.savePhaseCheckpoints(orchResults)
			orchUsed = true
		} else if orchErr != nil {
			we.notify(we.chatID, fmt.Sprintf("⚠️ Orchestrator 启动失败, fallback 对抗循环: %v", orchErr))
		}
	}

	if !orchUsed {
		we.autoScalePool(wf, designStages, generatorStages, evalStage, parallelStages, maxRounds)
		advResults, err := we.runAdversarialLoop(ctx, generatorStages, evalStage, maxRounds, terminator, objective, prevResults, team)
		allResults = append(allResults, advResults...)
		we.savePhaseCheckpoints(advResults)
		if err != nil {
			return allResults, err
		}
	}

	// Phase 3: E2E 对抗测试
	e2eResults := we.runE2EAdversarial(ctx, parallelStages, objective, prevResults, team)
	allResults = append(allResults, e2eResults...)
	we.savePhaseCheckpoints(e2eResults)

	// Phase 4: 收尾阶段
	finishResults := we.runFinishPhase(ctx, parallelStages, objective, prevResults, team)
	allResults = append(allResults, finishResults...)
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
func (we *WorkflowExecutor) runOrchestratedPhase(ctx context.Context, planOutput, objective string, prevResults map[string]string, team *ProductionTeam) ([]StageResult, error) {
	orch := NewOrchestrator(
		OrchestratorConfig{MaxParallel: 3, MaxRetries: 2, MicroTestAfter: true},
		we.dagTracker, we.factory, we.notify, we.pool, we.chatID,
	)

	if we.checkpoints != nil {
		orch.SetCheckpointStore(we.checkpoints)
	}
	if designDoc, ok := prevResults["design"]; ok {
		orch.SetDesignContext(designDoc, planOutput)
	}

	nodes, err := orch.ParsePlanToDAGWithRepair(ctx, planOutput, team.Name, we.factory)
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

// runAdversarialLoop Phase 2: Generator ↔ Evaluator 对抗循环
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
	we.notify(we.chatID, fmt.Sprintf("⚔️ Phase 2: 对抗循环 (最多 %d 轮)...", maxRounds))
	var lastGenOutput, lastEvalFeedback string
	var lastScore EvalScore

	for round := 1; round <= maxRounds; round++ {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		// Generator 执行
		genResults, genOutput := we.runGeneratorRound(ctx, generatorStages, round, maxRounds, lastGenOutput, lastEvalFeedback, objective, prevResults, team)
		allResults = append(allResults, genResults...)
		if len(genResults) > 0 && genResults[len(genResults)-1].Status != TaskCompleted {
			return allResults, fmt.Errorf("generator 第 %d 轮失败", round)
		}
		lastGenOutput = genOutput

		// 编译门禁
		lastEvalFeedback = we.runBuildGate(ctx, team, round, lastEvalFeedback)

		// Evaluator 审查
		evalResult, shouldBreak, bestOutput := we.runEvaluatorRound(ctx, evalStage, generatorStages, round, maxRounds, lastGenOutput, terminator, objective, prevResults, team, &lastEvalFeedback, lastScore)
		allResults = append(allResults, evalResult...)
		if bestOutput != "" {
			lastGenOutput = bestOutput
			prevResults[generatorStages[len(generatorStages)-1].Name] = bestOutput
		}
		// 更新 lastScore: 保留完整多维度 EvalScore (修复塌缩问题, 参考 MAgICoRe 多维度评分驱动)
		if len(evalResult) > 0 {
			if parsed, err := ParseEvalScoreJSON([]byte(evalResult[len(evalResult)-1].Output)); err == nil {
				lastScore = parsed
			} else if terminator != nil && len(terminator.ScoreHistory) > 0 {
				lastScore = HoldLastOrDefault(lastScore)
			}
		}
		// 即时 Keep/Revert (参考 MiniMax M2.7): 退化时 revert 到最佳版本
		if !shouldBreak && terminator != nil && round > 1 {
			if revert, bo, br := terminator.ShouldRevert(lastScore); revert {
				we.notify(we.chatID, fmt.Sprintf("⏪ 第 %d 轮退化, revert 到第 %d 轮最佳版本", round, br))
				lastGenOutput = bo
				prevResults[generatorStages[len(generatorStages)-1].Name] = bo
			}
		}
		if shouldBreak {
			break
		}
	}
	return allResults, nil
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

// buildFeedbackSection 构建跨轮反馈上下文
func (we *WorkflowExecutor) buildFeedbackSection(round int, lastOutput, lastFeedback string, team *ProductionTeam) string {
	section := ""
	if lastFeedback != "" {
		section = fmt.Sprintf("### Evaluator 第 %d 轮反馈 (必须全部修复):\n%s", round-1, lastFeedback)
	}
	if round > 1 && lastOutput != "" {
		prevSummary := lastOutput
		if len(prevSummary) > 16000 {
			prevSummary = prevSummary[:16000] + "\n...(上轮输出已截断)"
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

// runBuildGate 编译验证门禁
func (we *WorkflowExecutor) runBuildGate(ctx context.Context, team *ProductionTeam, round int, prevFeedback string) string {
	if team.Cwd == "" {
		return prevFeedback
	}
	buildErrors := runBuildCheck(team.Cwd)
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

	var lastE2EOutput string
	for round := 1; round <= 3; round++ {
		if ctx.Err() != nil {
			break
		}

		// E2E Tester
		e2ePrompt := we.buildE2EPrompt(objective, prevResults, lastE2EOutput, round)
		e2eStageDef := StageDef{Name: fmt.Sprintf("e2e-round%d", round), Role: "tester", Prompt: e2ePrompt}
		e2eResult := we.executeStage(ctx, e2eStageDef, objective, prevResults, team)
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

修复后确保 go build ./... && go test ./... 通过。
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
4. **编译验证**: go build ./... && go vet ./... && go test -race ./...

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

// executeAdversarial 对抗辩论执行
func (we *WorkflowExecutor) executeAdversarial(ctx context.Context, wf *WorkflowDef, objective string, team *ProductionTeam) ([]StageResult, error) {
	var allResults []StageResult
	rounds := wf.Rounds
	if rounds <= 0 {
		rounds = 3
	}

	var proposerStage, opponentStage, judgeStage *StageDef
	for i := range wf.Stages {
		switch wf.Stages[i].Role {
		case "proposer":
			proposerStage = &wf.Stages[i]
		case "opponent":
			opponentStage = &wf.Stages[i]
		case "judge":
			judgeStage = &wf.Stages[i]
		}
	}
	if proposerStage == nil || opponentStage == nil || judgeStage == nil {
		return nil, fmt.Errorf("辩论工作流需要 proposer, opponent, judge 角色")
	}

	var debateTranscript strings.Builder
	debateTranscript.WriteString("# Debate Transcript\n\n")

	// 在黑板上写入辩论主题
	if team.Blackboard != nil {
		team.Blackboard.Write("debate-topic", objective, "system", "context")
	}

	for round := 1; round <= rounds; round++ {
		if ctx.Err() != nil {
			return allResults, ctx.Err()
		}

		debateCtx := debateTranscript.String()
		if round == 1 {
			debateCtx = "(This is the opening round. Present your initial arguments.)"
		}

		// 正方发言
		proposerPrompt := strings.ReplaceAll(proposerStage.Prompt, "{objective}", objective)
		proposerPrompt = strings.ReplaceAll(proposerPrompt, "{debate_context}", debateCtx)

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
	}

	// 裁判裁决
	we.notify(we.chatID, "⚖️ 裁判裁决中...")
	judgePrompt := strings.ReplaceAll(judgeStage.Prompt, "{objective}", objective)
	judgePrompt = strings.ReplaceAll(judgePrompt, "{prev_result}", debateTranscript.String())

	sr := we.runAgent(ctx, judgeStage.Role, judgePrompt, team)
	sr.Name = "verdict"
	allResults = append(allResults, sr)

	return allResults, nil
}

// ExecuteSingleStage 公开的单阶段执行 (供 Coordinator 调用)。
func (we *WorkflowExecutor) ExecuteSingleStage(ctx context.Context, stage StageDef, objective string, prevResults map[string]string, team *ProductionTeam) StageResult {
	return we.executeStage(ctx, stage, objective, prevResults, team)
}

// executeStage 执行单个阶段。
// 集成 Blackboard 读/写 + V2 Task 创建/更新 + Structured Handoff + Evolution。
func (we *WorkflowExecutor) executeStage(ctx context.Context, stage StageDef, objective string, prevResults map[string]string, team *ProductionTeam) StageResult {
	ctx, endSpan := logging.WithSpan(ctx, "stage."+stage.Name)
	defer endSpan()
	logging.Event(ctx, "stage.start", "stage", stage.Name, "role", stage.Role, "team", team.Name)

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

	// 3. 执行 Agent
	sr := we.runAgent(ctx, stage.Role, prompt, team)
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
		// 增量学习: 失败阶段立即提炼 error pattern，不等团队结束
		if sr.Status == TaskFailed {
			we.evolution.LearnFromStage(traj)
		}
		if len(injectedExpIDs) > 0 {
			we.evolution.RecordBatchFeedback(injectedExpIDs, sr.Status == TaskCompleted)
		}
	}

	return sr
}

// executeParallel 并行执行多个阶段
func (we *WorkflowExecutor) executeParallel(ctx context.Context, stages []StageDef, objective string, prevResults map[string]string, team *ProductionTeam) []StageResult {
	results := make([]StageResult, len(stages))
	var wg sync.WaitGroup

	for i, stage := range stages {
		wg.Add(1)
		go func(idx int, s StageDef) {
			defer wg.Done()
			results[idx] = we.executeStage(ctx, s, objective, prevResults, team)
		}(i, stage)
	}

	wg.Wait()
	return results
}

// stageTimeout 单阶段执行超时 (防止 agent 无限循环)
const stageTimeout = 10 * time.Minute

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
		we.notify(we.chatID, fmt.Sprintf("⚠️ Agent **%s** 产出不合格: %s — 标记为失败并重试", role, reason))
		return StageResult{
			Role: role, Status: TaskFailed,
			Error:   fmt.Sprintf("产出验证失败: %s", reason),
			Output:  result,
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

	// 1. 基本长度检查 (有效产出通常 > 100 字符)
	if len(trimmed) < 50 {
		return "产出过短 (< 50 字符)，可能未实际执行任务"
	}

	// 2. 空转模式检测: 仅声明角色就绪、未提供实质内容
	lower := strings.ToLower(trimmed)
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
func buildStagePromptWithRoles(stage StageDef, objective string, prevResults map[string]string, roles *RoleRegistry) string {
	var prevOutput strings.Builder
	for _, dep := range stage.DependsOn {
		if r, ok := prevResults[dep]; ok {
			prevOutput.WriteString(fmt.Sprintf("### Output from %s:\n%s\n\n", dep, r))
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
				Name: "formatting", Role: "article-formatter",
				DependsOn: []string{"article-writing"},
				Prompt: `你是微信公众号排版和视觉设计专家。将文章优化为适合公众号发布的格式。

原始文章:
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

// runBuildCheck 在工作区运行 go build + go vet, 返回错误输出。
// 空字符串表示编译通过。
func runBuildCheck(cwd string) string {
	if cwd == "" {
		return ""
	}
	// 快速检查: 是否有 go.mod
	if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var errors []string
	for _, args := range [][]string{{"build", "./..."}, {"vet", "./..."}} {
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = cwd
		out, err := cmd.CombinedOutput()
		if err != nil {
			errors = append(errors, fmt.Sprintf("go %s 失败:\n%s", args[0], string(out)))
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

// buildPrevResultsSummary 将 prevResults map 构建为 summary 字符串 (用于 E2E prompt)
func buildPrevResultsSummary(prevResults map[string]string) string {
	var b strings.Builder
	for name, output := range prevResults {
		summary := output
		if len(summary) > 3000 {
			summary = summary[:3000] + "\n...(已截断)"
		}
		b.WriteString(fmt.Sprintf("### %s:\n%s\n\n", name, summary))
	}
	return b.String()
}

func filterParallel(stages []StageDef) []StageDef {
	if len(stages) <= 1 {
		return stages
	}
	// 如果所有 ready stages 具有相同的依赖且标记了 parallel, 可并行
	var parallel []StageDef
	for _, s := range stages {
		if s.Parallel || len(stages) > 1 {
			parallel = append(parallel, s)
		}
	}
	// 只有多个无依赖或同依赖的才并行
	if len(parallel) > 1 {
		deps0 := strings.Join(parallel[0].DependsOn, ",")
		allSameDeps := true
		for _, p := range parallel[1:] {
			if strings.Join(p.DependsOn, ",") != deps0 {
				allSameDeps = false
				break
			}
		}
		if allSameDeps {
			return parallel
		}
	}
	return stages[:1]
}
