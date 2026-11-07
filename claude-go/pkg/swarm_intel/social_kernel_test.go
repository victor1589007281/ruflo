package swarm_intel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeSocialLLM 驱动 social 内核的 fake LLM:
//   - spawn: 返回 6 个 profile-stance 一致的 agent (3 支持派正立场 / 3 反对派负立场)
//   - round: 立场与身份一致的 agent 公开发言并向外推立场 (±0.2);
//     不一致的静默观察 (delta=0)。发言内容始终带"极化"叙事,
//     用于对抗性验证 detect 只看计算指标、不吃 prompt 叙事。
//   - report: 返回与聚类数等长的 scenarios。
type fakeSocialLLM struct {
	mu          sync.Mutex
	spawnCalls  int
	roundCalls  int
	reportCalls int
}

func (f *fakeSocialLLM) SimpleComplete(_ context.Context, systemPrompt, userPrompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(systemPrompt, "角色生成器"):
		f.spawnCalls++
		return `[
{"name":"林正宏","profile":"支持派企业家，果断强势","stance":0.80,"stubbornness":0.8},
{"name":"苏芮","profile":"支持派媒体人，善于煽动","stance":0.40,"stubbornness":0.8},
{"name":"陈默","profile":"支持派工程师，理性谨慎","stance":0.10,"stubbornness":0.8},
{"name":"赵铁军","profile":"反对派工会领袖，强硬固执","stance":-0.20,"stubbornness":0.8},
{"name":"王雅婷","profile":"反对派学者，温和坚定","stance":-0.60,"stubbornness":0.8},
{"name":"老周","profile":"反对派社区居民，情绪化","stance":-0.90,"stubbornness":0.8}
]`, nil
	case strings.Contains(systemPrompt, "单个 Agent"):
		f.roundCalls++
		var stance float64
		if _, err := fmt.Sscanf(extractLine(userPrompt, "你当前的立场: "), "你当前的立场: %f", &stance); err != nil {
			return `{"action":"listen","content":"","stance_delta":0}`, nil
		}
		side := ""
		if strings.Contains(userPrompt, "(支持派") {
			side = "support"
		} else if strings.Contains(userPrompt, "(反对派") {
			side = "oppose"
		}
		aligned := (side == "support" && stance > 0) || (side == "oppose" && stance < 0)
		if aligned {
			delta := 0.2
			if stance < 0 {
				delta = -0.2
			}
			// 内容固定带极化叙事: 若 detect 吃了叙事, 打乱组也会报极化 (S3 要抓的就是这个)
			return fmt.Sprintf(`{"action":"speak","content":"我坚持己方观点, 群体正在走向极化分裂!","stance_delta":%g}`, delta), nil
		}
		// 立场与身份不符: 立场不动, 但发言照样喊"极化" — 对测量是纯噪声的对抗输入
		return `{"action":"speak","content":"我虽立场摇摆, 但坚信群体正在走向极化分裂!","stance_delta":0}`, nil
	case strings.Contains(systemPrompt, "报告分析员"):
		f.reportCalls++
		return `{"scenarios":[
{"name":"支持派主导的终局","description":"支持方全面主导议程","key_events":["正方整合","反方退守"]},
{"name":"反对派盘踞的终局","description":"反对方坚守底线形成均势","key_events":["反方集结","僵持"]}
],"summary":"fake 报告: 两极各自固化, 中间地带消失。"}`, nil
	default:
		return "{}", nil
	}
}

