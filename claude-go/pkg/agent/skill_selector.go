// skill_selector.go —— 技能选择与编排 (design/01 §4.7)。
//
// # 此前的形态与它的两个问题
//
// 技能注入原先是 roles.go 里一段静态拼接: `role.Skills`(文件路径) +
// `role.BuiltinSkills` + `RecommendedSkills(roleName)`, 用**条数上限**(3 个) 与
// **单技能字符上限**(800) 截断, 拼成 `<role_skills>` 块。两个问题:
//
//  1. **与任务内容无关**: `RecommendedSkills` 只按角色名索引, 完全不看本次
//     objective。同一个角色不论要做什么, 拿到的技能永远一样 —— 于是要么塞了用不上
//     的技能白烧 token, 要么真正相关的技能因为不在该角色的推荐表里而拿不到。
//  2. **预算是"条数×单条上限"而非总量**: 3 条 × 800 字符看着有界, 但文件型技能另有
//     2 条 × 1200 字符, 且各处上限彼此独立 —— 实际注入量没有一个地方能一眼看出上界,
//     也无法按"这次给技能多少 token 预算"来调。
//
// # 本文件的取舍
//
//   - **纯确定性打分, 不引入 LLM**。选技能这一步若要调 LLM, 就得在每个 stage 前多一次
//     往返, 而它要解决的恰恰是省 token 的问题。用关键词重叠打分, 可测、可解释、零成本。
//   - **显式声明永远优先**: 角色明确写了的技能一定进, 不参与打分竞争。选择器只决定
//     "剩余预算给谁", 不推翻人的显式意图。
//   - **总量预算取代条数上限**: 按估算字符总量卡, 并把被预算挤掉的技能记进
//     `Dropped` —— 此前被截断的技能是静默消失的, 没人知道注入里少了什么。
//   - **不改变默认行为**: `SelectForRole` 在未配置预算时退化为与原逻辑同序同量的结果,
//     roles.go 只在显式启用时才走选择器。6 个下游平台的注入内容不能因本文件而变。
package agent

import (
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/skills"
)

// SkillSource 技能进入注入清单的原因, 供可观测与调试 (为什么这个技能在这儿)。
type SkillSource string

const (
	SkillSourceDeclared  SkillSource = "declared"  // 角色显式声明, 不参与竞争
	SkillSourceRecommend SkillSource = "recommend" // 角色推荐表
	SkillSourceQuery     SkillSource = "query"     // 与本次 objective 关键词相关
)

// SkillPick 一条选中的技能及其理由。
type SkillPick struct {
	Name   string
	Source SkillSource
	Score  float64 // query 来源的相关度; declared 恒为 +Inf 语义上的最高优先(用 1e9 表示)
	Chars  int     // 正文字符数, 用于预算核算
}

// SkillSelection 一次选择的结果。
type SkillSelection struct {
	Picks []SkillPick
	// Dropped 因预算被挤掉的技能名。此前截断是静默的, 记下来才能回答
	// "为什么这个技能没被注入"。
	Dropped []string
	// UsedChars 实际选入的正文字符总量。
	UsedChars int
}

// Names 返回选中的技能名 (按选择顺序)。
func (s SkillSelection) Names() []string {
	out := make([]string, 0, len(s.Picks))
	for _, p := range s.Picks {
		out = append(out, p.Name)
	}
	return out
}

// SkillSelectorConfig 选择器参数。零值表示"退化为原行为"(不限预算、不做 query)。
type SkillSelectorConfig struct {
	// CharBudget 注入正文的字符总预算; <=0 表示不限 (退化为原行为)。
	CharBudget int
	// MaxSkills 条数上限; <=0 表示不限条数 (仅受 CharBudget 约束)。
	MaxSkills int
	// EnableQuery 是否按 objective 关键词做相关性补选; false 时只用声明 + 推荐。
	EnableQuery bool
	// MinQueryScore query 补选的最低相关度, 低于此值不入选 (默认 1 = 至少命中一个词)。
	MinQueryScore float64
}

// skillRegistryView 选择器需要的最小注册表能力。
// 用接口而非具体类型: 便于测试注入假注册表, 也不把 pkg/agent 绑死在 skills.Registry 上。
type skillRegistryView interface {
	Get(name string) (*skills.Skill, bool)
	Active() []*skills.Skill
}

// SkillSelector 技能选择器。
type SkillSelector struct {
	reg skillRegistryView
	cfg SkillSelectorConfig
}

// NewSkillSelector 构造。reg 为 nil 时返回 nil —— 调用方据此回落到原静态逻辑。
func NewSkillSelector(reg skillRegistryView, cfg SkillSelectorConfig) *SkillSelector {
	if reg == nil {
		return nil
	}
	if cfg.MinQueryScore <= 0 {
		cfg.MinQueryScore = 1
	}
	return &SkillSelector{reg: reg, cfg: cfg}
}

