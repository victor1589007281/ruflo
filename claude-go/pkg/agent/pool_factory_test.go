package agent

// pool_factory_test —— design/02 §3.3 "swarm 路径仍纯本地 (AgentPool.factory 未换)" 的验收。
//
// ---------------------------------------------------------------------------
// 为什么要有这组测试
// ---------------------------------------------------------------------------
//
// 上一轮把远程 runtime 接在了 `ProductionTeamManager.factory` 上, 那是 pipeline /
// graph **阶段**执行的出口。swarm 子任务走的是另一个字段 (`AgentPool.factory`,
// 由 `swarm.go:702` 的 `s.pool.Acquire` 读), 于是同一个进程里"阶段能派到远程 worker、
// swarm 子任务永远在本机"—— 而两者的症状 (子任务在本机跑) 与"远程 worker 没上线"
// 完全一样, 现场极难归因。所以这里要证明的是**通电**, 不是"方法存在"。
//
// ---------------------------------------------------------------------------
// 变异反证 (每条都真跑过一次红)
// ---------------------------------------------------------------------------
//
//	① 把 WrapAgentFactory 里 pool 那一段整块删掉 (退回上一轮的实现)
//	   → TestSwarm路径_WrapAgentFactory同时接管池   FAIL "swarm 子任务仍走本地工厂"
//	② 把 sameFactory 改成恒 true
//	   → TestSwarm路径_两工厂不同源时各自包装        FAIL "pool 的本地回退被换成了阶段工厂"
//	③ 把 AgentPool.setFactory 改成空实现
//	   → TestSwarm路径_WrapAgentFactory同时接管池   FAIL "swarm 子任务仍走本地工厂"
//	④ 把 Acquire 里的 currentFactory() 改回裸读 p.factory
//	   → TestSwarm路径_运行期读工厂无竞争 (-race)   FAIL "DATA RACE"

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// tagRunner 一个只回报自己出身的 AgentRunner —— 用来分辨"这次执行是哪个工厂造的"。
type tagRunner struct{ tag, role string }

func (r *tagRunner) Execute(_ context.Context, _ string) (string, error) {
	return r.tag + ":" + r.role, nil
}

// tagFactory 造一个带出身标记的工厂。
func tagFactory(tag string) CreateAgentFunc {
	return func(_ context.Context, role, _ string) (AgentRunner, error) {
		return &tagRunner{tag: tag, role: role}, nil
	}
}

// acquireTag 走真实的 Acquire 路径拿一次 runner 并读出它的出身。
func acquireTag(t *testing.T, p *AgentPool, role string) string {
	t.Helper()
	a, err := p.Acquire(context.Background(), role, "")
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	defer p.Release(a)
	out, err := a.Runner.Execute(context.Background(), "x")
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	return out
}

// newPTMWithPool 造一个最小可用的 PTM: 阶段工厂与池工厂**同源**, 与生产装配一致
// (feishu/bot.go:447 与 :559 传的都是 bot.sessions.CreateAgentRunner)。
func newPTMWithPool(t *testing.T, f CreateAgentFunc) (*ProductionTeamManager, *AgentPool) {
	t.Helper()
	pool := NewAgentPool(f, 4)
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: filepath.Join(t.TempDir(), "teams"),
		Factory: f,
		Pool:    pool,
		Notify:  func(_, _ string) {},
	})
	return ptm, pool
}