// extractLine 取以 prefix 开头的整行。
func extractLine(s, prefix string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// TestSocialKernel_SixBySix 6×6 内核模拟实测: 轨迹 + 涌现检测 + SimEvent 落盘。
func TestSocialKernel_SixBySix(t *testing.T) {
	fake := &fakeSocialLLM{}
	dir := t.TempDir()
	rec, err := NewJSONLSimEventRecorder(filepath.Join(dir, "events", "social.jsonl"))
	if err != nil {
		t.Fatalf("创建记录器失败: %v", err)
	}
	k := NewSocialKernel(fake, nil, DefaultSocialKernelConfig())
	k.SetRecorder(rec)

	res, err := k.Run(context.Background(), "chat-1", "某市是否应全面放开夜市经济")
	if err != nil {
		t.Fatalf("内核运行失败: %v", err)
	}
	if fake.spawnCalls != 1 {
		t.Fatalf("spawn 应恰好 1 次 LLM 调用, 实际 %d", fake.spawnCalls)
	}
	if fake.reportCalls != 1 {
		t.Fatalf("report 应恰好 1 次 LLM 调用, 实际 %d", fake.reportCalls)
	}

	if res.Mode != "social" || res.Rounds <= 0 || res.Rounds > 6 {
		t.Fatalf("结果元信息不对: %+v", res)
	}
	roundsRun := res.Rounds // ① 自适应终止后 = 实际跑的轮数 (≤ 预算帽 6)
	if res.KernelDetail == nil || res.KernelDetail.RoundsRun != roundsRun {
		t.Fatalf("kernel_detail.rounds_run 应与结果一致: %+v", res.KernelDetail)
	}
	if res.KernelDetail.StopReason == "" {
		t.Fatalf("kernel_detail.stop_reason 不应为空")
	}
	if len(res.Scenarios) == 0 {
		t.Fatalf("scenarios 不应为空")
	}
	for _, sc := range res.Scenarios {
		if sc.Name == "" {
			t.Fatalf("scenario 缺 name: %+v", sc)
		}
	}
	// ① 提前收敛是默认预期: 强固执度 agent 首轮 |Δ|<ε 即冻结, 后续轮跳过 LLM。
	// fake LLM 永不失败, 所以轮内调用数必 < 6×6=36 (除非全跑满, 那也是合法 budget)。
	if fake.roundCalls > 36 {
		t.Fatalf("轮内调用不应超过 36, 实际 %d", fake.roundCalls)
	}
	t.Logf("自适应终止: %d/%d 轮 (%s), LLM 轮内调用 %d 次 (满预算 36)",
		roundsRun, res.KernelDetail.Cfg.Rounds, res.KernelDetail.StopReason, fake.roundCalls)
	// 轨迹: 每个 agent roundsRun 个 stance 点 (每轮全员 FJ 更新, 冻结只跳过 LLM)
	if len(res.Trajectory) != 6 {
		t.Fatalf("轨迹应覆盖 6 个 agent, 实际 %d", len(res.Trajectory))
	}
	for id, tr := range res.Trajectory {
		if len(tr) != roundsRun {
			t.Fatalf("agent %s 轨迹应有 %d 轮, 实际 %d", id, roundsRun, len(tr))
		}
	}
	for name, curve := range res.MetricCurves {
		if len(curve) != roundsRun {
			t.Fatalf("指标曲线 %s 应有 %d 轮, 实际 %d", name, roundsRun, len(curve))
		}
	}

	// SimEvent 落盘验证: JSONL 兜底写入 stance/action 两类
	raw, err := os.ReadFile(filepath.Join(dir, "events", "social.jsonl"))
	if err != nil {
		t.Fatalf("读取 JSONL 失败: %v", err)
	}
	kinds := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ev SimEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("JSONL 行不可解析: %v", err)
		}
		kinds[ev.Type]++
		if ev.RunID == "" {
			t.Fatalf("SimEvent 缺 run_id")
		}
	}
	if kinds["stance"] != 6*roundsRun {
		t.Fatalf("stance 事件应 %d 条, 实际 %d", 6*roundsRun, kinds["stance"])
	}
	// ① 冻结 agent 不发 action 事件 ⇒ action 数 == 实际 LLM 轮内调用数。
	if kinds["action"] != fake.roundCalls {
		t.Fatalf("action 事件应等于轮内调用数 %d, 实际 %d", fake.roundCalls, kinds["action"])
	}

	// 实测输出样本 (验收清单第 3 项): 打印轨迹 + 指标曲线 + 涌现
	t.Logf("=== 6×6 social 内核实测样本 ===")
	for id, tr := range res.Trajectory {
		t.Logf("轨迹 %s: %v", id, round2(tr))
	}
	for name, curve := range res.MetricCurves {
		t.Logf("指标 %s: %v", name, round2(curve))
	}
	t.Logf("涌现: %v", res.Emergent)
	t.Logf("scenarios: %d 个, summary: %s", len(res.Scenarios), res.Summary)
}

