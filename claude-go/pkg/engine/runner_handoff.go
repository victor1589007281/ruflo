// runner_handoff.go —— 隔离运行被迫停止前的交接物 (H3-3)。
//
// # 缺口在哪
//
// H3-2 让隔离运行能扛住"进程被杀": 中间态每轮落盘, 重启后从断点继续。但它挡不住
// 另一种收场 —— run **活着**跑到了自己的上限 (token 预算耗尽 / maxTurns 用光), 这时
// 循环按设计返回, 进程好好地把整份历史留在内存里然后丢掉。
//
// 这种收场在长程任务上是常态而非例外 (150 轮的 coder stage 撞上限是预期内的),
// 而它丢掉的东西恰好是最贵的: 那 150 轮里读过的文件、编译报过的错、试过但不行的
// 方向。下一次执行拿到的是同一个 prompt 和**零上下文**, 于是从头把同样的弯路再走
// 一遍 —— 消耗一模一样, 只是这次可能没撞上限, 于是看起来"成功了", 没人会发现
// 前一半预算是在重复劳动。
//
// 对照口径: SWE-agent 的 1.1× 自动提交 —— 预算耗尽不是直接停, 而是留出一点余额
// 把当前工作提交成一个可被下一次利用的产物。
//
// # 为什么不是"把 H3-2 的中间态留着给下一次用"
//
// 因为那在语义上是错的, 而且错得静默。H3-2 的中间态是**同一次运行的现场**: 它被
// 恢复的前提是"上一次没跑完, 这次接着跑"。而跑到上限是**跑完了** —— 循环正常返回,
// 终态是 Finished。把一份 150 轮的原始历史接在"新一轮"的开头, 模型看到的是一份
// 自己刚写完的长对话, 既没有"这是上一轮"的边界, 也没有"哪些结论已经成立"的提炼;
// 它会以为任务已经做到一半然后开始改自己刚做的东西。
//
// 交接物是另一件东西: 一份**自足的、写给人读的**说明, 由模型在停下来的那一刻写,
// 下一次以"上一次的交接"的身份被注入。原始历史不传, 传的是模型自己的提炼。
//
// # 与 H3-2 的分工 (两条路互斥, 不是互补)
//
//	被中断 (进程被杀 / 超时)  ⇒ H3-2 恢复原始历史, 接着跑;
//	跑到上限 (预算 / 轮数)    ⇒ H3-3 交接说明, 下一次重跑但不必重新探索。
//
// 互斥是**结构保证**的: 交接说明只在 run 以错误收场时写, 而那种收场同时把
// run-state 标成 Finished, 于是下一次 loadRunState 必然返回 nil (见那里的守卫)。
// 两个都读到的情形在代码里不存在; 万一将来不存在的前提破掉了, RunIsolated 里的
// 分支顺序 (先恢复、恢复不到才读交接) 保证了历史那一路优先 —— 历史严格更丰富。
//
// # 落盘形态: markdown 而不是 JSON
//
// 兄弟文件 run-state 是 JSON, 因为它由程序反复读写。这一份不同: 它的**读者有两个**,
// 一个是下一个 run, 另一个是停下来看的人 (交接物本来就是这个意思)。markdown 让
// 后者可以直接 cat 出来看、贴进 PR 描述、交给接手的人。代价是读到的东西没法逐字段
// 校验 —— 所以身份校验退化成读首行那枚注释里的 key (见 loadHandoff)。
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

// handoffVersion 交接说明的格式版本, 写在首行注释里。
const handoffVersion = 1

// handoffMaxBytes 交接说明的落盘上限。超了就截断并标注 —— 交接物是**给下一次读的**,
// 一份 5MB 的说明比没有更糟: 它会挤掉下一次真正的工作上下文, 而那是它要省的东西。
const handoffMaxBytes = 256 << 10

// handoffLinePrefix 首行标记。没有它, loadHandoff 就没法把"我们自己写的"与
// "同名路径下碰巧存在的别的文件"分开 —— 而那种文件一旦被当成交接说明注入,
// 模型会拿一段来路不明的文本当上一次的进度 (见 loadHandoff)。
const handoffLinePrefix = "<!-- claude-go-handoff"

