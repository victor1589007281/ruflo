// refsweep.go —— 折叠引用目录的 TTL/上限清理 (H2-3)。
//
// # 为什么需要
//
// 引用名是内容的 sha256 (见 refstore.go), 所以**同一条** tool_result 重复折叠只占
// 一份文件。但"去重"约束的是内容的份数, 不是目录的总量: 一个工作区跑上几十个 run,
// 每个 run 折几百条不同的工具输出, 目录就是几万份文件, 只增不减。这是内容寻址解决
// 不了的那一半 —— 它挡的是重复, 不是总量。
//
// # 与工具层清理 (pkg/tool/orchestration.go) 的关键差别
//
// 工具层清理的是**执行期溢出**的 artifact: 结果已经完整交给模型了, 文件是事后留的
// 底, 删掉顶多是"以后想看没了"。折叠引用不同 —— 占位文本里写着 `full_artifact: <路径>`,
// 而**那条占位还在活的上下文里**。删掉文件, 活引用立刻变成悬空引用: 模型照占位给的
// 路径去 Read, 拿到 ENOENT, 白花一轮才发现要重跑工具 —— 正是落盘机制本来要消灭的那个
// 循环, 被清理机制重新制造出来。
//
// 所以这里比工具层多一条**硬约束**: 本进程发出去过的名字, 一律不删 (见 issued)。
// 代价是这个进程自己折出来的引用不受上限约束 —— 那是**如实交换**, 不是疏漏:
// 一个会话真折了两千条不同的工具输出, 它就有两千个活引用, 此时"压到一千条"和
// "让一半引用悬空"是同一件事。超上限这件事因此必须**被报出来** (CapSkipped), 而不是
// 静默发生 (见验收方法论: 不许有静默的上限)。
//
// 跨进程那一半仍然是有效的: claude-go 每个 run 一个新进程, 新进程的 issued 从空开始,
// **上一个 run 留下的引用全部是可删的陈旧文件** —— 计划里的验收 ("N 次重复 run 后
// 目录 ≤ 上限") 正是靠这一条成立。所以验收测试必须用 N 个**独立存储**模拟 N 个进程,
// 而不是一个存储反复 Put (后者测的是上一条硬约束, 方向相反)。两个方向都有测试。
//
// # 残留窗口 (说清楚, 不假装没有)
//
// 一个**上一次进程**发出、而占位仍留在**被恢复的会话**里的引用, 不受 issued 保护。
// 默认参数 (30 天 / 1000 条) 下这个窗口很窄: 年龄规则够不着新鲜文件, 条数规则要
// 目录超过 1000 份才会动手。把上限调到很小、或把 TTL 调到几小时, 窗口就会张开。
//
// # 环境变量
//
//	CLAUDE_GO_COMPACT_REF_MAX_FILES     条数上限, 默认 1000, 0 = 不限
//	CLAUDE_GO_COMPACT_REF_MAX_AGE_DAYS  天数上限, 默认 30,  0 = 不限
//
// 默认值与工具层一致, 但**变量名刻意不复用**工具层的: 两个目录的语义不同 (上面那段
// 差别), 共用一个旋钮会让"给溢出 artifact 调小上限"顺手把活引用也一起削掉, 而调的人
// 不会知道。
package compact

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
)

const (
	envRefMaxFiles   = "CLAUDE_GO_COMPACT_REF_MAX_FILES"
	envRefMaxAgeDays = "CLAUDE_GO_COMPACT_REF_MAX_AGE_DAYS"

	defaultRefMaxFiles   = 1000
	defaultRefMaxAgeDays = 30

	// 两次清理的最小间隔。每次 Put 都 ReadDir 不划算, 而目录涨到需要清理的量级是
	// 分钟级的, 十秒一次足够及时。与工具层取同一个值, 理由相同。
	refSweepInterval = 10 * time.Second
)

// lastRefSweep 上次清理时间 (UnixNano)。进程内共享 —— 目录按工作区划分, 同一进程里
// 多个存储实例指向同一个目录, 各扫各的没有意义。
var lastRefSweep atomic.Int64

