// runner_state.go —— 隔离运行 (RunIsolated) 的中间态持久化 (H3-2)。
//
// # 缺口在哪
//
// RunIsolated 的文档注释里写着两条自我豁免:
//
//   - 不持久化 SessionStore / Trajectory
//   - 不应用 Phase 1 的 autoCompact / microCompact (假设 stage 输出量适中)
//
// 两条在团队路径上都不成立。CLI 的 cliAgentRunner 把做事型角色 (coder/tester/
// reviewer) 的 MaxTurns 设成 150, 实测 "Phase1+Phase2 全量实现 6 分多钟才跑了
// 30 turns" —— 也就是这条路径上**常态**就是上百轮、几十万 token。两条豁免叠在
// 一起之后:
//
//  1. 消息历史只在内存里。进程一死 (OOM / 部署 / Ctrl-C), 前面 80 轮的读文件、
//     编译、试错全部归零, 重启后从零再来一遍 —— 而那恰恰是长程任务里最贵的部分。
//  2. 没有任何折叠。上下文一直涨到 API 硬拒 (prompt too long), 而 PTL 应急压缩
//     挂在 **HookChain** 上 (PhaseOnRecovery), RunIsolated 不走 HookChain ——
//     于是连那条兜底路径也不通。跑得越久的 run 越会撞死在这里, 撞死的方式是
//     一次 400, 而不是"被截断了但还在跑"。
//
// # 落盘的是消息数组本身
//
// 抽取的任务清单是一种更讨巧的做法 (只存"还剩什么"), 但隔离运行的产出**就是**
// 它走过的历史: coder 阶段的下一步取决于它刚刚读过哪个文件、编译报了什么错,
// 那些都不在清单里。所以这里存整份 messages —— 磁盘换正确性。上限
// (runStateMaxBytes) 是唯一的例外, 见那里的注释。
//
// # 恢复是对账, 不是盲信
//
// 文件按 run 的稳定身份命名, 但同名不等于同一次运行 (详见 RunKey)。加载时逐项
// 比对 Team/Workflow/Stage/Role/Cwd, 对不上就**当作全新运行**丢掉磁盘态。
// 这个方向的错是安全的: 丢掉的代价是重跑, 而反过来 (接上别人的历史) 会让模型
// 带着一段与当前任务无关的上下文继续做, 且它的 prompt 看上去完全正常 ——
// 那是一种查不出来的错。
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

// runStateVersion 落盘格式版本。读到不认识的值一律当全新运行 —— 与其猜怎么迁移,
// 不如重跑一次 (隔离运行的中间态是**缓存**不是账本, 丢掉永远是正确的退路)。
const runStateVersion = 1

// runStateMaxBytes 落盘上限。超了就不写, 并明确记一条日志。
//
// 为什么要有上限: messages 里可能夹着几十条几百 KB 的 tool_result, 无上限地写
// 会把一个 run 的状态文件推到几百 MB —— 而它每轮都要重写一次。上限取 32MiB:
// 正常的 100+ 轮编码任务远低于它 (折叠后的历史更是只有几万字符)。
//
// 超限时**不静默**: 记 Warn 并让 Save 返回 false, 于是"这次 run 没有中间态"
// 在读日志时是看得见的, 而不是要等到崩了重启才发现盘上什么都没有。
const runStateMaxBytes = 32 << 20

