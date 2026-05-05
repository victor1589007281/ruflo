# claude-go 提示词组件账本与 Token 优化复盘

更新日期: 2026-05-05

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