// 核心通电项: 生产装配处只调一次 WrapAgentFactory (cmd/claude-go/main.go:1346),
// 阶段与 swarm 两条路径都必须换到放置感知工厂。
func TestSwarm路径_WrapAgentFactory同时接管池(t *testing.T) {
	local := tagFactory("local")
	ptm, pool := newPTMWithPool(t, local)

	var wrapCalls int
	ptm.WrapAgentFactory(func(prev CreateAgentFunc) CreateAgentFunc {
		wrapCalls++
		return func(ctx context.Context, role, sp string) (AgentRunner, error) {
			// 真实的 worker.RuntimeFactory 会在 Pick 失败时报错; 这里模拟"命中远程"。
			if role == "fallback" {
				return prev(ctx, role, sp) // 回退到收到的本地工厂
			}
			return &tagRunner{tag: "remote", role: role}, nil
		}
	})

	// ① 阶段路径 (上一轮已通)。
	if got := ptm.AgentFactory(); got == nil {
		t.Fatal("阶段工厂被置空了")
	}
	sr, err := ptm.AgentFactory()(context.Background(), "coder", "")
	if err != nil {
		t.Fatalf("阶段工厂调用失败: %v", err)
	}
	if out, _ := sr.Execute(context.Background(), "x"); out != "remote:coder" {
		t.Errorf("阶段路径 = %q, 期望 remote:coder", out)
	}

	// ② swarm 路径 (本轮): swarm.go:702 读的就是这个池的工厂。
	if got := acquireTag(t, pool, "coder"); got != "remote:coder" {
		t.Errorf("swarm 子任务仍走本地工厂: %q, 期望 remote:coder", got)
	}
	// 回退语义仍在: 远程无解时用收到的本地工厂。
	if got := acquireTag(t, pool, "fallback"); got != "local:fallback" {
		t.Errorf("swarm 回退不对: %q, 期望 local:fallback", got)
	}

	// ③ wrap 只能被调一次: 生产的 wrap 带副作用 (注册 local-session runtime),
	// 调两次会用另一个本地工厂按同名覆盖前一次注册。
	if wrapCalls != 1 {
		t.Errorf("wrap 调用次数 = %d, 期望 1 (两个字段同源时复用同一份包装结果)", wrapCalls)
	}
}

// swarm 编排器拿到的就是 ptm.pool 这个**同一个指针** (teams.go:1254),
// 所以装配期换掉池的工厂对运行期的 swarm 立即生效。
func TestSwarm路径_编排器与PTM共用同一个池(t *testing.T) {
	ptm, pool := newPTMWithPool(t, tagFactory("local"))
	so := NewSwarmOrchestrator(ptm.llm, ptm.pool, ptm.taskTracker, ptm.notify, "chat", 8)
	if so.pool != pool {
		t.Fatal("swarm 编排器拿到的不是 ptm.pool —— 换池工厂对 swarm 不会生效")
	}
	ptm.WrapAgentFactory(func(CreateAgentFunc) CreateAgentFunc { return tagFactory("remote") })
	if got := acquireTag(t, so.pool, "coder"); got != "remote:coder" {
		t.Errorf("编排器侧 = %q, 期望 remote:coder", got)
	}
}

// 两个字段不同源时必须**分别** wrap: 复用阶段那份会把 swarm 的本地回退悄悄换掉。
func TestSwarm路径_两工厂不同源时各自包装(t *testing.T) {
	stageLocal := tagFactory("stage-local")
	poolLocal := tagFactory("pool-local")

	pool := NewAgentPool(poolLocal, 4)
	ptm := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: filepath.Join(t.TempDir(), "teams"),
		Factory: stageLocal,
		Pool:    pool,
		Notify:  func(_, _ string) {},
	})

	var seen []string
	ptm.WrapAgentFactory(func(prev CreateAgentFunc) CreateAgentFunc {
		r, _ := prev(context.Background(), "probe", "")
		out, _ := r.Execute(context.Background(), "x")
		seen = append(seen, out)
		return func(ctx context.Context, role, sp string) (AgentRunner, error) {
			return prev(ctx, role, sp) // 原样回退, 便于断言"回退的是谁"
		}
	})

	if len(seen) != 2 {
		t.Fatalf("wrap 调用次数 = %d, 期望 2 (两个字段不同源, 各自包装)", len(seen))
	}
	if got := acquireTag(t, pool, "coder"); got != "pool-local:coder" {
		t.Errorf("pool 的本地回退被换成了阶段工厂: %q, 期望 pool-local:coder", got)
	}
}

// ---------------------------------------------------------------------------
// 等价性: 未接线远程 runtime 时行为必须一字不变
// ---------------------------------------------------------------------------