// RunState 一次隔离运行的中间态。
type RunState struct {
	Version int `json:"version"`
	// Key 本状态所属的 run 身份 (见 RunKey), 冗余存一份用于自检。
	Key string `json:"key"`
	// —— 身份五元组: 恢复时逐项比对, 任一不符即丢弃 ——
	Team     string `json:"team,omitempty"`
	Workflow string `json:"workflow,omitempty"`
	Stage    string `json:"stage,omitempty"`
	Role     string `json:"role,omitempty"`
	Cwd      string `json:"cwd,omitempty"`

	// Objective 本 run 的起始目标 (首条 user 消息)。仅用于事后归因 —— 恢复与否
	// 不看它: 团队重试时同一个 stage 的 prompt 可能带上新的失败反馈, 那正是最
	// 该恢复的一次 (前面几十轮的探索仍然有效)。
	Objective string `json:"objective"`
	// Turn 已完成的轮数 (下次从这里继续)。
	Turn     int `json:"turn"`
	MaxTurns int `json:"maxTurns"`
	// Model 上次用的出站模型名。跨模型恢复是有风险的 (约定与工具用法都可能变),
	// 所以不带逻辑, 只落下来给人看。
	Model string `json:"model,omitempty"`

	// Messages 完整的历史 (含 tool_use / tool_result)。
	Messages []types.Message `json:"messages"`
	// LastText 最近一次 assistant 文本 —— 被杀死时它是唯一"模型已经说到哪了"的痕迹。
	LastText string `json:"lastText,omitempty"`
	// Folds 本 run 已发生的整段折叠次数 (恢复后继续累加)。
	Folds int `json:"folds"`
	// Resumes 本 run 被恢复过几次。
	Resumes int `json:"resumes"`
	// HandoffUsed 本 run 采纳了上一次留下的交接说明 (H3-3)。
	//
	// 落这一位是为了让"交接到底有没有被用上"成为一个**事后可查**的读数: 交接说明
	// 的效果是"少探索", 而那是一个关于行为差异的断言, 只能靠对比两次 run 的读数
	// 得到 —— 没有这一位, 就只剩"文件在不在"这一个装配态的判据, 而文件在不在与
	// 它有没有进上下文是两件事。
	HandoffUsed bool `json:"handoffUsed,omitempty"`

	UpdatedAt time.Time `json:"updatedAt"`
	// Finished 完成标记。**完成时写 true 而不是删文件**: "跑完了"与"崩在半路"
	// 必须分得开 —— 删掉的话, 下次启动看到一个不存在的文件, 与"从没跑过"同形,
	// 于是失败的 run 会被当成全新的重跑, 而成功的 run 的产物也就无从审计。
	Finished bool `json:"finished"`
	// Error 终态错误 (被杀死时为"进程没了", 这里通常为空 —— 那种情况看不到 defer)。
	Error string `json:"error,omitempty"`
}

// RunKey 一次隔离运行的**稳定身份**。
//
// "稳定"指跨进程重启不变, 因此**不能**用任何运行时生成的 ID (run UUID / span ID
// / pid): 重启后它们是新的, 永远匹配不上, 恢复也就永远不会发生 —— 而且这种失效
// 是静默的, 看上去一切正常。
//
// 用 Stage 而不是系统提示或 prompt 正文: 前者是编排层声明的稳定标识, 后两者会
// 随模型配置与上游反馈变动, 让键在"最需要恢复的那次"恰好变掉。
//
// 前缀固定 "isolated-", 为将来别的隔离入口 (若有) 留出区分位。
func RunKey(meta RunIdentity) string {
	h := sha256.New()
	for _, part := range []string{meta.Team, meta.Workflow, meta.Stage, meta.Role, meta.Cwd} {
		h.Write([]byte(part))
		h.Write([]byte{0}) // 分隔符: 没有它 ("ab","c") 与 ("a","bc") 会撞同一个键
	}
	sum := h.Sum(nil)
	return "isolated-" + hex.EncodeToString(sum[:8])
}

// RunIdentity 稳定身份的五个字段。空字段合法 (CLI 单跑没有 Team/Workflow),
// 但**五个全空**会让所有这类运行共用一个键 —— 见 IsEmpty 的用法。
type RunIdentity struct {
	Team     string
	Workflow string
	Stage    string
	Role     string
	Cwd      string
}

// Same 逐项比对身份 (恢复前的对账)。
func (a RunIdentity) Same(b RunIdentity) bool {
	return a.Team == b.Team && a.Workflow == b.Workflow &&
		a.Stage == b.Stage && a.Role == b.Role && a.Cwd == b.Cwd
}

