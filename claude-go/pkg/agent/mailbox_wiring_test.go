package agent

// mailbox_wiring_test.go —— 21.8.9-D6-3: 定向消息的**消费方**接线。
//
// ---------------------------------------------------------------------------
// 这一层要钉的三件事
// ---------------------------------------------------------------------------
//
// 收敛之后信箱只剩一份实现, 生产方 (SendMailMessage) 也一直通着 —— 但没有一个
// 消费方。于是 `/team msg` 回"📨 已发送", agent 那边什么都没有: 消息**写进去了、
// 没丢**, 只是从来没有人读。这一层补的就是那个读的人。
//
// 三件事各自会独立失效, 且失效时的表现**完全相同** —— "agent 没收到消息", 与
// "根本没人给他发消息"一模一样, 没有任何报错、没有日志差异、产出也正常:
//
//	1. executeStage 那条 (覆盖 pipeline/fanout/adversarial/graph 四条路径)
//	2. swarm 那条 (它自己拼 prompt 直调 Runner.Execute, 不经过 executeStage)
//	3. 三条边界 (预算 / 来源 / 不越权)
//
// 第 3 条尤其要有负例: 边界失效的方向是**注入得更多**, 而那看起来永远"更贴心"。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// mailTestTeam 造一个**挂在真管理器上**的团队。
//
// 必须走真管理器 (而不是手搓 &ProductionTeam{}): 取信的后端是经 team.mailStore()
// → mgr.stateStore 拿的, 手搓的团队 mgr 为 nil ⇒ 永远取不到信 ⇒ 这一整组用例会
// 集体假绿 (它们断言的恰好是"消息没进 prompt")。同理 StateStore 必须与生产方
// 同一个实例, 所以这里直接把它交给管理器。
func mailTestTeam(t *testing.T, store statestore.StateStore, name string) *ProductionTeam {
	t.Helper()
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir:    t.TempDir(),
		Notify:     func(_, _ string) {},
		StateStore: store,
	})
	team, err := ptm.CreateTeam(name, "swarm", "测试目标", "chat-mail")
	if err != nil {
		t.Fatalf("建团队失败: %v", err)
	}
	return team
}

// runStageCapturePrompt 跑一次真实的 executeStage, 返回宿主实际收到的 prompt。
//
// 断言**最终 prompt**而不是"Take 被调过": 后者在"取到了但没拼进 prompt"时照样绿。
func runStageCapturePrompt(t *testing.T, store statestore.StateStore, stage StageDef) string {
	t.Helper()
	team := mailTestTeam(t, store, "t-mail")
	var got string
	we := newProgExecutor(func(_ context.Context, role, prompt string) (string, error) {
		got = prompt
		// 产出必须过 validateStageOutputForRetry (否则阶段被判失败, 拿不到 prompt 读数)
		return stubStageOutput(role), nil
	})
	if res := we.executeStage(context.Background(), stage, "测试目标", map[string]string{}, team); res.Status != TaskCompleted {
		t.Fatalf("阶段执行失败: %v", res.Error)
	}
	return got
}

// swarmTaskCapturePrompt 跑一次真实的蜂群子任务, 返回 agent 实际收到的 prompt。
func swarmTaskCapturePrompt(t *testing.T, store statestore.StateStore, role, id string) string {
	t.Helper()
	team := mailTestTeam(t, store, "t-swarm")

	var got string
	pool := NewAgentPool(func(_ context.Context, r, _ string) (AgentRunner, error) {
		return &progStageRunner{role: r, fn: func(_ context.Context, role, prompt string) (string, error) {
			got = prompt
			return stubStageOutput(role), nil
		}}, nil
	}, 2)
	// notify 必须给: executeSubTask 尾部会调 s.notify(...) 报"伪并行"提醒, 而
	// orchestrated 之外的构造路径都可能留 nil (生产由 NewSwarmOrchestrator 兜)。
	s := &SwarmOrchestrator{pool: pool, stateDir: t.TempDir(), notify: func(_, _ string) {}}

	res := s.executeSubTask(context.Background(), SubTask{
		ID: id, Role: role, Description: "调研一下",
	}, "测试目标", map[string]string{}, team)
	if res.Status != TaskCompleted {
		t.Fatalf("蜂群子任务执行失败: %v", res.Error)
	}
	return got
}

