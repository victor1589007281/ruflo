package agent

// blackboard_watch.go —— 黑板 `Watch` 的**生产订阅方** (design/01 §六 标 🟡 的那行:
// "黑板 Watch —— 实现与语义齐 (缓冲 64/非阻塞派发/WatchDrops), 但**生产零订阅方**")。
//
// ---------------------------------------------------------------------------
// 设计要的归宿, 与"谁能真的当订阅方"
// ---------------------------------------------------------------------------
//
// §4.11 记的归宿是: "dashboard SSE、飞书进度播报订阅之, 取代 `updateHeartbeat`
// 回填 team.json 的轮询观测 (teams.go)"。逐个核实这两个候选:
//
//	dashboard SSE   ❌ 接不上。`pkg/dashboard.Provider` 是**纯磁盘**实现
//	                (provider.go 全是 `p.pathIn(...)` + `os.ReadFile`), 它连
//	                `*ProductionTeam` 都拿不到, 更拿不到进程内的 `*Blackboard`。
//	                要接就得把团队管理器的引用注进 dashboard —— 那是 M4 的接线,
//	                且会给一个只读观测面开一条持有可变运行态的口子。
//	飞书进度播报     ✅ 接得上, 就是本文件。团队执行与 `notify` 回调同在
//	                `ProductionTeamManager` 里 (executeWorkflow), 拿 `team.Blackboard`
//	                是本地字段访问。
//
// 所以本轮只接了**一个**订阅方, 且它接的是"进度"这一路信号。
//
// ---------------------------------------------------------------------------
// 它到底取代了什么 (以及为什么默认关)
// ---------------------------------------------------------------------------
//
// 改造前团队内部进度只有一条路出来: `Coordinator.heartbeatLoop` 每 30 秒
// (HeartbeatFreq) 醒一次 → `checkTeamHealth` → `team.updateHeartbeat(快照)` → 落盘。
// 这是**轮询**: 一个 8 秒就跑完的阶段在 team.json 上可能一次都没出现过, 而
// dashboard / `team status` 读的就是 team.json。
//
// 订阅黑板是**推**: 每个阶段一落 `<stage>-status`, 进度立刻更新。两条路的信号源
// 其实是同一批事件, 差别只在延迟。
//
// ⚠️ 默认关 (`CLAUDE_GO_BOARD_WATCH`)。三个理由, 都不是保守:
//
//  1. **它改写盘频率**。`updateHeartbeat` 内部 `persist()` 是一次 team.json 全量
//     覆盖写。轮询下每 30 秒一次; 推送下变成"每个阶段一次"。8+ 下游平台里有按
//     team.json mtime 判断"团队是否有动静"的用法, 写盘节奏是可感知行为。
//  2. **它改 `Progress.Phase` 的取值**。轮询回填的 Phase 来自 Coordinator
//     (`"LLM生成"`/`"编译"` 这类执行相位), 而黑板 key 给出的是**阶段名**
//     (`article-writing`)。两者语义不同, 混在同一个字段里会让既有展示"忽然换了
//     一套词表"。本文件因此**只在 Coordinator 尚无进展可报时**才写 Phase, 见 apply。
//  3. 关的时候必须真的什么都不做: 未开启时**连 Watch 都不调**, 于是
//     `bb.watchers` 恒空 ⇒ `notifyLocked` 第一行 `len(bb.watchers)==0` 直接返回
//     ⇒ 黑板写入路径上一次 select 都不做, 逐字节等价于改造前。
//
// ---------------------------------------------------------------------------
// 为什么不用更直觉的两种做法
// ---------------------------------------------------------------------------
//
//  1. **不在 `Blackboard.Write` 里直接调回调**(省掉整个 Watch 机制)。回调会在
//     **持写锁**的临界区里同步执行 (`writeLocked` → `notifyLocked` 都在 `bb.mu` 内),
//     于是一个回调里的 `persist()` 就把黑板写锁按住整个写盘时间, 而黑板写入在交付
//     主路径上。`Watch` 的非阻塞派发 + 缓冲 64 正是为此设计的, 绕开它等于把它的
//     设计目的作废。
//  2. **不订阅 `Watch("<stage>-status")` 这样的精确前缀**。前缀是 key 的**开头**,
//     而阶段名在开头、`-status` 在结尾 —— 精确前缀表达不了"任意阶段的 status"。
//     订阅全部 (`""`) 再按 category 过滤: `category == "progress"` 正是 pipeline
//     (workflow.go)、图路径 (graph_adapter.go)、novel-v2/v3 三条产生方**共用**的
//     分类, 一处过滤覆盖全部路径。

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
)

