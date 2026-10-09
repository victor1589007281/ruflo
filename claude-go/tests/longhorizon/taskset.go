// Package longhorizon —— H4 长程场景台。
//
// # 这个包补的是哪个缺口
//
// tests/eval 里已经有一个叫"长时间开发"的维度, 但它**从不运行 agent**: 它直接
// 调 compact.NewCompactor 然后断言"大消息触发了折叠"。那测的是**压缩器**, 不是
// "长程任务做得成不成"。方案 §21.5.6 说得直白: 没有这一层, 前面四阶段的验收只能
// 靠"构造一个必然失败的 run"这种手术式测试 —— 那能证明闸门通电了, **不能证明
// 长程任务成功率变高了**。这两件事完全不同。
//
// # 本包与"真实 run"的边界
//
// 场景台分成两半, 边界划在**引擎调用**那一行:
//
//   - 任务集 / 配对规则 / 配置 diff 校验 / 统计 —— 全部是纯逻辑, 本包**完整实现
//     并单测** (见 taskset_test.go / stats_test.go / ab_test.go)。
//   - 真实引擎调用 —— 由调用方注入的 Runner 承担 (见 ab.go)。本包提供一个
//     env-gated 的真实 Runner (run_test.go), 未设环境变量时**跳过**, 于是
//     `go test ./...` 在没有 LLM 的机器上仍然全绿。
//
// 这样切分的理由: 方案 §21.5.6 给 H4 的验收口径 (轮数 > 100 / compact span > 0 /
// 两组成功率与置信区间) 全是**运行读数**。把运行藏进一个不可替换的循环里, 就没法
// 在不起真实 run 的前提下验证这台机器算得对不对 —— 而"算得对不对"与"跑得成不成"
// 是两件必须分开验的事。
//
// # 判据是声明, 读数才是结论
//
// Task.MinTurns / Task.MustCompact 是**要求**, 不是事实: 任务集声明"这个任务应当
// 需要 100 轮以上、且应当跨过压缩阈值"。它究竟需不需要, 只有 H4-2 的 A/B 台跑出来
// 才知道 (见 AssertAcceptance)。把要求写成注释而让断言缺失, 就回到了"装配态验收";
// 把要求直接当成事实, 就是自欺。
package longhorizon

import (
	"fmt"
	"sort"
	"strings"
)

// 验收下限 —— 方案 §21.5.6 H4-1 的原文口径。
const (
	// MinTaskCount 任务集的最小规模 (方案: "构造 ≥ 8 个")。
	MinTaskCount = 8
	// MinLongHorizonTurns 长程任务的轮数下限 (方案: "需要 100+ 轮工具调用")。
	MinLongHorizonTurns = 100
)

// Task 一个长程场景。
//
// 四个字段共同定义"客观终态": Goal 是给人看的意图, Fixture 是初始代码,
// VerifyCmd 是**机器判定的通过条件** (退出码 0)。没有 VerifyCmd 的任务不是长程
// 任务, 是开放式写作 —— 后者测不出成功率, 因为"做完了吗"没有答案。
type Task struct {
	// ID 稳定标识 (落库/对比用)。改了 ID 等于换了一个任务, 历史不可比。
	ID string
	// Goal 交给 agent 的任务描述 (声明意图, 不是判据)。
	Goal string
	// Fixture 初始代码的键 (见 fixtures.go)，Materialize 到工作目录。
	Fixture string
	// VerifyCmd 客观终态判定命令。退出码 0 = 这个任务成功。
	//
	// 刻意用命令而不是 Go 函数: 判据要能被 agent **自己**跑起来 (它才能自我
	// 修正), 也要能在 run 结束后被台子独立复跑一遍 (证明结论不是自报的)。
	VerifyCmd []string
	// MinTurns 期望的最少回合数 —— 低于它说明这个任务**不够长程**,
	// 要么 fixture 太简单, 要么 agent 绕过了。这是任务集自身的质量闸门。
	MinTurns int
	// MustCompact 是否要求运行期间至少发生一次上下文折叠。
	// 长程任务的价值恰恰在于"跨压缩阈值之后还记不记得住", 没撞到阈值的 run
	// 没有测到这件事, 它的成功不能算长程的成功。
	MustCompact bool
	// Tags 分类标签 (便于分组统计)。
	Tags []string
}

