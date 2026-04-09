// Roles — 统一 Agent 角色注册表。
//
// 集中管理所有 Agent 的角色定义、系统提示词和专属 Skills，
// 消除 workflow.go / swarm.go / intent.go / session.go 中分散的硬编码。
//
// 两类角色:
//   - workflow: 参与多 Agent 协作工作流 (architect, coder, tester, etc.)
//   - standalone: 独立运行 (intent-recognizer, consolidator, distiller, etc.)
//
// 每个角色可配置:
//   - SystemPrompt: 角色定位提示词模板 (支持 {objective} 等占位符)
//   - Skills: 角色专属技能文件路径列表
//   - Tags: 角色标签 (用于搜索和匹配)
//
//	┌──────────────────────────────────────────────────────┐
//	│ RoleRegistry                                         │
//	│  Get(name)         → 获取角色定义                    │
//	│  ListByCategory()  → 按类别列出角色                  │
//	│  MergedPrompt()    → 合并角色提示词 + 专属 Skills    │
//	│  RegisterCustom()  → 注册用户自定义角色              │
//	└──────────────────────────────────────────────────────┘
package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RoleDef 角色定义。
type RoleDef struct {
	Name         string   `json:"name"`
	Category     string   `json:"category"`     // "workflow" | "standalone"
	Description  string   `json:"description"`
	SystemPrompt string   `json:"systemPrompt"` // 支持 {objective}, {prev_result} 等占位符
	Skills       []string `json:"skills"`       // 角色专属技能文件相对路径
	Tags         []string `json:"tags"`
}

// RoleRegistry 角色注册表。
type RoleRegistry struct {
	roles map[string]*RoleDef
	mu    sync.RWMutex
	cwd   string // 项目根目录 (用于解析 skill 路径)
}

// NewRoleRegistry 创建角色注册表并注册所有内置角色。
func NewRoleRegistry(cwd string) *RoleRegistry {
	rr := &RoleRegistry{
		roles: make(map[string]*RoleDef),
		cwd:   cwd,
	}
	rr.registerBuiltins()

	// 尝试从磁盘加载用户自定义角色
	customDir := filepath.Join(cwd, ".claude", "agents")
	rr.loadCustomRoles(customDir)

	return rr
}

// Get 获取角色定义 (不存在则返回 nil)。
func (rr *RoleRegistry) Get(name string) *RoleDef {
	rr.mu.RLock()
	defer rr.mu.RUnlock()
	return rr.roles[name]
}

// MergedPrompt 合并角色系统提示词 + 专属 Skills 内容。
// objective/prevResult 用于替换模板占位符。
func (rr *RoleRegistry) MergedPrompt(roleName, objective, prevResult string) string {
	role := rr.Get(roleName)
	if role == nil {
		return ""
	}

	prompt := role.SystemPrompt
	prompt = strings.ReplaceAll(prompt, "{objective}", objective)
	prompt = strings.ReplaceAll(prompt, "{prev_result}", prevResult)

	// 加载角色专属 Skills
	if len(role.Skills) > 0 {
		var skillContent strings.Builder
		skillContent.WriteString("\n\n<role_skills>\n")
		for _, sp := range role.Skills {
			fullPath := sp
			if !filepath.IsAbs(sp) {
				fullPath = filepath.Join(rr.cwd, sp)
			}
			data, err := os.ReadFile(fullPath)
			if err != nil {
				continue
			}
			content := string(data)
			if len(content) > 4096 {
				content = content[:4096] + "...(truncated)"
			}
			skillContent.WriteString(fmt.Sprintf("### Skill: %s\n%s\n\n", filepath.Base(sp), content))
		}
		skillContent.WriteString("</role_skills>")
		prompt += skillContent.String()
	}

	return prompt
}

// ListByCategory 按类别列出角色。
func (rr *RoleRegistry) ListByCategory(category string) []*RoleDef {
	rr.mu.RLock()
	defer rr.mu.RUnlock()
	var result []*RoleDef
	for _, r := range rr.roles {
		if category == "" || r.Category == category {
			result = append(result, r)
		}
	}
	return result
}

// RegisterCustom 注册用户自定义角色。
func (rr *RoleRegistry) RegisterCustom(role *RoleDef) {
	if role == nil || role.Name == "" {
		return
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rr.roles[role.Name] = role
}

// loadCustomRoles 从磁盘加载用户自定义角色。
// 目录结构: .claude/agents/{role-name}.json
func (rr *RoleRegistry) loadCustomRoles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var role RoleDef
		if json.Unmarshal(data, &role) == nil && role.Name != "" {
			rr.mu.Lock()
			rr.roles[role.Name] = &role
			rr.mu.Unlock()
			log.Printf("[Roles] 加载自定义角色: %s (%s)", role.Name, role.Category)
		}
	}
}

// Count 返回角色总数。
func (rr *RoleRegistry) Count() int {
	rr.mu.RLock()
	defer rr.mu.RUnlock()
	return len(rr.roles)
}

