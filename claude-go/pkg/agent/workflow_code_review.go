// workflow_code_review.go — 专业代码审查团队工作流。
//
// 参考:
//   - CodeReviewer (Microsoft, ESEC/FSE 2022): 预训练审查模型
//   - CodeAgent (EMNLP 2024): 多智能体代码审查
//   - CodeReviewQA (ACL 2025): 分解审查推理
//   - BlueCodeAgent (MSR): Red/Blue 对抗审查
//   - Qodo PR-Agent: 开源 PR 审查工具
//
// 执行流程:
//   代码摄入 ∥ 上下文检索 → 合并上下文 → 4路专家并行审查 → 对抗质疑 → 测试验证 → 报告生成
//
// 使用新编排引擎 (pkg/orchestrator) 驱动 DAG 调度。
package agent

func codeReviewWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "code-review",
		Description: "代码审查团队: 静态分析→多维专家审查→对抗质疑→测试验证→报告",
		Mode:        "orchestrated",
		Stages: []StageDef{
			{
				Name: "ingest", Role: "ingester",
				Prompt: `你是**代码摄入专家**。

审查任务: {objective}

## 任务
1. **解析代码范围**: 识别变更的文件、函数、模块
2. **提取上下文**: 确定变更涉及的依赖、接口、数据流
3. **分类变更**: 新增/修改/删除, 功能性/重构/修复
4. **风险标注**: 标记涉及安全、并发、数据库、API 的变更

## 输出格式
- 变更文件清单 (文件名 + 变更类型 + 行数)
- 涉及的核心模块和接口
- 初步风险评级 (高/中/低)
- 审查重点建议`,
				Parallel: true,
			},
			{
				Name: "static-analysis", Role: "static-analyzer",
				Prompt: `你是**静态分析专家**。

审查任务: {objective}

## 任务
1. **代码规范检查**: 命名规范, 代码风格, 注释完整性
2. **潜在缺陷扫描**: 空指针, 资源泄漏, 错误处理不当, 未使用变量
3. **安全扫描**: SQL注入, XSS, 路径遍历, 硬编码密钥, 不安全加密
4. **性能问题**: N+1查询, 不必要的内存分配, 阻塞调用
5. **复杂度分析**: 圈复杂度, 认知复杂度, 函数长度

## 输出格式
对每个发现使用统一格式:
- **严重级别**: blocker / high / medium / low / nit
- **类别**: correctness / security / performance / style / maintainability
- **位置**: 文件名:行号
- **描述**: 问题说明
- **建议**: 具体修复方案

按严重级别降序排列。`,
				Parallel: true,
			},
			{
				Name: "context-retrieve", Role: "context-retriever",
				Prompt: `你是**代码上下文检索专家**。

审查任务: {objective}

## 任务
1. **项目规范**: 检索项目的编码规范、架构决策记录 (ADR)
2. **历史模式**: 检索类似模块的审查历史和已知模式
3. **依赖分析**: 分析变更的上下游影响
4. **测试覆盖**: 评估现有测试对变更区域的覆盖情况

## 输出
- 相关的编码规范要点
- 类似变更的历史审查建议
- 影响范围评估
- 测试覆盖缺口`,
				Parallel: true,
			},
			{
				Name: "logic-review", Role: "logic-reviewer",
				DependsOn: []string{"ingest", "static-analysis", "context-retrieve"},
				Prompt: `你是**逻辑正确性审查专家** (参考 CodeReviewQA 的推理分解方法)。

审查任务: {objective}

前置分析:
{prev_result}

## 审查焦点
1. **算法正确性**: 逻辑错误, 边界条件, Off-by-one, 死循环
2. **并发安全**: 竞态条件, 死锁, 数据竞争, goroutine 泄漏
3. **错误处理**: 错误传播链, panic recovery, 优雅降级
4. **数据完整性**: 状态一致性, 事务边界, 幂等性
5. **接口契约**: API 兼容性, 参数验证, 返回值语义

## 输出
对每个发现使用统一格式 (severity + category + file:line + description + suggestion)。
特别注明 confidence 评分 (0.0-1.0)。`,
				Parallel: true,
			},
			{
				Name: "security-review", Role: "security-reviewer",
				DependsOn: []string{"ingest", "static-analysis", "context-retrieve"},
				Prompt: `你是**安全审查专家** (Red Team 视角, 参考 OWASP Top 10 + CWE)。

审查任务: {objective}

前置分析:
{prev_result}

## 审查焦点
1. **注入漏洞**: SQL注入, 命令注入, XSS, LDAP注入
2. **认证授权**: 权限提升, IDOR, 会话管理
3. **数据安全**: 敏感数据泄露, 加密不当, 日志脱敏
4. **输入验证**: 缺少校验, 不安全的反序列化
5. **供应链安全**: 依赖漏洞, 不安全的第三方库

## Red Team 思维
对每个安全发现, 描述:
- **攻击向量**: 攻击者如何利用此漏洞
- **影响范围**: 最坏情况下的影响
- **PoC 思路**: 概念验证攻击思路
- **修复方案**: 具体的修复代码`,
				Parallel: true,
			},
			{
				Name: "performance-review", Role: "performance-reviewer",
				DependsOn: []string{"ingest", "static-analysis", "context-retrieve"},
				Prompt: `你是**性能审查专家**。

审查任务: {objective}

前置分析:
{prev_result}

## 审查焦点
1. **时间复杂度**: 算法选择, 不必要的遍历, 可优化的热路径
2. **内存效率**: 内存分配模式, GC 压力, 内存泄漏
3. **I/O 效率**: 数据库查询优化, 网络调用聚合, 缓存策略
4. **并发利用**: 并行度, channel 缓冲, 锁粒度
5. **可扩展性**: 水平扩展瓶颈, 状态管理, 连接池

对每个性能问题给出量化影响估算。`,
				Parallel: true,
			},
			{
				Name: "style-review", Role: "style-reviewer",
				DependsOn: []string{"ingest", "static-analysis", "context-retrieve"},
				Prompt: `你是**代码风格与可维护性审查专家**。

审查任务: {objective}

前置分析:
{prev_result}

## 审查焦点
1. **命名规范**: 变量/函数/类型命名是否清晰表达意图
2. **代码组织**: 包结构、文件划分、函数长度 (< 50行)
3. **注释质量**: 是否解释了 "为什么" 而非 "做了什么"
4. **DRY 原则**: 重复代码、可抽取的公共逻辑
5. **SOLID 原则**: 单一职责、开闭原则、依赖倒置
6. **测试友好**: Mock 便利性、依赖注入、可测试性

severity 一般为 low 或 nit, 除非严重影响可维护性。`,
				Parallel: true,
			},
			{
				Name: "adversarial-challenge", Role: "skeptical-reviewer",
				DependsOn: []string{"logic-review", "security-review", "performance-review", "style-review"},
				Prompt: `你是**Skeptical Reviewer** (对抗质疑者, 参考 BlueCodeAgent)。

审查任务: {objective}

各专家的审查发现:
{prev_result}

## 你的职责
你需要对前面 4 位专家的审查结果进行**质疑和验证**:

1. **误报检测**: 哪些发现是误报? 给出具体反驳理由
2. **遗漏补充**: 专家们遗漏了什么重要问题?
3. **严重级别校准**: 哪些发现的严重级别被高估或低估?
4. **建议可行性**: 修复建议是否真的可行? 有没有副作用?
5. **优先级排序**: 如果只能修 3 个问题, 应该修哪 3 个?

## 输出
- 确认的发现 (验证通过)
- 驳回的发现 (误报 + 理由)
- 新增的发现 (专家遗漏)
- 校准后的严重级别
- Top 3 必修问题`,
			},
			{
				Name: "test-verify", Role: "test-verifier",
				DependsOn: []string{"adversarial-challenge"},
				Prompt: `你是**测试驱动验证专家** (Test-Driven Review)。

审查任务: {objective}

对抗审查后的确认发现:
{prev_result}

## 任务
对每个 severity >= high 的确认发现, 生成验证测试:

1. **编写测试代码**: 用测试证明问题确实存在
2. **回归测试**: 编写修复后的回归测试
3. **边界测试**: 对发现涉及的边界条件补充测试

## 输出
- 每个高严重发现的验证测试代码
- 测试执行预期结果
- 测试覆盖的边界条件列表`,
			},
			{
				Name: "report", Role: "report-writer",
				DependsOn: []string{"test-verify"},
				Prompt: `你是**审查报告撰写专家**。

审查任务: {objective}

完整审查结果:
{prev_result}

## 输出审查报告

### 报告结构
1. **执行摘要**: 一段话总结审查结论 (通过/有条件通过/不通过)
2. **关键发现**: Top 问题列表 (按严重级别排序)
3. **详细发现**: 每个发现的完整信息
4. **测试验证**: 测试驱动验证的结果
5. **改进建议**: 按优先级排序的改进清单
6. **审查统计**: 发现总数, 各级别分布, 各类别分布

### Finding 格式
每个发现必须包含:
- severity (blocker/high/medium/low/nit)
- category (correctness/security/performance/style/maintainability)
- file:line
- title
- description
- suggestion
- confidence (0.0-1.0)
- verified_by_test (true/false)`,
			},
		},
	}
}
