package agent

// mail_relay_test.go —— 21.8.12 P5 / D6-3 缺口 2 级 2: 信箱的**运行中**投递。
//
// ---------------------------------------------------------------------------
// 这批用例要钉的东西, 以及为什么断言落在"最终注入文本"上
// ---------------------------------------------------------------------------
//
// 阶段起点那条路径 (executeStage 的 takeMailFor, 见 mailbox_wiring_test.go) 断言的
// 是**最终 prompt**。这一条不一样: 消息不在 prompt 里, 而在**运行中**被塞进引擎。
// 所以判据只能落在两处之一 ——
//
//	(a) 中继真的调了 InjectMidRun —— 用假注入器记下来;
//	(b) 引擎真的收到了 (那是 pkg/engine/inject_midrun_test.go 的判据)。
//
// 这里做 (a): 两层的接缝在**这里**, 而"注入器被调了"与"模型看到了"是两个不同的
// 事实, 各自要有自己的用例。只测 (b) 的话, 中继整个被摘掉 (startMidRunMailRelay
// 变成 no-op) 一个用例都不会红 —— 而它正是本次要补的那根线。
//
// ---------------------------------------------------------------------------
// 第二条纪律: 取信是破坏性的, 所以"退回"必须有用例
// ---------------------------------------------------------------------------
//
// Take 推进游标, 同一条不会再投递。中继取到信却投不出去 (引擎刚好收尾) 时若不退回,
// 那条用户消息就**永远消失** —— 而它本该在下一次读取时正常送达。于是"补了一条运行中
// 通道"反而让某些消息再也收不到, 且没有任何报错。退回用例 (下面前两条) 就是钉这个的。

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/statestore"
)

// fakeInjector 记录中继交给它的注入文本, 并按脚本决定接不接受。
type fakeInjector struct {
	mu     sync.Mutex
	got    []string
	accept bool
}

func (f *fakeInjector) CanInjectMidRun() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accept
}

func (f *fakeInjector) InjectMidRun(text string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.accept {
		return false
	}
	f.got = append(f.got, text)
	return true
}

