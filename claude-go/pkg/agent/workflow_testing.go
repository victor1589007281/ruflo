// workflow_testing.go — 研发测试团队工作流。
//
// 参考:
//   - TestCase-Eval (ACL 2025): 故障覆盖与暴露率评估
//   - TestEval (NAACL 2025): 行/分支/路径覆盖难度分析
//   - Chaos Engineering (Netflix): 故障注入验证系统韧性
//   - Mutation Testing (PIT/go-mutesting): 用变异分数衡量测试质量
//   - Property-Based Testing (QuickCheck, Hypothesis): LLM 提出属性
//   - Qodo Gen: IDE 级测试生成
//
// 执行流程:
//   代码分析 → 风险建模 → 测试计划分解 → 4路并行生成 → 执行 → Flake检测 → 变异分析 → 门禁 → 报告
package agent

func testingWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "testing",
		Description: "测试团队: 代码分析→风险建模→测试生成(单元/集成/属性/混沌)→执行→变异→报告",
		Mode:        "orchestrated",
		Stages: []StageDef{
			{
				Name: "code-analysis", Role: "code-analyst",
				Prompt: `你是**代码分析专家**。

测试任务: {objective}

## 分析任务
1. **代码结构**: 包/模块/函数清单, 依赖关系图
2. **复杂度分析**: 圈复杂度, 认知复杂度, 函数长度
3. **变更历史**: 高频变更的模块 (变更热点)
4. **现有测试**: 已有测试的覆盖范围和质量评估
5. **技术栈**: 语言/框架/数据库/外部服务

## 输出
- 代码结构摘要
- 复杂度热力图 (按模块)
- 现有测试覆盖评估
- 需要重点测试的模块列表`,
			},
			{
				Name: "risk-model", Role: "risk-assessor",
				DependsOn: []string{"code-analysis"},
				Prompt: `你是**风险评估专家** (参考 Risk-Based Testing 理论)。

测试任务: {objective}

代码分析结果:
{prev_result}

## 风险建模任务
1. **业务风险**: 哪些模块故障会导致严重业务影响?
2. **技术风险**: 并发问题、数据一致性、外部依赖故障
3. **变更风险**: 近期变更引入 bug 的概率
4. **集成风险**: 模块间交互的脆弱点

## 输出
风险矩阵: 模块 × (影响 × 概率) → 测试优先级
每个模块的推荐测试类型和深度`,
			},
			{
				Name: "test-plan", Role: "test-planner",
				DependsOn: []string{"risk-model"},
				Prompt: `你是**测试计划架构师**。

测试任务: {objective}

风险评估结果:
{prev_result}

## 测试计划任务
1. **测试矩阵**: 模块 × 测试类型 × 优先级
2. **任务拆解**: 将每个测试需求拆解为具体的测试函数
3. **依赖排序**: 确定测试执行顺序 (单元 → 集成 → 属性 → 混沌)
4. **资源估算**: 每类测试的 LLM 调用次数和预计耗时

## 输出
- 详细的测试计划矩阵
- 每个测试函数的描述 (名称 + 目标 + 输入 + 预期)
- 执行顺序和依赖关系
- 总体资源和时间估算`,
			},
			{
				Name: "gen-unit-tests", Role: "unit-generator",
				DependsOn: []string{"test-plan"},
				Prompt: `你是**单元测试生成专家**。

测试任务: {objective}

测试计划:
{prev_result}

## 生成要求
1. **表驱动测试**: 使用 Go 标准的 table-driven 风格
2. **边界覆盖**: 空值/零值/最大值/溢出/类型边界
3. **错误路径**: 每个 error return 都要有对应测试
4. **Mock**: 使用接口 + mock 隔离外部依赖
5. **命名**: Test{函数名}_{场景} 格式

## 输出
完整可运行的 _test.go 文件, 包含:
- 所有导入
- 测试辅助函数
- 表驱动测试用例
- 每个测试的注释说明测试目的`,
				Parallel: true,
			},
			{
				Name: "gen-integration-tests", Role: "integration-generator",
				DependsOn: []string{"test-plan"},
				Prompt: `你是**集成测试设计专家**。

测试任务: {objective}

测试计划:
{prev_result}

## 生成要求
1. **端到端场景**: 覆盖核心业务流程
2. **数据库测试**: 使用 testcontainers 或内存数据库
3. **API 测试**: HTTP handler 的请求/响应验证
4. **并发测试**: 使用 sync.WaitGroup + goroutine 的并发场景
5. **Setup/Teardown**: 完整的测试环境搭建和清理

## 输出
完整的集成测试代码, 包含测试环境管理。`,
				Parallel: true,
			},
			{
				Name: "gen-property-tests", Role: "property-generator",
				DependsOn: []string{"test-plan"},
				Prompt: `你是**属性测试和模糊测试专家** (参考 QuickCheck, testing/quick)。

测试任务: {objective}

测试计划:
{prev_result}

## 生成要求
1. **不变量属性**: 识别代码必须满足的数学/逻辑不变量
   例: encode(decode(x)) == x, sort(xs) 有序且等长
2. **生成器**: 为每个属性设计随机输入生成器
3. **收缩器**: 当属性违反时最小化反例
4. **Fuzz 测试**: 使用 Go 1.18+ 的 testing.F

## 输出
- 属性列表及其形式化描述
- 完整的属性测试代码
- Fuzz 测试种子语料`,
				Parallel: true,
			},
			{
				Name: "gen-chaos-tests", Role: "chaos-engineer",
				DependsOn: []string{"test-plan"},
				Prompt: `你是**混沌测试工程师** (参考 Netflix Chaos Engineering 原则)。

测试任务: {objective}

测试计划:
{prev_result}

## 混沌场景设计
1. **网络故障**: 延迟注入、连接中断、DNS 解析失败
2. **依赖故障**: 数据库超时、缓存不可用、消息队列满
3. **资源耗尽**: 内存不足、goroutine 泄漏、文件描述符耗尽
4. **数据异常**: 格式错误的输入、超大 payload、编码异常
5. **并发风暴**: 突发并发、重复请求、乱序到达

## 输出
- 混沌场景清单 (场景 + 注入方式 + 验证目标)
- 每个场景的测试代码 (Go 实现)
- 预期行为: 系统应如何优雅降级
- 恢复验证: 故障消除后系统应恢复正常`,
				Parallel: true,
			},
			{
				Name: "run-tests", Role: "test-runner",
				DependsOn: []string{"gen-unit-tests", "gen-integration-tests", "gen-property-tests", "gen-chaos-tests"},
				Prompt: `你是**测试执行和分析专家**。

测试任务: {objective}

生成的测试代码:
{prev_result}

## 执行任务
1. **编译检查**: 确认所有测试代码可编译
2. **执行模拟**: 分析每个测试的预期执行结果
3. **覆盖率估算**: 基于测试用例推算行覆盖率和分支覆盖率
4. **Flake 风险**: 识别可能的 flaky 测试 (时间依赖/随机性/环境)

## 输出
- 测试执行结果摘要
- 覆盖率估算报告
- Flaky 风险评估
- 失败测试的根因分析`,
			},
			{
				Name: "compile-check", Role: "compile-validator",
				DependsOn: []string{"run-tests"},
				Prompt: `你是**编译验证专家**。

测试任务: {objective}

测试执行结果:
{prev_result}

## 验证任务
1. **语法检查**: 逐行检查生成的测试代码, 识别语法错误
   - 变量名不一致 (如 range ttl vs range ttls)
   - 括号/引号不匹配
   - 缺失的逗号/分号
2. **依赖检查**: 确认所有 import 都是标准库或已声明的依赖
   - 使用不存在的包/函数 (如 rand.String 在标准库中不存在)
   - 引用未定义的类型/方法
3. **类型检查**: 确认类型匹配
   - 接口实现完整性
   - 函数签名正确性
4. **编译可行性**: 判断代码是否可通过 go build / go test

## 输出
- ✅ PASS: 所有测试代码可编译 (无问题)
- ❌ FAIL: 列出所有编译错误, 并给出修正后的代码

**重要: 如果发现问题, 必须输出完整修正后的 _test.go 文件代码。**`,
			},
			{
				Name: "mutation-analysis", Role: "mutation-analyst",
				DependsOn: []string{"compile-check"},
				Prompt: `你是**变异测试分析专家** (参考 PIT/go-mutesting)。

测试任务: {objective}

测试执行结果:
{prev_result}

## 变异分析任务
1. **变异类型**: 条件反转、边界修改、返回值修改、删除语句
2. **存活变异体**: 哪些变异没有被现有测试杀死?
3. **测试加固**: 为存活的变异体补充测试
4. **变异分数**: 杀死数 / 总变异数

## 输出
- 变异类型分布
- 存活变异体列表 (变异位置 + 变异类型)
- 补充测试代码 (杀死存活变异体)
- 最终变异杀死率`,
			},
			{
				Name: "quality-gate", Role: "gate-keeper",
				DependsOn: []string{"mutation-analysis"},
				Prompt: `你是**质量门禁审判者**。

测试任务: {objective}

全部测试和变异分析结果:
{prev_result}

## 门禁标准
| 指标 | 阈值 | 说明 |
|:----:|:----:|:-----|
| 行覆盖率 | ≥ 80% | 核心模块 ≥ 90% |
| 分支覆盖率 | ≥ 70% | 关键路径 ≥ 85% |
| 变异杀死率 | ≥ 70% | 安全相关 ≥ 85% |
| Flaky 率 | < 5% | 0% 为理想 |
| 集成测试通过率 | 100% | 不允许失败 |

## 输出
- **PASSED / FAILED** (门禁结论)
- 各指标的实际值 vs 阈值
- 未达标项的改进建议
- 下次迭代的测试增强方向`,
			},
			{
				Name: "test-report", Role: "test-reporter",
				DependsOn: []string{"quality-gate"},
				Prompt: `你是**测试报告撰写专家**。

测试任务: {objective}

完整测试结果:
{prev_result}

## 输出完整测试报告

### 结构
1. **执行摘要**: 测试结论和关键数字
2. **测试矩阵**: 各类测试的数量和结果
3. **覆盖率报告**: 行/分支覆盖详情
4. **变异分析**: 变异杀死率和存活热点
5. **混沌测试结果**: 各场景的韧性评估
6. **风险评估**: 剩余风险和建议
7. **改进路线图**: 分优先级的测试增强计划`,
			},
		},
	}
}