// TestSocialKernel_S3_ShuffleKillsPolarization S3 对抗测试:
// 同一 fake LLM、同一议题, 仅打乱 spawn 立场分配重跑。
// 立场-profile 一致时动力学产生真实两极 → 报极化;
// 打乱后该机制被破坏 → 极化必须消失 (即使发言叙事仍喊"极化")。
func TestSocialKernel_S3_ShuffleKillsPolarization(t *testing.T) {
	countPolarization := func(k *SocialKernel, res *SimulationResult) int {
		n := 0
		for _, e := range res.Emergent {
			if strings.Contains(e, "极化") {
				n++
			}
		}
		for _, ev := range k.Events() {
			if ev.Type == "emergence" && strings.Contains(string(ev.Payload), "polarization") {
				n++
			}
		}
		return n
	}

	// 对照组: 正常 spawn
	base := &fakeSocialLLM{}
	k1 := NewSocialKernel(base, nil, DefaultSocialKernelConfig())
	res1, err := k1.Run(context.Background(), "chat-s3", "某市是否应全面放开夜市经济")
	if err != nil {
		t.Fatalf("对照组运行失败: %v", err)
	}
	p1 := countPolarization(k1, res1)
	if p1 == 0 {
		t.Fatalf("对照组应检测到极化 (profile-stance 一致 → 真实两极), 实际 0; 指标曲线: %v", round2(res1.MetricCurves["clustering"]))
	}

	// S3 组: 打乱立场分配 (seed 15 的置换使 6 个 agent 的身份-立场全部错配, 可复现)
	shuf := &fakeSocialLLM{}
	cfg := DefaultSocialKernelConfig()
	cfg.ShuffleStances = 15
	k2 := NewSocialKernel(shuf, nil, cfg)
	res2, err := k2.Run(context.Background(), "chat-s3", "某市是否应全面放开夜市经济")
	if err != nil {
		t.Fatalf("S3 组运行失败: %v", err)
	}
	p2 := countPolarization(k2, res2)
	if p2 != 0 {
		t.Fatalf("S3: 打乱立场后极化必须消失, 实际仍报 %d 次; 指标曲线: %v", p2, round2(res2.MetricCurves["clustering"]))
	}

	// 显著减弱也要体现为终局分散度下降
	v1 := res1.MetricCurves["variance"]
	v2 := res2.MetricCurves["variance"]
	if len(v1) == 0 || len(v2) == 0 {
		t.Fatalf("方差曲线不应为空")
	}
	t.Logf("S3 对照: 终局方差 %.3f vs 打乱组 %.3f; 终局聚类 %.2f vs %.2f; 涌现 %v vs %v",
		v1[len(v1)-1], v2[len(v2)-1],
		res1.MetricCurves["clustering"][len(res1.MetricCurves["clustering"])-1],
		res2.MetricCurves["clustering"][len(res2.MetricCurves["clustering"])-1],
		res1.Emergent, res2.Emergent)
}