func (f *fakeInjector) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// relayTestStore 造一个带一条定向消息的信箱后端。
func relayTestStore(t *testing.T, to, content string) statestore.StateStore {
	t.Helper()
	store := statestore.NewMemStore()
	if err := NewPersistentMailbox(store, "t-relay").Send(
		MailMessage{From: "user", To: to, Content: content}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	return store
}

// ---------------------------------------------------------------------------
// 正向: 运行中真的送出去了
// ---------------------------------------------------------------------------

// TestRelayInjectsWhileRunning 是这条通道的核心正向判据。
func TestRelayInjectsWhileRunning(t *testing.T) {
	store := relayTestStore(t, "coder", "更正: 端口是 8443")
	inj := &fakeInjector{accept: true}

	relayMailOnce(inj, store, "t-relay", "coder", "implement")

	got := inj.texts()
	if len(got) != 1 {
		t.Fatalf("运行中应注入 1 条, 实到 %d 条", len(got))
	}
	if !strings.Contains(got[0], "更正: 端口是 8443") {
		t.Fatalf("注入的正文不对: %q", got[0])
	}
	// 来源标记必须在 —— 与阶段起点那条路径同源 (FormatMailboxForPrompt 是唯一渲染处)。
	if !strings.Contains(got[0], "来自 user") {
		t.Errorf("运行中注入丢掉了来源标记 (模型无从判断该不该采信): %q", got[0])
	}
	if !strings.Contains(got[0], "定向消息") {
		t.Errorf("运行中注入少了标题, 模型看不出这是别人单独发给它的: %q", got[0])
	}
}

// TestRelaySkipsWhenNobodyCanInject 断言引擎没在跑时**一个字节都不取**。
//
// 这是整条通道无损的前提 (约束 ①)。少了这个早退, 每一轮轮询都会"取出来又退回去":
// 信箱日志随轮询次数增长, 而且每条都带一条"退回"告警 —— 噪声把真信号盖掉。
//
// 判据落在**信箱的游标**上而不只是返回值: 一封没被取走的信, 下一次 takeMailFor
// 必须还能读到它。
func TestRelaySkipsWhenNobodyCanInject(t *testing.T) {
	store := relayTestStore(t, "coder", "先别动手")
	inj := &fakeInjector{accept: false}

	relayMailOnce(inj, store, "t-relay", "coder", "implement")

	if n := len(inj.texts()); n != 0 {
		t.Fatalf("引擎没在跑却注入了 %d 条", n)
	}
	// ★ 判据是**信箱里的条数**, 不是"还能不能读到它"。
	//
	// 只断言"下一次 takeMailFor 读得到"是**测不出早退缺失**的: 少了早退的实现会取信、
	// 注入落空、再把消息退回去 —— 读得到, 但信箱日志里多出一条重复的 (Inbox 会读到 2 条)。
	// 这正是"取出来又退回去"的代价: 日志随轮询次数增长, 每条还带一条退回告警。
	// (我第一版就是这么写的, 变异 M37 存活才发现 —— 判据必须落在**副作用**上。)
	if got := len(NewPersistentMailbox(store, "t-relay").Inbox("coder")); got != 1 {
		t.Fatalf("引擎没在跑时中继取信又退回 —— 信箱里积了 %d 条 (应为 1 条): "+
			"轮询每两秒一次, 这会一直涨, 而每条都带一条退回告警", got)
	}
	// 顺带确认那一条还是原来那条 (上面的条数断言不关心内容)。
	msgs, err := takeMailFor(store, "t-relay", "coder", "implement")
	if err != nil {
		t.Fatalf("取信失败: %v", err)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "先别动手") {
		t.Fatalf("引擎没在跑时这封信丢了 (读到 %d 条)", len(msgs))
	}
}

// TestRelayAddressesStageNameToo 断言**发到阶段名**的消息在运行中同样送得到。
//
// 与阶段起点那条路径同一条理由 (见 TestExecuteStage按角色与阶段名两个地址都能收到):
// 团队列表给用户看的是 `- **<阶段名>** (<角色>)`, 两个名字都在眼前, 而 /team msg 的
// <Agent> 参数没有任何一处说"必须是角色"。只认角色的中继会让"发给 implement"的消息
// 在**运行中**静默丢掉 —— 用户看到"已发送", agent 那边什么都没有。
//
// 这条同时钉住 stageFrom(ctx) 那一句: 把它换成空串, 这个地址就收不到了 (变异 M40)。
func TestRelayAddressesStageNameToo(t *testing.T) {
	store := relayTestStore(t, "implement", "发给阶段名的更正")
	inj := &fakeInjector{accept: true}

	relayMailOnce(inj, store, "t-relay", "coder", "implement")

	got := inj.texts()
	if len(got) != 1 || !strings.Contains(got[0], "发给阶段名的更正") {
		t.Fatalf("发给阶段名的消息在运行中没有送达 (注入 %d 条): "+
			"这与阶段起点那条路径的收件人范围不一致, 而差别只在'跑得快不快'上现形", len(got))
	}
}

// TestRelayReturnsMailWhenInjectionLoses 断言"取了但没投出去"的消息**退回信箱**。
//
// 复现的是那个极窄的窗口: CanInjectMidRun 通过之后、InjectMidRun 之前引擎收尾了。
// 不退回的话, 这条消息在下一次读取时不会出现 —— 用户以为发到了, 团队永远看不到。
func TestRelayReturnsMailWhenInjectionLoses(t *testing.T) {
	store := relayTestStore(t, "coder", "这条会落空")
	// accept=true 让 CanInjectMidRun 通过, 然后在注入时改成拒绝 —— 模拟窗口。
	inj := &losingInjector{}

	relayMailOnce(inj, store, "t-relay", "coder", "implement")

	msgs, err := takeMailFor(store, "t-relay", "coder", "implement")
	if err != nil {
		t.Fatalf("取信失败: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("注入落空的消息必须退回信箱, 却读到 %d 条 —— "+
			"它本该在下一次读取时送达, 现在永远收不到了", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "这条会落空") {
		t.Fatalf("退回的不是原来那条: %q", msgs[0].Content)
	}
}

// TestRelayReturnDeduplicatesBroadcast 断言退回的**广播**不会与原件重复投递。
//
// 退回靠的是"再追加一条"实现 (见 returnMail 的论证), 于是有人会合理地怀疑它会重复。
// 它不会 —— takeMailFor 的去重键 (From + 写入时刻 + 正文) 把原件与退回件并成一条。
// 这条用例把这个依赖钉住: 哪天有人把那个去重键改宽, 这里会红。
func TestRelayReturnDeduplicatesBroadcast(t *testing.T) {
	store := statestore.NewMemStore()
	// 广播 (To 为空): 任何地址都匹配, 所以退回件与原件必然同批出现。
	if err := NewPersistentMailbox(store, "t-relay").Send(
		MailMessage{From: "user", Content: "全员通知"}); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	relayMailOnce(&losingInjector{}, store, "t-relay", "coder", "implement")

	msgs, err := takeMailFor(store, "t-relay", "coder", "implement")
	if err != nil {
		t.Fatalf("取信失败: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("退回的广播被重复投递了 %d 条 —— 同一句话出现两遍比收不到更像出 bug", len(msgs))
	}
}

// losingInjector: CanInjectMidRun 通过, InjectMidRun 落空 (那个窄窗口)。
type losingInjector struct{}

func (losingInjector) CanInjectMidRun() bool      { return true }
func (losingInjector) InjectMidRun(string) bool   { return false }

// ---------------------------------------------------------------------------
// 接线: 中继真的被挂上去了
// ---------------------------------------------------------------------------

// TestRelayIsWiredIntoStageExecution 断言 runAgent **真的**把中继挂起来了。
//
// 上面几条都直接调 relayMailOnce, 所以它们全绿而 startMidRunMailRelay 被换成 no-op
// (或 runAgent 里那一行被删) 时没有任何一条会红 —— 接线本身必须单独有用例。
//
// 做法: 让 runner 的 Execute 阻塞住 (中继只在运行中才有意义), 在它跑着的时候往信箱
// 投一条消息, 然后等注入器收到它。
func TestRelayIsWiredIntoStageExecution(t *testing.T) {
	store := statestore.NewMemStore()
	inj := &fakeInjector{accept: true}
	// 中继只对实现了 MidRunMailInjector 的 runner 生效 —— 这里给一个支持注入的 runner。
	runner := &relayProbeRunner{inj: inj, entered: make(chan struct{}), release: make(chan struct{})}

	we := &WorkflowExecutor{
		factory: func(_ context.Context, _, _ string) (AgentRunner, error) { return runner, nil },
		notify:  func(_, _ string) {},
	}
	team := mailTestTeam(t, store, "t-relay")

	go func() {
		we.executeStage(context.Background(),
			StageDef{Name: "implement", Agent: "coder"}, "目标", map[string]string{}, team)
	}()

	select {
	case <-runner.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("阶段没有进入执行 —— 前置条件不成立")
	}
	// runner 已经在跑了, 现在发两条: 一条发给**角色**, 一条发给**阶段名**。
	//
	// 两个地址都要发, 因为它们在实现里走的是两条不同的来路: 角色是 runAgent 的形参,
	// 阶段名要从创建 ctx 回读 (stageFrom)。只发角色的话, 把回读那一句换成空串照样
	// 全绿 —— 而那条路上"发给 implement 的消息收不到"与"没人给他发消息"完全同形。
	for _, to := range []string{"coder", "implement"} {
		if err := NewPersistentMailbox(store, "t-relay").Send(
			MailMessage{From: "user", To: to, Content: "运行中发来的更正-" + to}); err != nil {
			t.Fatalf("投递失败: %v", err)
		}
	}

	want := []string{"运行中发来的更正-coder", "运行中发来的更正-implement"}
	deadline := time.After(15 * time.Second)
	for {
		all := strings.Join(inj.texts(), "\n")
		missing := ""
		for _, w := range want {
			if !strings.Contains(all, w) {
				missing = w
				break
			}
		}
		if missing == "" {
			close(runner.release)
			return
		}
		select {
		case <-deadline:
			close(runner.release)
			t.Fatalf("阶段跑着的时候 %q 没有被注入 (收到的注入: %q) —— "+
				"中继没挂上, 或某个收件地址没接上 (两个地址的来路不同, 见上面的注释)",
				missing, all)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// relayProbeRunner 支持运行中注入的 AgentRunner: Execute 一直阻塞到 release 被关闭。
type relayProbeRunner struct {
	inj     *fakeInjector
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *relayProbeRunner) Execute(ctx context.Context, _ string) (string, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
	case <-ctx.Done():
	case <-time.After(30 * time.Second):
	}
	return stubStageOutput("coder"), nil
}

func (r *relayProbeRunner) CanInjectMidRun() bool    { return r.inj.CanInjectMidRun() }
func (r *relayProbeRunner) InjectMidRun(s string) bool { return r.inj.InjectMidRun(s) }

// ---------------------------------------------------------------------------
// 开关
// ---------------------------------------------------------------------------

// TestRelayRespectsOffSwitch 断言关掉时这条通道**完全不工作** (而不是"少工作一点")。
//
// 一个"关掉了但仍在投递"的开关比没有开关更糟: 运维按它做的判断全部落空, 而出问题时
// 第一件被怀疑的事 ("那个开关到底生效没有") 恰恰是最难自证的。
func TestRelayRespectsOffSwitch(t *testing.T) {
	t.Setenv("CLAUDE_GO_MIDRUN_MAIL", "0")
	if midRunMailEnabled() {
		t.Fatal("CLAUDE_GO_MIDRUN_MAIL=0 时应关掉")
	}
	// 关掉时 startMidRunMailRelay 必须返回 noop —— 判据是"不会有人被注入"。
	store := relayTestStore(t, "coder", "不该被注入")
	inj := &fakeInjector{accept: true}
	runner := &relayProbeRunner{inj: inj, entered: make(chan struct{}), release: make(chan struct{})}
	close(runner.release) // 让它立刻返回, 不需要 entere
	team := mailTestTeam(t, store, "t-relay")
	stop := startMidRunMailRelay(context.Background(), runner, team, "coder", "implement")
	defer stop()
	time.Sleep(3 * mailRelayInterval)
	if n := len(inj.texts()); n != 0 {
		t.Fatalf("开关关掉了却注入了 %d 条", n)
	}
}

// TestRelayDefaultOn 断言默认开。
//
// 默认关的话这条通道等于没做 —— 一个没人打开的开关与一段死代码在验收上完全一样。
func TestRelayDefaultOn(t *testing.T) {
	os.Unsetenv("CLAUDE_GO_MIDRUN_MAIL")
	if !midRunMailEnabled() {
		t.Fatal("未设 env 时应默认开启 (否则这条通道在真实部署里永远不会生效)")
	}
}

// ---------------------------------------------------------------------------
// 不适用: 不该报错, 也不该空转
// ---------------------------------------------------------------------------

// TestRelayNotApplicableIsSilent 断言四种"这条通道不适用"的情形都返回 noop 而非报错。
//
// 没有团队 (单 agent 路径) / runner 不支持注入 (worker 远端) / 团队没有信箱后端 ——
// 三者各自有别的投递路径, 在运行中这条上报错只会变成噪声, 而噪声会训练人跳过这一行。
func TestRelayNotApplicableIsSilent(t *testing.T) {
	store := statestore.NewMemStore()
	team := mailTestTeam(t, store, "t-relay")
	plain := &progStageRunner{role: "coder", fn: func(context.Context, string, string) (string, error) {
		return "x", nil
	}}
	cases := []struct {
		why    string
		runner AgentRunner
		team   *ProductionTeam
	}{
		{"没有团队", plain, nil},
		{"runner 不支持注入", plain, team},
		{"runner 为 nil", nil, team},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			stop := startMidRunMailRelay(context.Background(), tc.runner, tc.team, "coder", "implement")
			stop()
			stop() // 可重复调用: 中继的停止函数会 defer, 也可能被显式调一次
		})
	}
}
