// Package harness 是 claude-go 面向**业务宿主**的稳定契约层。
//
// 背景（Moqi design/07 R-H 系列）：engine 本身是合格的 run loop，但直接暴露给业务层
// 有三个不可接受的形状 —— `Host.Run` 只返回 string、`SessionStoreInterface.Append*`
// 没有 error 返回值、hook 失败会中断整轮。业务层因此无法渲染引用/usage、无法知道
// 落盘失败、无法让"可选增强失败应降级"成立。
//
// 本包**只定义契约与通用载体**，不含领域语义：
//   - 不定义"事实/事件/因果/情感/关系"（那是业务层的 schema）；
//   - 不实现"该不该提起"的记忆使用决策（那是业务层的策略）；
//   - 不 import 任何业务仓库。
//
// 装配方向是 Moqi → pkg/harness → pkg/engine。engine 可重构，本包语义化版本
// （见 VERSION），字段只增不删。
package harness

import (
	"context"
	"encoding/json"
	"time"

	"github.com/anthropic/claude-go/pkg/types"
)

// VERSION 是契约层的语义化版本。
//
// 只增不删：删字段 = 破坏已发布的下游；废弃字段先标 Deprecated，过一个版本再删。
// 新增能力默认**关闭**，关闭时旧路径请求字节必须不变（R-H16 验收第 2 条）。
const VERSION = "1.1.0"

// ============================================================================
// 一次 turn 的输入
// ============================================================================

// TurnRequest 是一次 turn 的完整输入。业务层发起对话时填这个结构，不再拼 engine.Config。
type TurnRequest struct {
	SessionID string `json:"sessionId"`
	// TurnID 是**幂等键**。同一 TurnID 重放两次只产生一条用户消息与一条助手消息
	// （R-H4 验收第 3 条）。业务层必须为每个逻辑 turn 生成一次并对重试复用同一个值
	// —— 用随机新 ID 重试就等于放弃幂等，sink 侧无法分辨"重试"与"又说了一遍"。
	TurnID string `json:"turnId"`
	// OwnerID 是租户/用户身份。空串等价于本地默认 owner（保持单用户旧行为，
	// 见 R-H6），而不是"无主"——"无主"会让跨 owner 隔离断言变成恒真。
	OwnerID   string `json:"ownerId,omitempty"`
	EpisodeID string `json:"episodeId,omitempty"`
	// Role 是本轮的角色（见 RoleDialogue 等）。空 = RoleDialogue。
	//
	// 它只影响模型路由与成本归集：抽取/反思/分类可以走便宜模型，对话走强模型。
	// 路由结果**必须**可观测 —— 见 TurnResult.Model。
	Role string `json:"role,omitempty"`
	// Input 是本轮用户输入的内容块。多模态附件走 Attachments 引用，不要在这里内联 base64。
	Input       []types.ContentBlock `json:"input,omitempty"`
	Attachments []AttachmentRef      `json:"attachments,omitempty"`
	Policy      TurnPolicy           `json:"policy,omitempty"`

	// Safety 由业务层在发起 turn 前给出（R-H3）。harness 只负责携带与执行拦截：
	// Decision="block" 时在**任何模型调用之前**失败；"degrade" 原样进 TurnResult
	// 与 trace，供业务层决定如何呈现。
	//
	// 做成输入字段而不是让 harness 自己判定：危机识别/合规策略属于业务层，
	// harness 不知道"什么算不安全"；但它必须能保证"业务说不能发就一个字节都
	// 不发给模型"，否则携带这个结构没有任何意义。
	Safety SafetyVerdict `json:"safety,omitempty"`

	// Speaker / Sensitivity 由业务层提供，原样写进 UserRecord/AssistantRecord
	// （R-H4 记录的 schema 字段）。空值表示沿用宿主的默认口径。
	Speaker     string `json:"speaker,omitempty"`
	Sensitivity string `json:"sensitivity,omitempty"`
}