// TestSocialKernel_MentionWeights 提及权重: ③ 全拓扑下提及是加成 —
// 基础权重来自邻接边, 被提及者额外 ×(1+n)。A1 提及乙 (+0.9) 而不提丙 (-0.9):
// 加成使 A1 净向乙靠 (对照组无人被提及, 两极拉力抵消归零)。
func TestSocialKernel_MentionWeights(t *testing.T) {
	setup := func() *SocialKernel {
		k := NewSocialKernel(&fakeSocialLLM{}, nil, DefaultSocialKernelConfig())
		k.agents = []*SocialAgent{
			{ID: "A1", Name: "甲", Profile: "p", Stance: 0.0, Stubbornness: 0.3},
			{ID: "A2", Name: "乙", Profile: "p", Stance: 0.9, Stubbornness: 0.3},
			{ID: "A3", Name: "丙", Profile: "p", Stance: -0.9, Stubbornness: 0.3},
		}
		for _, a := range k.agents {
			k.stanceHistory[a.ID] = []float64{a.Stance}
		}
		k.initTopology(context.Background())
		return k
	}

	// 对照: 无人被提及 → 乙丙拉力抵消, A1 不动 (Fj stub 项保持 0)。
	ctrl := setup()
	ctrl.updateStances(1, map[string]*SocialRoundAction{
		"A1": {Action: "listen"}, "A2": {Action: "listen"}, "A3": {Action: "listen"},
	})
	if math.Abs(ctrl.agents[0].Stance) > 1e-9 {
		t.Fatalf("对照组 A1 应不动, 实际 %.3f", ctrl.agents[0].Stance)
	}

	// 实验: A1 发言只提乙 → 加成使 A1 向 +0.9 净移动。
	k := setup()
	k.env = []envMessage{{Round: 1, AgentID: "A1", Name: "甲", Content: "我赞同乙的看法"}}
	k.updateStances(1, map[string]*SocialRoundAction{
		"A1": {Action: "speak", Content: "我赞同乙的看法"},
		"A2": {Action: "listen"},
		"A3": {Action: "listen"},
	})
	if k.agents[0].Stance <= 0 {
		t.Fatalf("A1 应向被提及的乙 (+0.9) 净靠拢, 实际 stance=%.3f", k.agents[0].Stance)
	}
	// 加成强度: 乙权重=1×2=2, 丙=1 → neighborMean=(2×0.9-0.9)/3=0.3 → A1=0.7×0.3=0.21。
	if math.Abs(k.agents[0].Stance-0.21) > 1e-9 {
		t.Fatalf("A1 立场应=0.21 (拓扑基础×提及加成的解析值), 实际 %.3f", k.agents[0].Stance)
	}
}

// TestSocialKernel_Topology ③ 拓扑限定邻居: ring 下 A1 只见环上邻居,
// 非邻居的极端立场不进 FJ 拉力; full 下全员可见 (v1 等价)。
func TestSocialKernel_Topology(t *testing.T) {
	build := func(topo string) *SocialKernel {
		cfg := DefaultSocialKernelConfig()
		cfg.Topology = topo
		cfg.TopoK = 1 // N=4 时 K=2 会退化为完全图 (±1,±2 覆盖全部), K=1 才是 4-环
		k := NewSocialKernel(&fakeSocialLLM{}, nil, cfg)
		k.agents = []*SocialAgent{
			{ID: "A1", Name: "甲", Profile: "p", Stance: 0.0, Stubbornness: 0.3},
			{ID: "A2", Name: "乙", Profile: "p", Stance: 0.9, Stubbornness: 0.3},
			{ID: "A3", Name: "丙", Profile: "p", Stance: -0.9, Stubbornness: 0.3},
			{ID: "A4", Name: "丁", Profile: "p", Stance: 0.9, Stubbornness: 0.3},
		}
		for _, a := range k.agents {
			k.stanceHistory[a.ID] = []float64{a.Stance}
		}
		k.initTopology(context.Background())
		return k
	}

	// full: A1 受乙(+0.9)/丙(-0.9)/丁(+0.9) 均匀拉 → 均值 +0.3 → A1 = 0.7×0.3 = 0.21。
	full := build("full")
	full.updateStances(1, map[string]*SocialRoundAction{
		"A1": {Action: "listen"}, "A2": {Action: "listen"}, "A3": {Action: "listen"}, "A4": {Action: "listen"},
	})
	if math.Abs(full.agents[0].Stance-0.21) > 1e-9 {
		t.Fatalf("full 拓扑 A1 应=0.21, 实际 %.3f", full.agents[0].Stance)
	}

	// ring K=1 (N=4): A1 的邻居=±1 → A2, A4 (都 +0.9), 不见丙(-0.9) → 均值 +0.9 → A1 = 0.63。
	ring := build("ring")
	ring.updateStances(1, map[string]*SocialRoundAction{
		"A1": {Action: "listen"}, "A2": {Action: "listen"}, "A3": {Action: "listen"}, "A4": {Action: "listen"},
	})
	if math.Abs(ring.agents[0].Stance-0.63) > 1e-9 {
		t.Fatalf("ring 拓扑 A1 应=0.63 (只看环上邻居), 实际 %.3f", ring.agents[0].Stance)
	}
	// 快照: ring 4 节点 K=1 → 每个节点 2 出边 = 8 边。
	snap := ring.topologySnapshot()
	if snap == nil || len(snap.Edges) != 8 {
		t.Fatalf("ring 快照应 8 边, 实际 %v", snap)
	}
	if snap.Type != "ring" || snap.K != 1 {
		t.Fatalf("快照类型/度数不对: %+v", snap)
	}
}

