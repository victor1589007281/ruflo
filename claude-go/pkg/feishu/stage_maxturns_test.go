// stage_maxturns_test.go —— H4-3 落在**团队 stage 装配面**上的一半验收。
//
// pkg/agent/modelconfig 那边判档位本身 (值对不对、分档分明不明), 这里判它有没有
// 真的被接到 stage 的配置上 —— 而那是两件事: 档位解析全绿、Execute 里忘了调,
// 生产上每个 stage 仍然拿 40。方案 §21.5.6 的验收 (中位数 > 100) 要这类**接线的
// 读数**, 不是"有个函数返回了 150"。
//
// # 另一半 (真实 run 的中位数)
//
// 那条要 H4-1 的任务集 + 真实 run, 按本轮的验收边界 (逐项落地 + 单元验收, 不启动
// 真实 run) 不做。这里判的是它成立的必要条件: 上限本身够了、且接上了。
package feishu

import (
	"encoding/json"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
)

// TestH43_长程档真的抬高了stage上限 接线主臂。
func TestH43_长程档真的抬高了stage上限(t *testing.T) {
	// 现状: 模型级没声明 (0), 基线是 defaultResolved.MaxTurns = 25,
	// 长程档解析出 150。
	got := stageMaxTurns(0, 0, 25, 150)
	if got <= 100 {
		t.Fatalf("长程 stage 的上限 = %d, 必须 > 100 —— 这正是方案 §21.5.6 的验收前提; "+
			"实得这个数说明 150 没被接上, stage 仍停在基线", got)
	}
}

// TestH43_未开分层时stage上限逐字不变 本次改动最重要的回归臂。
//
// longTurns=0 表示"配置里没写 longHorizonMaxTurns"。此时上限必须**逐字**等于基线 ——
// 否则这次改动会在没有任何人配置的情况下, 改掉生产上每一个团队 stage 的止损线,
// 而且没有任何读数会指出这件事。
func TestH43_未开分层时stage上限逐字不变(t *testing.T) {
	for _, base := range []int{25, 40, 1, 150} {
		if got := stageMaxTurns(0, 0, base, 0); got != base {
			t.Fatalf("未开分层时 stage 上限 = %d, 应为基线 %d (逐字不变)", got, base)
		}
	}
}

// TestH43_配置写小了不缩小 —— 判据必须是"严格更大", 不是 >=。
//
// >= 的失效方向很难查: 运维配了个比 maxTurns 小的长程档 (复制粘贴事故),
// 结果长程任务的回合上限**反而变小**, 而配置看上去是对的。
func TestH43_配置写小了不缩小(t *testing.T) {
	if got := stageMaxTurns(0, 0, 40, 10); got != 40 {
		t.Fatalf("长程档 (10) 小于基线 (40) 时, 上限 = %d, 应为 40 —— "+
			"配了长程档反而跑得更短是最难归因的一种失效", got)
	}
}

// TestH43_只放开产码任务 反面臂 —— 分层的存在意义。
//
// 一个把 stageMaxTurns 写成"恒返回 longTurns"的实现能让第一条主臂全绿, 而它等价于
// 没分层。这条判: 常规任务与短程任务的上限**不得**被长程档的配置抬高。
func TestH43_只放开产码任务(t *testing.T) {
	res := modelconfig.NewConfigResolver(nil, modelconfig.GlobalConfig{
		DefaultMaxTurns: 25, LongHorizonMaxTurns: 150,
	}, nil)

	cases := []struct {
		name   string
		sig    modelconfig.TaskSignals
		wantGT bool // 是否应当超过基线
	}{
		{"产码工作流", modelconfig.TaskSignals{Role: "coder", ProducesCode: true}, true},
		{"不产码的普通任务", modelconfig.TaskSignals{Role: "coordinator", ProducesCode: false}, false},
		{"思考型角色", modelconfig.TaskSignals{Role: "architect", ProducesCode: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			class := modelconfig.ClassifyTask(c.sig)
			got := stageMaxTurns(0, 0, 25, res.MaxTurns(class))
			if (got > 25) != c.wantGT {
				t.Fatalf("上限 = %d (wantGT=%v) —— 分档没生效或分反了", got, c.wantGT)
			}
		})
	}
}