// 生产默认 (未配 --dispatch-mode queue) 压根不会调 WrapAgentFactory, 池必须原样。
func TestSwarm路径_未接线时行为等价(t *testing.T) {
	_, pool := newPTMWithPool(t, tagFactory("local"))
	if got := acquireTag(t, pool, "coder"); got != "local:coder" {
		t.Errorf("未接线时 = %q, 期望 local:coder", got)
	}
	// 统计口径也不能变。
	st := pool.Stats()
	if st.TotalSpawns != 1 || st.TotalDone != 1 || st.ActiveCount != 0 {
		t.Errorf("池统计被改动: %+v", st)
	}
}

// wrap 返回 nil = 放弃替换 (与 WrapAgentFactory 同语义), 且 nil wrap / nil 池不 panic。
func TestSwarm路径_退化输入守卫(t *testing.T) {
	ptm, pool := newPTMWithPool(t, tagFactory("local"))
	ptm.WrapAgentFactory(nil)
	pool.WrapFactory(nil)
	ptm.WrapAgentFactory(func(CreateAgentFunc) CreateAgentFunc { return nil })
	if got := acquireTag(t, pool, "coder"); got != "local:coder" {
		t.Errorf("放弃替换后 = %q, 期望 local:coder", got)
	}

	// 没有池的 PTM (CLI 的部分形态) 仍只换阶段工厂, 不 panic。
	noPool := NewProductionTeamManager(TeamManagerConfig{
		BaseDir: filepath.Join(t.TempDir(), "teams"),
		Factory: tagFactory("local"),
		Notify:  func(_, _ string) {},
	})
	noPool.WrapAgentFactory(func(CreateAgentFunc) CreateAgentFunc { return tagFactory("remote") })
	r, err := noPool.AgentFactory()(context.Background(), "c", "")
	if err != nil {
		t.Fatalf("无池 PTM 换工厂失败: %v", err)
	}
	if out, _ := r.Execute(context.Background(), "x"); out != "remote:c" {
		t.Errorf("无池 PTM 阶段工厂 = %q", out)
	}

	var nilPool *AgentPool
	nilPool.WrapFactory(func(CreateAgentFunc) CreateAgentFunc { return nil }) // 不 panic
	if nilPool.Factory() != nil {
		t.Error("nil 池的 Factory() 应为 nil")
	}
}

// 工厂缺席时 Acquire 必须诚实报错而不是 panic ——
// swarm 子任务失败会被记成 TaskFailed 继续跑, 而 nil 解引用会带走整个 bot 进程。
func TestSwarm路径_无工厂时诚实报错(t *testing.T) {
	pool := NewAgentPool(nil, 2)
	if _, err := pool.Acquire(context.Background(), "coder", ""); err == nil {
		t.Fatal("无工厂时 Acquire 应报错")
	}
	// 报错路径必须归还槽位, 否则连续几次就把池占满了。
	if st := pool.Stats(); st.ActiveCount != 0 || st.TotalFailed != 1 {
		t.Errorf("失败路径未归还槽位: %+v", st)
	}
}

// -race 下并发 Acquire: 改造前 Acquire 裸读 p.factory, 装配期换工厂与运行期读取
// 之间没有 happens-before。现在读取持锁。
//
// 读写必须**真的重叠**才探得到竞争: 一次性各起一个 goroutine 往往在检测器看到之前
// 就跑完了 (裸读版本第一版这么写就是绿的 —— 那种"绿"什么都没证明)。这里读侧与
// 写侧各自循环, 由 stop 通道同时收尾。
func TestSwarm路径_运行期读工厂无竞争(t *testing.T) {
	pool := NewAgentPool(tagFactory("local"), 8)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				a, err := pool.Acquire(context.Background(), fmt.Sprintf("r%d-%d", n, j), "")
				if err != nil {
					return
				}
				pool.Release(a)
			}
		}(i)
	}
	// 与并发 Acquire 同时换工厂 —— 语义上不推荐 (见 WrapFactory 注释), 但绝不该是
	// 数据竞争: -race 该在这里报 DATA RACE, 而不是让它在生产偶发。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			pool.WrapFactory(func(CreateAgentFunc) CreateAgentFunc { return tagFactory("remote") })
		}
		close(stop)
	}()
	wg.Wait()
}