// handoffReasonMaxTurns 轮数上限终止的原因名。
//
// 与预算那条 (`internal_hook.RunBudgetExhaustedReason`) 并列而非合并: 两者在下一次
// 注入时都只是说明文字, 但在排查时"这次是被钱拦下的还是被步数拦下的"是两件事 ——
// 前者要调预算, 后者要调 maxTurns。给出同一个字符串等于把这条信息丢掉。
const handoffReasonMaxTurns = "max_turns"

// HandoffReason 终止原因 (写进交接说明, 供人与下一次执行阅读)。
type HandoffReason string

const (
	// HandoffByBudget token 预算耗尽。
	HandoffByBudget HandoffReason = HandoffReason(internal_hook.RunBudgetExhaustedReason)
	// HandoffByTurns 轮数上限用光。
	HandoffByTurns HandoffReason = HandoffReason(handoffReasonMaxTurns)
)

// handoffAt 写一份交接说明所需要的全部现场。
//
// 收成一个结构体而不是给 emitHandoff 开一长串参数, 是因为这些值在 RunIsolated
// 里散在三个作用域 (身份在开头、messages/turn 在循环里、systemPrompts 在中段),
// 逐个传参时最容易犯的错是**传错那个同类型的量** (model 与 role 都是 string,
// turn 与 maxTurns 都是 int), 编译器一声不吭。
type handoffAt struct {
	Key      string
	Identity RunIdentity
	// Objective 本次 run 的起始目标。
	Objective string
	Reason    HandoffReason
	// Detail 人读的终止细节 (上限/实耗), 直接进正文的一个条目。
	Detail   string
	Turn     int
	MaxTurns int
	Model    string

	// System / Messages / Client / MaxTokens 只用于**发起交接请求本身**。
	System    []string
	Messages  []types.Message
	Client    *api.Client
	MaxTokens int
}

// handoffPath 交接说明路径 (未装配目录时为空)。
func (e *QueryEngine) handoffPath(key string) string {
	if key == "" {
		return ""
	}
	return e.runFilePath("handoff", key+".md")
}