// Select 为一次执行选技能。
//
//	declared   角色显式声明的技能名 (永远入选, 不参与打分)
//	recommend  角色推荐表给出的技能名 (按给定顺序竞争)
//	objective  本次任务描述; EnableQuery 时用于相关性补选
//
// 选择顺序: declared → recommend → query 补选, 每步都受预算约束。
// 顺序即优先级: 人的显式意图 > 角色画像 > 内容相关性。
func (s *SkillSelector) Select(declared, recommend []string, objective string) SkillSelection {
	var sel SkillSelection
	if s == nil {
		return sel
	}
	seen := map[string]bool{}

	// 预算核算: 未配置 CharBudget 时视为无限, 与原行为一致。
	budget := s.cfg.CharBudget
	unlimited := budget <= 0
	fits := func(n int) bool {
		if unlimited {
			return true
		}
		return sel.UsedChars+n <= budget
	}
	countOK := func() bool {
		return s.cfg.MaxSkills <= 0 || len(sel.Picks) < s.cfg.MaxSkills
	}

	add := func(name string, src SkillSource, score float64) {
		if name == "" || seen[name] {
			return
		}
		sk, ok := s.reg.Get(name) // Get 已排除 shadow/archived (治理状态在运行期生效)
		if !ok {
			return
		}
		seen[name] = true
		n := len(sk.Body)
		// declared 是人的显式意图: 即使超预算也入选, 只是会把后续挤掉。
		// 若连显式声明都因预算被丢, 用户会看到"我明明写了却没生效"的困惑。
		if src != SkillSourceDeclared && (!fits(n) || !countOK()) {
			sel.Dropped = append(sel.Dropped, name)
			return
		}
		sel.Picks = append(sel.Picks, SkillPick{Name: name, Source: src, Score: score, Chars: n})
		sel.UsedChars += n
	}

	for _, n := range declared {
		add(n, SkillSourceDeclared, 1e9)
	}
	for _, n := range recommend {
		add(n, SkillSourceRecommend, 0)
	}

	if s.cfg.EnableQuery && strings.TrimSpace(objective) != "" {
		for _, c := range s.rankByQuery(objective, seen) {
			if !countOK() || !fits(c.Chars) {
				sel.Dropped = append(sel.Dropped, c.Name)
				continue
			}
			add(c.Name, SkillSourceQuery, c.Score)
		}
	}
	return sel
}

// rankByQuery 按 objective 与技能 description/when_to_use 的关键词重叠打分, 降序返回。
//
// 只看 description 与 when_to_use 而不看正文: 正文动辄数千字, 与长文本做重叠会让
// 几乎所有技能都"相关"; 而这两个字段本就是作者写的"我是干什么的/什么时候用我",
// 正是用于匹配的字段。
func (s *SkillSelector) rankByQuery(objective string, skip map[string]bool) []SkillPick {
	terms := tokenizeForSkillMatch(objective)
	if len(terms) == 0 {
		return nil
	}
	var out []SkillPick
	for _, sk := range s.reg.Active() {
		if sk == nil || skip[sk.Name] {
			continue
		}
		hay := tokenizeForSkillMatch(sk.Description + " " + sk.WhenToUse + " " + sk.Name)
		if len(hay) == 0 {
			continue
		}
		score := 0.0
		for t := range terms {
			if hay[t] {
				score++
			}
		}
		if score < s.cfg.MinQueryScore {
			continue
		}
		out = append(out, SkillPick{Name: sk.Name, Source: SkillSourceQuery, Score: score, Chars: len(sk.Body)})
	}
	// 相关度降序; 同分按名字升序保证**确定性**——注入内容必须可复现,
	// 否则同一任务两次跑会得到不同 prompt, prompt 缓存也跟着失效。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// tokenizeForSkillMatch 极简分词: 按非字母数字切分并小写, 丢掉长度 1 的碎片。
//
// 中文没有空格, 故对 CJK 额外按**双字滑窗**切分 —— 这与仓内既有的字符级 n-gram
// 相似度思路一致 (本仓无 embedding 端点), 足够把"事件溯源""快照恢复"这类词组
// 匹配上, 而不引入分词依赖。
func tokenizeForSkillMatch(s string) map[string]bool {
	out := map[string]bool{}
	var cur []rune
	flush := func() {
		if len(cur) >= 2 {
			out[strings.ToLower(string(cur))] = true
		}
		cur = cur[:0]
	}
	var cjk []rune
	flushCJK := func() {
		for i := 0; i+1 < len(cjk); i++ {
			out[string(cjk[i:i+2])] = true
		}
		cjk = cjk[:0]
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			flushCJK()
			cur = append(cur, r)
		case r >= 0x4E00 && r <= 0x9FFF: // CJK 统一汉字
			flush()
			cjk = append(cjk, r)
		default:
			flush()
			flushCJK()
		}
	}
	flush()
	flushCJK()
	return out
}
