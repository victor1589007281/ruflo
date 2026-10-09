// sessionshard.go — 会话 ID 分片策略的 A/B 实验开关。
//
// # 背景: 会话 ID 同时决定两件事, 而这两件事的方向相反
//
// 出站请求带的 X-Claude-Code-Session-Id 在 opencode zen/go 上游不只是"标签":
//
//	(a) 前缀缓存分片键 —— 同一会话内前缀不变的部分命中同一份缓存;
//	(b) 路由亲和键   —— 上游按会话 ID 决定把请求落到哪个后端/可用区。
//
// 只按 RunID 分片 (现状) 时 (a) 最优: 一次 run 内所有轮次、所有节点共用一份
// 前缀缓存, 历史大头全命中。代价是 (b) 最差: 一次 run 的并发节点请求全部压在
// 同一个后端上, 而一次 run 内部本身是并行的 (团队多阶段)。
//
// 把 NodeID 也并进会话键 (实验臂) 后每个节点各占一个会话: 请求在网关后端上摊得
// 更开 (b 改善), 但同一 run 内不同节点之间不再共享缓存条目 —— 而不同节点的
// system 前缀本来就不同, **簇级**局部性是升是降不是先验可判的: 节点内命中率不变
// 而节点数变多, 总缓存条目数变多, 单条目的复用次数变少。这正是"反方向的力"。
//
// # 所以做成 A/B, 而不是直接改
//
// 分桶按 RunID 的哈希取模, **不是随机数**: 一次 run 横跨几十上百个请求, 每个请求
// 各自随机会导致同一次 run 的会话 ID 中途改变 —— 那既毁掉缓存, 也让实验组别在
// run 内部混杂, 两组都测不出东西。按 RunID 哈希则同一次 run 恒定落在同一臂。
//
// # 观测口径
//
// 每次请求带上 X-CG-Session-Arm 头 (纯函数产物, 可与 RunID 对账), 网关侧把它记进
// llm_call 日志。对比两臂的 cache_read / input 比例即可判定方向 —— 只看
// cache_read 绝对值没有意义, 两个臂的会话数量级本来就不同。
package api

import (
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

// SessionArm 是会话分片策略的实验臂。
type SessionArm string

const (
	// ArmRun: 会话键 = RunID。一次 run 一个会话 (现状行为, 对照组)。
	ArmRun SessionArm = "run"
	// ArmRunNode: 会话键 = RunID + NodeID。一次 run 的每个节点各一个会话 (实验组)。
	ArmRunNode SessionArm = "run+node"
)

// SessionArmHeader 出站请求上标识本请求落在哪一臂的头。
//
// 存在的意义是**归因不靠反推**: 臂是 RunID 的纯函数, 理论上离线重算即可, 但
// 重算要求分析方与产生方对哈希实现逐位一致 —— 一旦哪天换了哈希, 历史数据的
// 归属就变成一笔糊涂账。显式发头把这件事钉在证据里。
const SessionArmHeader = "X-CG-Session-Arm"

// sessionShardPercent 是**实验组**的占比 (0-100), 原子读。
//
// 0 = 全对照组 (= 关闭实验, 行为与改造前逐字节一致), 100 = 全实验组。
// 用原子量而非普通 int: 热加载可以中途改占比, 而读方在每个请求的 goroutine 上。
var sessionShardPercent atomic.Int64

// treatingRunBuckets 分桶空间。取 100 让"百分比"直接等于桶阈值, 省一次乘法。
const treatingRunBuckets = 100

// SetSessionShardPercent 设置实验组占比 (0-100), 超出范围会被夹到边界。
//
// 供配置热加载与测试调用; 生产默认由 InitSessionShardFromEnv 在进程启动时设置。
func SetSessionShardPercent(p int) {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	sessionShardPercent.Store(int64(p))
}

// SessionShardPercent 返回当前实验组占比。
func SessionShardPercent() int { return int(sessionShardPercent.Load()) }

// InitSessionShardFromEnv 从 CLAUDE_GO_SESSION_SHARD 读取实验配置。
//
// 取值:
//
//	""/"off"/"0"   → 关闭 (全对照组, 与改造前行为一致)
//	"1".."100"     → 实验组占比 (如 "50" = 一半 run 走 run+node)
//	"on"/"all"/"1" 之外的非数字 → 视为 100 (全实验组)
//
// 单独一个 "1" 有歧义 (占比 1% 还是"打开"): 按**数字优先**解释为 1%。
// 想表达"全开"用 "on"/"all"/"100"。
func InitSessionShardFromEnv() int {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_GO_SESSION_SHARD")))
	switch raw {
	case "", "off", "false", "no":
		SetSessionShardPercent(0)
	case "on", "all", "true", "yes":
		SetSessionShardPercent(100)
	default:
		n, err := strconv.Atoi(raw)
		if err != nil {
			// 认不出的值按**关闭**处理, 而不是打开: 实验臂是有代价的那一侧
			// (会话数变多), 拼错一个环境变量就悄悄全量切换是不能接受的。
			SetSessionShardPercent(0)
		} else {
			SetSessionShardPercent(n)
		}
	}
	return SessionShardPercent()
}

// armForRun 按 RunID 把一次 run 确定性地分到某一臂。
//
// FNV-1a 而非 crypto 哈希: 这里要的是均匀分桶, 不是抗碰撞。用 fnv 还顺带让
// "同一 RunID 在两台机器上算出同一臂" 成为一条不需要同步状态的性质。
func armForRun(runID string) SessionArm {
	if SessionShardPercent() <= 0 {
		return ArmRun
	}
	if SessionShardPercent() >= 100 {
		return ArmRunNode
	}
	if runID == "" {
		// 没有 RunID 的调用 (会话直连 / dashboard 诊断) 不参与实验: 它们本来
		// 就不带会话头, 分桶对它们没有可比的基线。
		return ArmRun
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(runID))
	if int(h.Sum32()%treatingRunBuckets) < SessionShardPercent() {
		return ArmRunNode
	}
	return ArmRun
}

// SessionArmForTrace 返回这条 trace 落在哪一臂 (供日志/归因使用)。
func SessionArmForTrace(t Trace) SessionArm { return armForRun(t.RunID) }

// sessionKeyForTrace 算出该 trace 应使用的会话 ID; 空串表示不发会话头。
//
// 与改造前的差异被严格限制在实验臂内 —— 对照组返回的就是 t.RunID, 一个字节不差。
func sessionKeyForTrace(t Trace) string {
	if armForRun(t.RunID) == ArmRunNode {
		// 实验臂: 节点也进会话键。NodeID 为空时退化为 RunID 单独成键
		// (没有节点可分的调用, 硬塞一个空段只会让键与对照组同形却不同值)。
		if t.NodeID != "" {
			return shardJoin(t.RunID, t.NodeID)
		}
		if t.RunID != "" {
			return t.RunID
		}
		return t.NodeID
	}
	// 对照组: 与改造前逐字节一致 —— 只认 RunID。
	if t.RunID != "" {
		return t.RunID
	}
	return ""
}

// shardJoin 把 RunID 与 NodeID 拼成会话键。
//
// 分隔符取 \x00 (不会出现在任何一个 ID 里): 用 ":" / "_" 这类可见字符会让
// ("a:b", "c") 与 ("a", "b:c") 拼出同一个键, 两个不相干的节点共享会话 ——
// 那正好把实验臂想测的"节点各自成会话"给抵消掉一部分。
func shardJoin(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\x00")
}