// emitHandoff 在 run 让出之前, 用一次**不带工具**的请求让模型写下"进度 + 下一步",
// 落盘成交接说明 (H3-3)。
//
// 全程 fail-soft: 这是 run 临终前多做的一件事, 它失败不该改变 run 的终态错误 ——
// 那会让"预算耗尽"变成"写交接失败", 把一个成本止损问题伪装成 I/O 问题。
//
// **一次不带工具的请求**是这个能力唯一多花的钱 (对照 SWE-agent 的 1.1×):
//   - 不带工具是必须的 —— 带了工具, 模型可能选择"再做一步"而不是交代, 于是交接说明
//     里全是新的 tool_use, 而下一步的进展不会落进说明 (它还没发生);
//   - 只发一次 —— 若这次也失败, 退回去的是一份没有交接说明的 run, 与改造前相同,
//     而不是一个重试环 (临终重试是拿数据换取一个"看起来做完了"的假象)。
//
// ctx 已取消时**不需要**在这里判: api.Client 的重试环在第一次上线之前就有同一句
// 判据 (pkg/api/client.go:1705 —— 在 http.NewRequestWithContext 与 Client.Do 两者
// 之前), 带取消的 ctx 到那里就返回了, 一个字节都不会出去。
//
// 我在这里写过一句 `if ctx.Err() != nil { return }`, 注释说的是"那次请求注定失败"。
// 变异测试把它整个删掉: 从传输层数到的上线次数、磁盘上的文件、落盘的日志三者逐字节
// 相同 —— 它是一个**不改行为的守卫**。删掉它不是因为那句话错了, 而是因为一个永远
// 不改变行为的分支只会让下一个读代码的人以为这条路上有保护, 而真实的保护在另一个
// 包里; 哪天那个判据被挪走, 这里不会跟着变, 也没有任何一处会报错。
func (e *QueryEngine) emitHandoff(ctx context.Context, in handoffAt) {
	if e == nil || e.runStateDir == "" || in.Key == "" || in.Client == nil {
		return
	}
	// 一次都没跑起来就没什么可交接的。写一份"我什么也没做"的说明书, 下一次读到会
	// 以为自己有一份进度 —— 比没有交接物更糟。
	if len(in.Messages) < 2 {
		return
	}

	req := append(append([]types.Message(nil), in.Messages...), types.Message{
		Type: types.MessageTypeUser,
		UUID: internal_hook.GenerateUUID(),
		Content: []types.ContentBlock{{
			Type: types.ContentBlockText,
			Text: handoffPrompt(in.Reason, in.Detail),
		}},
		CreatedAt: time.Now(),
	})

	start := time.Now()
	resp, err := in.Client.SendMessage(ctx, messagesToAPI(req, nil), in.System, nil, in.MaxTokens)

	// 这次调用也是真花钱的一次调用, 进轨迹账本。**写在错误返回之前**: 失败的交接
	// 调用同样是"这个 run 临终时花了什么"的一部分, 而那种调用恰恰是最需要能事后看到的
	// (它解释了为什么磁盘上没有交接说明)。Handoff 标记见 llmCallSpan 的注释。
	var summary, stop string
	var usage *types.Usage
	model := in.Model
	if resp != nil {
		summary = strings.TrimSpace(internal_hook.JoinAssistantText(resp.Content))
		usage, stop = resp.Usage, resp.StopReason
		if resp.Model != "" {
			model = resp.Model
		}
	}
	e.writeLLMCallSpan(ctx, llmCallSpan{
		Start: start, Model: model, System: in.System,
		Messages: messagesToAPI(req, nil), Tools: nil,
		Output: summary, Usage: usage, StopReason: stop, Err: err,
		Attempt: 0, Isolated: true, Handoff: true,
	})

	if err != nil {
		logging.For("engine").Warn("交接请求失败, 本次 run 不留交接说明",
			"key", in.Key, "reason", string(in.Reason), "err", err)
		return
	}
	if summary == "" {
		// 空说明与"没有说明"在下一次看来是同一件事, 但成因不同 (模型没说话 vs 没发请求),
		// 报出来才能分清。不写文件: 一份空文件会让下一次以为读到了交接。
		logging.For("engine").Warn("交接请求返回了空内容, 不落盘",
			"key", in.Key, "reason", string(in.Reason))
		return
	}

	// 到这里为止一个字节都没落盘 —— 正文仅仅在内存里。落盘是最后一步, 所以上面
	// 任何一处 return (请求失败 / 空内容) 都不会留下半份文件。
	//
	// 没有 else 分支: "没装配目录"在上面的入口守卫 (e.runStateDir == "") 就返回了,
	// 走到这里 handoffPath 必然非空。曾写过一个 `else { 记一条 Info }`, 而变异测试
	// 实测把那条路径整个删掉没有任何用例会红 —— 一个永远跑不到的分支不是保险,
	// 是让读者以为这里有一种"没有目录但仍在记账"的状态。(入口守卫上压着一条
	// TestH33_NoAssemblyMeansNoHandoff 的空目录臂, 那条是通电的。)
	doc := renderHandoff(in, summary)
	path := e.handoffPath(in.Key)
	if err := writeFileAtomic(path, []byte(doc)); err != nil {
		logging.For("engine").Warn("交接说明落盘失败",
			"key", in.Key, "path", path, "err", err)
		return
	}
	logging.For("engine").Info("已留下交接说明",
		"key", in.Key, "reason", string(in.Reason), "turn", in.Turn, "bytes", len(doc))
}