// TurnPolicy 是单轮预算与工具策略。零值 = 全部取 engine 默认，与改造前一致。
type TurnPolicy struct {
	// ToolProfile 是工具档位（如 "companion"：只允许记忆检索/文件读取/记忆更正，
	// 禁止 shell/代码执行/任意写文件）。
	ToolProfile string `json:"toolProfile,omitempty"`
	// AllowedTools 三态，与 engine.Config.AllowedTools 语义一致且**不得**宽松化：
	//   nil    = 不受白名单限制（仅受 DisabledTools 约束）；
	//   非 nil = 仅白名单内工具可见可执行，**空集 = 全部拒绝**（fail-closed）。
	AllowedTools *[]string `json:"allowedTools,omitempty"`
	MaxTurns     int       `json:"maxTurns,omitempty"`
	MaxTokens    int       `json:"maxTokens,omitempty"`
	// MaxRunTokens 按 tokens 计的止损线（输入+输出+缓存读写）。
	MaxRunTokens int `json:"maxRunTokens,omitempty"`
	// MaxWall 是墙钟上限（0 = 不设限）。engine 原有预算只有 turns 与 tokens，
	// 这两者都管不住"一轮里一个工具卡十分钟"。
	MaxWall time.Duration `json:"maxWall,omitempty"`
	// MaxToolCalls 是工具调用次数上限（0 = 不设限）。
	MaxToolCalls int `json:"maxToolCalls,omitempty"`
	// MaxCost 是按金额计的上限（0 = 不设限）。单位由业务层与 ModelRoles 的价目表约定。
	MaxCost float64 `json:"maxCost,omitempty"`
	// ModelRoles 按角色覆盖模型：dialogue / extract / reflect / classify。
	// 未列出的角色走 engine 默认。**不允许静默换模型** —— 路由结果必须进 trace。
	//
	// 模型别名本身携带 provider（如 "opencode:deepseek-v4.1-flash"），所以
	// "角色→提供商"通过别名解析完成；harness 不再单独存 provider 字段，避免
	// 出现"模型说 A、provider 说 B"的两份事实。
	ModelRoles map[string]string `json:"modelRoles,omitempty"`
	// RoleLimits 是按角色的单轮上限（R-H17）。与 MaxTokens/MaxCost 同时存在时
	// 取更严格的那个；未列出的角色只用轮级上限。
	RoleLimits map[string]RoleLimit `json:"roleLimits,omitempty"`
}

// RoleLimit 是单个角色的单轮上限。零值字段表示该维度不额外限制。
type RoleLimit struct {
	MaxTokens int     `json:"maxTokens,omitempty"`
	MaxCost   float64 `json:"maxCost,omitempty"`
}

// 模型角色常量。业务层用这些键填 TurnPolicy.ModelRoles。
const (
	RoleDialogue = "dialogue"
	RoleExtract  = "extract"
	RoleReflect  = "reflect"
	RoleClassify = "classify"
)

// ============================================================================
// 一次 turn 的输出
// ============================================================================

// TurnResult 是结构化 turn 结果，取代 `Host.Run` 的裸 string（R-H3）。
//
// 业务层应当**不需要解析 prose** 就能渲染引用、工具调用、usage、终止原因与降级标记。
type TurnResult struct {
	TurnID    string `json:"turnId"`
	SessionID string `json:"sessionId"`
	OwnerID   string `json:"ownerId,omitempty"`
	EpisodeID string `json:"episodeId,omitempty"`

	// Text 是最终助手文本（Blocks 里 text 块的拼接），保留给旧消费方。
	Text string `json:"text"`
	// Blocks 是完整内容块。**未知类型的块必须原样保留**（含未知 metadata），
	// harness 不认识不等于可以丢弃 —— 丢掉之后往返一次就变成数据丢失。
	Blocks []types.ContentBlock `json:"blocks,omitempty"`

	Citations []Citation       `json:"citations,omitempty"`
	ToolCalls []ToolCallRecord `json:"toolCalls,omitempty"`

	// Model 是本轮**实际使用**的模型（R-H17）。空串表示沿用 engine 默认、未做覆盖。
	// 路由必须可观测：不允许静默换模型。
	Model string `json:"model,omitempty"`

	Usage types.Usage `json:"usage"`
	// UsageKnown 区分"消耗为 0"与"没采到"。上游失败调用不带 usage 是正常的，
	// 把它当 0 会让失败的 run 显得没花钱；恒 0 与未采集长得一样是本仓踩过的坑。
	UsageKnown bool `json:"usageKnown"`
	// Cost 是本轮成本（R-H17）。CostKnown=false 时 Cost 无意义。
	//
	// 与 UsageKnown 分开：业务层可能没装配价目表（CostKnown=false），也可能
	// 装了但上游没回 usage（两者都 false）。混成一个布尔就分不出是哪一种。
	Cost      float64 `json:"cost,omitempty"`
	CostKnown bool    `json:"costKnown"`

	Terminal types.Terminal `json:"terminal"`
	Safety   SafetyVerdict  `json:"safety"`

	TraceID string `json:"traceId,omitempty"`
	// Persisted=false 表示助手消息**没有**落到宿主存储；已经流出的文本仍然有效，
	// 业务层可据此重试持久化。
	Persisted bool `json:"persisted"`
	// Degraded 列出本轮降级项（如 "context_degraded"、"hook_timeout:memory_inject"）。
	// 空切片与 nil 都表示无降级。
	Degraded []string `json:"degraded,omitempty"`

	// Partial 表示 Text/Blocks 是"被中断前已经收到的部分内容"（R-H10）。
	// 取消、预算耗尽、流中断时为 true；正常完成为 false。
	//
	// 仅凭 Terminal.Reason 也能推断，但业务层渲染时最常问的是"这段文字能不能
	// 直接当完整回复用"，给一个显式布尔位比让它逐个 reason 判更不容易漏。
	Partial bool `json:"partial,omitempty"`
}

