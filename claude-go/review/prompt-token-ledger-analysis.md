# claude-go 提示词组件账本与 Token 优化复盘

更新日期: 2026-05-05

## 0. 后续优化执行记录

本节记录在上一轮 P0/P1/P2 建议之后继续落地的优化，以及同一组飞书场景的复测结果。

新增代码优化:

- Tool result artifact 化: 超长工具结果写入 `.claude-go/artifacts/tool-results/`，prompt 内只保留摘要和 `full_artifact` 引用。
- Prompt debug 保留/脱敏: 增加 `promptDebugMaxFiles`、`promptDebugMaxBytes`、`promptDebugSampleRate`、`promptDebugRedact`，默认脱敏 API key / bearer / sk-*。
- Orchestrator 防卡死: agent 调用增加 bounded timeout；watchdog 文案改为真实无进展时间。
- WBS 粒度预算: Planner 从“14-18 个任务、每个 8-15 分钟”改为“普通应用/CLI 4-8 个任务、禁止默认超过 10 个”。
- WBS 解析修复: task id 支持数字和字符串（如 `"4a"`），避免合法 JSON 因 id 类型失败而退回文本 fallback。
- DAG 隔离修复: Orchestrator 任务 subject 加 `[orch]` 前缀，避免把 research/design/plan 阶段误补为孤儿任务。
- 对抗轮次预算: simple=1 轮、medium=2 轮、complex=3 轮；未标注任务按 target files/title 推断。
- simple fast-pass: simple 小任务 scoped build 通过后跳过 reviewer/tester LLM，直接记录结构化评分。
- tester 本地化: “编译验证/测试”类任务直接跑本地 build/test，不再调用 tester LLM。
- E2E 本地门禁: 先扫 TODO/STUB，再跑 build/test；通过则跳过 E2E tester LLM，失败则快速失败并输出本地错误。
- ToolProfile 再收紧: chat/coding 默认不暴露 WebFetch/WebSearch；只有 research/web 意图才开放网页工具。
- 角色可观测性: `/role show` 和 CLI roles 输出实际注入 skills 与预计 chars。
- 运行目录能力: 增加 `/cwd` 查询和 `/cwd set <path>`，同步刷新 session role registry 与 team cwd。

最新验证:

- `go test ./pkg/api ./pkg/mcp ./pkg/feishu ./pkg/engine ./pkg/metrics ./pkg/agent ./pkg/skills ./pkg/tool ./pkg/tool/builtin ./cmd/claude-go` 通过。
- `go build -o /tmp/claude-go-feishu-optimized-final ./cmd/claude-go` 通过。
- opt4 复测确认 WBS 直接解析为 6 个任务，`restored=0`、`orphaned=0`，DAG 隔离修复生效。
- opt5 复测确认 simple fast-pass 生效: 首个任务从 opt4 的 5m38s 降到 44s；最终 tester 本地验证前仍被 LLM 任务拖住。
- opt6 复测确认 tester 本地化生效: “集成测试与验证”从 6 分钟 LLM timeout 降到 1 秒本地完成；6/6 Orchestrator 任务完成。
- opt6 仍在 E2E tester LLM 遇到 429/context deadline 后失败；随后已将 E2E 改为本地门禁并通过编译测试，完整长烟测未再重复跑一轮，避免继续消耗供应商限流窗口。

复测数据:

| run | prompt files | debug size | calls | total_tokens | input_tokens | cache_read | output_tokens | 结果 |
|---|---:|---:|---:|---:|---:|---:|---:|---|
| opt2 | 470 | 32MB | 470 | 6341298 | 1028509 | 5186378 | 126411 | 25m timeout，review/coder 多轮与任务粒度过细 |
| opt5 | 242 | 20MB | 242 | 3923727 | 480748 | 3333927 | 109052 | 任务执行提速，E2E LLM 429/context deadline |
| opt6 | 235 | 13MB | 235 | 2375511 | 360594 | 1938855 | 76062 | Orchestrator 6/6 完成，E2E LLM 429/context deadline |

关键组件变化（opt2 → opt6）:

| component | opt2 avg_chars | opt6 avg_chars | 变化 |
|---|---:|---:|---:|
| tools_schema_chars | 9171 | 8987 | -2.0% |
| mcp_tools_chars | 0 | 0 | 持平 |
| role_skills_chars | 2011 | 894 | -55.5% |
| messages_chars | 32456 | 22810 | -29.7% |
| system_chars | 4378 | 3564 | -18.6% |
| blackboard_chars | 30 | 333 | +1010% |

