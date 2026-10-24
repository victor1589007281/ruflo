// filerewind.go 实现 Rewind —— 文件级检查点的回滚出口 (H3-1)。
//
// # 缺口在哪
//
// filehistory.go 把每一次 Write / StrReplace 的写前状态都留了版本, 装配也早就
// 接好了 (CLI 与飞书都在 SetFileHistory)。**唯独没有任何东西能把版本读回来**:
// Restore 写完之后零调用方, 只在它自己的单测里被调过。
//
// 这是本方案里"建了没通电"最纯粹的一个形态 —— 与 16.3 记的治理机制缺口同族。
// 检查点在每一轮写入里都真的在跑, 磁盘上真的躺着几十个版本, 而长程任务里
// "第 80 轮发现自己第 60 轮改错了"这件事发生时, 唯一的自救手段是模型凭记忆
// 把文件改回去 —— 那正是它刚刚证明自己做不好的事。
//
// # 为什么是工具而不是自动回滚
//
// 自动回滚需要一个"现在改坏了"的判据, 而那个判据不存在: 改动是否算坏, 只有在
// 后续编译/测试/用户反馈里才显形, 而且"坏"往往要跨好几个文件才好判断。能判断的
// 只有模型自己 —— 它是唯一知道"刚才是哪一步引入的"的角色。所以这里给的是
// **出口**而不是**策略**: 把 Restore 变成模型伸手可及的一个动作。
//
// # 为什么不记回退后的新版本
//
// Restore 之后**不**调 checkpointAfterWrite, 于是"最高版本 == 磁盘内容"这条
// 不变式在回退后被暂时打破。这是刻意的, 而且与 PreviousVersion 按磁盘内容
// (而非版本清单下标) 找锚点是**同一条设计**:
//
//	v1 v2 v3, 磁盘 = v3
//	  退一步 → PreviousVersion 认磁盘是 v3 ⇒ 给 v2, 恢复 v2
//	  再退一步 → 磁盘现在是 v2 ⇒ 给 v1, 恢复 v1   ← 继续往前
//
// 若在恢复后记一版 (v4 = v2 的内容), 第二次回退的锚点就成了 v4, PreviousVersion
// 会给出 v3 —— 连按两次等于在原地前后横跳, 而两次都报"成功"。这条被
// TestH31_ConsecutiveRewindKeepsGoingBack 钉住。
//
// # 失败必须报错
//
// 回退失败而谎报成功会直接损坏工作区 (模型以为文件已经回去了, 于是继续在错误
// 状态上往下做)。所以本工具在**任何**取不到版本、写不进去的路径上都返回
// IsError, 且区分三种取不到版本的情形 —— 它们的正确处置完全不同:
//
//	未启用检查点   → 别再试这个工具, 直接修文件
//	没有版本史     → 本会话没改过这个文件, 无可回退
//	磁盘内容不在史里 → 文件被本会话之外的东西改过, 回退会覆盖掉别人的改动
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// RewindToolName 工具名。
const RewindToolName = "Rewind"

// rewindMaxRefsInReply 结果里回显的版本引用个数上限 (最旧的在前面截掉)。
//
// 全量回显在一个长会话里可能有几十个引用, 每个 ~20 字符 —— 为了回答"还能退到哪"
// 而把这几十个塞进上下文, 是在用长程任务最稀缺的资源换一句几乎不会用到的话。
// 最近几个才是真要用的。
const rewindMaxRefsInReply = 5

type rewindInput struct {
	Path string `json:"path"`
	Ref  string `json:"ref,omitempty"`
}

// RewindTool 把文件回退到 file-history 记录过的某个版本。
type RewindTool struct{}

// NewRewindTool 构造 Rewind 工具。
func NewRewindTool() *RewindTool { return &RewindTool{} }

// Name 返回工具名 "Rewind"。
func (t *RewindTool) Name() string { return RewindToolName }

// Description 返回给模型看的工具说明: 省略 ref 即回退一步, 可连续回退。
func (t *RewindTool) Description() string {
	return `Rewind a file to an earlier version recorded by the file-history checkpoint.

A version is saved automatically before every Write / StrReplace, so any edit made
in this session can be undone — including one made many turns ago.

Omit "ref" to step back one version; call again to keep stepping further back.
Pass an explicit "ref" to jump straight to a specific version.

The file is restored byte-for-byte. If no version is available the call fails and
the file is left untouched.`
}

