// experience.go —— 经验记忆闭环 (手册 13.3.3 L7: ExpeL/ReasoningBank 式经验卡)。
//
// 闭环形态 (全部冻结权重):
//
//	会话末蒸馏: 把本次会话轨迹交给独立档位反思器 (H3 不同源), 蒸馏 1-3 条
//	  经验卡 (任务关键词/规则/来源), 追加进 JSONL 卡库;
//	会话初注入: 按当前任务文本的关键词重合度检索 top-K, 注入 system prompt
//	  的 <experience> 段——弱模型只负责"读经验", 阅读比自省容易 (13.3.3 L7 原则)。
//
// 纪律 (13.3.6): 卡带溯源与成功率元数据; 注入导致 replay 回归即整批回滚;
// 检索是确定性关键词重合 (无 embedding 依赖, 25 tok/s 场景不多花一次调用)。
package learners

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ExperienceCard 一条经验卡。
type ExperienceCard struct {
	ID        string   `json:"id"`
	TaskKeys  []string `json:"task_keys"` // 任务关键词 (检索命中面)
	Rule      string   `json:"rule"`      // 可执行规则 (命令式, 一句话)
	Source    string   `json:"source"`    // 溯源: 会话/草案/手工
	Wins      int      `json:"wins"`      // 注入后任务成功次数 (门禁元数据)
	Fails     int      `json:"fails"`     // 注入后任务失败次数
	CreatedAt string   `json:"created_at"`
}

// experiencePath 卡库位置。
func experiencePath(stateDir string) string {
	return filepath.Join(evoDir(stateDir), "experience_cards.jsonl")
}

// LoadExperienceCards 读卡库 (不存在返回空, 不报错)。
func LoadExperienceCards(stateDir string) ([]ExperienceCard, error) {
	data, err := os.ReadFile(experiencePath(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ExperienceCard
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c ExperienceCard
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("经验卡第 %d 行解析失败: %w", i+1, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// AppendExperienceCards 追加写入 (流式, 中断不丢已写)。
func AppendExperienceCards(stateDir string, cards []ExperienceCard) error {
	if len(cards) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(experiencePath(stateDir)), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(experiencePath(stateDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, c := range cards {
		if c.ID == "" {
			c.ID = "exp-" + shortHash(c.Rule)
		}
		if c.CreatedAt == "" {
			c.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// RetrieveExperience 按任务文本检索 top-K 经验卡。
//
// 评分 = 关键词重合数×2 + (Wins−Fails)×0.5 + 新鲜度微调; 只返回正分卡。
// 关键词重合: TaskKeys 中每个词出现在任务文本里计 1 分 (大小写不敏感子串)。
// 刻意不用 embedding: 一次 embedding 调用对本机箱也是开销, 而卡的关键词本来就
// 是为"被检索"设计的受控词表。
func RetrieveExperience(cards []ExperienceCard, taskText string, topK int) []ExperienceCard {
	if topK <= 0 {
		topK = 3
	}
	task := strings.ToLower(taskText)
	type scored struct {
		card  ExperienceCard
		score float64
	}
	var hits []scored
	for _, c := range cards {
		overlap := 0
		for _, k := range c.TaskKeys {
			k = strings.ToLower(strings.TrimSpace(k))
			if k != "" && strings.Contains(task, k) {
				overlap++
			}
		}
		sc := float64(overlap)*2 + float64(c.Wins-c.Fails)*0.5
		if sc > 0 {
			hits = append(hits, scored{c, sc})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].card.ID < hits[j].card.ID
	})
	var out []ExperienceCard
	for i, h := range hits {
		if i >= topK {
			break
		}
		out = append(out, h.card)
	}
	return out
}

// FormatExperienceHints 把检索到的卡渲染成 prompt 注入段 (空集返回空串)。
func FormatExperienceHints(cards []ExperienceCard) string {
	if len(cards) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<experience>\n")
	b.WriteString("以下来自历史任务的经验规则, 被验证有效, 请遵循:\n")
	for i, c := range cards {
		fmt.Fprintf(&b, "%d. %s\n", i+1, c.Rule)
	}
	b.WriteString("</experience>\n\n")
	return b.String()
}

// DistillExperience 会话末蒸馏: 反思器读轨迹摘要, 产出 1-3 条经验卡。
//
// 守卫 (对齐 EvolvePrompt 的三硬约束精神):
//   - 反思器必须独立档位 (调用方保证, nil 即拒);
//   - 产出必须可解析为 JSON 数组, 每条 Rule 非空且 <=200 字符 (防空洞/膨胀卡);
//   - 一次最多 3 条 (经验爆炸会稀释注入质量)。
func DistillExperience(ctx context.Context, r Reflector, transcript, source string) ([]ExperienceCard, error) {
	if r == nil {
		return nil, fmt.Errorf("learners: 未提供 Reflector, 经验蒸馏跳过 (H3: 蒸馏器与主模型不同源)")
	}
	if strings.TrimSpace(transcript) == "" {
		return nil, fmt.Errorf("learners: 轨迹为空, 不蒸馏")
	}
	sys := "你是经验蒸馏器。阅读一段 coding agent 会话轨迹摘要, 提炼可复用的经验规则。" +
		"只输出一个 JSON 数组, 不要任何其他文字。数组 1-3 个元素, 每个元素形如 " +
		"{\"task_keys\": [\"关键词1\", \"关键词2\"], \"rule\": \"一条命令式规则\"}。" +
		"规则要求: 具体可执行 (形如\"做 X 前先 Y\"\"失败时改 Z\"), 不超过 60 字; " +
		"只提炼本次轨迹里被验证有效或无效的做法, 不写泛泛的编程常识。"
	user := "=== 会话轨迹摘要 ===\n" + truncate(transcript, 6000) + "\n\n请输出经验卡 JSON 数组。"
	out, err := r.SimpleComplete(ctx, sys, user)
	if err != nil {
		return nil, fmt.Errorf("learners: 蒸馏调用失败: %w", err)
	}
	raw := strings.TrimSpace(stripFence(out))
	// 容错: 截取第一个 [ 到最后一个 ] (模型在 JSON 前后加废话是常态)
	if i, j := strings.Index(raw, "["), strings.LastIndex(raw, "]"); i >= 0 && j > i {
		raw = raw[i : j+1]
	}
	var parsed []struct {
		TaskKeys []string `json:"task_keys"`
		Rule     string   `json:"rule"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("learners: 蒸馏产出不可解析 (%v), 不入库", err)
	}
	var cards []ExperienceCard
	for _, p := range parsed {
		rule := strings.TrimSpace(p.Rule)
		if rule == "" || len(rule) > 600 {
			continue
		}
		if len(cards) >= 3 {
			break
		}
		cards = append(cards, ExperienceCard{
			TaskKeys: p.TaskKeys,
			Rule:     rule,
			Source:   source,
		})
	}
	if len(cards) == 0 {
		return nil, fmt.Errorf("learners: 蒸馏产出无有效卡, 不入库")
	}
	return cards, nil
}
