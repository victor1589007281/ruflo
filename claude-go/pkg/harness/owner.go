// owner.go — owner（租户/用户）作用域在 harness 层的错误语义（R-H6）。
//
// # 载体不在这里
//
// NormalizeOwner / WithOwner / OwnerFromContext / HasOwner / OwnerScopedKey 的唯一
// 实现落在 `pkg/types/owner.go`，本文件只做**同包内别名**转发。为什么必须下沉：
//
//	harness → engine → tool → types
//
// engine 与 tool 都要读 owner（ToolContext 要把它交给工具、engine 要把它贴到
// trace），而它们**不能** import harness。若在 harness 里定义 ctx key、再由 engine
// 自己定义一份，就会有两个互不可见的 key —— 写的一方与读的一方各认一个，owner
// 永远是空，而代码看起来完全正确。这类缺陷不报错、不 panic，只能靠"只此一份实现"
// 从结构上避免。
//
// # 这里只留 harness 专有的东西
//
// CheckSameOwner 与 ErrOwnerMismatch 保留在 harness：它们要返回 *harness.Error
// （带 ErrorKind），而 types 包不认识 ErrorKind。把类型判断留在契约层、把载体
// 放在底层，是这两件事唯一合理的分工。
package harness

import (
	"context"
	"errors"
	"fmt"

	"github.com/anthropic/claude-go/pkg/types"
)

// DefaultOwnerID 是单用户/未指定身份时的 owner（见 types.DefaultOwnerID）。
const DefaultOwnerID = types.DefaultOwnerID

// NormalizeOwner 把空/纯空白 owner 归一为 DefaultOwnerID，其余原样返回。
//
// 实现见 types.NormalizeOwner —— 转发而不是复制，保证全仓只有一套归一规则。
func NormalizeOwner(s string) string { return types.NormalizeOwner(s) }

// WithOwner 把 owner 归一化后写入 ctx（nil ctx 返回 nil）。
func WithOwner(ctx context.Context, owner string) context.Context {
	return types.WithOwner(ctx, owner)
}

// OwnerFromContext 读取 ctx 中的 owner；缺失时返回 DefaultOwnerID。
func OwnerFromContext(ctx context.Context) string { return types.OwnerFromContext(ctx) }

// HasOwner 报告 ctx 里是否**显式**写过 owner。
//
// 用途见 types.HasOwner：鉴权边界要能分出"单用户模式"与"忘了带身份"。
func HasOwner(ctx context.Context) bool { return types.HasOwner(ctx) }

// OwnerScopedKey 生成 owner 命名空间下的 key：`<len>:<owner>/<key>`。
func OwnerScopedKey(owner, key string) string { return types.OwnerScopedKey(owner, key) }

// ErrOwnerMismatch 是 owner 越界的哨兵错误，可用 errors.Is 判定。
//
// 除了"给程序分支用"之外，它还是审计面：宿主日志里出现它就意味着有一条跨 owner
// 的访问尝试（不论最终是否被拦），这类事件必须可检索。
var ErrOwnerMismatch = errors.New("harness: owner mismatch")

// CheckSameOwner 校验 ctx 中的 owner 与目标资源 owner 一致。
//
// 比较前两边都归一化：空 ctx-owner 是 "local"，显式 "local" 也是 "local"，
// 二者相等；空目标 owner 同样按 "local" 处理 —— 于是"单用户旧数据（owner 为空）"
// 与"单用户新调用（owner 缺省）"可以互访，而任何真实的多租户串号都会被抓到。
//
// 返回 *Error 且 Kind=ErrKindSafety，原始错误是 ErrOwnerMismatch，因此
// errors.Is(err, ErrOwnerMismatch) 与类型断言 *harness.Error 都成立。选 safety
// 而不是 internal：这是被拒绝的越权访问，不是程序缺陷，上层不应重试。
func CheckSameOwner(ctx context.Context, owner string) error {
	ctxOwner := OwnerFromContext(ctx)
	target := NormalizeOwner(owner)
	if ctxOwner == target {
		return nil
	}
	return NewError(ErrKindSafety, "", fmt.Errorf("%w: ctx=%q target=%q", ErrOwnerMismatch, ctxOwner, target))
}