// Label 身份的人读形式 —— 只进日志与报错消息, **不作任何判等依据**。
//
// 判等一律走 RunKey/Same (哈希五元组)。拿这个字符串当身份用会踩一个安静的坑:
// 它对空字段做了省略, 于是 (team="", stage="x") 与 (team="x", stage="") 可能拼出
// 相似甚至相同的串, 而它们是完全不同的两个写者。
//
// 五个字段全空时返回**空串**, 不再自己兜一个 "(无身份)": 那层兜底在真正的消费方
// (WriteConflictError.Error 的 `=="" ⇒ (未知)`) 已经有了, 两处各兜一层只会让
// 其中一层永远不被观察到 —— 变异测试把它改成返回空串, 全部用例照样绿, 因为它们
// 读到的消息一模一样。一个不改变任何可观察量的分支不是保险, 是让人以为这里有
// 一种"有身份但叫无身份"的状态。
func (a RunIdentity) Label() string {
	var parts []string
	for _, kv := range []struct{ k, v string }{
		{"team", a.Team}, {"wf", a.Workflow}, {"stage", a.Stage},
		{"role", a.Role}, {"cwd", a.Cwd},
	} {
		if strings.TrimSpace(kv.v) != "" {
			parts = append(parts, kv.k+"="+kv.v)
		}
	}
	return strings.Join(parts, " ")
}

// identityOf 从已落盘的状态里取回身份五元组。
func (s *RunState) identityOf() RunIdentity {
	return RunIdentity{Team: s.Team, Workflow: s.Workflow, Stage: s.Stage, Role: s.Role, Cwd: s.Cwd}
}

// SetRunStateDir 装配隔离运行的中间态目录 (nil / 空串 = 关闭, 与改造前一致)。
//
// 与 file-history 用同一个 stateDir: 两者都是"这个工作区上发生过什么"的旁路
// 账本, 分散到两个根目录只会让清理与备份漏掉一个。
//
// 这一个目录同时承载 H3-2 (中断恢复的中间态) 与 H3-3 (被迫停止时的交接说明):
// 两者是同一条 run 的两种收场, 身份、生命周期、清理时机完全一致, 拆成两个开关
// 只会多出一个"开了一个忘了开另一个"的静默半生效态 —— 而那正是这两项能力各自
// 要防的东西 (H3-2 怕崩了没东西可恢复, H3-3 怕停了没东西可交接)。
func (e *QueryEngine) SetRunStateDir(dir string) {
	if e == nil {
		return
	}
	e.runStateDir = strings.TrimSpace(dir)
}

// runFilePath 本 run 账本下某个子目录里的文件路径 (未装配时为空)。
//
// 空返回值是**唯一的关闭开关**: 所有落盘/读盘入口都先问它, 于是"没装配"与
// "装配了但写不进去"在代码里是同一个判断, 不会有一个入口漏掉。
func (e *QueryEngine) runFilePath(sub, name string) string {
	if e == nil || e.runStateDir == "" || name == "" {
		return ""
	}
	return filepath.Join(e.runStateDir, sub, name)
}

// runStatePath 状态文件路径 (未装配时为空)。
func (e *QueryEngine) runStatePath(key string) string {
	if key == "" {
		return ""
	}
	return e.runFilePath("run-state", key+".json")
}

// loadRunState 读回一次运行的中间态。
//
// 返回 nil 表示"没有可恢复的状态", 分三种情形, 且**都走同一条出口**: 文件不
// 存在 / 读不动 / 内容不认识 / 身份对不上。调用方对它们应当做同一件事 (从零
// 开始), 所以不区分 —— 唯一需要区分的是"文件在但身份不符", 那种情况留一条
// Info 日志, 因为它是"键撞了"的信号 (见 RunKey)。
func (e *QueryEngine) loadRunState(key string, id RunIdentity) *RunState {
	path := e.runStatePath(key)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil // 从没跑过, 或已被清理
	}
	var st RunState
	if err := json.Unmarshal(data, &st); err != nil {
		logging.For("engine").Warn("run-state 损坏, 按全新运行处理", "key", key, "err", err)
		return nil
	}
	if st.Version != runStateVersion {
		logging.For("engine").Info("run-state 版本不符, 按全新运行处理",
			"key", key, "got", st.Version, "want", runStateVersion)
		return nil
	}
	if st.Finished {
		// 上一次已经跑完了。同一个 stage 再次执行是**新一轮**(可能带着新反馈),
		// 不该把上一轮的完整历史接上去 —— 那会让模型看见一份已经交付过的草稿。
		return nil
	}
	if !st.identityOf().Same(id) {
		// 键撞了。这**不是**错误情形 —— 它是"两个运行共用了一个键"的读数,
		// 丢磁盘态是安全的那个方向 (重跑 vs 接上别人的上下文)。
		logging.For("engine").Info("run-state 身份不符 (键碰撞), 按全新运行处理",
			"key", key, "stored", st.identityOf(), "want", id)
		return nil
	}
	if len(st.Messages) == 0 {
		return nil
	}
	return &st
}

