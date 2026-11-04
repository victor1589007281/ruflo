// owner.go — owner（租户/用户）身份的进程内载体（Moqi R-H6）。
//
// # 为什么载体放在 types 而不是 harness
//
// R-H6 要求 OwnerID 贯穿 Config、TurnContext、ToolContext、StateStore bucket、trace、
// metrics —— 而这些分属四个包：types / engine / tool / harness。依赖方向是
//
//	harness → engine → tool → types
//
// harness 不能被子系统 import（那是反向依赖），所以 ctx 载体必须落在**最底层**的
// types 包里，上层各自引用同一份。若在 harness 里定义、再由 engine 复制一份，就会
// 出现两个不同的 ctx key —— 写入方与读取方各认一个，owner 永远是空，而代码看起来
// 完全正确。这类"两套 key 互相看不见"的缺陷不会报错，只能靠"只此一份实现"避免。
//
// # 为什么空 owner 归一成常量而不是保留空串
//
// 契约层各处都有 OwnerID 字段，但"空 owner 算什么"此前没有定义。放任空串流动会带来
// 一类特别隐蔽的验收漏洞：凡是写成
//
//	if gotOwner != wantOwner { t.Fatal("跨 owner 泄漏") }
//
// 的隔离断言，在两个 owner 都是空串时恒为假（不报错）；也就是说**恰恰在没有身份
// 的调用路径上，隔离测试最绿**。归一成具体值让比较重新有内容，同时保留单用户旧
// 行为（本地进程里所有调用都归 "local"，互相之间仍然相等）。
//
// 归一化只解决"空"，不解决"编造"：调用方仍须把真实 owner 从鉴权结果带进 ctx，
// 不能用请求体里的自报字段 —— 自报 owner 等于让攻击者自己选租户。
package types

import (
	"context"
	"strconv"
	"strings"
)

// DefaultOwnerID 是单用户/未指定身份时的 owner。
//
// 选具体字符串而不是空串，理由见文件头：空串会让"隔离成立"的断言失去区分力。
const DefaultOwnerID = "local"

// NormalizeOwner 把空/纯空白 owner 归一为 DefaultOwnerID，其余原样返回。
//
// 只 strip 首尾空白、不改内部大小写或分隔符：owner 来自鉴权系统的稳定标识，
// 静默改写它会让同一个用户在不同入口拿到不同 owner（进而"数据不见了"）。
func NormalizeOwner(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultOwnerID
	}
	return s
}

// ownerCtxKey 是未导出的 context key 类型。
//
// 用自定义空结构体类型而不是字符串：字符串 key 可能被别的包用同一个字面量覆盖
// （context.WithValue 与 map 一样只看相等），一旦碰撞，别的包就能伪造 owner。
// 未导出类型保证只有本包能写入这个 key，读也只能经 OwnerFromContext。
type ownerCtxKey struct{}

// WithOwner 把 owner 归一化后写入 ctx（nil ctx 返回 nil，与 context 包约定一致）。
//
// 在写入点归一而不是只在读取点归一：ctx 可能在包间传递，任何一个只读 ctx 而不
// 调用 OwnerFromContext 的下游都不该看到空 owner。这样"ctx 里的 owner 永不为空"
// 成为一条全局不变量。
func WithOwner(ctx context.Context, owner string) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, ownerCtxKey{}, NormalizeOwner(owner))
}

// OwnerFromContext 读取 ctx 中的 owner；缺失（或值类型不对）时返回 DefaultOwnerID。
//
// 不返回 (string, bool)：调用方几乎总是需要一个可比较的 owner，逼它们写 `if !ok`
// 只会诱使省略检查、退化回空串。需要区分"确实没带身份"的地方应当看请求的鉴权
// 结果，而不是看这个函数。
func OwnerFromContext(ctx context.Context) string {
	if ctx == nil {
		return DefaultOwnerID
	}
	if v, ok := ctx.Value(ownerCtxKey{}).(string); ok {
		return NormalizeOwner(v)
	}
	return DefaultOwnerID
}

// HasOwner 报告 ctx 里是否**显式**写过 owner。
//
// 与 OwnerFromContext 分开的理由：后者把"没带"归成默认值，于是调用方分不出
// "这个请求是单用户模式"与"这个请求忘了带身份"。鉴权边界上必须能分开 —— 前者
// 可以按本地 owner 处理，后者应当拒绝。
func HasOwner(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(ownerCtxKey{}).(string)
	return ok
}

// OwnerScopedKey 生成 namespaced key：`<len>:<owner>/<key>`。
//
// 碰撞分析（owner 与 key 里都可能出现任意字符，含 ":" "/" "#"）：
//
//	朴素写法 owner + ":" + key 在多行/自造 owner 下会撞；
//	owner + "/" + key 同样撞；加分隔符却不转义都只是把碰撞藏起来。
//
// 长度前缀是可证明单射的：解析时先读到第一个 ':'（第一段必是纯十进制数字），
// 再精确取该长度的字节作为 owner，随后必须是一个 '/'，剩下全部是 key。任何一条
// 目标串只有唯一一种 (owner, key) 拆分，所以
// OwnerScopedKey("a","bc")="1:a/bc" 与 OwnerScopedKey("ab","c")="2:ab/c" 必然不等。
// 这也顺带保证不含分隔符歧义：分隔符出现在 owner/key 内部时由长度前缀兜住。
func OwnerScopedKey(owner, key string) string {
	o := NormalizeOwner(owner)
	var b strings.Builder
	b.Grow(len(o) + len(key) + 4)
	b.WriteString(strconv.Itoa(len(o)))
	b.WriteByte(':')
	b.WriteString(o)
	b.WriteByte('/')
	b.WriteString(key)
	return b.String()
}