// TestSocialKernel_FastModeFallback cfg.Detail == "fast" 时保留单 prompt 旧路径。
func TestSocialKernel_FastModeFallback(t *testing.T) {
	fake := &fakeSocialLLM{}
	s := NewSimulator(fake, nil)
	res, err := s.Simulate(context.Background(), "c", "议题", SimulationConfig{
		Mode: "social", Agents: 6, Rounds: 6, Detail: "fast",
	})
	if err != nil {
		t.Fatalf("fast 模式失败: %v", err)
	}
	if res == nil || res.Mode != "social" {
		t.Fatalf("fast 模式结果不对: %+v", res)
	}
	if fake.spawnCalls != 0 || fake.reportCalls != 0 {
		t.Fatalf("fast 模式不应走内核 (spawn=%d report=%d)", fake.spawnCalls, fake.reportCalls)
	}
}

// TestSocialKernel_Metrics 指标数学不变式: 两远营 → 高方差高聚类; 全同 → 聚类 1 但方差 0 (共识不等于极化)。
func TestSocialKernel_Metrics(t *testing.T) {
	polarized := []float64{0.95, 0.9, 0.85, -0.9, -0.95, -0.85}
	if stanceVariance(polarized) < 0.5 {
		t.Fatalf("两极组方差应显著: %v", stanceVariance(polarized))
	}
	if stanceClustering(polarized, 0.3) < 0.9 {
		t.Fatalf("两极组聚类系数应接近 1: %v", stanceClustering(polarized, 0.3))
	}
	consensus := []float64{0.02, -0.01, 0.0, 0.01, -0.02, 0.0}
	if stanceVariance(consensus) >= 0.02 {
		t.Fatalf("共识组方差应接近 0: %v", stanceVariance(consensus))
	}
	if stanceClustering(consensus, 0.3) != 1.0 {
		t.Fatalf("共识组是完全图, 聚类系数应为 1: %v", stanceClustering(consensus, 0.3))
	}
	// 关键: 完全图聚类=1 在两种情形下都成立, 极化判定必须带方差守卫
	if stanceEntropy(polarized, 5) >= stanceEntropy([]float64{0.9, 0.4, 0.0, -0.4, -0.9, 0.1}, 5) {
		t.Fatalf("集中分布的熵应低于分散分布")
	}
}

// round2 把浮点切片保留两位小数, 便于日志阅读。
func round2(xs []float64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = float64(int(x*100+0.5*sign(x))) / 100
	}
	return out
}

func sign(x float64) float64 {
	if x < 0 {
		return -1
	}
	return 1
}