// TestH43_节点与模型的声明优先于档位 —— 优先级顺序的回归臂。
//
// 顺序写反 (档位排在前面) 会让两件事同时失效: 图节点显式声明的回合数被顶掉、
// 按模型写死的 kimi 40 被顶掉。两者都是"某个声明静默不生效", 在运行读数上只表现
// 为"这个 stage 跑了别的轮数", 无从归因。
func TestH43_节点与模型的声明优先于档位(t *testing.T) {
	t.Run("图节点声明赢", func(t *testing.T) {
		if got := stageMaxTurns(300, 0, 25, 150); got != 300 {
			t.Fatalf("节点声明 300 被改成了 %d —— 节点级声明的回合数此前从未被读过, "+
				"接上之后不该又让它被档位顶掉", got)
		}
	})
	t.Run("模型声明赢", func(t *testing.T) {
		if got := stageMaxTurns(0, 40, 25, 150); got != 40 {
			t.Fatalf("模型级 40 被改成 %d —— kimi 系配的 40 是按那个模型的回合开销"+
				"定的, 不该被按任务时程算出来的档位覆盖", got)
		}
	})
	t.Run("模型声明为 0 时档位才生效", func(t *testing.T) {
		if got := stageMaxTurns(0, 0, 25, 150); got != 150 {
			t.Fatalf("两条声明都没有时, 档位该生效, 实得 %d", got)
		}
	})
}

// TestH43_整条链路: 配置 -> 解析器 -> 档位 -> stage 上限。
//
// 这条臂的存在理由是**一次变异存活**: 把 sessionAgentRunner 里的
// `sm.modelResolver.MaxTurns(class)` 改成恒 0 (解析器根本没被问), 上面所有
// 用例照样全绿 —— 因为它们喂的是 stageMaxTurns 的四个参数, 而"第四个参数是
// 怎么算出来的"没有一条看着。那一处的失效正是最典型的**配了没生效**:
// 配置写得对、解析器返回得对、stage 拿到的仍是基线, 运行读数上看不出任何异常。
//
// 所以这条从**配置**起步, 一路走到 stage 上限, 中间不许有一步被替换成常量。
func TestH43_整条链路(t *testing.T) {
	reg := modelconfig.NewProviderRegistry()
	reg.RegisterProvider(modelconfig.ProviderConfig{
		Name: "p", BaseURL: "https://x.example/v1", APIKey: "k",
		Models: map[string]modelconfig.ModelConfig{"p:m": {}},
	})
	resolver := modelconfig.NewConfigResolver(reg, modelconfig.GlobalConfig{
		DefaultModelAlias: "p:m", DefaultMaxTurns: 25, LongHorizonMaxTurns: 150,
	}, nil)

	newRunner := func(workflow, role string) *sessionAgentRunner {
		sm := &SessionManager{}
		sm.SetModelResolver(resolver)
		return &sessionAgentRunner{sm: sm, role: role, runMeta: agent.RunMetadata{Workflow: workflow, Role: role}}
	}

	t.Run("产码工作流走到 150", func(t *testing.T) {
		r := newRunner("development", "coder")
		if got := r.resolveStageMaxTurns(0, 25); got != 150 {
			t.Fatalf("产码 stage 的上限 = %d, 应为配置里的 150 —— "+
				"这条链上任何一步被换成常量, 结果都会退回 25", got)
		}
	})

	t.Run("不产码工作流停在基线", func(t *testing.T) {
		r := newRunner("techblog", "coordinator")
		if got := r.resolveStageMaxTurns(0, 25); got != 25 {
			t.Fatalf("不产码 stage 的上限 = %d, 应为基线 25", got)
		}
	})

	t.Run("没注入解析器时退回基线而不是 panic", func(t *testing.T) {
		var sm *SessionManager
		r := &sessionAgentRunner{sm: sm, role: "coder", runMeta: agent.RunMetadata{Workflow: "development"}}
		// 刻意不判 got: sm 为 nil 时这里会先 panic, 那也是一种失败, 且比断言更显眼。
		if got := r.resolveStageMaxTurns(0, 25); got != 25 {
			t.Fatalf("无解析器时上限 = %d, 应为基线 25", got)
		}
	})
}

