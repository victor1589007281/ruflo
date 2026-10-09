package learners

// target.go —— 草案作用对象的标识 (21.8.12 P1)。
//
// ---------------------------------------------------------------------------
// 问题是什么
// ---------------------------------------------------------------------------
//
// Proposal.Target 原本是一个**裸字符串**, 全仓对它的校验只有"非空" (workflow.go:301,
// gepa.go:235, evotools.go:343)。于是:
//
//   - 谁都能写进去任何字符串, 写错一个字母不会有任何报错, 只会让这份草案永远
//     match 不上任何东西 —— 而"草案没生效"与"没有这份草案"在下游看起来一样;
//   - 它只能表达"某工作流的某阶段"这一种对象 (格式 "<workflow>/<stage>",
//     见 pkg/agent/evolution_structure.go 的 resolveStagePrompt)。角色注释里写着
//     "或 role 名", 但**没有任何生产者真的产出过 role 形式的 target** ——
//     注释描述了一个不存在的形态 (这正是本仓库反复出现的那类"自述的判据从未实现");
//   - 六种资产 (agent/workflow/plugin/skill/tool/hook) 里只有 workflow 的 stage
//     享受得到版本与谱系。给技能也做一套, 就会长成第二套 Target。
//
// ---------------------------------------------------------------------------
// 改法: 给 target 一个**封闭的** kind 集合, 且不动在野的字节
// ---------------------------------------------------------------------------
//
// 形态 `<kind>:<name>`, 例如 `skill:commit` / `agent:coder`。与 pkg/asset.Asset.Key()
// 逐字节同形 —— 资产的标识与草案的作用对象是同一个东西, 分叉成两种写法必然漂移。
//
// **旧格式一个字节都不改**: 既有的 9 份草案里 target 是 `if-question-expand/增量出题`
// 这种 `<workflow>/<stage>`。把它们迁移成 `stage:...` 会让全部既有草案当场 match 不上,
// 而"match 不上"的表现是灰度静默不生效 (prompt_canary 找不到实验就原样返回) ——
// 一次没有报错的回退。所以:
//
//   - StageTarget 继续产出**逐字节相同**的旧字符串, 只是把它的定义收到这一处;
//   - 带标签形态只用于**从来没有过生产者**的那几种 kind, 不存在迁移问题;
//   - ParseTarget 把不带已知 kind 前缀的串一律判成 legacy, 原样交还。

import (
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/asset"
)

// TargetKind 草案作用对象的种类。
//
// 取值 = asset 的六种资产 + 一个 stage。stage 单列而不并进 workflow: "改这份工作流"
// 与"改这份工作流的第三阶段"是两个不同粒度的作用对象, 合并之后就没法只改一个阶段了。
type TargetKind string

const (
	// TargetKindLegacy 无标签的旧格式 ("<workflow>/<stage>")。
	//
	// 保留成一个**显式的**取值而不是用空串表示"不知道": 消费方必须能区分
	// "这是一份旧草案"与"这个 target 坏了", 前者要照常生效, 后者要报错。
	TargetKindLegacy TargetKind = "legacy"
	// TargetKindStage 某个工作流的某个阶段, 形如 `stage:development/implement`。
	TargetKindStage TargetKind = "stage"
)

// TargetKindFor 把资产 kind 转成草案 target kind (两者字符串相同, 这里只做合法性收口)。
func TargetKindFor(k asset.Kind) (TargetKind, error) {
	if !k.Valid() {
		return "", fmt.Errorf("learners: 资产 kind %q 非法", k)
	}
	return TargetKind(k), nil
}

// FormatTarget 拼出带标签的 target。
//
// name 里不允许出现换行与控制字符: target 会进 jsonl 与日志, 一个嵌了换行的
// name 能把一条记录掰成两行, 而下游按行读的解析器会静默丢掉后半截。
func FormatTarget(kind TargetKind, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("learners: target 的 name 不能为空")
	}
	if strings.ContainsAny(name, "\n\r\t") {
		return "", fmt.Errorf("learners: target 的 name 含控制字符 (%q)", name)
	}
	switch kind {
	case TargetKindLegacy:
		return "", fmt.Errorf("learners: 不能构造 legacy 形式的 target (它是解析的产物, 不是可构造的形态)")
	case TargetKindStage, TargetKind(asset.KindAgent), TargetKind(asset.KindWorkflow),
		TargetKind(asset.KindPlugin), TargetKind(asset.KindSkill),
		TargetKind(asset.KindTool), TargetKind(asset.KindHook):
		return string(kind) + ":" + name, nil
	}
	return "", fmt.Errorf("learners: 未知的 target kind %q", kind)
}