// RefSweepStats 一次清理的读数。
//
// **Ran 与"删了几个"是两件事**: 一次扫过目录但一个都没删 (全部是活引用, 或都还新鲜)
// 与"根本没扫"在磁盘上完全同形, 而它们的含义相反 —— 前者说明上限已经开始吃紧, 后者
// 说明还没轮到。只报"删了几个"的组件, 成功与没运行逐字节相同。
//
// 这个读数是**机器可读的**: 日志只在真删了东西时才打 (与工具层一致, 避免十秒一条
// 噪声), 所以"清理到底跑没跑、被保护挡掉多少"只能从这里问。
type RefSweepStats struct {
	// Ran 这次调用真的扫了目录 (节流挡住时为 false, 见 LastSweep 的语义)。
	Ran bool
	// Scanned 目录里读到的常规文件条数。
	Scanned int
	// FilesBefore / FilesAfter 清理前后的文件条数。
	FilesBefore int
	FilesAfter  int
	// RemovedAge / RemovedCap 分别由年龄规则与条数规则删掉的条数。
	RemovedAge int
	RemovedCap int
	// Protected 目录里属于**本进程发出**因而免删的条数。
	Protected int
	// CapSkipped 条数规则**本想删但因为是活引用而放过**的条数。
	//
	// 非零 = 这次清理没能把目录压到上限, 而原因是保护而不是配额。这是本文件里唯一
	// 一个"没做到"的读数, 必须报出来: 它非零时, 磁盘增长不是配置没生效, 是活引用
	// 真的多到了那个量级。
	CapSkipped int
}

// refLimitFromEnv 读一个整数配置。
//
// 配错 (非数字 / 负数) 退回默认值而不是当成"不限" —— 与工具层同一条纪律: 一个拼错
// 的值不该静默把清理关掉, 那是配置错误里最难发现的一种 (关掉清理的后果要几周后才
// 显形)。**显式写 0 才是"不限"**, 它是合法取值。
func refLimitFromEnv(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		logging.For("compactref").Warn("清理配置无效, 用默认值", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

// maybeSweepRefs 节流后执行一次清理。由 Put 在写完文件之后调用。
//
// 节流是进程级的 (lastRefSweep): 目录属于工作区, 同一进程里多个存储实例指向它,
// 各扫各的只会让 ReadDir 变成每 Put 一次的成本。
func maybeSweepRefs(s *ToolResultRefStore) {
	if s == nil {
		return
	}
	now := time.Now()
	if prev := lastRefSweep.Load(); prev != 0 && now.Sub(time.Unix(0, prev)) < refSweepInterval {
		return
	}
	// 竞争失败也不重试: 另一个 goroutine 正在扫, 让它扫完就是。
	if !lastRefSweep.CompareAndSwap(lastRefSweep.Load(), now.UnixNano()) {
		return
	}
	s.sweep(refLimitFromEnv(envRefMaxFiles, defaultRefMaxFiles),
		refLimitFromEnv(envRefMaxAgeDays, defaultRefMaxAgeDays), now)
}

// sweep 执行一次清理并返回读数。单独拆成方法 (而不是埋在 maybeSweepRefs 里) 是为了
// 让测试能绕过节流直接驱动它 —— 否则每条用例都得跟一个十秒的时间窗赛跑。
//
// 两条规则独立生效, 不是二选一: 先按年龄删, 再按条数删最旧的。
func (s *ToolResultRefStore) sweep(maxFiles, maxAgeDays int, now time.Time) RefSweepStats {
	st := RefSweepStats{Ran: true}
	if s == nil {
		return RefSweepStats{}
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		// 目录还不存在 (= 这个工作区从没折过东西) 是最常见的一种, 不是错误。
		s.recordSweep(st)
		return st
	}

	type refFile struct {
		name string
		mod  time.Time
	}
	items := make([]refFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue // 不碰子目录: 目录结构不是本机制建的, 语义不明
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, refFile{name: e.Name(), mod: info.ModTime()})
	}
	st.FilesBefore, st.Scanned = len(items), len(items)

	// 保护集合先算出来, 与"这次开了哪几条规则"无关 —— 否则把年龄规则关掉
	// (maxAgeDays=0) 时 Protected 会报 0, 读起来像"没有活引用", 而实际是"没数"。
	prot := s.issuedNames()
	for _, it := range items {
		if prot[it.name] {
			st.Protected++
		}
	}

	live := items[:0:0]
	if maxAgeDays > 0 {
		cutoff := now.AddDate(0, 0, -maxAgeDays)
		for _, it := range items {
			// 保护在年龄判定**之前**: 一条刚写出去、但因为时钟跳变或测试摆布而
			// mtime 很旧的活引用, 照样不能删。判据是"发没发出过", 不是"看起来旧不旧"。
			if prot[it.name] {
				live = append(live, it)
				continue
			}
			if it.mod.Before(cutoff) {
				if os.Remove(filepath.Join(s.dir, it.name)) == nil {
					st.RemovedAge++
				}
				continue
			}
			live = append(live, it)
		}
	} else {
		live = append(live, items...)
	}

	if maxFiles > 0 && len(live) > maxFiles {
		// 按修改时间升序, 从最旧的开始删到刚好剩 maxFiles 条。
		sort.Slice(live, func(i, j int) bool { return live[i].mod.Before(live[j].mod) })
		for i := 0; i < len(live)-maxFiles; i++ {
			// 活引用到此为止: 宁可超上限也不让一条在上下文里被指着的路径消失。
			// 放过几个就记几个, 让"没压下去"这件事有读数可查。
			if prot[live[i].name] {
				st.CapSkipped++
				continue
			}
			if os.Remove(filepath.Join(s.dir, live[i].name)) == nil {
				st.RemovedCap++
			}
		}
	}

	st.FilesAfter = st.FilesBefore - st.RemovedAge - st.RemovedCap
	s.recordSweep(st)

	if st.RemovedAge+st.RemovedCap > 0 {
		logging.For("compactref").Info("折叠引用清理",
			"dir", s.dir, "按年龄", st.RemovedAge, "按条数", st.RemovedCap,
			"活引用", st.Protected, "因保护未删", st.CapSkipped, "剩余", st.FilesAfter)
	}
	return st
}

// issuedNames 复制一份活引用集合。
//
// 复制而不是直接把 map 交出去: 清理过程要读文件系统 (慢, 且可能阻塞), 不该握着锁跑。
// 集合本身**只增不减** —— 一个名字只要发出过就永远是活的: 折叠占位不会因为"后来又
// 折了一次"而失效, 它得等到整个会话结束才谈得上消失, 而那时进程已经换了。
func (s *ToolResultRefStore) issuedNames() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.issued) == 0 {
		return nil
	}
	out := make(map[string]bool, len(s.issued))
	for k := range s.issued {
		out[k] = true
	}
	return out
}