// Citation 是一条可渲染的引用。ID 由业务层生成并在检索 trace 中可反查。
type Citation struct {
	ID      string  `json:"id"`
	Source  string  `json:"source,omitempty"`
	Title   string  `json:"title,omitempty"`
	URI     string  `json:"uri,omitempty"`
	Snippet string  `json:"snippet,omitempty"`
	Score   float64 `json:"score,omitempty"`
}

// ToolCallRecord 是一次工具调用的结构化记录。
type ToolCallRecord struct {
	ToolUseID string          `json:"toolUseId,omitempty"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input,omitempty"`
	// InputHash 是参数的稳定哈希（幂等判定与缓存键都用它，不用原始字节）。
	InputHash string `json:"inputHash,omitempty"`
	IsError   bool   `json:"isError,omitempty"`
	// DurationMS 是本次调用耗时。
	DurationMS int64 `json:"durationMs,omitempty"`
	// ResultPreview 是结果的短预览（前 500 字节）。完整结果在 transcript 的工具
	// 消息里，这里只是让业务层不必再翻历史就能渲染"做了什么、返回了什么"。
	ResultPreview string `json:"resultPreview,omitempty"`
	// ResultBytes 是**完整结果**的字节数（不是预览长度）。业务层据此判断
	// "证据有多大"以及预览是否被截断，而不必把全文塞进 trace。
	ResultBytes int `json:"resultBytes,omitempty"`
	// ResultTruncated 表示完整结果超过了 ResultPreview 的 500 字节预览窗口。
	// 注意它**不**表示引擎侧做过裁剪 —— 引擎的裁剪只反映在 ResultBytes 上,
	// 两者是不同的口径, 不能互相推断。
	ResultTruncated bool `json:"resultTruncated,omitempty"`
	// Trust 是结果的信任标签 (H-F2)。"untrusted" = 内容来自外部网页/搜索,
	// 只能当数据不能当指令; 空 = 未标记 (本地工具)。
	Trust string `json:"trust,omitempty"`
	// SourceURI 是结果来源 URI (抓取 URL / search:<query>)。空 = 未标记。
	SourceURI string `json:"sourceUri,omitempty"`
	// Provenance 记录来源，便于审计"这个结论是哪次读取得来的"。
	Provenance Provenance `json:"provenance,omitempty"`
}

