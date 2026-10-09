// agent_assets.go —— agent 资产的**查表**与**清单** (21.8.12 P1 + P5)。
//
// ---------------------------------------------------------------------------
// 两个缺口, 同一条根
// ---------------------------------------------------------------------------
//
// 缺口 A (P5 前半): `subagent_type` 从来没有被查过表。模型在 Agent 工具里填什么,
// 就原样进 RunOptions.SubagentType, 然后只被用来做**子串猜测** ——
// profileForRunOptions 拿它 lower + strings.Contains 一下 ("coder"/"review"/...),
// 猜出工具画像。于是 `subagent_type: "my-review-bot"` 与 `subagent_type: "reviewer"`
// 得到同一个画像, 而前者对应的那份 .md 声明**存在也读不到**。
//
// 缺口 B (P5 后半): 模型不知道自己有哪些 agent 可选。Agent 工具的 Description 是
// 写死的一段英文, 里面没有清单 —— 模型只能凭"猜一个像样的名字"来填 subagent_type。
//
// 两个缺口必须一起补, **只补一个不算补**:
//   - 只补查表: 模型依然猜不出该填什么名字, 于是绝大多数派生走的还是猜的那条路;
//   - 只补清单: 模型知道了名字, 而填进去之后没有任何东西按它查表 —— 清单变成
//     一串"说了但不生效"的承诺, 比没有更坏 (模型会以为自己派了个 reviewer)。
//
// 本文件负责**清单与查表本身**; 查表的**调用点**在 pkg/feishu/session.go 的
// runNestedAgent (那里才拿得到 RunOptions 与 ConstraintSet)。两边合起来才算 P5 落地,
// 判断标准是 pkg/feishu/p5_agent_lookup_test.go —— 它直接驱动**真实的派生入口**,
// 在资产声明与运行时约束之间做对账, 而不是另写一份平行逻辑自己测自己。
//
// 那个文件里的每条接线都做过变异反证 (把接线删掉, 测试必须变红)。这不是形式:
// 第一版把资产档位同时经 WithToolProfile 与 fallback 闭包两条路送达, 删掉任意一条
// 测试都**依然全绿** —— 一份可以整条删掉的接线与没接上完全等价, 而它看起来是接好的。
// 同一条声明只能有一条送达路径。
//
// ---------------------------------------------------------------------------
// 名字匹配一律 exact, 这里没有一个 lenient 分支
// ---------------------------------------------------------------------------
//
// 查表 miss 时调用方回退到既有的角色名子串猜测 (那是 P3 明列的待退役项), 并**留痕**。
// 关键是: 那个回退住在调用点, 不在本文件 —— 本文件提供一个 exact 的答案或 nil。
// 让本文件"猜一下"会让 `reviewer2` 静默拿到 `reviewer` 的提示词与工具档位,
// 而那是一条没人能察觉的权限替换。
//
// ---------------------------------------------------------------------------
// 为什么清单是有界的
// ---------------------------------------------------------------------------
//
// 清单会进**每一个** Agent 工具的 schema 描述里, 也就是每一次请求的系统提示词。
// 93 个角色 + 用户自定义资产全列上去, 是给每个请求白加几千 token。所以:
//   - 只列 active 的 agent 资产 (shadow/archived 不进 —— 否则影子资产会被模型直接调用,
//     那等于绕过了所有灰度机制);
//   - 条数有上限 (agentManifestMaxItems), 超出部分如实截断并写明还有几条 ——
//     静默截断会让"模型不知道有它"与"它不存在"变得一样。
package agent

import (
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/asset"
)

// agentManifestMaxItems 清单最多列几条。超出部分会被如实标注, 不静默丢弃。
//
// 这个预算比 asset.Listing 的通用上限小得多, 因为它的去向特殊: 清单进的是
// **Agent 工具的 Description**, 也就是每一次请求的工具 schema —— 不是某一类角色
// 才看得到的系统提示段落。每条都是一个 agent 名 + 一句描述, 24 条已经接近千 token。
const agentManifestMaxItems = 24

var (
	agentCatalogMu sync.RWMutex
	agentCatalog   *asset.Set
)

// SetAgentCatalog 装入进程级的 agent 资产索引 (启动期调用一次)。
//
// 传 nil 表示"没有资产体系", 此时查表一律 miss、清单为空 —— 行为与改造前逐字节
// 一致。这一点是硬要求: 六种资产在绝大部分部署上都还是空的, 空目录不该改变任何
// 既有执行行为。
func SetAgentCatalog(s *asset.Set) {
	agentCatalogMu.Lock()
	agentCatalog = s
	agentCatalogMu.Unlock()
}

// AgentCatalog 取当前索引 (可能为 nil)。
func AgentCatalog() *asset.Set {
	agentCatalogMu.RLock()
	defer agentCatalogMu.RUnlock()
	return agentCatalog
}

// LookupAgentAsset 按名字查一份 agent 资产 (没有资产体系 / 没这个名字 → nil)。
//
// 名字匹配**不折叠大小写也不做前缀**: `subagent_type` 是一个标识符而不是一句自然
// 语言。模糊匹配会让 `reviewer2` 意外命中 `reviewer`, 而那是一条静默的权限/提示词
// 替换 —— 模型以为派了 A, 实际跑的是 B。
func LookupAgentAsset(name string) *asset.Asset {
	s := AgentCatalog()
	if s == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	return s.Get(asset.KindAgent, name)
}

// AgentManifest 返回喂给模型的 agent 清单 (Markdown 片段), 空清单返回空串。
//
// 空串而不是"（无）"这类占位: 调用方 (AgentTool.Description) 会用 `if m != ""`
// 拼接, 一个恒非空的占位串会让空资产体系也往每次请求里塞一段无意义文本。
//
// 渲染本身走 asset.Listing —— 清单长什么样只有一处定义。这里只负责**这一类的**
// 两件策略: 加一句说明这些名字该填到哪 (subagent_type), 以及自己的条数预算。
func AgentManifest() string {
	s := AgentCatalog()
	if s == nil {
		return ""
	}
	body := s.Listing(asset.KindAgent, asset.KindTag(asset.KindAgent), agentManifestMaxItems)
	if body == "" {
		return ""
	}
	// 说明句放在清单**前面**: 模型是顺序读的, 读完一串名字才知道该填哪儿,
	// 它得回头重新理解这一段的用途。
	var b strings.Builder
	b.WriteString("\n\nAvailable agent types (pass one as `subagent_type`):\n")
	b.WriteString(body)
	return b.String()
}