// markIssued 记下一个已发出去的引用名。
func (s *ToolResultRefStore) markIssued(name string) {
	s.mu.Lock()
	if s.issued == nil {
		s.issued = make(map[string]bool, 64)
	}
	s.issued[name] = true
	s.mu.Unlock()
}

// recordSweep 保存最近一次读数。
func (s *ToolResultRefStore) recordSweep(st RefSweepStats) {
	s.mu.Lock()
	s.lastSweep = st
	s.mu.Unlock()
}

// LastSweep 返回最近一次清理的读数 (nil 存储返回零值, Ran 为 false)。
//
// 存在的理由就是上一节说的那件事: 日志只在真删了东西时打, 所以"清理跑过但一个没删"
// 与"清理从没跑过"必须有一个地方能分开问。
func (s *ToolResultRefStore) LastSweep() RefSweepStats {
	if s == nil {
		return RefSweepStats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSweep
}

// SweepNow 立刻执行一次清理 (不走节流), 返回读数。
//
// 给两个场景用: 测试直接驱动; 以及进程退出前想把目录收一下的地方 —— 那种地方不能
// 容忍"被十秒节流挡掉"。
func (s *ToolResultRefStore) SweepNow() RefSweepStats {
	if s == nil {
		return RefSweepStats{}
	}
	return s.sweep(refLimitFromEnv(envRefMaxFiles, defaultRefMaxFiles),
		refLimitFromEnv(envRefMaxAgeDays, defaultRefMaxAgeDays), time.Now())
}