// boardProgressCategory 进度类条目的分类名。
//
// 三条产生方 (workflow.go:904 / graph_adapter.go / workflow_novel_v2.go:321) 写的
// 都是这个字面量。取常量是为了下一个人改分类名时能一次改全 —— 现在它散在四处。
const boardProgressCategory = "progress"

// boardStatusSuffix 进度条目 key 的后缀 (`<阶段名>-status`)。
const boardStatusSuffix = "-status"

// boardWatchEnabled 黑板订阅开关 (默认关, 理由见文件头)。
func boardWatchEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_GO_BOARD_WATCH"))) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

// teamBoardWatcher 一次团队运行的黑板进度订阅方 (design/01 §4.11)。
type teamBoardWatcher struct {
	team  *ProductionTeam
	coord *Coordinator
	ch    <-chan BoardEntry

	stopOnce sync.Once
	done     chan struct{}

	// seen 已处理的进度条目数 (仅供收尾日志与测试断言)。
	mu   sync.Mutex
	seen int
}

// startTeamBoardWatcher 起一个黑板进度订阅方。返回 nil 表示未开启/无黑板 ——
// 调用方对 nil 调 stop() 是安全的 (方法有 nil receiver 保护)。
func startTeamBoardWatcher(team *ProductionTeam, coord *Coordinator) *teamBoardWatcher {
	if team == nil || team.Blackboard == nil || !boardWatchEnabled() {
		// 关闭时**不调 Watch**: 见文件头理由 3 (bb.watchers 恒空 ⇒ 派发路径零开销)。
		return nil
	}
	w := &teamBoardWatcher{
		team:  team,
		coord: coord,
		ch:    team.Blackboard.Watch(""),
		done:  make(chan struct{}),
	}
	go w.loop()
	return w
}

// loop 消费订阅通道直到它被 StopWatch 关闭。
//
// 退出条件是**通道关闭**而不是一个额外的 stop chan: `StopWatch` 先从派发表摘除
// 再 close (全程持写锁), 所以 `range` 收到关闭就意味着"再也不会有新事件了" ——
// 用 stop chan 的话 select 可能在通道里还有缓冲事件时就退出, 那些事件静默丢失
// 而 WatchDrops 也不会计数 (它只计派发时满了的)。
func (w *teamBoardWatcher) loop() {
	defer close(w.done)
	for e := range w.ch {
		w.apply(e)
	}
}

// apply 把一条黑板条目折成进度推送。
func (w *teamBoardWatcher) apply(e BoardEntry) {
	if e.Category != boardProgressCategory {
		return
	}
	stage := strings.TrimSuffix(e.Key, boardStatusSuffix)
	if stage == e.Key {
		// 不是 `<阶段>-status` 形态 (novel-v3 的章节键也是 progress 分类但另一形态):
		// 不猜, 直接忽略。猜错的后果是把一个章节号当成阶段名写进 Progress.Phase。
		return
	}
	w.mu.Lock()
	w.seen++
	w.mu.Unlock()

	// Coordinator 的进展是**更权威**的一路 (它带执行相位与轮次), 所以这里只做增量:
	// 取它的当前快照, 仅在 Phase 为空时用阶段名补位, 并把 UpdatedAt 刷成"刚才"。
	//
	// 为什么不直接 `coord.ReportProgress(stage, ...)`: 那会把 Coordinator 的
	// L2 停滞判据 (phase/iteration 不变即停滞) 的输入换成阶段名, 于是一个在同一
	// 阶段里空转 40 分钟的团队会因为"阶段名没变但 UpdatedAt 一直在刷"而**永远不被
	// 判停滞** —— 那是把一个 fail-closed 的守护改成 fail-open, 红线。
	prog := ProgressState{}
	if w.coord != nil {
		prog = w.coord.CurrentProgress()
	}
	if prog.Phase == "" {
		prog.Phase = stage
	}
	prog.UpdatedAt = time.Now()
	w.team.updateHeartbeat(prog)
}

// stop 取消订阅并等消费 goroutine 收敛。nil receiver 安全 (未开启时调用方拿到 nil)。
func (w *teamBoardWatcher) stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		w.team.Blackboard.StopWatch("", w.ch)
		<-w.done
		w.mu.Lock()
		seen := w.seen
		w.mu.Unlock()
		// drops 一起打出来: 缓冲 64 满时黑板会丢事件并计数, 而"进度少了几条"
		// 从结果上完全看不出来 —— 不打这个数就等于让降级不可观测。
		logging.For("teams").Info("黑板进度订阅收尾", "team", w.team.Name,
			"applied", seen, "board_drops", w.team.Blackboard.WatchDrops())
	})
}