解释:

- MCP schema 继续保持 0，说明 MCP 懒加载代理稳定生效。
- role skills 已从 opt2 的平均 2.0K chars 降到 opt6 的 894 chars。
- messages avg 从 32.5K 降到 22.8K，但 opt6 的 E2E LLM 仍把 tester messages 拉到约 99.9K；E2E 本地门禁落地后应继续下降。
- blackboard avg 上升是因为团队阶段 now 更积极写入 summary/score/result，可接受；后续可给 blackboard prompt 引用加 top-k。
- 普通飞书问答 opt6 只有 11 次调用、约 46K total tokens，且未再触发 Chrome/browser fallback。

当前判断:

- 之前最大浪费项（MCP/schema 全量注入）已解决。
- 第二层浪费项（WBS 过细、5 轮对抗、tester/reviewer 默认 LLM）已明显收敛。
- 新主风险是“质量门禁与预算门禁的平衡”: simple fast-pass 会节省 token，但如果上游编译修复失败仍继续 unblock，后续任务会连锁失败。建议下一轮把“未达标任务”标记为 failed 并阻塞依赖，而不是 completed-with-warning。
- 供应商 429 已成为烟测尾延迟主因；建议对 429 中携带的部分有效输出做 stage salvage，或在团队长任务中启用 provider fallback。

## 1. 本轮结论

已按“提示词组件账本”思路落地 metrics，并基于 `/Users/huaquan.liang/.claude-go/config/config.json` 派生临时配置启动新版飞书 Bot 做同样场景压测。

本轮代码优化已完成:

- P0: 增加 `ToolProfile`，Feishu/chat/research/coding/team/admin 分场景注入工具，不再默认全量注册 coding/team/admin/computer/cron 工具。
- P0: MCP 默认改为 `MCPToolSearch` + `MCPToolInvoke` 代理工具，避免 238 个 MCP tools 的 schema 每轮进 prompt。
- P0: `Skill` tool description 不再塞完整技能列表，system prompt 只保留短技能清单。
- P1: role skills 限制为 Top 3，单个正文 800 chars；development workflow 的 researcher/planner/reviewer/architect 默认用本地 team profile，减少外网检索和写工具暴露。
- P1: team handoff / prev_result 改成 summary + blackboard/artifact ref，避免上游正文在 `handoff` 和 `prev_result` 双份重复。
- P1: provider `contextWindow` 已接入 Feishu 主会话、nested agent、team stage 的 Engine/Compactor。
- P2: Auto Plan 注入文案压缩为短 mode tag。

验证结果:

- `go test ./pkg/api ./pkg/mcp ./pkg/feishu ./pkg/engine ./pkg/metrics ./pkg/agent ./pkg/skills ./pkg/tool/builtin` 通过。
- `go build -o /tmp/claude-go-feishu-optimized ./cmd/claude-go` 通过。
- 新版 Bot 已用临时配置启动并连接飞书 WebSocket。
- smoke 样本生成 178 个 prompt debug JSON，目录约 14MB。
- `/go development` 仍未正常收敛，25 分钟后 smoke timeout；卡点变成 Orchestrator 评审阶段状态机停滞，而不是 MCP/schema 体积。

## 2. 实测样本

临时配置:

- `/tmp/claude-go-feishu-opt-20260505-012225.json`
- Prompt debug: `/Users/huaquan.liang/.claude-go/prompt-debug-codex-opt-20260505-012225`
- Bot binary: `/tmp/claude-go-feishu-optimized`

样本输入:

```text
帮我分析 claude-go 的飞书 Bot 架构，并给出改造计划。
```

```text
/go development 开发个golang ToDo 应用，输出到工作目录下
```

执行结果:

- 第一条复杂飞书问答完成，响应约 1372 字符。
- `/go development` 进入 research/design/plan/orchestrator，并实际写入 ToDo 应用文件。
- 团队 `codex-smoke-development-opt-15555` 在 Orchestrator 评审 round 1 停滞，心跳显示 pool 活跃为 0、team 仍 running。
- 已产出文件包括 `/Users/huaquan.liang/test/todo-app/internal/domain/todo.go`、`/Users/huaquan.liang/test/todo-app/internal/ports/repository.go`、`/Users/huaquan.liang/test/todo-app/go.mod` 等。

## 3. 优化前后对比