// TestH43_配置文件里的长程档真的读得进来 配置解析面。
//
// 这条臂的存在理由同样是**一次变异存活**: 把 ToModelConfigJSON 里
// `LongHorizonMaxTurns: jc.AI.LongHorizonMaxTurns` 那一行删掉, 上面所有用例
// 照样全绿 —— 因为它们都是手工构造 GlobalConfig, 没有一条经过 JSON 解析。
// 而那一行恰恰是运维唯一能碰到的地方: 删了它, 配置里写什么都不生效, 且不报错。
//
// 从**原始 JSON 字节**起步, 一路走到解析出的回合上限。
func TestH43_配置文件里的长程档真的读得进来(t *testing.T) {
	const raw = `{
	  "ai": {
	    "modelAlias": "p:m",
	    "maxTurns": 25,
	    "longHorizonMaxTurns": 150
	  }
	}`
	var jc JSONConfig
	if err := json.Unmarshal([]byte(raw), &jc); err != nil {
		t.Fatalf("测试用 JSON 不合法: %v", err)
	}
	if jc.AI == nil {
		t.Fatal("ai 段没解析出来 —— 用例本身失效, 后面的断言无从谈起")
	}
	if jc.AI.LongHorizonMaxTurns != 150 {
		t.Fatalf("JSON 里的 longHorizonMaxTurns 没解析进 AISection: %d —— "+
			"json tag 写错时这里就会是 0, 而 0 的语义是「没配置」", jc.AI.LongHorizonMaxTurns)
	}

	out := jc.ToModelConfigJSON()
	res := modelconfig.NewConfigResolver(
		modelconfig.NewProviderRegistry(), out.AI.GlobalConfig, nil)

	got := res.MaxTurns(modelconfig.TaskClassLongHorizon)
	if got != 150 {
		t.Fatalf("配置里写了 150, 解析出的长程档 = %d —— "+
			"ToModelConfigJSON 那条映射漏了, 而漏了不报错: 它只是安静地不生效", got)
	}
	if s := res.MaxTurns(modelconfig.TaskClassStandard); s != 25 {
		t.Fatalf("常规档 = %d, 应为 25 (不该被长程档的配置串味)", s)
	}
}

// TestH43_未装配解析器时不放行 降级装配 (测试 / 部分 CLI 路径)。
//
// 解析器为 nil 时 longHorizonMaxTurns 必须返回 0 (= 不覆盖), 而不是 panic,
// 也不是凭空造一个默认值 —— 后者会让"没配"与"配了 150"在读数上一样。
func TestH43_未装配解析器时不放行(t *testing.T) {
	var sm *SessionManager
	if got := sm.longHorizonMaxTurns(modelconfig.TaskClassLongHorizon); got != 0 {
		t.Fatalf("nil SessionManager 应返回 0 (不覆盖), 实得 %d", got)
	}

	bare := &SessionManager{}
	if got := bare.longHorizonMaxTurns(modelconfig.TaskClassLongHorizon); got != 0 {
		t.Fatalf("未注入解析器时应返回 0 (不覆盖), 实得 %d —— "+
			"凭空给一个默认值会让 没配 与 配了 在读数上完全一样", got)
	}
}
