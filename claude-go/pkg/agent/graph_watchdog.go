package agent

// graph_watchdog.go —— 图级停滞检测的**生产接线** (design/01 §4.3 双层 watchdog
// 里缺的那一层; 检测本体在 pkg/graph/watchdog.go)。
//
// ---------------------------------------------------------------------------
// 默认关, 且"关"是真的什么都不做
// ---------------------------------------------------------------------------
//
// 未设 `CLAUDE_GO_GRAPH_WATCHDOG` 时本文件返回 nil ⇒ `Policies.Watchdog` 为 nil
// ⇒ 引擎连巡检 goroutine 与 ctx 派生都不做 ⇒ 现存全部图逐字节不变。
//
// 为什么必须默认关: "多久没有 journal 事件算停滞"在本仓的真实负载上极难定 ——
// 整本小说起草、大仓索引、单章开发这些阶段几十分钟不产出中间事件是**正常**的
// (journal 事件的粒度是节点, 不是 turn)。一个默认开启并自动判失败的 watchdog
// 会把这些合法长阶段杀掉, 而 8+ 下游平台在用 :18080。
//
// ---------------------------------------------------------------------------
// 开关语义 (与既有 CLAUDE_GO_GRAPH_* 一族同形)
// ---------------------------------------------------------------------------
//
//	CLAUDE_GO_GRAPH_WATCHDOG=1|on|true    开启, 动作 notify (**只观测**)
//	CLAUDE_GO_GRAPH_WATCHDOG=fail         开启, 动作 fail (停滞即放弃本次运行)
//	CLAUDE_GO_GRAPH_WATCHDOG_STALL=15m    覆盖停滞阈值 (Go duration)
//	CLAUDE_GO_GRAPH_WATCHDOG_CHECK=30s    覆盖巡检间隔
//	CLAUDE_GO_GRAPH_WATCHDOG_MAX_NOTICES=1  最多判定几次 (0=不限)
//
// 干预 (`fail`) **只能靠显式写 "fail" 拿到**, `=1` 拿不到 —— 一个想"先看看"的人
// 不会因为写了 1 而意外获得中止团队的能力。
//
// 阈值缺省**沿用 coordinator.go 的既有数字**, 不另立一套:
//
//	停滞阈值 = watchdogProgressStaleThreshold (10min, L2 进展检测)
//	巡检间隔 = watchdogCheckInterval          (60s)
//
// 为什么取 L2 那两个而不是 L1 的 5min/15min: 图级这一层做的正是**进展检测**
// (journal 尾部无事件), 与 coordinator 的 L2 是同一件事的两种可观测量; L1 的
// 5min/15min 量的是 activity 心跳, 那是节点级那一层已经在用的。

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/graph"
)

// graphWatchdogSpec 从环境读图级 watchdog 声明。nil = 关闭 (缺省)。
//
// 返回值直接塞进 `GraphSpec.Policies.Watchdog`。**覆盖表里已声明的 watchdog 优先**
// (见调用点): 声明式覆盖比环境变量更具体, 反过来会让运维一设环境变量就把某张图
// 精心声明的阈值全部抹平。
func graphWatchdogSpec() *graph.WatchdogSpec {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_GO_GRAPH_WATCHDOG")))
	if raw == "" || raw == "0" || raw == "off" || raw == "false" {
		return nil
	}
	spec := &graph.WatchdogSpec{
		StallSec: int(watchdogProgressStaleThreshold / time.Second),
		CheckSec: int(watchdogCheckInterval / time.Second),
		Action:   graph.WatchdogNotify,
	}
	if raw == graph.WatchdogFail {
		spec.Action = graph.WatchdogFail
	}
	// 未知取值 (比如手抖写了 "yes") 退到 notify 而**不是**关闭: "我开了但它没起"
	// 比"我开了但只观测"难排障得多 —— 后者日志里立刻能看到 watchdog 的痕迹。
	// 真正危险的那一档 (fail) 仍然只认精确字面量, 所以退档方向是安全的。
	if d := envDuration("CLAUDE_GO_GRAPH_WATCHDOG_STALL"); d > 0 {
		spec.StallSec = int(d / time.Second)
	}
	if d := envDuration("CLAUDE_GO_GRAPH_WATCHDOG_CHECK"); d > 0 {
		spec.CheckSec = int(d / time.Second)
	}
	if n := envInt("CLAUDE_GO_GRAPH_WATCHDOG_MAX_NOTICES"); n > 0 {
		spec.MaxNotices = n
	}
	return spec
}

// applyGraphWatchdog 把环境声明的 watchdog 装进图 (已声明则不动)。
//
// 单独一个函数而不是在 runGraphSpec 里写三行: 图 spec 的来源有三条 (模板直译 /
// 覆盖表 / 动态注册), 而这一步必须发生在**它们汇合之后、Validate 之前** ——
// 放错位置的表现是"某些工作流的 watchdog 不生效"且毫无报错。
func applyGraphWatchdog(spec *graph.GraphSpec) {
	if spec == nil || spec.Policies.Watchdog != nil {
		return
	}
	spec.Policies.Watchdog = graphWatchdogSpec()
}

// envDuration 读一个 Go duration 环境变量 (解析失败 → 0, 即"没设")。
//
// 解析失败当没设而不是报错: 这几个变量是运维旋钮, 一个打错的 "10mm" 不该让整个
// 团队起不来 —— 而缺省值本身是安全的 (只观测)。
func envDuration(key string) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// envInt 读一个正整数环境变量 (解析失败/非正 → 0, 即"没设"; 理由同 envDuration)。
func envInt(key string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