// ---------------------------------------------------------------------------
// 1. executeStage 注入点
// ---------------------------------------------------------------------------

// TestExecuteStage注入定向消息 pipeline 路径。断言的是**最终 prompt 里有什么**,
// 而不是"Take 被调过" —— 后者在"取到了但没拼进 prompt"时照样绿。
func TestExecuteStage注入定向消息(t *testing.T) {
	store := statestore.NewMemStore()
	mb := NewPersistentMailbox(store, "t-mail")
	if err := mb.Send(MailMessage{From: "user", To: "coder", Content: "把日志级别调成 debug"}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}

	prompt := runStageCapturePrompt(t, store, StageDef{Name: "implement", Agent: "coder"})

	if !strings.Contains(prompt, "把日志级别调成 debug") {
		t.Fatalf("发给 coder 的定向消息没进 prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "来自 user") {
		t.Errorf("消息没带来源标记 (来自谁), 模型无从判断该不该采信:\n%s", prompt)
	}
}

// TestExecuteStage按角色与阶段名两个地址都能收到 是那个"静默丢弃"的负例。
//
// 团队列表给用户看的是 `- **<阶段名>** (<角色>)`, 两个名字都在眼前, 而 /team msg 的
// <Agent> 参数没有任何一处说"必须是角色"。只认一个地址的实现会让另一种写法**静默
// 丢掉** —— 用户看到"已发送", agent 什么都没有。
func TestExecuteStage按角色与阶段名两个地址都能收到(t *testing.T) {
	for _, to := range []string{"coder", "implement"} {
		store := statestore.NewMemStore()
		mb := NewPersistentMailbox(store, "t-mail")
		if err := mb.Send(MailMessage{From: "user", To: to, Content: "内容-" + to}); err != nil {
			t.Fatalf("投递失败: %v", err)
		}
		prompt := runStageCapturePrompt(t, store, StageDef{Name: "implement", Agent: "coder"})
		if !strings.Contains(prompt, "内容-"+to) {
			t.Errorf("发到地址 %q 的消息丢了 —— 用户看得见这个名字却收不到信:\n%s", to, prompt)
		}
	}
}

// TestExecuteStage广播只注入一次 —— 去重的负例。
//
// Take 对广播 (To 为空) 是**无条件匹配**的, 所以收两个地址就会把同一条广播取两次。
// 一次注入两条一模一样的消息, 比收不到更像"出 bug 了": 模型会以为对方催了两遍。
func TestExecuteStage广播只注入一次(t *testing.T) {
	store := statestore.NewMemStore()
	mb := NewPersistentMailbox(store, "t-mail")
	if err := mb.Send(MailMessage{From: "user", To: "", Content: "全体注意"}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}

	prompt := runStageCapturePrompt(t, store, StageDef{Name: "implement", Agent: "coder"})

	if n := strings.Count(prompt, "全体注意"); n != 1 {
		t.Fatalf("广播应恰好注入一次, 实际 %d 次 —— 两个地址各取了一遍:\n%s", n, prompt)
	}
}

// TestExecuteStage没消息时提示词一个字节不变 是"6 个下游平台行为不变"的回归面。
//
// 先跑有消息的那次拿到基线, 再跑空信箱: 空信箱这一次的 prompt 必须与**完全没有接线**
// 时逐字节相同。做不到这一点, 就是让所有既有团队的每一轮 prompt 都变了个样 ——
// 前缀缓存全失效, 且没有任何人有理由知道为什么。
func TestExecuteStage没消息时提示词一个字节不变(t *testing.T) {
	withMail := runStageCapturePrompt(t, statestore.NewMemStore(),
		StageDef{Name: "implement", Agent: "coder"})

	// 空信箱: 走的是同一条 takeMailFor, 只是取不到东西。
	empty := statestore.NewMemStore()
	_ = NewPersistentMailbox(empty, "t-mail") // 建过桶, 但一条消息都没有
	noMail := runStageCapturePrompt(t, empty, StageDef{Name: "implement", Agent: "coder"})

	if noMail != withMail {
		t.Fatalf("空信箱的提示词与基线不同 —— \"没有消息\"不该改变任何东西:\n--- 基线 ---\n%s\n--- 空信箱 ---\n%s",
			withMail, noMail)
	}
	if strings.Contains(noMail, "定向消息") {
		t.Fatalf("空信箱也拼进了定向消息段:\n%s", noMail)
	}
}

// TestExecuteStage取走后不重复投递 游标语义在真实注入点上的确认。
//
// 二轮/精修会把同一个阶段跑第二遍。游标若没生效, 第二遍会把第一遍已经处理过的消息
// 再注入一次 —— 而"又收到一遍旧指令"看起来就像对方真的又说了一遍。
func TestExecuteStage取走后不重复投递(t *testing.T) {
	store := statestore.NewMemStore()
	mb := NewPersistentMailbox(store, "t-mail")
	if err := mb.Send(MailMessage{From: "user", To: "coder", Content: "只此一次"}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}

	first := runStageCapturePrompt(t, store, StageDef{Name: "implement", Agent: "coder"})
	second := runStageCapturePrompt(t, store, StageDef{Name: "implement", Agent: "coder"})

	if !strings.Contains(first, "只此一次") {
		t.Fatalf("第一遍没注入:\n%s", first)
	}
	if strings.Contains(second, "只此一次") {
		t.Fatalf("第二遍又注入了同一条 (游标没生效):\n%s", second)
	}
}

// ---------------------------------------------------------------------------
// 2. swarm 注入点
// ---------------------------------------------------------------------------

// TestSwarm注入定向消息 蜂群那条独立接线。
//
// 蜂群自己拼 prompt 后直调 agent.Runner.Execute, **不经过 executeStage** ——
// 上面那一整组用例全绿也证明不了这一条通。
func TestSwarm注入定向消息(t *testing.T) {
	store := statestore.NewMemStore()
	mb := NewPersistentMailbox(store, "t-swarm")
	if err := mb.Send(MailMessage{From: "user", To: "researcher", Content: "优先看近三个月的"}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}

	prompt := swarmTaskCapturePrompt(t, store, "researcher", "t1")
	if !strings.Contains(prompt, "优先看近三个月的") {
		t.Fatalf("蜂群子任务没拿到定向消息:\n%s", prompt)
	}
}

// TestSwarm两个地址都能收到 蜂群侧的同一条负例 (task.ID 与 task.Role)。
func TestSwarm两个地址都能收到(t *testing.T) {
	for _, to := range []string{"researcher", "t1"} {
		store := statestore.NewMemStore()
		mb := NewPersistentMailbox(store, "t-swarm")
		if err := mb.Send(MailMessage{From: "user", To: to, Content: "内容-" + to}); err != nil {
			t.Fatalf("投递失败: %v", err)
		}
		prompt := swarmTaskCapturePrompt(t, store, "researcher", "t1")
		if !strings.Contains(prompt, "内容-"+to) {
			t.Errorf("蜂群侧发到地址 %q 的消息丢了:\n%s", to, prompt)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. 三条边界 (全部是负例)
// ---------------------------------------------------------------------------

// TestFormatMailbox预算上限 边界一: 一个失控的生产者不该把阶段 prompt 顶爆。
func TestFormatMailbox预算上限(t *testing.T) {
	var msgs []MailMessage
	for i := 0; i < 50; i++ {
		msgs = append(msgs, MailMessage{
			From: "user", To: "coder", Content: strings.Repeat("很长的一段内容。", 200),
		})
	}
	got := FormatMailboxForPrompt(msgs)

	// 上界 = 预算 + 固定开销 (段标题 + "另有 N 条" 报数行)。
	// 曾经这里写的是 len(got) > Budget*2 —— 那个界宽到拦不住 rune/字节单位错配
	// (中文按 rune 截断会超预算 3 倍, 却仍然落在 2 倍以内)。界要贴着预算才有分辨力。
	if len(got) > MailboxPromptBudget+512 {
		t.Fatalf("注入长度 %d 超出预算 %d —— 预算没生效", len(got), MailboxPromptBudget)
	}
	// 预算耗尽必须**如实报数**, 不能静默丢弃: "没人给我发消息"与"消息被丢了"如果长得
	// 一样, 排查时根本没有下手处。
	if !strings.Contains(got, "条定向消息因预算未注入") {
		t.Fatalf("被预算砍掉的消息没有如实报数:\n%s", got)
	}
}

// TestFormatMailbox条数上限 边界的另一半: 内容都很短时, 挡住的是条数。
func TestFormatMailbox条数上限(t *testing.T) {
	var msgs []MailMessage
	for i := 0; i < 50; i++ {
		msgs = append(msgs, MailMessage{From: "user", To: "coder", Content: "短"})
	}
	got := FormatMailboxForPrompt(msgs)

	if n := strings.Count(got, "- 来自 user"); n != MailboxMaxMessages {
		t.Fatalf("应恰好注入 %d 条, 实际 %d 条", MailboxMaxMessages, n)
	}
	if !strings.Contains(got, "40 条定向消息因预算未注入") {
		t.Fatalf("被条数上限挡下的没有如实报数:\n%s", got)
	}
}

// TestFormatMailbox来源与时刻必须带出来 边界二。
//
// 信箱里装的是**别人说的话**。不标来源, 模型没有任何依据判断该不该采信, 也分不清
// "用户直接指示"和"某个同僚 agent 的推测"; 不标时刻, 分不清"两小时前那条"与"刚刚那条"。
func TestFormatMailbox来源与时刻必须带出来(t *testing.T) {
	ts := time.Date(2026, 9, 25, 14, 30, 5, 0, time.UTC)
	got := FormatMailboxForPrompt([]MailMessage{
		{From: "user", To: "coder", Content: "改一下", Timestamp: ts},
	})

	if !strings.Contains(got, "来自 user") {
		t.Errorf("没带来源:\n%s", got)
	}
	if !strings.Contains(got, "2026-09-25 14:30:05") {
		t.Errorf("没带写入时刻:\n%s", got)
	}
	// 空 From 不能渲染成"来自  (时刻)" 这种半截话。
	anon := FormatMailboxForPrompt([]MailMessage{{Content: "改一下", Timestamp: ts}})
	if !strings.Contains(anon, "(未署名)") {
		t.Errorf("空 From 应显式标成未署名:\n%s", anon)
	}
}

// TestFormatMailbox内容不含消息结构本身 边界三: 逐字段白名单, 不是整条 marshal。
//
// 这条负例守的是**将来**: MailMessage 今天只有 From/To/Content/Type/Timestamp, 但
// 谁哪天给它加一个 `Tools` 或 `Permission` 字段, 整条 marshal 就会把它自动送进 prompt
// —— 于是模型可以靠"给自己发一条消息"扩大自己的动作空间。白名单让新增字段默认不进。
//
// 用 Type 当那个"不该出现"的代表: 它在结构体里存在, 但对模型毫无意义 (是内部路由
// 字段), 出现在 prompt 里就是"结构体泄漏了"的证据。
func TestFormatMailbox内容不含消息结构本身(t *testing.T) {
	got := FormatMailboxForPrompt([]MailMessage{
		{From: "user", To: "coder", Content: "正文", Type: "internal-routing-token"},
	})

	if strings.Contains(got, "internal-routing-token") {
		t.Fatalf("消息结构体本身漏进了 prompt (整条 marshal 的形态):\n%s", got)
	}
	// 逐字段渲染的产物里**一个双引号都不该有** —— 这是"没有整条 marshal"最干净的一刀:
	// JSON 形态必然带引号, 而这个渲染形态全是纯文本。
	if strings.Contains(got, `"`) {
		t.Fatalf("prompt 里出现了 JSON 引号 —— 说明是整条 marshal 出来的:\n%s", got)
	}
	// 寻址元数据 (To) 对模型没有意义, 不该占 prompt。
	if strings.Contains(got, "To") {
		t.Fatalf("收件人字段不该出现 (模型不需要知道消息的寻址元数据):\n%s", got)
	}
}

// TestTakeMailFor去重不误伤不同消息 去重键的分辨力 —— 两个方向都要钉。
//
// 去重键是 (From, 写入时刻, 正文) 而**不含 To**。被它压掉的那一条, 在现存实现里
// 只可能是两个地址各取一遍的**同一条广播** (Take 的游标是按地址记的, 广播对每个
// 地址都匹配) —— 这是有意接受的代价, 因为代价的另一面是"广播被注入两遍", 那看起来
// 像对方催了两遍, 更糟。
//
// 但它**不能**把两条真正不同的消息压成一条: 不同的人、或不同的正文、或不同的时刻,
// 都必须各自留下来。去重过宽的表现是"少了一条指令", 而那与"对方没发"同形。
func TestTakeMailFor去重不误伤不同消息(t *testing.T) {
	ts := time.Date(2026, 9, 25, 14, 30, 5, 0, time.UTC)
	store := statestore.NewMemStore()
	mb := NewPersistentMailbox(store, "t-dedup")
	for _, m := range []MailMessage{
		{From: "user", To: "", Content: "全体广播正文", Timestamp: ts}, // 两个地址都会取到它
		{From: "user", To: "coder", Content: "定向给 coder", Timestamp: ts},
		{From: "critic", To: "coder", Content: "定向给 coder", Timestamp: ts}, // 只有 From 不同
		{From: "user", To: "coder", Content: "另一条", Timestamp: ts},
	} {
		if err := mb.Send(m); err != nil {
			t.Fatalf("投递失败: %v", err)
		}
	}

	msgs, err := takeMailFor(store, "t-dedup", "coder", "implement")
	if err != nil {
		t.Fatalf("取信失败: %v", err)
	}
	// 广播 1 条 + 三条定向 = 4。若去重把广播之外的也算重了, 这里会小于 4;
	// 若去重失效, 广播会被两个地址各取一遍 ⇒ 5 条。
	if len(msgs) != 4 {
		t.Fatalf("应取到 4 条 (广播 1 + 定向 3), 实际 %d 条: %+v", len(msgs), msgs)
	}
	// 反向: 同人同时刻同文的那一条 (To 为空, 两地址各匹配一次) 只算一条。
	// 注意不能数 "广播" 二字 —— 段标题本身就写着"(发给你的, 不是广播)", 那会让计数
	// 恒多 1, 用例永远红 (第一版就是这么写的)。数的是正文里那个独有串。
	if n := strings.Count(FormatMailboxForPrompt(msgs), "全体广播正文"); n != 1 {
		t.Fatalf("广播应只注入一次, 实际 %d 次", n)
	}
}

// TestFormatMailbox空输入返回空串 是"没消息 ⇒ 提示词不变"的前提条件。
func TestFormatMailbox空输入返回空串(t *testing.T) {
	if got := FormatMailboxForPrompt(nil); got != "" {
		t.Fatalf("nil 应返回空串, got %q", got)
	}
	// 全是空内容的消息也不该拼出一个只有标题的空壳段。
	if got := FormatMailboxForPrompt([]MailMessage{{From: "user", To: "coder"}}); got != "" {
		t.Fatalf("全部为空内容时应返回空串, got %q", got)
	}
}