// TaskSet 长程场景台的任务集。
//
// 8 个任务覆盖同一件事的不同侧面: 全部是"按规格实现一个小包, 让测试通过" ——
// 也就是写→跑→改的循环, 它是长程压力的来源 (每一步都要读回上一步的产物), 也是
// 唯一有客观终态的形态。刻意**不混入**重构类/开放类任务: 它们的"完成"要人判,
// 一旦要人判, A/B 两组的对照就不再是自动的, 而 H4-2 的整条链都建立在自动判定上。
var TaskSet = []Task{
	{
		ID: "lexer", Fixture: "lexer",
		Goal:      "实现 pkg/lexer: 按 lexer_test.go 里的规格把表达式串切成 token 序列 (含数字/标识符/运算符/括号, 未知字符报错并带位置)。",
		VerifyCmd: []string{"go", "test", "./..."},
		MinTurns:  MinLongHorizonTurns, MustCompact: true,
		Tags: []string{"parse", "error-handling"},
	},
	{
		ID: "rpn", Fixture: "rpn",
		Goal:      "实现 pkg/rpn: 逆波兰表达式求值, 支持 + - * / 与一元负号, 除零与栈不配对要返回错误 (见 rpn_test.go)。",
		VerifyCmd: []string{"go", "test", "./..."},
		MinTurns:  MinLongHorizonTurns, MustCompact: true,
		Tags: []string{"parse", "error-handling"},
	},
	{
		ID: "lru", Fixture: "lru",
		Goal:      "实现 pkg/lru: 带容量上限的 LRU 缓存, O(1) Get/Set, 并暴露命中/淘汰计数 (见 lru_test.go)。",
		VerifyCmd: []string{"go", "test", "./..."},
		MinTurns:  MinLongHorizonTurns, MustCompact: true,
		Tags: []string{"datastruct"},
	},
	{
		ID: "semver", Fixture: "semver",
		Goal:      "实现 pkg/semver: 解析/比较版本号, 并实现受限于 ^ ~ 与比较符的约束匹配 (见 semver_test.go)。",
		VerifyCmd: []string{"go", "test", "./..."},
		MinTurns:  MinLongHorizonTurns, MustCompact: true,
		Tags: []string{"parse", "datastruct"},
	},
	{
		ID: "retry", Fixture: "retry",
		Goal:      "实现 pkg/retry: 指数退避重试策略, 支持最大次数/上限/可重试判定, 注入 clock 以便测试 (见 retry_test.go)。",
		VerifyCmd: []string{"go", "test", "./..."},
		MinTurns:  MinLongHorizonTurns, MustCompact: true,
		Tags: []string{"concurrency", "error-handling"},
	},
	{
		ID: "matrix", Fixture: "matrix",
		Goal:      "实现 pkg/matrix: 矩阵乘/转置/行列式, 维度不匹配返回错误 (见 matrix_test.go)。",
		VerifyCmd: []string{"go", "test", "./..."},
		MinTurns:  MinLongHorizonTurns, MustCompact: true,
		Tags: []string{"numeric"},
	},
	{
		ID: "csvq", Fixture: "csvq",
		Goal:      "实现 pkg/csvq: 对 CSV 行集做 select/where/order/limit 的最小查询, 支持按列名取字段 (见 csvq_test.go)。",
		VerifyCmd: []string{"go", "test", "./..."},
		MinTurns:  MinLongHorizonTurns, MustCompact: true,
		Tags: []string{"data"},
	},
	{
		ID: "jsonpath", Fixture: "jsonpath",
		Goal:      "实现 pkg/jsonpath: 支持 $.a.b[0] 形式的最小路径取值, 缺字段返回错误而不是零值 (见 jsonpath_test.go)。",
		VerifyCmd: []string{"go", "test", "./..."},
		MinTurns:  MinLongHorizonTurns, MustCompact: true,
		Tags: []string{"parse", "data"},
	},
}