// InputSchema 返回入参 schema: path 必填, ref 可选。
func (t *RewindTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The file to rewind (absolute, or relative to the working directory)."
			},
			"ref": {
				"type": "string",
				"description": "Version reference to restore, e.g. \"21b2927e60d9342c@v2\". Omit to step back one version."
			}
		},
		"required": ["path"]
	}`)
}

// IsReadOnly 恒为 false —— 回退会写盘。
func (t *RewindTool) IsReadOnly(_ json.RawMessage) bool        { return false }
// IsConcurrencySafe 恒为 false —— 回退与其它写操作不可并发。
func (t *RewindTool) IsConcurrencySafe(_ json.RawMessage) bool { return false }

// CheckPermissions 在 Plan 模式下拒绝回退 (它是一次真实的文件写入)。
func (t *RewindTool) CheckPermissions(input json.RawMessage, tctx *tool.ToolContext) *types.PermissionResult {
	if tctx == nil || tctx.PermissionMode != types.PermissionModePlan {
		return nil
	}
	// 回退是一次真实的文件写入, 与 StrReplace 同性质 —— 规划期同样不可用。
	// 计划文件目录那条例外在这里**不适**用: 规划期唯一该被写的是计划本身,
	// 而回退会按版本覆盖任意文件, 包括计划目录里别的文件。
	return &types.PermissionResult{
		Behavior: types.PermissionDeny,
		Reason:   "Plan mode: 回退操作不可用 (需要写文件)",
	}
}

// Call 执行回退: 省略 ref 时取上一步版本并逐字节恢复; 三种取不到版本的情形分别报错。
//
// 算法解读：
//
//	原理：把"退回一步"变成模型伸手可及的一个动作。省略 ref 时按**磁盘当前内容**反查上一个
//	      版本, 而不是按版本清单下标。
//	关键不变量：回退后**不**记新版本 (不调 checkpointAfterWrite)。若记了, 第二次回退的锚点
//	      会变成刚写下的那一版, 连按两次等于在原地前后横跳, 且两次都报成功。因此"最高版本 ==
//	      磁盘内容"这条不变式在回退后被**刻意**暂时打破。
//	代价与边界：磁盘内容不在版本史里时拒绝回退 (说明文件被本会话之外的东西改过, 硬退会
//	      覆盖别人的改动); 三种取不到版本的情形分别报错, 因为它们的正确处置完全不同。
//	设计权衡：给的是**出口**而不是**策略** —— "现在改坏了"没有可自动判定的判据, 唯一知道
//	      是哪一步引入问题的角色是模型自己。
func (t *RewindTool) Call(_ context.Context, input json.RawMessage, tctx *tool.ToolContext) (*tool.ToolResult, error) {
	var in rewindInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tool.ToolResult{Content: fmt.Sprintf("输入解析错误: %v", err), IsError: true}, nil
	}
	if strings.TrimSpace(in.Path) == "" {
		return &tool.ToolResult{Content: "错误: path 不能为空", IsError: true}, nil
	}

	h := CurrentFileHistory()
	if h == nil {
		// 未启用 vs 无版本可退是**两件事**: 前者是这台机器上根本没记过版本, 后者是
		// 记过但退不动。都退不了, 但只有前者该被明确告知"别再用这个工具" ——
		// 混成一句"没有可回退的版本"会把模型引向反复重试一条永远不通的路。
		return &tool.ToolResult{
			Content: "错误: 本会话未启用文件检查点 (file-history), 没有任何版本可回退。" +
				"请直接编辑文件把它改回正确内容, 不要重试本工具。",
			IsError: true,
		}, nil
	}

	filePath := expandPath(in.Path, tctx.Cwd)
	sid := sessionIDOf(tctx)

	ref := strings.TrimSpace(in.Ref)
	steppedBack := ref == ""
	if steppedBack {
		ref = h.PreviousVersion(filePath, sid)
		// 省略 ref = 退一步: 锚点是**磁盘当前内容**而不是版本清单下标, 连按两次才会真正往前退。
		if ref == "" {
			return t.noTargetResult(h, filePath, sid), nil
		}
	}

	if err := h.Restore(filePath, ref, sid); err != nil {
		// 措辞明确带上"文件未改动": Restore 在写盘失败/版本缺失时都可能返回,
		// 而模型下一步的动作取决于"磁盘现在是什么状态", 不是错误原因。
		return &tool.ToolResult{
			Content: fmt.Sprintf("回退失败, 文件未改动: %v", err),
			IsError: true,
		}, nil
	}

	// 见文件头: 这里**不**调 checkpointAfterWrite。

	msg := fmt.Sprintf("已将 %s 回退到版本 %s", filePath, ref)
	if steppedBack {
		msg += " (上一步)"
	}
	msg += "。\n文件内容已逐字节恢复为该版本。"
	if left := h.Versions(filePath, sid); len(left) > 0 {
		msg += fmt.Sprintf("\n该文件可回退的版本 (%d 个): %s", len(left), tailRefs(left, rewindMaxRefsInReply))
	}
	return &tool.ToolResult{Content: msg}, nil
}

// noTargetResult 构造"退不了一步"时的失败结果, 并区分两种成因。
func (t *RewindTool) noTargetResult(h *FileHistory, filePath, sid string) *tool.ToolResult {
	vers := h.Versions(filePath, sid)
	if len(vers) == 0 {
		return &tool.ToolResult{
			Content: fmt.Sprintf("错误: %s 没有任何已记录的版本, 无可回退 —— "+
				"本会话还没有通过 Write / StrReplace 改过这个文件。", filePath),
			IsError: true,
		}
	}
	return &tool.ToolResult{
		Content: fmt.Sprintf("错误: %s 的当前磁盘内容不在版本史里 (被本会话之外的东西改过), "+
			"没有可安全回退的目标 —— 继续退会覆盖掉那次外部改动。现有版本: %s",
			filePath, tailRefs(vers, rewindMaxRefsInReply)),
		IsError: true,
	}
}

// tailRefs 取最近 n 个版本引用拼成一行 (最旧的在前面被截掉)。
func tailRefs(refs []string, n int) string {
	if len(refs) > n {
		refs = refs[len(refs)-n:]
	}
	return strings.Join(refs, ", ")
}