// save 原子落盘 (临时文件 + rename), 避免写一半崩溃留下半个 JSON。
//
// 返回 false 表示**这一次没写成功** (超限 / I/O 错误), 调用方据此记一条日志。
// 不返回 error: 中间态是旁路能力, 写不进去不该影响本次运行 —— 与 file-history
// 的 fail-soft 同一条纪律。
func (s *RunState) save(path string) bool {
	if path == "" || s == nil {
		return false
	}
	s.Version = runStateVersion
	s.UpdatedAt = time.Now()
	data, err := json.Marshal(s)
	if err != nil {
		return false
	}
	if len(data) > runStateMaxBytes {
		return false
	}
	return writeFileAtomic(path, data) == nil
}

// writeFileAtomic 原子地把 data 写到 path (临时文件 + rename)。
//
// 抽出来是因为 H3-3 的交接说明要用同一套语义 —— 而"两个地方各写一份原子落盘"
// 迟早会分叉成"一处原子的、一处不是", 后者只在进程恰好在写一半时被杀才暴露。
//
// rename 的原子性是这条纪律的全部理由: 直接 os.WriteFile 先截断再写, 中途被杀
// 留下的是半个文件, 而**读它的那次启动正好是崩溃之后的那一次** —— 也就是唯一
// 需要这份文件的那一次。
func writeFileAtomic(path string, data []byte) error {
	if path == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// 失败时把临时文件带走: 留一个 .tmp 会让"上一次写到一半"与"写完了"
		// 在目录列表里长得一样。
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// isolatedFoldMinMessages 触发折叠前至少要有多少条消息。
//
// 不用"估算 token 超阈值"作为唯一判据: AutoCompact 自己会判阈值, 这里挡的是
// 另一个东西 —— 消息太少时折叠只会把仅有的几条换成"摘要 + 尾部", 净损失。
// 12 条 ≈ 4 轮工具往返 + 尾部保护 6 条。
const isolatedFoldMinMessages = 12

// compactIsolated 对隔离运行的历史做两级折叠, 与主循环的 PhasePreCompact 同序。
//
// 顺序不能反 (先 micro 再 fold), 与主循环一致: micro 把大 tool_result 换成占位,
// 让这次估算更接近折叠后的大小; 反过来先 fold, 折完的边界上那段前缀不含
// tool_result (见 compactLocked 末尾的说明), micro 就白跑了。
//
// 返回折叠后的消息与本次是否真的折了 (用于记账与落盘)。
func (e *QueryEngine) compactIsolated(ctx context.Context, messages []types.Message, model string) ([]types.Message, bool) {
	if e.Compactor == nil || len(messages) < isolatedFoldMinMessages {
		return messages, false
	}

	before := len(messages)
	out, counts := e.Compactor.MicroCompactMessages(messages)
	folded := counts.Folded > 0

	// FoldIfNeeded 内部判阈值, 不需折叠时返回 (nil, nil) —— 与 AutoCompact 同约定。
	// 它出错 (摘要请求失败) 时**保持原历史不动**: 折叠是省钱的优化, 失败就该退回
	// 未折叠继续跑, 而不是把 run 打死。这和 PTL 应急压缩不同 (那边已经没有退路)。
	if foldedOut, err := e.Compactor.FoldIfNeeded(ctx, out, model); err != nil {
		logging.For("engine").Warn("隔离运行折叠失败, 保持原历史继续", "err", err)
	} else if foldedOut != nil {
		out = foldedOut
		folded = true
	}

	if folded {
		// 只记"折过一次 + 前后条数": 具体的收益读数由 Compactor 的 onCompact
		// 回调报 (H2-1), 那条路两条折叠路径共用。这里重复报一遍只会把同一个
		// 事实写两处, 而两处的口径迟早会分叉。
		logging.For("engine").Info("隔离运行已折叠历史",
			"messagesBefore", before, "messagesAfter", len(out))
	}
	return out, folded
}

// runStateFrom 用当前循环的现场构造/更新一份中间态。
func (e *QueryEngine) runStateFrom(st *RunState, messages []types.Message, lastText string, turn int, model string) {
	st.Turn = turn
	st.Model = model
	// 深拷贝一份再落盘: messages 是循环的活对象, 而 save 在 defer 里跑 —— 那时
	// 循环可能还有别的 goroutine 在读它 (工具结果追加)。JSON 序列化发生在 save
	// 内部, 这里不拷贝的话序列化会在锁外读一份正在被追加的切片。
	st.Messages = append([]types.Message(nil), messages...)
	st.LastText = lastText
}

// newRunState 造一份新的中间态。
func (e *QueryEngine) newRunState(key string, id RunIdentity, objective string, maxTurns int) *RunState {
	return &RunState{
		Version:   runStateVersion,
		Key:       key,
		Team:      id.Team,
		Workflow:  id.Workflow,
		Stage:     id.Stage,
		Role:      id.Role,
		Cwd:       id.Cwd,
		Objective: objective,
		MaxTurns:  maxTurns,
	}
}

// finishRunState 落终态。**先标 Finished 再写盘**, 所以一次写盘就够。
func (e *QueryEngine) finishRunState(st *RunState, finalText string, runErr error) {
	path := e.runStatePath(st.Key)
	if path == "" {
		return
	}
	st.Finished = true
	st.LastText = finalText
	if runErr != nil {
		st.Error = runErr.Error()
	} else {
		st.Error = ""
	}
	if !st.save(path) {
		logging.For("engine").Warn("run-state 终态落盘失败", "key", st.Key, "turn", st.Turn)
	}
	// H3-3: 跑完了就把上一次留下的交接说明清掉。
	//
	// 条件必须是 `runErr == nil` 而**不能**是"无条件清": 被迫停止的那一次 run 也走
	// 这个 defer (它 err != nil), 而无条件清会把同一次 run 刚写下的交接说明就地删掉
	// —— 于是"有交接物"这条验收在最该有它的那条路径上永远为假。顺序上 emitHandoff
	// 在函数体里跑、这里在 defer 里跑, 中间没有第二次机会。
	//
	// 为什么要清: 交接说明的语义是"我半路停下了, 下一步在这里"。一次成功的 run 之后
	// 这句话就是**假的** —— 留着它, 下一个同身份的 run 会读到"还有没做完的事"而
	// 去重做已经完成的活。它不是日志 (日志不能说谎), 是**一次性的交接单**;
	// "这个 run 干过什么"的凭据是 run-state 文件本身, 那个不清。
	if runErr == nil {
		e.clearHandoff(st.Key)
	}
}

// persistRunState 中途落盘。
func (e *QueryEngine) persistRunState(st *RunState) {
	path := e.runStatePath(st.Key)
	if path == "" {
		return
	}
	if !st.save(path) {
		// 明确报出来。"这次运行的中间态没落盘"与"落盘了"在磁盘上长得一样
		// (都是那个文件), 差别只在**内容旧**——不报的话要等崩了重启才发现。
		logging.For("engine").Warn("run-state 中途落盘失败 (超限或 I/O 错误)",
			"key", st.Key, "turn", st.Turn, "limit", runStateMaxBytes)
	}
}

// resumeNotice 恢复时插进历史的一段说明。
//
// 为什么必须有: 模型看到的是一段**没有起点**的对话 —— 它不知道自己是恢复来的,
// 于是可能把"上次跑到一半"理解成"任务本来就是这样", 或者重新从头做一遍已经做完
// 的事。这段文字是唯一能让它把当前状态接上的东西。
//
// 措辞刻意不含任何"下一步该做什么"的指令: 那是模型自己的判断, harness 只负责
// 告诉它"你以为的起点其实不是起点"。
const resumeNotice = "[harness] 本次执行是从上次中断处恢复的: 上面的对话是本 run 之前已经" +
	"完成的部分 (进程被杀, 工具结果与中间结论都已保留)。不要从头重做；" +
	"先确认当前进度, 再从断点继续。"

// appendResumeNotice 把说明插到恢复历史与新输入之间。
func appendResumeNotice(msgs []types.Message) []types.Message {
	out := append([]types.Message(nil), msgs...)
	return append(out, types.Message{
		Type: types.MessageTypeUser,
		Content: []types.ContentBlock{{
			Type: types.ContentBlockText,
			Text: resumeNotice,
		}},
		CreatedAt: time.Now(),
	})
}
