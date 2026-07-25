package agent

// graph_interceptors.go —— 图执行拦截器的生产装配 (design/01 §4.10)。
//
// pkg/graph 定义了切面挂载点与链语义 (interceptor.go), 但内核不认识 agent 语义,
// 所以"装哪些拦截器、参数从哪来"在这一层决定。此前生产恒空链 —— 切面建成未通电,
// 本文件就是那条线。
//
// ## 为什么默认预算是"从图算出来的真上界"而不是"不限制"
//
// 默认不限制等于拦截器挂了但不起作用 (本仓反复吃过的"建成未通电")。默认限死一个
// 拍脑袋的数又会改变现状行为 —— 有 8+ 个下游平台在用 :18080, 那是生产事故。
//
// 折中: 从 GraphSpec **算出**一次合法运行的节点执行次数上界 (节点数 × 重试 ×
// 循环轮次, 动态展开/扇出按 MaxTotalNodes 上界算), 再乘一个宽裕系数。合法运行
// 达不到它, 失控运行会撞上它。所以它不改变任何正常行为, 只是给"无界重试/循环
// 烧钱"这类历史故障加一道兜底闸。真要设紧预算就用下面的环境变量。
//
// ## 环境变量 (都缺省 = 走算出来的上界)
//
//	CLAUDE_GO_GRAPH_BUDGET_NODE_RUNS   整图节点执行次数上限
//	CLAUDE_GO_GRAPH_BUDGET_WALLCLOCK   整图墙钟预算 (Go duration, 如 "45m")
//	CLAUDE_GO_GRAPH_BUDGET_TOKENS      整图 token 预算 (需 runner 回报用量才生效)
//	CLAUDE_GO_GRAPH_BUDGET_ON_EXCEED   超限动作: fail (默认) | skip
//	CLAUDE_GO_GRAPH_INTERCEPTORS       链开关: 逗号分隔的拦截器名; "off"=空链

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/graph"
)

// budgetSlack 算出来的上界乘的宽裕系数。
// 2 倍是刻意的: 上界推导已按各闸的最大值取, 再翻一倍确保合法运行绝不会撞闸 ——
// 这道闸的定位是"兜住失控", 不是"精确控成本"。要精确控就用环境变量。
const budgetSlack = 2

// graphInterceptors 装配一次图运行的拦截器链。
//
// 顺序即语义 (见 pkg/graph/interceptor.go): budget 必须在最外层 —— 被预算拒掉的
// 节点不该被别的拦截器记账/记轨迹。
func graphInterceptors(we *WorkflowExecutor, team *ProductionTeam, spec graph.GraphSpec) []graph.NodeInterceptor {
	enabled := parseInterceptorSwitch(os.Getenv("CLAUDE_GO_GRAPH_INTERCEPTORS"))
	if enabled != nil && len(enabled) == 0 {
		return nil // 显式 off
	}

	var out []graph.NodeInterceptor
	if enabled == nil || enabled["budget"] {
		b := budgetFromEnv(spec)
		out = append(out, graph.NewBudgetManager(b, time.Now()))
	}
	return out
}

// parseInterceptorSwitch 解析链开关。
// 返回 nil = 未设置 (全开); 返回空 map = 显式关闭全部。
func parseInterceptorSwitch(v string) map[string]bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if strings.EqualFold(v, "off") || strings.EqualFold(v, "none") {
		return map[string]bool{}
	}
	set := map[string]bool{}
	for _, name := range strings.Split(v, ",") {
		if n := strings.TrimSpace(name); n != "" {
			set[n] = true
		}
	}
	return set
}

// budgetFromEnv 环境变量优先, 缺省用从 spec 算出的上界。
func budgetFromEnv(spec graph.GraphSpec) graph.Budget {
	b := graph.Budget{
		MaxNodeRuns: nodeRunCeiling(spec),
		OnExceed:    strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_BUDGET_ON_EXCEED")),
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_BUDGET_NODE_RUNS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			b.MaxNodeRuns = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_BUDGET_WALLCLOCK")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			b.MaxWallClock = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_BUDGET_TOKENS")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			b.MaxTokens = n
		}
	}
	return b
}

// nodeRunCeiling 算一次**合法**运行最多能有多少次节点执行。
//
// 推导 (每一项都取该闸允许的最大值, 故结果是真上界):
//   - 节点基数: 声明了 map/展开时按 MaxTotalNodes 上界算 (运行图可以长到那么大),
//     否则就是声明的节点数;
//   - 每节点最多 (maxRetries+1) 次尝试 × 最多 maxLoopIters 轮节点级循环;
//   - loop-group 再乘组级最大轮次;
//   - 最后乘 budgetSlack。
//
// 返回 0 表示算不出上界 (不该发生, 保守起见 = 不限制而不是限死)。
func nodeRunCeiling(spec graph.GraphSpec) int {
	base := len(spec.Nodes)
	if base == 0 {
		return 0
	}
	// 动态展开/扇出会让运行图长大, 上界就是总量闸。
	if specHasDynamicGrowth(spec) {
		limit := spec.Policies.MaxTotalNodes
		if limit <= 0 {
			limit = graph.DefaultMaxTotalNodes
		}
		if limit > base {
			base = limit
		}
	}

	retries := 0
	if spec.Policies.DefaultRetry != nil {
		retries = spec.Policies.DefaultRetry.MaxRetries
	}
	loopIters, groupIters := 1, 1
	for _, n := range spec.Nodes {
		if n.Retry != nil && n.Retry.MaxRetries > retries {
			retries = n.Retry.MaxRetries
		}
		if n.Loop != nil && n.Loop.MaxIterations > loopIters {
			loopIters = n.Loop.MaxIterations
		}
		// GroupPolicy.Loop 是值不是指针 (组循环策略必填), 轮次在它里面。
		if n.Group != nil && n.Group.Loop.MaxIterations > groupIters {
			groupIters = n.Group.Loop.MaxIterations
		}
	}

	ceiling := base * (retries + 1) * loopIters * groupIters * budgetSlack
	if ceiling <= 0 { // 溢出保护: 宁可不限制也不要限出一个负数/0 把整图拒死
		return 0
	}
	return ceiling
}

// specHasDynamicGrowth 图是否可能在运行期长大 (map 扇出 / 动态展开 / 组循环)。
func specHasDynamicGrowth(spec graph.GraphSpec) bool {
	for _, n := range spec.Nodes {
		if n.Map != nil || n.Expand != nil || n.Group != nil {
			return true
		}
	}
	return false
}