// ParseTarget 解析 target。
//
// 判定顺序 (这个顺序就是全部要害):
//  1. 空 → 报错。一个空 target 在旧代码里是一份"永远 match 不上"的草案, 静默无效。
//  2. 冒号前的部分**是**已知 kind → 带标签形态。
//  3. 其余一律 legacy, Name 是原串。
//
// 第 3 条必须兜底而不是报错: 旧格式 `<workflow>/<stage>` 里的 workflow 名可以含冒号,
// 也可以恰好叫 "skill"。把它按未知格式拒掉, 等于让一批在跑的灰度实验当场失效。
func ParseTarget(s string) (kind TargetKind, name string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", fmt.Errorf("learners: target 为空 (空 target 的草案永远不会生效)")
	}
	prefix, rest, ok := strings.Cut(s, ":")
	if ok {
		if k, kerr := normalizeTargetKind(prefix); kerr == nil {
			rest = strings.TrimSpace(rest)
			if rest == "" {
				return "", "", fmt.Errorf("learners: target %q 的 kind 后缺少名字", s)
			}
			return k, rest, nil
		}
	}
	return TargetKindLegacy, s, nil
}

// normalizeTargetKind 判断一个前缀是不是已知 kind。
func normalizeTargetKind(prefix string) (TargetKind, error) {
	p := strings.ToLower(strings.TrimSpace(prefix))
	switch TargetKind(p) {
	case TargetKindStage, TargetKind(asset.KindAgent), TargetKind(asset.KindWorkflow),
		TargetKind(asset.KindPlugin), TargetKind(asset.KindSkill),
		TargetKind(asset.KindTool), TargetKind(asset.KindHook):
		return TargetKind(p), nil
	}
	// "legacy" 是解析产物, 不接受被**构造**出来: 允许它作为前缀会让
	// `legacy:x` 这类字符串有一个"看起来合法"的解释, 而它没有任何生产者。
	return "", fmt.Errorf("learners: %q 不是已知的 target kind", prefix)
}

// ValidateTarget 校验一个 target 字符串 (生产者写盘前的闸)。
//
// 存在的理由: 此前全仓对 target 只有 `!= ""` 一项校验, 于是拼错的对象名会一路写到
// 磁盘, 直到消费方 match 不上为止 —— 而没有报错。这里至少把"形状"卡住:
// 非空、kind 前缀合法 (或明确是 legacy)、名字里没有控制字符。
//
// 它**不**校验对象是否存在 (target 指向的技能可能还没被注册, 那份草案是给将来的),
// 也不校验名字的拼写 —— 那不是形状问题。
func ValidateTarget(s string) error {
	kind, name, err := ParseTarget(s)
	if err != nil {
		return err
	}
	if strings.ContainsAny(name, "\n\r\t") {
		return fmt.Errorf("learners: target %q 的名字含控制字符", s)
	}
	if kind == TargetKindLegacy && !strings.Contains(name, "/") {
		// 旧格式是 "<workflow>/<stage>"。不带斜杠的 legacy target 既不可能是旧格式,
		// 也不是带标签格式 —— 它是一个形状不明的串。放过去会让它永远 match 不上。
		return fmt.Errorf("learners: target %q 既非 <kind>:<name> 也不含 '/', 形状不明", s)
	}
	return nil
}

// StageTarget 构造"某工作流的某阶段"的 target (**旧格式, 逐字节不变**)。
//
// 定义收到这里而不是散在生产者 (pkg/agent/evolution_structure.go) 与消费者
// (pkg/agent/prompt_canary.go) 各拼一次: 两边各拼一遍正是"产得出草案却永远灰度不了"
// 的生成机制 —— 只要有一边改了拼法, 另一边不会报错, 只会静默不匹配。
func StageTarget(workflow, stage string) string {
	return strings.TrimSpace(workflow) + "/" + strings.TrimSpace(stage)
}

// AssetTarget 构造"某资产"的 target, 形如 `skill:commit`。
//
// 与 StageTarget 的差别不只是拼法: 资产 target 是**新**形态, 只有新形态的消费方
// (P3 起的资产版本与谱系) 会去 match 它, 因此它没有"既有草案会失效"的包袱。
func AssetTarget(kind asset.Kind, name string) (string, error) {
	tk, err := TargetKindFor(kind)
	if err != nil {
		return "", err
	}
	return FormatTarget(tk, name)
}