// --- 内置角色定义 ---

func (rr *RoleRegistry) registerBuiltins() {
	// ========== Workflow 角色 ==========

	rr.roles["architect"] = &RoleDef{
		Name: "architect", Category: "workflow",
		Description: "高级软件架构师: 分析需求、设计架构、制定技术方案",
		Tags:        []string{"design", "architecture", "planning"},
		SystemPrompt: `You are a senior software architect. Analyze the requirement and produce a detailed technical design.

Requirement: {objective}

Output a design document with:
1. Architecture overview and key design decisions
2. Component breakdown with interfaces
3. Data flow and state management
4. File structure and naming conventions
5. Edge cases and error handling strategy

Be specific about implementation details. Output in markdown.`,
	}

	rr.roles["coder"] = &RoleDef{
		Name: "coder", Category: "workflow",
		Description: "高级开发工程师: 根据设计实现代码",
		Tags:        []string{"implementation", "coding", "development"},
		SystemPrompt: `You are an expert software developer. Implement the solution based on the architecture design.

Objective: {objective}

Architecture Design:
{prev_result}

Write clean, production-quality code. Include proper error handling, logging, and documentation.
Create all necessary files. Use the tools available to write files and run commands.`,
	}

	rr.roles["reviewer"] = &RoleDef{
		Name: "reviewer", Category: "workflow",
		Description: "高级代码审查员: 审查代码质量、安全、最佳实践",
		Tags:        []string{"review", "security", "quality"},
		SystemPrompt: `You are a senior code reviewer. Review the implementation for quality, security, and best practices.

Objective: {objective}

Implementation summary:
{prev_result}

Review checklist:
1. Code correctness and logic errors
2. Security vulnerabilities (injection, auth bypass, data leak)
3. Performance issues (N+1 queries, memory leaks, blocking calls)
4. Error handling completeness
5. API design and naming conventions
6. Documentation quality

Provide specific, actionable feedback with file paths and line references.`,
	}

	rr.roles["tester"] = &RoleDef{
		Name: "tester", Category: "workflow",
		Description: "质量工程师: 编写全面的测试用例",
		Tags:        []string{"testing", "quality", "verification"},
		SystemPrompt: `You are a quality engineer. Write comprehensive tests for the implementation.

Objective: {objective}

Implementation summary:
{prev_result}

Write tests covering:
1. Unit tests for all public functions
2. Edge cases and error paths
3. Integration tests if applicable
4. Test data setup and cleanup

Use the project's testing framework. Ensure tests are deterministic and independent.`,
	}

	rr.roles["researcher"] = &RoleDef{
		Name: "researcher", Category: "workflow",
		Description: "技术调研员: 深入调研技术方案",
		Tags:        []string{"research", "analysis", "investigation"},
		SystemPrompt: `You are a technical researcher. Investigate the topic thoroughly.

Topic: {objective}

Focus on:
- Current state of the art and recent developments
- Key technologies, frameworks, and tools
- Technical trade-offs and limitations
- Code examples and implementation patterns

Provide detailed findings with references where possible. Output in structured markdown.`,
	}

	rr.roles["synthesizer"] = &RoleDef{
		Name: "synthesizer", Category: "workflow",
		Description: "高级分析师: 综合多方调研结果",
		Tags:        []string{"synthesis", "analysis", "report"},
		SystemPrompt: `You are a senior analyst. Synthesize all research findings into a comprehensive report.

Topic: {objective}

Research Findings:
{prev_result}

Produce a final report with:
1. Executive Summary (key findings in 3-5 bullets)
2. Technical Analysis (synthesized from all researchers)
3. Market Analysis
4. Risk Assessment
5. Recommendations (prioritized, actionable)
6. Conclusion

Be concise but thorough. Highlight agreements and contradictions between researchers.`,
	}

	rr.roles["proposer"] = &RoleDef{
		Name: "proposer", Category: "workflow",
		Description: "辩论正方: 提出并论证主张",
		Tags:        []string{"debate", "argumentation"},
		SystemPrompt: `You are the PROPOSER in a structured debate. Argue IN FAVOR of the proposition.

Proposition: {objective}

{prev_result}

Make your strongest arguments:
1. Present clear, evidence-based reasoning
2. Address any counterarguments from the opponent
3. Provide specific examples and data
4. Strengthen any weakened arguments

Be persuasive but intellectually honest. Acknowledge valid opposing points while explaining why your position is stronger.`,
	}

	rr.roles["opponent"] = &RoleDef{
		Name: "opponent", Category: "workflow",
		Description: "辩论反方: 提出反驳论证",
		Tags:        []string{"debate", "counterargument"},
		SystemPrompt: `You are the OPPONENT in a structured debate. Argue AGAINST the proposition.

Proposition: {objective}

{prev_result}

Make your strongest counterarguments:
1. Identify weaknesses in the proposer's arguments
2. Present alternative perspectives and evidence
3. Highlight risks, costs, and unintended consequences
4. Propose better alternatives if applicable

Be rigorous and critical but fair. Don't use straw man arguments.`,
	}

	rr.roles["judge"] = &RoleDef{
		Name: "judge", Category: "workflow",
		Description: "辩论裁判: 评判双方论证并作出裁决",
		Tags:        []string{"debate", "judgment", "evaluation"},
		SystemPrompt: `You are the JUDGE in a structured debate. Evaluate both sides and render a verdict.

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
	}

	// ========== 金融专家团队角色 ==========

	rr.roles["market-analyst"] = &RoleDef{
		Name: "market-analyst", Category: "workflow",
		Description: "量化交易分析师: 技术指标、价格趋势、成交量分析",
		Tags:        []string{"finance", "trading", "technical-analysis"},
	}
	rr.roles["sentiment-analyst"] = &RoleDef{
		Name: "sentiment-analyst", Category: "workflow",
		Description: "金融情绪分析师: 市场情绪、资金流向、分析师共识",
		Tags:        []string{"finance", "sentiment", "market-mood"},
	}
	rr.roles["financial-analyst"] = &RoleDef{
		Name: "financial-analyst", Category: "workflow",
		Description: "高级财务分析师 (CFA): 财报分析、估值、同业对比",
		Tags:        []string{"finance", "fundamental", "valuation"},
	}
	rr.roles["news-tracker"] = &RoleDef{
		Name: "news-tracker", Category: "workflow",
		Description: "金融新闻追踪专家: 事件分析、行业动态、宏观因素",
		Tags:        []string{"finance", "news", "events"},
	}
	rr.roles["risk-assessor"] = &RoleDef{
		Name: "risk-assessor", Category: "workflow",
		Description: "高级风险管理专家 (FRM): 风险矩阵、仓位建议、止损策略",
		Tags:        []string{"finance", "risk", "management"},
	}
	rr.roles["trade-advisor"] = &RoleDef{
		Name: "trade-advisor", Category: "workflow",
		Description: "首席投资策略师: 综合评分、交易策略、投资建议",
		Tags:        []string{"finance", "strategy", "recommendation"},
	}

	// ========== 技术博客/公众号写作团队角色 ==========

	rr.roles["source-analyst"] = &RoleDef{
		Name: "source-analyst", Category: "workflow",
		Description: "资深源码分析专家: 深入分析开源代码架构和核心实现",
		Tags:        []string{"writing", "source-code", "analysis"},
	}
	rr.roles["tech-investigator"] = &RoleDef{
		Name: "tech-investigator", Category: "workflow",
		Description: "技术调查记者: 全方位搜集背景信息、社区生态、行业影响",
		Tags:        []string{"writing", "investigation", "research"},
	}
	rr.roles["fact-checker"] = &RoleDef{
		Name: "fact-checker", Category: "workflow",
		Description: "技术事实核验专家: 验证技术准确性、数据可靠性、时效性",
		Tags:        []string{"writing", "verification", "accuracy"},
	}
	rr.roles["tech-writer"] = &RoleDef{
		Name: "tech-writer", Category: "workflow",
		Description: "顶级技术自媒体作者: 撰写深度技术分析文章",
		Tags:        []string{"writing", "content", "article"},
	}
	rr.roles["article-formatter"] = &RoleDef{
		Name: "article-formatter", Category: "workflow",
		Description: "公众号排版专家: 视觉优化、SEO、互动设计",
		Tags:        []string{"writing", "formatting", "design"},
	}

	// ========== Standalone 角色 ==========

	rr.roles["intent-recognizer"] = &RoleDef{
		Name: "intent-recognizer", Category: "standalone",
		Description: "意图识别器: 从中文自然语言中提取团队命令",
		Tags:        []string{"intent", "nlp", "chinese"},
	}

	rr.roles["experience-distiller"] = &RoleDef{
		Name: "experience-distiller", Category: "standalone",
		Description: "经验提炼器: 从执行轨迹中提炼可复用经验",
		Tags:        []string{"evolution", "learning", "distillation"},
	}

	rr.roles["memory-consolidator"] = &RoleDef{
		Name: "memory-consolidator", Category: "standalone",
		Description: "记忆整理器: Dreaming 机制中的记忆合并和清理",
		Tags:        []string{"dreaming", "memory", "consolidation"},
	}

	rr.roles["swarm-decomposer"] = &RoleDef{
		Name: "swarm-decomposer", Category: "standalone",
		Description: "蜂群分解器: 将复杂目标动态拆解为子任务",
		Tags:        []string{"swarm", "decomposition", "planning"},
	}

	rr.roles["swarm-merger"] = &RoleDef{
		Name: "swarm-merger", Category: "standalone",
		Description: "蜂群汇聚器: 综合多子任务结果",
		Tags:        []string{"swarm", "merge", "synthesis"},
	}
}
