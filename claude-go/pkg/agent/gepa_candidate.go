// gepa_candidate.go —— GEPA/replay 用的可导出 prompt 候选 (手册 13.3.3 L5)。
//
// 与 tierCandidate (evo_tiers.go, 不导出) 同形, 但供 `evo replay`/`evo gepa-step`
// CLI 装配: 候选 = "一份 prompt 正文 + 一个真实模型档位", Produce 把 prompt 当
// system、任务 objective 当 user 打一次真实调用。刻意不套 QueryEngine ——
// 归因要落在产物本身 (evo_tiers.go 头注释的设计理由在此完全适用)。
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/evolution/replay"
)

// PromptCandidate 一份 prompt 正文在某模型档位上的回放候选 (replay.Candidate)。
type PromptCandidate struct {
	Label  string
	Body   string      // 被测 prompt 正文 (作 system)
	Client *api.Client // 被打分的模型档位 (如本地 lfm2.5/gemma4)
}

// 编译期接口断言: 必须与 replay.Candidate 严格同形。
var _ replay.Candidate = (*PromptCandidate)(nil)

// Name 见 replay.Candidate。
func (c *PromptCandidate) Name() string { return c.Label }

// Produce 见 replay.Candidate。错误原样上抛 (空产出与"没跑起来"必须可区分,
// 见 evo_tiers.go tierCandidate.Produce 的纪律注释)。
func (c *PromptCandidate) Produce(ctx context.Context, t replay.Task) (string, error) {
	if c == nil || c.Client == nil {
		return "", fmt.Errorf("prompt 候选 %s: LLM 客户端未注入", c.Label)
	}
	sys := strings.TrimSpace(c.Body)
	if sys == "" {
		return "", fmt.Errorf("prompt 候选 %s: 被测正文为空", c.Label)
	}
	user := strings.TrimSpace(t.Objective)
	if t.Input != "" {
		user = user + "\n\n" + strings.TrimSpace(t.Input)
	}
	if user == "" {
		return "", fmt.Errorf("prompt 候选 %s: 任务无 objective", c.Label)
	}
	return c.Client.SimpleComplete(ctx, sys, user)
}