// TaskByID 按 ID 取任务。
func TaskByID(id string) (Task, bool) {
	for _, t := range TaskSet {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

// TaskIDs 返回任务 ID 列表 (保持 TaskSet 的顺序)。
//
// 顺序**刻意稳定**: H4-2 要求配对, 而"配对"落在数据上就是"两次运行的 (任务, 顺序)
// 完全一致"。一个 map 遍历出来的顺序会让两次运行的第 i 个样本根本不是同一个任务,
// 于是配对统计退化成两组无关样本的对比 —— 而这在汇总数字上完全看不出来。
func TaskIDs() []string {
	out := make([]string, 0, len(TaskSet))
	for _, t := range TaskSet {
		out = append(out, t.ID)
	}
	return out
}

// Validate 检查单个任务自身是否成立。
func (t Task) Validate() error {
	var problems []string
	if strings.TrimSpace(t.ID) == "" {
		problems = append(problems, "ID 为空")
	}
	if strings.TrimSpace(t.Goal) == "" {
		problems = append(problems, "Goal 为空 (agent 没有输入)")
	}
	if strings.TrimSpace(t.Fixture) == "" {
		problems = append(problems, "Fixture 为空 (没有初始代码)")
	}
	if len(t.VerifyCmd) == 0 {
		problems = append(problems, "VerifyCmd 为空 —— 没有客观终态的任务测不出成功率")
	}
	if t.MinTurns < MinLongHorizonTurns {
		problems = append(problems, fmt.Sprintf(
			"MinTurns=%d < %d —— 这个任务不被当成长期任务, 不该放进本任务集",
			t.MinTurns, MinLongHorizonTurns))
	}
	if !t.MustCompact {
		problems = append(problems, "MustCompact=false —— 没撞压缩阈值的 run 没测到「跨压缩存活」, 不算长程验收")
	}
	if len(problems) > 0 {
		return fmt.Errorf("任务 %q 不合规: %s", t.ID, strings.Join(problems, "; "))
	}
	return nil
}

// ValidateTaskSet 检查整个任务集 —— 这是任务集自己的验收面。
//
// 三条都是**可以失败**的: 条数不足 (方案要求 ≥8)、ID 重复 (会让配对统计把两个
// 任务当成一个)、fixture 缺失 (运行时才炸, 且报错指向 Materialize 而非任务定义)。
// 少任何一条, 任务集缩水都不报错。
func ValidateTaskSet(ts []Task) error {
	if len(ts) < MinTaskCount {
		return fmt.Errorf("任务集只有 %d 个, 方案 §21.5.6 要求 ≥ %d 个 —— "+
			"少于这个数, 配对统计的置信区间宽到没有判别力", len(ts), MinTaskCount)
	}

	seen := make(map[string]bool, len(ts))
	for _, t := range ts {
		if err := t.Validate(); err != nil {
			return err
		}
		if seen[t.ID] {
			return fmt.Errorf("任务 ID 重复: %q —— 重复 ID 会让配对统计把两个任务"+
				"当成同一个, 而汇总数字上完全看不出来", t.ID)
		}
		seen[t.ID] = true

		if _, ok := fixtureFiles(t.Fixture); !ok {
			return fmt.Errorf("任务 %q 的 fixture %q 不存在 —— fixtures.go 里没有这个键",
				t.ID, t.Fixture)
		}
	}
	return nil
}

// FixtureNames 返回已登记的全部 fixture 名 (排序), 便于测试断言"没有孤儿 fixture"。
func FixtureNames() []string {
	out := make([]string, 0, len(fixtures))
	for k := range fixtures {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
