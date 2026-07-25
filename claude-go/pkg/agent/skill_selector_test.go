package agent

import (
	"testing"

	"github.com/anthropic/claude-go/pkg/skills"
)

// fakeSkillReg 满足 skillRegistryView，便于精确控制技能集合与正文长度。
type fakeSkillReg struct{ m map[string]*skills.Skill }

func newFakeReg(list ...*skills.Skill) *fakeSkillReg {
	r := &fakeSkillReg{m: map[string]*skills.Skill{}}
	for _, s := range list {
		r.m[s.Name] = s
	}
	return r
}
func (r *fakeSkillReg) Get(n string) (*skills.Skill, bool) { s, ok := r.m[n]; return s, ok }
func (r *fakeSkillReg) Active() []*skills.Skill {
	out := make([]*skills.Skill, 0, len(r.m))
	for _, s := range r.m {
		out = append(out, s)
	}
	return out
}

func sk(name, desc, when string, bodyLen int) *skills.Skill {
	body := make([]byte, bodyLen)
	for i := range body {
		body[i] = 'x'
	}
	return &skills.Skill{Name: name, Description: desc, WhenToUse: when, Body: string(body)}
}

// nil 选择器必须可空安全——调用方据此回落到原静态逻辑。
func TestSkillSelector_nil可空安全(t *testing.T) {
	if NewSkillSelector(nil, SkillSelectorConfig{}) != nil {
		t.Error("reg 为 nil 时应返回 nil")
	}
	var s *SkillSelector
	if got := s.Select([]string{"a"}, []string{"b"}, "obj"); len(got.Picks) != 0 {
		t.Error("nil 选择器应返回空选择")
	}
}

// 顺序即优先级：declared > recommend > query。
func TestSkillSelector_优先级顺序(t *testing.T) {
	reg := newFakeReg(
		sk("decl", "声明的", "", 100),
		sk("rec", "推荐的", "", 100),
		sk("qry", "事件溯源 与快照", "处理 事件溯源", 100),
	)
	s := NewSkillSelector(reg, SkillSelectorConfig{EnableQuery: true})
	got := s.Select([]string{"decl"}, []string{"rec"}, "如何做事件溯源")
	names := got.Names()
	if len(names) < 3 {
		t.Fatalf("应选中 3 个, 实得 %v", names)
	}
	if names[0] != "decl" || names[1] != "rec" {
		t.Errorf("优先级顺序错: %v", names)
	}
	if got.Picks[0].Source != SkillSourceDeclared || got.Picks[1].Source != SkillSourceRecommend {
		t.Errorf("来源标注错: %+v", got.Picks)
	}
}

// 总量预算取代条数上限：被挤掉的必须记进 Dropped（此前截断是静默的）。
func TestSkillSelector_预算挤掉的技能被记录(t *testing.T) {
	reg := newFakeReg(sk("a", "", "", 600), sk("b", "", "", 600), sk("c", "", "", 600))
	s := NewSkillSelector(reg, SkillSelectorConfig{CharBudget: 1000})
	got := s.Select(nil, []string{"a", "b", "c"}, "")
	if len(got.Picks) != 1 {
		t.Fatalf("预算 1000 只够 1 个 600, 实选 %d 个", len(got.Picks))
	}
	if len(got.Dropped) != 2 {
		t.Errorf("应记录 2 个被挤掉的, 实得 %v", got.Dropped)
	}
	if got.UsedChars != 600 {
		t.Errorf("UsedChars = %d, 期望 600", got.UsedChars)
	}
}

// 显式声明即使超预算也入选——否则用户会看到"我明明写了却没生效"。
func TestSkillSelector_声明的技能不被预算挤掉(t *testing.T) {
	reg := newFakeReg(sk("big", "", "", 5000), sk("rec", "", "", 100))
	s := NewSkillSelector(reg, SkillSelectorConfig{CharBudget: 500})
	got := s.Select([]string{"big"}, []string{"rec"}, "")
	if len(got.Picks) != 1 || got.Picks[0].Name != "big" {
		t.Fatalf("声明的 big 必须入选, 实得 %v", got.Names())
	}
	// 声明的已吃满预算, 推荐的应被挤掉
	if len(got.Dropped) != 1 || got.Dropped[0] != "rec" {
		t.Errorf("超预算后推荐项应被挤掉, Dropped=%v", got.Dropped)
	}
}

