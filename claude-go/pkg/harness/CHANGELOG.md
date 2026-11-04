# pkg/harness 契约版本

`VERSION` 是 `pkg/harness` 对外契约（`RunTurn` / `TurnSink` / `TurnEvent` /
`TurnResult` / `TurnPolicy` 等）的语义化版本。规则见 `types.go` 的注释：
**字段只增不删**；废弃字段先标 `Deprecated`，过一个版本再删；新增能力默认关闭，
关闭时旧路径的出站请求字节必须不变。

下游应以 `github.com/anthropic/claude-go/pkg/harness` 的 tag（`harness/vX.Y.Z`）
或 commit pin，而不是本地 `replace` 路径。

## 1.1.0 — 2026-11-04

向后兼容的新增。零值行为与 1.0.0 一致。

- `TurnResult.ToolCalls[]` 新增 `ResultBytes`、`ResultTruncated`、`Trust`、`SourceURI`。
  完整结果仍**不**进 `TurnResult`；这些字段只是摘要与元数据。
- 新增 `ToolResultSink` 与 `ToolResultRecord`：宿主可选择接收工具结果的完整内容。
  `RunnerOptions.ToolResults` 为 nil 时行为与 1.0.0 完全相同；sink 失败只追加
  `Degraded: tool_result_sink_failed:<tool>`，不失败 turn。
- `TurnEvent.Tool`（`ToolEvent`）新增 `InputPreview`、`ResultPreview`、`Trust`、
  `SourceURI`。预览上限 500 字节；空值表示该阶段没有内容。
- `pkg/types.ContentBlock` 新增仅进程内的 `Trust` / `SourceURI`（`json:"-"`）；
  `pkg/types.StreamEvent` 新增 `ToolResultFull` / `ToolTrust` / `ToolSource`，
  只供进程内消费，不进入 SSE/HTTP 出口。
- `pkg/tool.ToolResult` 新增 `Trust` / `SourceURI`；`ToolMetaInputConfirmation`
  提供入参级确认（`NeedsConfirmationFor`），未知域名可 fail closed。
- 新增 `builtin.DeepResearch` 工具，注册在 `research` 与 `analysis` 档位。
  evidence-first 输出；底层 `search.Manager` 未装配时明确返回 `unavailable`。

## 1.0.0 — 2026-10

首个进程内契约版本：`RunTurn`、`TurnSink`、`TurnContextProvider`、`TurnPolicy`、
事件流与 `Replay`、成本/预算账本、`CompleteJSON`、任务队列、附件与多模态、
prompt cache 布局。