// ToolResultRecord 是一次工具调用的**完整结果**记录 (H-F1 证据闭环)。
//
// # 为什么单独成一条记录, 而不是塞进 ToolCallRecord/TurnResult
//
// TurnResult 会进 trace、会被序列化、可能被整体缓存/上报。把整页网页塞进去
// 会让 trace 从 KB 级膨胀到 MB 级, 且正文里的私密数据会跟着观测面走。所以
// 完整结果只经由宿主实现的 ToolResultSink 落**业务可控**的存储, trace 里
// 只保留 ResultBytes/ResultPreview 这样的摘要。
//
// ToolCallRecord 也刻意**不**含全集: 它进 TurnResult, 同样的膨胀问题。
type ToolResultRecord struct {
	// OwnerID/SessionID/TurnID 是归属键。与 ToolSink 的其它记录同源, 用于
	// 把证据挂回具体的 turn (证据可追溯、幻觉引用防线)。
	OwnerID   string `json:"ownerId,omitempty"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`

	ToolUseID string          `json:"toolUseId,omitempty"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input,omitempty"`
	InputHash string          `json:"inputHash,omitempty"`
	// FullResult 是工具结果的原文。可能很大; 由宿主决定如何存储。
	FullResult string `json:"fullResult"`
	IsError    bool   `json:"isError,omitempty"`
	DurationMS int64  `json:"durationMs,omitempty"`
	// Trust/SourceURI 是 H-F2 的不可信标记与来源, 原样来自引擎事件。
	Trust     string `json:"trust,omitempty"`
	SourceURI string `json:"sourceUri,omitempty"`
	// ExecutedAt 是工具开始执行的时刻。
	ExecutedAt time.Time `json:"executedAt"`
}

// ToolResultSink 是完整工具结果的可选落库契约 (H-F1)。
//
// 与 TurnSink 分开: 不是每个宿主都需要证据账本, 而一旦需要, 写入失败也**不能**
// 让 turn 失败 (证据是增强能力, 不是回复的硬依赖)。Runner 在助手消息持久化
// **之前**调用它; 失败只记一条 Degraded ("tool_result_sink_failed:<tool>",
// 每个工具最多一次), 本轮照常完成。
type ToolResultSink interface {
	AppendToolResult(ctx context.Context, rec ToolResultRecord) error
}

// Provenance 是工具结果的来源信息。
type Provenance struct {
	Tool       string    `json:"tool,omitempty"`
	SourceID   string    `json:"sourceId,omitempty"`
	ExecutedAt time.Time `json:"executedAt,omitempty"`
}

// SafetyVerdict 是业务层给出的安全判定，harness 只负责携带与执行拦截。
type SafetyVerdict struct {
	// Decision: allow | block | degrade。
	Decision   string   `json:"decision,omitempty"`
	Reasons    []string `json:"reasons,omitempty"`
	Categories []string `json:"categories,omitempty"`
}

// ============================================================================
// 流式事件（R-H12）
// ============================================================================

// EventKind 是 TurnEvent 的类型。
type EventKind string

const (
	EventDelta       EventKind = "delta"
	EventBlockDone   EventKind = "block_done"
	EventMessageDone EventKind = "message_done"
	EventToolStart   EventKind = "tool_start"
	EventToolDone    EventKind = "tool_done"
	EventCitation    EventKind = "citation"
	EventUsage       EventKind = "usage"
	EventTerminal    EventKind = "terminal"
	EventError       EventKind = "error"
	EventDegraded    EventKind = "degraded"
)

// TurnEvent 是流式事件。Seq 在**同一 turn 内**单调递增且唯一，支持 after=Seq 续放。
//
// 引用事件与文本增量分开（EventCitation 带 Citation），前端不必解析 prose 才能
// 画出引用角标。
type TurnEvent struct {
	Seq        int64           `json:"seq"`
	TurnID     string          `json:"turnId"`
	Kind       EventKind       `json:"kind"`
	Delta      string          `json:"delta,omitempty"`
	BlockIndex int             `json:"blockIndex,omitempty"`
	Citation   *Citation       `json:"citation,omitempty"`
	Tool       *ToolEvent      `json:"tool,omitempty"`
	Usage      *types.Usage    `json:"usage,omitempty"`
	Terminal   *types.Terminal `json:"terminal,omitempty"`
	// Err 是 EventError 的可读信息。
	Err string `json:"err,omitempty"`
	// Note 是 EventDegraded 的降级原因。
	Note string `json:"note,omitempty"`
}