// renderHandoff 把现场与模型自述拼成交接说明正文。
//
// 首行是那枚注释标记 (见 handoffLinePrefix) —— 它承担身份校验, 所以**必须**是首行,
// 且 key 必须与文件名里的那一份相同。正文刻意是给人读的: 下一个接手的人 (或人)
// 要能只看这一份文件就明白"上次做到哪、为什么停、从哪继续"。
func renderHandoff(in handoffAt, summary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s v%d key=%s -->\n", handoffLinePrefix, handoffVersion, in.Key)
	fmt.Fprintf(&b, "# 交接说明: %s / %s\n\n", orNone(in.Identity.Stage, "(无 stage)"), orNone(in.Identity.Role, "(无 role)"))
	fmt.Fprintf(&b, "- 生成时刻: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "- 停止原因: %s%s\n", in.Reason, in.Detail)
	fmt.Fprintf(&b, "- 已用轮数: %d / 上限 %d\n", in.Turn, in.MaxTurns)
	if in.Model != "" {
		fmt.Fprintf(&b, "- 出站模型: %s\n", in.Model)
	}
	fmt.Fprintf(&b, "- 身份: team=%q workflow=%q stage=%q role=%q cwd=%q\n",
		in.Identity.Team, in.Identity.Workflow, in.Identity.Stage, in.Identity.Role, in.Identity.Cwd)
	fmt.Fprintf(&b, "\n## 本次目标\n\n%s\n", strings.TrimSpace(in.Objective))
	fmt.Fprintf(&b, "\n## 进度与下一步 (上一次执行自述)\n\n%s\n", summary)

	out := b.String()
	if len(out) > handoffMaxBytes {
		// 截断也要明说 —— 一份被砍掉结尾的交接说明读起来是"说完了", 而它没有。
		note := "\n\n> [harness] 交接说明超过上限已被截断, 上面的内容不完整。\n"
		cut := handoffMaxBytes - len(note)
		if cut < 0 {
			cut = 0
		}
		out = out[:cut] + note
	}
	return out
}

// orNone 空串给一个显式占位 —— 交接说明里 "(无 stage)" 与一片空白是不同的信息。
func orNone(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// handoffKeyOf 从首行里取出 key (取不到时为空串)。
//
// 按空白切分取 token, 而不是子串检索: 首行是 `<!-- claude-go-handoff v1 key=X -->`,
// 而 token 边界正是"key 到哪结束"的唯一可靠信号。
func handoffKeyOf(header string) string {
	for _, f := range strings.Fields(header) {
		if rest, ok := strings.CutPrefix(f, "key="); ok {
			return rest
		}
	}
	return ""
}

// loadHandoff 读回上一次留下的交接说明正文 (没有则返回空串)。
//
// 判据落在首行的标记与 key 上, 而不是"这个路径下有没有文件":
//
//   - 没有标记 ⇒ 不是我们写的。这个路径可能被别的工具或人占用了, 直接注入等于把
//     一段来路不明的文本交给模型当"上一次的进度";
//   - key 不符 ⇒ 文件被搬错了位置 (或身份与路径的对应关系被破坏)。虽然路径本身
//     由 key 算出来 (见 RunKey), 正常情况下不可能不符, 但这条守卫的成本是一次
//     字符串比较, 而它挡掉的错是"把 A 的进度当成 B 的"—— 事后无从归因的那一类。
//
// 读不动 (权限 / 竞态删除) 也返回空串: 交接到此为止, 下一次老老实实重新探索 ——
// 这是安全的那个方向。
func (e *QueryEngine) loadHandoff(key string) string {
	path := e.handoffPath(key)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := string(data)
	nl := strings.IndexByte(text, '\n')
	if nl < 0 || !strings.HasPrefix(text, handoffLinePrefix) {
		logging.For("engine").Warn("交接说明缺少首行标记, 不采用", "path", path)
		return ""
	}
	header := text[:nl]
	// key 必须**按 token 整体相等**, 不能用 strings.Contains(header, "key="+key):
	// 那样 key="isolated-abc" 会命中 key="isolated-abc-tail" 那份文件 —— 前缀命中
	// 没有边界, 而这两份文件的主人不是同一个身份。第一版正是这么写的, 被
	// TestH33_HandoffWithWrongKeyIsRefused 的第二段抓了出来。
	if got := handoffKeyOf(header); got != key {
		logging.For("engine").Warn("交接说明的 key 与路径不符, 不采用",
			"path", path, "stored", got, "want", key, "header", header)
		return ""
	}
	body := strings.TrimSpace(text[nl+1:])
	if body == "" {
		return ""
	}
	return body
}

// clearHandoff 删掉某个身份的交接说明 (跑完了 / 或已被新的一份取代)。
//
// 尽力而为: 文件不在是**正常**情形 (绝大多数 run 从没留下过交接说明), 所以
// ENOENT 静默; 其余错误报 Warn —— "没删掉"会让下一次读到一份过期的进度, 那是
// 需要有人知道的。
func (e *QueryEngine) clearHandoff(key string) {
	path := e.handoffPath(key)
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		logging.For("engine").Warn("清理过期交接说明失败", "path", path, "err", err)
	}
}

// handoffPrompt 让模型在停下来的那一刻写交接说明的指令。
//
// 措辞里有三处是刻意的, 每一处都对应一种会写坏的交接说明:
//
//  1. "不要再调用任何工具" —— 不说的话, 一个正干到兴头上的 coder 会继续 Read/Edit,
//     于是这次请求的产出全是 tool_use, 而下一轮的进展不会落进说明 (它还没发生)。
//     请求本身也没带工具描述, 但模型在长历史里见过工具, 会试着用文本伪造调用。
//  2. "不要复述上面的对话 (它们不会被带过去)" —— 这是**唯一**能让说明自足的一条。
//     模型默认假设读者看得见上下文, 于是写出"按上面说的改完了"这种对下一次毫无
//     信息量的句子。必须显式告诉它: 上面那份历史不会被带过去。
//  3. 点名要文件路径与命令 —— "完成了登录功能"没法行动, "改了 auth/login.go 并留下
//     一个失败的 TestLogin (需要先跑 go generate)" 才能让人接着干。
func handoffPrompt(reason HandoffReason, detail string) string {
	return fmt.Sprintf(
		"[harness] 本次执行因为「%s」%s 即将被终止, 之后不会再有继续的机会。\n\n"+
			"请用你**最后这一次输出**写一份交接说明, 交给下一次执行。要求:\n\n"+
			"1. 不要调用任何工具, 也不要描述你「打算」做什么 —— 只写已经确定的事实;\n"+
			"2. 已经完成了什么: 改动/确认过的文件用完整路径列出, 并说明每个文件到此为止的状态;\n"+
			"3. 还没做什么: 下一步应该从哪开始, 以及为什么是从那里 (哪个结论把它推出来的);\n"+
			"4. 继续之前必须先读哪些文件、先跑哪条命令; 有没有失败的测试或编译错误, 原文是什么。\n\n"+
			"注意: 你**不要**复述我们上面的对话 —— 那些内容不会被带过去。这份说明是给一个"+
			"完全没有本次上下文的执行者看的, 它必须能只看这一份说明就知道从哪继续。",
		reason, detail)
}

// handoffMessage 把交接说明包成一条注入用的 user 消息。
//
// 外面这两句说明是必须的: 只说"下面是上一次的交接"还不够 —— 模型需要知道那一次是
// **被迫停下**而不是失败或完成 (否则它会去"修"一件没坏的事), 以及它**不必**重走
// 说明里已经写明的探索 (否则它会礼貌地照做一遍, 交接省钱的效果全没了)。
func handoffMessage(body string) types.Message {
	return types.Message{
		Type: types.MessageTypeUser,
		UUID: internal_hook.GenerateUUID(),
		Content: []types.ContentBlock{{
			Type: types.ContentBlockText,
			Text: "[harness] 下面是**上一次**执行同一任务留下的交接说明。那一次执行没有失败, " +
				"也不是半途而废 —— 它是跑到了自己的预算/轮数上限而被迫停下, 已经做完的部分" +
				"仍然有效。\n请以它为基础继续: 说明里已经写明的探索**不要重做** " +
				"(需要核实的地方再去读), 直接从它写明的下一步开始。\n\n" +
				"===== 交接说明开始 =====\n" + body + "\n===== 交接说明结束 =====\n",
		}},
		CreatedAt: time.Now(),
	}
}