// query 补选按相关度降序；同分按名字升序保证确定性（否则 prompt 不可复现、缓存失效）。
func TestSkillSelector_query确定性排序(t *testing.T) {
	reg := newFakeReg(
		sk("zz", "事件溯源", "", 10),
		sk("aa", "事件溯源", "", 10),
		sk("hi", "事件溯源 快照 恢复", "", 10),
	)
	s := NewSkillSelector(reg, SkillSelectorConfig{EnableQuery: true})
	first := s.Select(nil, nil, "事件溯源 快照 恢复").Names()
	for i := 0; i < 5; i++ {
		if got := s.Select(nil, nil, "事件溯源 快照 恢复").Names(); !equalStrs(got, first) {
			t.Fatalf("同一输入两次结果不同: %v vs %v (注入必须可复现)", first, got)
		}
	}
	if first[0] != "hi" {
		t.Errorf("命中最多的应排第一, 实得 %v", first)
	}
	if len(first) >= 3 && !(first[1] == "aa" && first[2] == "zz") {
		t.Errorf("同分应按名字升序, 实得 %v", first)
	}
}

// 未启用 query 时不做相关性补选（退化为原行为）。
func TestSkillSelector_未启用query不补选(t *testing.T) {
	reg := newFakeReg(sk("rel", "事件溯源", "", 10))
	s := NewSkillSelector(reg, SkillSelectorConfig{})
	if got := s.Select(nil, nil, "事件溯源"); len(got.Picks) != 0 {
		t.Errorf("未启用 query 不应补选, 实得 %v", got.Names())
	}
}

// 未配置预算时不限量——这是"退化为原行为"的支点。
func TestSkillSelector_未配预算则不限(t *testing.T) {
	reg := newFakeReg(sk("a", "", "", 9000), sk("b", "", "", 9000))
	s := NewSkillSelector(reg, SkillSelectorConfig{})
	if got := s.Select(nil, []string{"a", "b"}, ""); len(got.Picks) != 2 {
		t.Errorf("未配预算应全选, 实得 %v", got.Names())
	}
}

// 去重：同名只入一次，且不因重复出现在 declared+recommend 而重复计费。
func TestSkillSelector_去重(t *testing.T) {
	reg := newFakeReg(sk("dup", "", "", 100))
	s := NewSkillSelector(reg, SkillSelectorConfig{})
	got := s.Select([]string{"dup"}, []string{"dup", "dup"}, "")
	if len(got.Picks) != 1 {
		t.Errorf("应去重为 1 个, 实得 %v", got.Names())
	}
	if got.UsedChars != 100 {
		t.Errorf("UsedChars = %d, 重复计费了", got.UsedChars)
	}
}

// 注册表里没有的名字被静默跳过（角色配置可能引用已删技能）。
func TestSkillSelector_未知技能跳过(t *testing.T) {
	reg := newFakeReg(sk("real", "", "", 10))
	s := NewSkillSelector(reg, SkillSelectorConfig{})
	got := s.Select([]string{"ghost", "real"}, nil, "")
	if len(got.Picks) != 1 || got.Picks[0].Name != "real" {
		t.Errorf("未知技能应被跳过, 实得 %v", got.Names())
	}
}

// MaxSkills 条数上限与预算共同生效。
func TestSkillSelector_条数上限(t *testing.T) {
	reg := newFakeReg(sk("a", "", "", 10), sk("b", "", "", 10), sk("c", "", "", 10))
	s := NewSkillSelector(reg, SkillSelectorConfig{MaxSkills: 2})
	got := s.Select(nil, []string{"a", "b", "c"}, "")
	if len(got.Picks) != 2 {
		t.Errorf("MaxSkills=2 应只选 2 个, 实得 %v", got.Names())
	}
	if len(got.Dropped) != 1 {
		t.Errorf("超出条数的应记 Dropped, 实得 %v", got.Dropped)
	}
}

// 中文按双字滑窗切分——本仓无 embedding 端点，且中文没有空格。
func TestTokenizeForSkillMatch_中文双字滑窗(t *testing.T) {
	got := tokenizeForSkillMatch("事件溯源")
	for _, want := range []string{"事件", "件溯", "溯源"} {
		if !got[want] {
			t.Errorf("缺少双字片段 %q, 实得 %v", want, got)
		}
	}
	// 英文按非字母数字切分并小写, 长度 1 的碎片丢掉
	en := tokenizeForSkillMatch("Event-Sourcing a B")
	if !en["event"] || !en["sourcing"] {
		t.Errorf("英文分词错: %v", en)
	}
	if en["a"] || en["b"] {
		t.Errorf("长度 1 的碎片不应保留: %v", en)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