上轮基线: 83 次 LLM 调用，total tokens 3378804。

本轮优化后: 178 次 LLM 调用，total tokens 3081048。

| 指标 | 优化前 | 优化后 | 变化 |
|---|---:|---:|---:|
| LLM calls | 83 | 178 | +114.5% |
| input_tokens | 2288694 | 445698 | -80.5% |
| cache_read_tokens | 1047264 | 2586964 | +147.0% |
| output_tokens | 42846 | 48386 | +12.9% |
| total_tokens | 3378804 | 3081048 | -8.8% |

解释:

- 即使本轮调用次数翻倍，total tokens 仍下降约 8.8%，说明工具/MCP schema 瘦身有效。
- input tokens 大幅下降，主要来自 MCP schema 归零和 built-in tools profile 化。
- cache_read_tokens 上升，说明 provider 端 prompt cache 命中较多；账单口径需要按供应商是否折扣 cache read 单独核算。
- 总量没有按 schema 降幅等比例下降，是因为 Orchestrator 停滞导致 coder/reviewer/tester 多轮调用，`messages_chars` 成为新的主成本。

## 4. Prompt Component 对比

| component | 优化前 avg_chars | 优化后 avg_chars | 变化 |
|---|---:|---:|---:|
| tools_schema_chars | 104932 | 9621 | -90.8% |
| mcp_tools_chars | 86245 | 0 | -100.0% |
| prev_result_chars | 1677 | 64 | -96.2% |
| blackboard_chars | 429 | 92 | -78.5% |
| messages_chars | 34129 | 39601 | +16.0% |
| system_chars | 3409 | 5108 | +49.8% |
| role_skills_chars | 536 | 2904 | +441.8% |
| memory_chars | 260 | 277 | +6.5% |
| skill_listing_chars | 0 | 0 | 0 |

本轮优化后组件聚合:

| component | sum_chars | avg_chars | max_chars |
|---|---:|---:|---:|
| tools_schema_chars | 1712494 | 9621 | 11858 |
| mcp_tools_chars | 0 | 0 | 0 |
| messages_chars | 7048969 | 39601 | 119448 |
| system_chars | 909296 | 5108 | 10258 |
| role_skills_chars | 516944 | 2904 | 4064 |
| memory_chars | 49300 | 277 | 3654 |
| blackboard_chars | 16410 | 92 | 1146 |
| prev_result_chars | 11333 | 64 | 935 |
| skill_listing_chars | 0 | 0 | 0 |

重要修正:

- P0 的最大收益已经验证: `tools_schema_chars` 从平均 105K 降到 9.6K，`mcp_tools_chars` 从 86K 降到 0。
- `prev_result_chars` 和 `blackboard_chars` 已明显下降，summary/ref 方案有效。
- `role_skills_chars` 在本轮实测中反而偏高，压测后已继续收紧到 Top 3 + 800 chars；这项需要下一轮短样本复测确认。
- 新主因变成 `messages_chars`，尤其是 Orchestrator 评审停滞后的长消息历史。

## 5. 按来源/角色统计

| source | workflow | role | calls | input | cache_read | output | total |
|---|---|---|---:|---:|---:|---:|---:|
| feishu_main | auto_plan | classifier | 1 | 160 | 0 | 79 | 239 |
| feishu_main | - | chat | 17 | 35454 | 126266 | 3473 | 165193 |
| team_stage | development | researcher | 9 | 46608 | 54631 | 3149 | 104388 |
| team_stage | development | architect | 5 | 28101 | 36726 | 8142 | 72969 |
| team_stage | development | planner | 8 | 26092 | 122722 | 6444 | 155258 |
| team_stage | development | coder | 100 | 190140 | 2027203 | 17048 | 2234391 |
| team_stage | development | reviewer | 18 | 76782 | 102985 | 5990 | 185757 |
| team_stage | development | tester | 19 | 42005 | 116431 | 3562 | 161998 |

结论:

- `coder` 仍是最大成本方，100 次调用累计 223.4 万 total tokens。
- 调用次数从 42 增加到 100，说明 schema 瘦身后成本问题转移到任务编排和评审循环。
- `researcher` 从 54.5 万 total 降到 10.4 万 total，说明研发场景减少外部检索和工具暴露是有效方向。

## 6. 当前风险

### 6.1 Orchestrator 评审阶段停滞

本轮 smoke 失败原因:

- 团队状态: running。
- Agent 进度: 3/6。
- Coordinator 心跳: pool 活跃 0，team 运行 0，idle 3。
- Watchdog 提示: 停留在阶段 `[评审]` round 1，疑似卡住。
- 最终: 25 分钟 timeout，context canceled。

这说明团队长任务仍有状态机风险:

- Orchestrator 某个评审/微测任务可能没有正确写 checkpoint。
- DAG 中存在 ready/in_progress/pending 状态不一致。
- Watchdog 当前只通知“疑似卡住”，没有把团队收敛为 failed 或自动 checkpoint-stop。

建议作为下一轮 P0:

- Orchestrator 增加 `activeTaskCount`、`readyTaskCount`、`inProgressTaskCount` 指标。
- 当 `pool active=0` 且 `ready=0` 且团队 running 超过阈值时，自动诊断 DAG 并 fail，而不是继续空转。
- Watchdog 文案和逻辑对齐: 当前“30 分钟无实际进展”实际可能约 11 分钟触发。
- 对评审 round 增加硬超时，超时后写入失败 checkpoint 并释放 DAG。

### 6.2 Messages 成本成为新主因

`messages_chars` 本轮平均 39.6K，最大 119K。

建议:

- Tool result 入 messages 前按类型做摘要，完整内容保存 artifact。
- reviewer/tester 的重复反馈用 hash/ref 去重。
- Orchestrator 每个 task round 后只保留 `summary/files/decision/errors/next_action`，不要保留完整对话。
- 当 `messages_chars > 30000` 时触发 micro-compact，不等总 context 接近窗口。

### 6.3 Prompt Debug 保留策略

178 次调用生成约 14MB debug 文件。建议增加:

- `promptDebugMaxFiles`
- `promptDebugMaxBytes`
- `promptDebugSampleRate`
- API key / Authorization / secret pattern 脱敏

## 7. 已落地代码清单

主要改动:

- `pkg/tool/builtin/profile.go`: 新增 ToolProfile 和分场景工具注册。
- `pkg/feishu/mcp_proxy_tools.go`: 新增 MCP 搜索/调用代理工具。
- `pkg/feishu/session.go`: Feishu 主会话、nested agent、team stage 使用 profile registry；contextWindow 接入 Engine/Compactor。
- `pkg/skills/skills.go`: 新增短技能清单；SkillTool description 去完整列表。
- `pkg/agent/roles.go`: role skills Top 3 + 正文限长。
- `pkg/agent/blackboard.go`: handoff 改为摘要 + blackboard ref。
- `pkg/agent/workflow.go`: prev_result 改为 summary/ref。
- `pkg/feishu/bot.go`: Auto Plan 注入压缩。
- `pkg/engine/engine.go`: Config 增加 ContextWindow。

## 8. 后续优先级

P0:

- 修复 Orchestrator/评审停滞收敛，避免长时间 running 空转。
- 给 `messages_chars` 加硬阈值压缩和 tool_result artifact ref。
- 补 prompt debug 保留/脱敏策略。

P1:

- 增加 `/role show <role>` 展示“实际注入 skills”及预计 chars。
- development workflow 默认禁用 researcher 外网搜索，除非目标显式包含“最新/网页/外部资料”。
- 将 provider cache read token 按供应商折扣口径单独展示。

P2:

- 支持 `/cwd` 查询与临时切换，避免配置 cwd 与用户口中的仓库不一致。
- 对复杂 Auto Plan 改成 engine mode flag，而不是把 mode tag 放入 user message。

## 9. 查询命令

查看本轮 prompt debug 聚合:

```bash
jq -s '{calls:length, tokens:{input:(map(.usage.input_tokens // 0)|add), cache_read:(map(.usage.cache_read_tokens // 0)|add), output:(map(.usage.output_tokens // 0)|add), total:(map(.usage.total_tokens // 0)|add)}}' /Users/huaquan.liang/.claude-go/prompt-debug-codex-opt-20260505-012225/*.json
```

按组件聚合:

```bash
jq -s '. as $all | ["system_chars","tools_schema_chars","mcp_tools_chars","role_skills_chars","memory_chars","blackboard_chars","prev_result_chars","messages_chars"] | map(. as $k | {component:$k, sum:($all|map(.prompt_components[$k] // 0)|add), max:($all|map(.prompt_components[$k] // 0)|max)})' /Users/huaquan.liang/.claude-go/prompt-debug-codex-opt-20260505-012225/*.json
```
