package agent

// evo_tiers.go —— H6 多档冒烟的真实档位构造器 (design/03 §4.5)。
//
// 为什么这个文件必须存在: `builtin.SetEvoTierFactory` 是 H6"真跑多档"的注入点, 而它
// **零生产调用方** —— 于是 `evo_smoke` 只有一个内置的确定性"装配档", 而装配档不是模型
// 档位, 单独跑时 `MinTiers>=2` 必然拒绝。也就是说 H6 的闸语义写完了但从没真跑过多档:
// 标准的"已建成未通电"。本文件就是那条线。
//
// ## 为什么档位必须是不同的 model, 不能拿主模型凑
//
// H6 的立意是"同一个产物在**不同能力档位**上都不崩"。用主模型跑两遍只能证明它不
// 随机崩, 证不了"换个弱一点的模型也还能用" —— 而后者才是晋升一个技能/prompt 到
// 全量前真正要问的问题。`builtin` 包的注释已经明确拒绝"拿主模型凑一个假的第二档"。
//
// 所以这里的档位来源是**真实配置**: primary = 当前主模型, fallback = 配置里声明的
// 备用模型。没配 fallback 就只给 primary 一档 —— 那时 `Smoke` 会因 `MinTiers` 不足
// 而拒绝, **这是正确行为**, 不是缺陷: 没有第二档就确实没做过多档验证。
//
// ## 候选做什么
//
// 一次候选执行 = 把被测产物 (技能正文 / prompt 版本) 当 system prompt, 把冒烟任务的
// objective 当 user message, 打一次真实 LLM 调用。**刻意不套 QueryEngine**: 冒烟要
// 验的是"这份产物本身能不能驱动出可用产出", 套上工具循环后失败原因会混进工具与多轮
// 交互, 归因不到产物上。

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/evolution/replay"
)

// tierCandidate 一个档位下的被测产物候选 (实现 replay.Candidate)。
type tierCandidate struct {
	name   string
	client *api.Client
	body   string // 被测产物正文 (技能 SKILL.md / prompt 版本)
}

// Name 见 replay.Candidate。
func (c *tierCandidate) Name() string { return c.name }

// Produce 见 replay.Candidate: 产物作 system, 任务 objective 作 user, 打一次真调用。
//
// 错误原样上抛 —— `Smoke` 把错误计入该档失败率, 而失败率超阈值就是拒绝理由。
// 这里**绝不能**把错误吞掉返回空串: 空产出会被覆盖率判定当成"跑了但没评分",
// 与"根本没跑起来"混成一档, 冒烟结论就失真了。
func (c *tierCandidate) Produce(ctx context.Context, t replay.Task) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("evo 档位 %s: LLM 客户端未注入", c.name)
	}
	sys := strings.TrimSpace(c.body)
	if sys == "" {
		return "", fmt.Errorf("evo 档位 %s: 被测产物正文为空", c.name)
	}
	user := strings.TrimSpace(t.Objective)
	if user == "" {
		return "", fmt.Errorf("evo 档位 %s: 冒烟任务无 objective", c.name)
	}
	return c.client.SimpleComplete(ctx, sys, user)
}

// EvoTierFactory 构造 H6 多档冒烟的真实档位。
//
// 返回的档位数可能是 1 (没配 fallback) —— 调用方 (`Smoke`) 会因 MinTiers 不足而拒绝,
// 那是正确的: 没有第二档就确实没做过多档验证, 不该放行。
//
// 档位名固定为 primary/fallback-N, 便于报告比对与回归。
func EvoTierFactory(c *api.Client) func(ctx context.Context, product, body string) []replay.Tier {
	if c == nil {
		return nil
	}
	return func(_ context.Context, _, body string) []replay.Tier {
		tiers := []replay.Tier{{
			Name:      "primary",
			Model:     c.Model,
			Candidate: &tierCandidate{name: "primary:" + c.Model, client: c, body: body},
		}}
		// fallback 档位: 与 FallbackReflector 同一取法 —— WithModel 共享 HTTP 客户端与
		// RateLimitGuard, 冒烟调用同样受全局配额约束, 不会绕过限流偷跑。
		seen := map[string]bool{c.Model: true}
		for _, m := range c.FallbackModels {
			m = strings.TrimSpace(m)
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			fc := c.WithModel(m)
			tiers = append(tiers, replay.Tier{
				Name:      fmt.Sprintf("fallback-%d", len(tiers)),
				Model:     m,
				Candidate: &tierCandidate{name: "fallback:" + m, client: fc, body: body},
			})
			// 两档就够 MinTiers 了; 再多只是线性烧钱, 边际信息很小。
			// 真要跑更多档就显式提高上限, 不在这里默认全跑。
			if len(tiers) >= 2 {
				break
			}
		}
		return tiers
	}
}