// ToolEvent 是工具开始/结束事件。
type ToolEvent struct {
	ToolUseID string `json:"toolUseId,omitempty"`
	Name      string `json:"name"`
	IsError   bool   `json:"isError,omitempty"`
	// InputPreview / ResultPreview 各是前 500 字节 (H-F5 输入可见性)。
	//
	// 为什么预览而不是全文: 事件会进 SSE/trace, 整页网页或大文件输入会让
	// 每个事件都变成 MB 级。需要全文的消费方走 ToolResultSink (H-F1)。
	// 空串 = 该阶段没有内容 (如工具开始事件没有结果)。
	InputPreview  string `json:"inputPreview,omitempty"`
	ResultPreview string `json:"resultPreview,omitempty"`
	// Trust/SourceURI 是 H-F2 的不可信标记与来源。空 = 未标记。
	Trust     string `json:"trust,omitempty"`
	SourceURI string `json:"sourceUri,omitempty"`
}

// ============================================================================
// 宿主接管的持久化（R-H4）
// ============================================================================

// UserRecord 是待持久化的用户消息。
type UserRecord struct {
	OwnerID   string `json:"ownerId,omitempty"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	EpisodeID string `json:"episodeId,omitempty"`
	MessageID string `json:"messageId,omitempty"`

	Blocks      []types.ContentBlock `json:"blocks,omitempty"`
	Attachments []AttachmentRef      `json:"attachments,omitempty"`
	// Speaker 是说话人标识（陪伴场景下区分用户与角色）。
	Speaker string `json:"speaker,omitempty"`
	// Sensitivity 是敏感度分级，由业务层决定；harness 只透传。
	Sensitivity string    `json:"sensitivity,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// AssistantRecord 是待持久化的助手消息。
type AssistantRecord struct {
	OwnerID   string `json:"ownerId,omitempty"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	EpisodeID string `json:"episodeId,omitempty"`
	MessageID string `json:"messageId,omitempty"`

	Blocks      []types.ContentBlock `json:"blocks,omitempty"`
	Attachments []AttachmentRef      `json:"attachments,omitempty"`
	Usage       types.Usage          `json:"usage"`
	UsageKnown  bool                 `json:"usageKnown"`
	Terminal    types.Terminal       `json:"terminal"`
	Citations   []Citation           `json:"citations,omitempty"`
	// Safety 是业务给本轮的安全判定。重放轮必须还原同一份判定，否则同一
	// TurnID 两次请求会看到两个不同的 safety 结果。
	Safety SafetyVerdict `json:"safety,omitempty"`
	// Speaker 是说话人标识（陪伴场景下可能是角色而非用户）。
	Speaker string `json:"speaker,omitempty"`
	// Sensitivity 是敏感度分级，由业务层决定；harness 只透传。
	Sensitivity string    `json:"sensitivity,omitempty"`
	Model       string    `json:"model,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// TurnState 是 turn 的状态标记（用于崩溃恢复判定 R-H11）。
type TurnState struct {
	OwnerID   string `json:"ownerId,omitempty"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	// State: pending | running | completed | failed | interrupted。
	// "interrupted" 专门表达"用户消息已提交、助手消息未提交"的在途 turn：
	// 重启后不能假装它完成了，也不能自动重放模型调用（会产生重复或不同的回复）。
	State     string    `json:"state"`
	Persisted bool      `json:"persisted"`
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TurnSink 是宿主接管的持久化契约。
//
// 三个方法**全部返回 error**，这是与旧 SessionStoreInterface 的关键差别：旧接口
// `AppendUserMessage(msg, cwd, model)` 没有返回值，落盘失败无法上报。
type TurnSink interface {
	// EnsureUser 幂等确认用户消息已持久化。
	//
	// 业务 API 可能在 RunTurn 之前就写过这条消息；此时返回 already=true 且不重复写。
	// harness 在调用模型**之前**调它 —— 返回 error 则不进入模型调用（验收第 1 条）。
	EnsureUser(ctx context.Context, rec UserRecord) (already bool, err error)

	// AppendAssistant 在发出最终完成事件**之前**写助手消息。
	//
	// 返回 error 时：已经流出的文本不丢、TurnResult.Persisted=false、业务可重试。
	AppendAssistant(ctx context.Context, rec AssistantRecord) error

	// MarkTurn 记录 turn 状态。同 (owner, session, turnID) 重复调用是覆盖，不是追加。
	MarkTurn(ctx context.Context, state TurnState) error
}

// ============================================================================
// 会话 / Episode / 附件（R-H5）
// ============================================================================

// SessionSpec 是会话规格。PersonaVersion / ProfileVersion 参与稳定前缀 hash，
// 它们的变更**是**一次合法的缓存失效（R-H2）。
type SessionSpec struct {
	ID             string    `json:"id"`
	OwnerID        string    `json:"ownerId,omitempty"`
	PersonaVersion string    `json:"personaVersion,omitempty"`
	ProfileVersion string    `json:"profileVersion,omitempty"`
	ModelPolicy    string    `json:"modelPolicy,omitempty"`
	ToolProfile    string    `json:"toolProfile,omitempty"`
	AllowedTools   *[]string `json:"allowedTools,omitempty"`
}

// AttachmentRef 是附件的引用（不是内容）。
//
// 同一文件两次上传必须产生**相同** BlobHash 且只存一份 blob；转录里不得内联完整
// base64（R-H5 验收）。Kind 取值 image | pdf | audio | text。
type AttachmentRef struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	MIME     string   `json:"mime,omitempty"`
	BlobHash string   `json:"blobHash"`
	Name     string   `json:"name,omitempty"`
	Anchors  []Anchor `json:"anchors,omitempty"`
}

// 附件种类常量。
const (
	AttachmentImage = "image"
	AttachmentPDF   = "pdf"
	AttachmentAudio = "audio"
	AttachmentText  = "text"
)

// Anchor 是附件内的定位（如 PDF 页码、图片区域、音频时间戳）。
type Anchor struct {
	Kind  string  `json:"kind"`
	Start int     `json:"start,omitempty"`
	End   int     `json:"end,omitempty"`
	Ref   string  `json:"ref,omitempty"`
	Score float64 `json:"score,omitempty"`
}

// ============================================================================
// 逐轮上下文提供器（R-H1）
// ============================================================================

// TurnSummary 是历史 turn 的摘要视图，供 provider 判断"这一轮该检索什么"。
type TurnSummary struct {
	TurnID string `json:"turnId"`
	Text   string `json:"text"`
}

// TurnContext 是交给 provider 的只读视图。
//
// History 是**只读**的：provider 不得修改它，也不得把自己产出的块塞回历史。
type TurnContext struct {
	OwnerID     string               `json:"ownerId,omitempty"`
	SessionID   string               `json:"sessionId"`
	TurnID      string               `json:"turnId"`
	EpisodeID   string               `json:"episodeId,omitempty"`
	Input       []types.ContentBlock `json:"input,omitempty"`
	Attachments []AttachmentRef      `json:"attachments,omitempty"`
	RecentTurns []TurnSummary        `json:"recentTurns,omitempty"`
	// SentUpTo 是 engine 已发出的消息条数水位线：[0, SentUpTo) 一个字节都不能改。
	SentUpTo int             `json:"sentUpTo"`
	History  []types.Message `json:"-"`
}

// ContextBlock 是一条请求级上下文块。
type ContextBlock struct {
	// ID 必须稳定且可去重：同一 ID 重复出现时只保留第一个（R-H1 顺序确定、按 ID 去重）。
	ID   string `json:"id"`
	Text string `json:"text"`
	// Priority 越大越优先保留（token 预算不足时从低到高裁剪）。
	Priority int `json:"priority,omitempty"`
	// Tokens 是该块的估算 token 数；0 表示由 harness 估算。
	Tokens int        `json:"tokens,omitempty"`
	Refs   []Citation `json:"refs,omitempty"`
}

// ContextPatch 是 provider 的输出。
type ContextPatch struct {
	Blocks []ContextBlock `json:"blocks,omitempty"`
	// Trace 是检索过程的可观测信息（业务层自定义形状，harness 只透传）。
	Trace string `json:"trace,omitempty"`
	// Degraded 非空表示本次降级（如超时），harness 记入 TurnResult.Degraded。
	Degraded string `json:"degraded,omitempty"`
	// Critical 表示这次失败是业务显式要求"失败即拒绝"的（R-H1）。
	//
	// 只有 provider **同时返回 error** 且 Critical=true 时本轮才失败；正常返回的
	// patch 即使标了 Critical 也没有意义 —— 不可用这条字段表达"这轮必须带证据"，
	// 那会把可选增强变成硬依赖。默认 false = 失败降级、本轮照常回复。
	Critical bool `json:"critical,omitempty"`
}

// TurnContextProvider 每个用户 turn 调用**恰好一次**，在第一次模型请求之前。
//
// 不是每个工具循环请求都调；产出是**临时块**：只进入本次模型请求，不写 transcript、
// 不进下一轮历史、不参与压缩，且放在缓存断点之后 —— 不得修改稳定 system 前缀。
//
// 返回 error 时默认降级（空 patch）而不是中断本轮；只有显式标记 critical 才失败。
type TurnContextProvider interface {
	ProvideTurnContext(ctx context.Context, tc TurnContext) (ContextPatch, error)
}

// ============================================================================
// Prompt 布局与缓存契约（R-H2）
// ============================================================================

// PromptSegment 是布局中的一段。
type PromptSegment struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	// Hash 是该段字节的稳定哈希。稳定段 hash 变化即 PrefixHash 变化。
	Hash  string `json:"hash"`
	Bytes int    `json:"bytes,omitempty"`
}

// 缓存失效原因。业务层据此判断"这次失效是我想要的（画像升版）还是 bug"。
const (
	InvalidationNone        = ""
	InvalidationPersona     = "persona_version"
	InvalidationProfile     = "profile_version"
	InvalidationSystem      = "system_prompt"
	InvalidationToolSet     = "tool_set"
	InvalidationCompression = "compression"
	InvalidationExplicit    = "explicit"
)

// PromptLayout 是 prompt 的稳定/动态分层与缓存断点描述。
type PromptLayout struct {
	// StableSegments 是**稳定前缀**：只在显式版本变更时改变。
	StableSegments []PromptSegment `json:"stableSegments,omitempty"`
	// DynamicSegments 是断点之后的动态内容（含本轮临时上下文块）。
	DynamicSegments []PromptSegment `json:"dynamicSegments,omitempty"`
	// Breakpoints 是 cache_control 断点所在的动态段下标/位置。
	// 总数不得超过 provider 上限（Anthropic 为 4）；超限必须在发送前失败。
	Breakpoints []int `json:"breakpoints,omitempty"`
	// PlannedSystemBreakpoints 是**实际计划发给上游**的 system 侧断点下标（R-H2）。
	//
	// 与 Breakpoints 分开是因为两者下标空间不同：Breakpoints 是布局内的动态段位置，
	// 而本字段直接对应 api.CachePlan.SystemIndexes —— 出站请求里 system 数组的下标。
	// 合成一个字段会让"布局说断了、请求其实没断"无法被发现。
	// 为空表示本轮没有干预缓存计划（由 api 层内置启发式决定）。
	PlannedSystemBreakpoints []int `json:"plannedSystemBreakpoints,omitempty"`
	// PrefixHash 是稳定前缀的聚合哈希。两轮之间画像不变时它必须**逐字节相同**。
	PrefixHash string `json:"prefixHash"`
	// InvalidationReason 是本轮 PrefixHash 变化的原因（见 Invalidation* 常量）。
	InvalidationReason string `json:"invalidationReason,omitempty"`
}

// PromptLayoutSink 接收布局观测。harness 同时上报**本地**布局 hash 与 provider
// 真实 cache read/write —— 只报本地 hash 命中是自欺，因为那只是本地记账。
type PromptLayoutSink interface {
	RecordPromptLayout(ctx context.Context, layout PromptLayout, usage types.Usage) error
}

// ============================================================================
// 任务队列（R-H13）
// ============================================================================

// Task 是一个持久后台任务。至少一次投递；幂等由 handler 的 IdempotencyKey 保证。
type Task struct {
	ID   string `json:"id,omitempty"`
	Kind string `json:"kind"`
	// IdempotencyKey 由 handler 定义：同一 key 重复执行必须产生同一结果。
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	// MaxAttempts 0 = 队列默认（3）。
	MaxAttempts int `json:"maxAttempts,omitempty"`
	// VisibleAt 是"什么时候可以被 claim"（延迟任务）；零值 = 立刻。
	VisibleAt time.Time `json:"visibleAt,omitempty"`
	// RequireCaps 是需要的能力标签，不具备全部标签的 worker 不得 claim。
	RequireCaps []string `json:"requireCaps,omitempty"`
}

// TaskID 是任务标识。
type TaskID string

// ClaimedTask 是 claim 成功后的任务视图（含租约）。
type ClaimedTask struct {
	Task
	// Attempts 是**已派发次数**（含本次）。必须在派发时自增，否则反复崩溃的
	// worker 会让它永远停在 0，重试上限形同虚设。
	Attempts   int       `json:"attempts"`
	LeaseUntil time.Time `json:"leaseUntil"`
	Worker     string    `json:"worker,omitempty"`
}

// TaskQueue 是持久任务队列。
//
// 持久化由 agentDB/StateStore 提供，运行时由 harness 提供，handler 由业务层提供。
// 语义是**至少一次**投递：业务 handler 必须容忍重复执行。
type TaskQueue interface {
	Enqueue(ctx context.Context, task Task) (TaskID, error)
	// Claim 取一个可执行任务；没有任务返回 (nil, nil) —— 空队列不是错误。
	Claim(ctx context.Context, worker string, kinds []string, lease time.Duration) (*ClaimedTask, error)
	Ack(ctx context.Context, id TaskID) error
	// Nack 把任务放回队列（可带退避）；超过 MaxAttempts 进死信。
	Nack(ctx context.Context, id TaskID, cause error) error
	// Renew 续租。续租必须能**失败**：租约已被抢走时要报错，否则两个 worker
	// 会同时认为自己持有任务。
	Renew(ctx context.Context, id TaskID, lease time.Duration) error
	// DeadLetters 返回进入死信的任务 ID（运维面）。
	DeadLetters(ctx context.Context) ([]TaskID, error)
}

// ============================================================================
// Harness 门面（R-H16）
// ============================================================================

// Harness 是业务层唯一入口。
type Harness interface {
	// RunTurn 跑一轮。emit 可为 nil（不需要流式）。返回的 TurnResult 恒非 nil，
	// 即使 error 非 nil —— 业务层需要 Terminal 与部分文本才能正确渲染失败。
	RunTurn(ctx context.Context, req TurnRequest, emit func(TurnEvent)) (*TurnResult, error)
	// Cancel 取消一个在途 turn。已产生的部分文本保留并标记 partial（见 Terminal）。
	Cancel(turnID string) error
}

// ============================================================================
// 错误分类（Q-H7）
// ============================================================================

// ErrorKind 是错误分类。
type ErrorKind string

const (
	ErrKindModel    ErrorKind = "model"
	ErrKindTool     ErrorKind = "tool"
	ErrKindBudget   ErrorKind = "budget"
	ErrKindSafety   ErrorKind = "safety"
	ErrKindSink     ErrorKind = "sink"
	ErrKindCanceled ErrorKind = "canceled"
	ErrKindInternal ErrorKind = "internal"
	// ErrKindModality 表示模型不支持输入里含有的模态（R-H11）。
	//
	// 单列一类而不是并进 model：业务层的处置完全不同 —— 模型错误该重试或换模型，
	// 模态不支持该降级（拿掉图片重问）或明确告知用户，重试同样请求一定还是错。
	ErrKindModality ErrorKind = "modality"
	// ErrKindContext 表示业务显式标为 critical 的逐轮上下文失败（R-H1）。
	// 与"降级"不同：降级不是错误，critical 失败必须让业务层知道这一轮没有证据。
	ErrKindContext ErrorKind = "context"
	// ErrKindAttachment 表示附件引用解析失败（R-H5/R-H11）：blob 取不到、超预算、
	// 或种类无法表达为协议内容块。
	//
	// 与 modality 分开：modality 是"模型不支持这种输入"，换模型或去掉附件都能继续；
	// 附件解析失败是"这份输入本身拿不到/不合法"，换模型也没用，必须让用户重传或
	// 调整预算。混成一类会让业务层对两种相反的情形给出同一条用户提示。
	ErrKindAttachment ErrorKind = "attachment"
)

// Error 是带分类的 harness 错误。
type Error struct {
	Kind ErrorKind
	// TurnID 便于业务层把错误对回到某一轮。
	TurnID string
	Err    error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return string(e.Kind)
	}
	return string(e.Kind) + ": " + e.Err.Error()
}

// Unwrap 支持 errors.Is/As 穿透到原始错误。
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewError 构造分类错误。
func NewError(kind ErrorKind, turnID string, err error) *Error {
	return &Error{Kind: kind, TurnID: turnID, Err: err}
}
