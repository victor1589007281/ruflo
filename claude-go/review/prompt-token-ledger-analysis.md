# claude-go 提示词组件账本与 Token 消耗分析

更新日期: 2026-05-05

## 1. 本次验证结论

本轮已用 `/Users/huaquan.liang/.claude-go/config/config.json` 派生临时 debug 配置启动飞书 Bot，并开启提示词调试。

实际启动结果:

- 最新源码编译成功，二进制: `/tmp/claude-go-feishu-metrics-smoke`。
- 飞书 Bot 已启动并连接 WebSocket。
- 配置中的 `ruflo` MCP 已连接，发现 238 个 MCP tools。
- Prompt debug 已开启，当前目录: `/Users/huaquan.liang/.claude-go/prompt-debug-codex-20260505-003605`。
- 本轮 prompt debug 产生 83 个 JSON 文件，目录约 21MB。

本轮新增修正:

- 修复 MCP 初始化可能卡死的问题: MCP `initialize` / `tools/list` 现在会继承 context，飞书 Bot 初始化 MCP 时也加了 30 秒超时保护。
- 修复 metrics 标签覆盖问题: `api.Client.Tag=feishu` 不再覆盖调用 context 中的 `source=feishu_main/team_stage`。
- Auto Plan 复杂度分类器也补了 `source=feishu_main`、`workflow=auto_plan`、`purpose=complexity_classifier` 账本标签。

核心结论:

- metrics 采集正常，已经能解释“是哪一块撑大 prompt”。
- 最大成本不是 system prompt，也不是 role skills，而是 MCP/tool schema。
- 在当前配置下，每次 Feishu/Team LLM 调用平均携带约 105K 字符 tool schema，其中约 86K 字符来自 MCP tools。
- `/go development` 样本 25 分钟内产生约 311 万 team-stage total tokens，且最终因团队空转等待被 smoke timeout 停止，说明研发团队长任务不仅贵，还有 stall/收敛风险。

## 2. 新增 metrics 字段

每次 LLM 调用会写入以下业务标签:

- `source`: `feishu_main`、`nested_agent`、`team_stage` 等。
- `purpose`: 飞书主会话 chatID、团队名或子 Agent 类型。
- `workflow`: 团队工作流名，例如 `development`。
- `role`: 团队角色名，例如 `coder`、`tester`、`reviewer`。
- `component`: 仅用于 prompt component 指标。

每次调用会记录 9 类提示词组件:

- `system_chars`: 最终 system prompt 字符数。
- `tools_schema_chars`: 全部 tool schema 字符数。
- `mcp_tools_chars`: `mcp_` 工具 schema 字符数。
- `skill_listing_chars`: `<available_skills>` 技能清单字符数。
- `role_skills_chars`: `<role_skills>` 角色技能正文字符数。
- `memory_chars`: CLAUDE.md / relevant memories / structured memories / dream memory 等记忆上下文字符数。
- `blackboard_chars`: 团队 handoff 和飞书会话注入上下文估算字符数。
- `prev_result_chars`: 上游阶段结果、设计文档、调研输入等依赖结果字符数。
- `messages_chars`: 当前 messages 中 text/content/thinking/tool input 的字符数。

新增 metric:

- `llm_prompt_component_chars`
- `llm_prompt_component_tokens`，当前按 `chars / 4` 做近似估算。

## 3. Prompt Debug 开关

配置方式:

```json
{
  "engine": {
    "promptDebug": true,
    "promptDebugDir": "/Users/huaquan.liang/.claude-go/prompt-debug"
  }
}
```

行为:

- `promptDebug=false` 或缺省时不落盘。
- `promptDebug=true` 时，每次 `StreamMessage` / `SendMessage` 生成一个 JSON 文件。
- 文件权限为 `0600`，目录权限为 `0700`。
- 文件包含完整 request body、system、messages、tool schema、stream events / response、usage、prompt component 账本。

注意:

- Debug 文件包含完整 prompt、用户输入、工具结果、项目上下文和可能的密钥片段，只建议短期开启。
- 建议后续增加保留策略: 最大文件数、最大目录大小、自动脱敏、按 source/role 采样。

## 4. 本轮实测样本

样本输入:

```text
帮我分析 claude-go 的飞书 Bot 架构，并给出改造计划。
```

```text
/go development 开发个golang ToDo 应用，输出到工作目录下
```

执行说明:

- 普通/复杂飞书问答走 `feishu_main` 主会话链路。
- Auto Plan 复杂度分类器走 `source=feishu_main, workflow=auto_plan`。
- `/go development` 走 `source=team_stage, workflow=development`，并覆盖 `researcher/architect/planner/coder/reviewer/tester`。
- smoke 为避免真实飞书回消息副作用，本地模拟时禁用了真实发送，但保留 MCP 和 LLM 调用链路。
- 配置中的 `cwd` 是 `/Users/huaquan.liang/test`，所以架构分析回复指出当前工作目录不是 claude-go 仓库；如要让 Bot 直接读 claude-go 源码，应把 `cwd` 切到仓库或在消息中给绝对路径。

产出:

- ToDo 应用已实际输出到 `/Users/huaquan.liang/test/todo-app` 和 `/Users/huaquan.liang/test/todo`。
- 团队在 25 分钟 smoke timeout 后被停止，最终 `team.json` 状态为 `stopped`，已持久化 `research/design/plan` 三个阶段，coder/reviewer/tester 的调用和文件产出已发生，但 orchestrator 未正常收敛完成。

## 5. 实测 Token 数据

本轮从 `2026-05-05T00:36` 后统计，共 83 次 LLM 调用。

按来源/角色统计:

| source | workflow | role | calls | input_tokens | cache_read_tokens | output_tokens | total_tokens |
|---|---|---:|---:|---:|---:|---:|---:|
| feishu_main | auto_plan | classifier | 1 | 160 | 0 | 208 | 368 |
| feishu_main | - | chat | 8 | 191285 | 71207 | 1277 | 263769 |
| team_stage | development | researcher | 15 | 389119 | 142892 | 13112 | 545123 |
| team_stage | development | architect | 8 | 290074 | 39867 | 9672 | 339613 |
| team_stage | development | planner | 2 | 73807 | 0 | 2557 | 76364 |
| team_stage | development | coder | 42 | 1097244 | 789271 | 12598 | 1899113 |
| team_stage | development | reviewer | 4 | 151286 | 0 | 2551 | 153837 |
| team_stage | development | tester | 3 | 95719 | 4027 | 871 | 100617 |

总量:

- total tokens: 3378804。
- input tokens: 2288694。
- cache read tokens: 1047264。
- output tokens: 42846。

关键解释:

- `coder` 是最大消耗方，42 次调用累计约 189.9 万 total tokens。
- `feishu_main` 的一次复杂问答虽然只有 8 次主会话调用，但因为每轮携带完整工具 schema，累计也达到 26.4 万 total tokens。
- Auto Plan 分类器本身很便宜，只有 368 total tokens；贵的是分类之后进入主会话和工具循环。

## 6. 实测 Prompt Component 数据

本轮 83 次调用的组件聚合:

| component | count | sum_chars | avg_chars | max_chars |
|---|---:|---:|---:|---:|
| tools_schema_chars | 83 | 8709382 | 104932 | 106259 |
| mcp_tools_chars | 83 | 7158354 | 86245 | 87297 |
| messages_chars | 83 | 2832742 | 34129 | 75837 |
| system_chars | 83 | 282910 | 3409 | 7837 |
| prev_result_chars | 83 | 139163 | 1677 | 26441 |
| role_skills_chars | 83 | 44460 | 536 | 988 |
| blackboard_chars | 83 | 35591 | 429 | 4646 |
| memory_chars | 83 | 21594 | 260 | 3648 |
| skill_listing_chars | 83 | 0 | 0 | 0 |

结论:

- `tools_schema_chars` 是第一大项，平均约 105K 字符。
- `mcp_tools_chars` 占 tool schema 的绝大部分，平均约 86K 字符。
- `messages_chars` 是第二大项，长工具循环下最高达到 75K 字符。
- `role_skills_chars` 远小于工具 schema，当前不是主要成本。
- `skill_listing_chars=0` 说明本轮 Feishu/Team prompt 没有以 `<available_skills>` 形式注入完整技能清单；这降低了 token，但也意味着“skills 是否真正被角色使用”要看 `role_skills_chars` 和角色 prompt，而不能只看全局 skill 列表。

## 7. 对上一版分析的修正

上一版判断方向正确:

- 先加账本，不凭感觉砍 prompt。
- 飞书主会话、嵌套 Agent、研发团队都需要 source/workflow/role 标签。
- 长期运行的风险主要来自工具 schema、message history、blackboard、prev_result 和 plan。

需要修正/加强的点:

- MCP/tool schema 的优先级应从 P0 提到最高优先级。本轮实测中它远超 memory、blackboard、role skills。
- `skill_listing_chars` 本轮为 0，说明全量 skills listing 不是当前实际成本大头。
- `/go development` 的成本不只是 plan 大，而是 “MCP schema × 工具循环 × 多角色 × cache read/input 重复”。
- 团队执行存在空转风险: smoke timeout 前 pool 活跃为 0、team 仍 running，说明需要 watchdog 发现“无活跃 agent 但团队未收敛”的状态。

## 8. 优化方案

### P0: MCP/Tool Schema 懒加载

当前每轮都发送 238 个 MCP tools，导致约 87K 字符固定成本。

建议:

- Feishu 主会话默认只暴露基础工具、WebSearch/WebFetch、FileRead/Glob/Grep 和一个 `MCPToolSearch`。
- 当用户意图命中特定 MCP server/工具类别时，再按需展开具体 MCP tool schema。
- `/go development` 默认禁用与开发无关的 MCP tools，例如 finance、trading、browser automation 之外的非必要工具。
- 给每个 workflow/role 建立 ToolProfile:
  - researcher: WebSearch/WebFetch/FileRead/Grep。
  - architect/planner: FileRead/Grep/TodoWrite/Plan tools。
  - coder: FileRead/FileWrite/FileEdit/Bash/Glob/Grep。
  - tester: Bash/FileRead/Grep。
  - reviewer: FileRead/Grep。

预期收益:

- 如果把 MCP schema 从 87K 降到 5K-15K 字符，本轮 83 次调用可少发送约 150 万到 170 万等效 tokens。

### P0: Team Stall Watchdog

本轮 `/go development` 在 25 分钟后被停止，team 仍处于 running，但心跳显示 pool 活跃为 0。

建议:

- 增加 team-level idle timeout: 连续 N 分钟无 LLM call、无 tool call、无 stage 进展时自动 fail 或 checkpoint stop。
- Coordinator 心跳中加入 `lastLLMAt`、`lastToolAt`、`lastStageAt`。
- 如果 orchestrator 已无 active task 但 team 未完成，写入诊断并收敛为 failed，而不是无限 running。
- dashboard/metrics 增加 `team_idle_sec`、`team_stall_count`。

### P0: Debug Retention 与脱敏

Prompt debug 很有用，但本轮 83 次调用生成 21MB。

建议:

- 配置 `promptDebugMaxFiles`、`promptDebugMaxBytes`、`promptDebugSampleRate`。
- 默认脱敏 `apiKey`、`Authorization`、`x-api-key`、常见 secret pattern。
- 大型 stream events 可保留尾部摘要，完整事件仅在 `promptDebugFull=true` 时保存。

### P1: 消息历史与工具结果压缩

本轮 `messages_chars` 平均 34K，最大 75K。

建议:

- 工具结果入 messages 前按 tool 类型做结构化压缩。
- Bash/FileRead 大输出默认保存 artifact ref，只把摘要回填给模型。
- 对重复的 tool_result 做 hash 去重。
- 当 `messages_chars > 30000` 时触发 micro-compact，不等总 token 超阈值。

### P1: Development Workflow Artifact Ref

本轮 `prev_result_chars` 最大 26441，后续长 plan 会继续放大。

建议:

- research/design/plan 输出落盘后，后续阶段只传摘要 + 文件路径 + section refs。
- task prompt 使用 `designRef`、`constraintID`、`acceptanceID`，不要重复粘贴完整设计正文。
- Blackboard 只保存可执行事实和决策，不保存整段报告。

### P1: 角色 Skills 精准注入

本轮 `role_skills_chars` 不是主因，但仍可优化。

建议:

- 每个角色只注入 top 1-3 个技能摘要。
- 技能正文用 `SkillTool` 按需读取，system 中只放技能名、用途、触发条件。
- 为 `/role show` 和 metrics 增加“实际注入 skills”展示，避免用户看不到角色到底用了什么技能。

### P2: CWD 与项目上下文防错

本轮配置 `cwd=/Users/huaquan.liang/test`，所以飞书架构分析无法直接读取 claude-go 源码。

建议:

- 当用户提到 `claude-go`、`当前项目`、`这个仓库` 且 cwd 不匹配时，Bot 主动提示当前 cwd。
- 支持 `/cwd` 查询和 `/cwd set <path>` 临时切换。
- `/go development ... 输出到工作目录下` 保持使用 cwd，但代码分析类任务建议自动识别 repo path。

## 9. 查询命令

按角色统计调用次数:

```bash
jq -r 'select(.name=="llm_call_count") | [.labels.source, (.labels.workflow // ""), (.labels.role // ""), (.labels.purpose // "")] | @tsv' /Users/huaquan.liang/.claude-go/metrics/llm.jsonl
```

按组件聚合:

```bash
jq -r 'select(.name=="llm_prompt_component_chars") | [.labels.component,.value] | @tsv' /Users/huaquan.liang/.claude-go/metrics/llm.jsonl |
awk -F'\t' '{sum[$1]+=$2; cnt[$1]++; if($2>max[$1]) max[$1]=$2} END{for(k in sum) printf "%s count=%d avg=%.1f max=%d\n", k,cnt[k],sum[k]/cnt[k],max[k]}'
```

查看 prompt debug:

```bash
ls -lh /Users/huaquan.liang/.claude-go/prompt-debug-codex-20260505-003605
jq '{source,workflow,role,purpose,status,usage,prompt_components}' /Users/huaquan.liang/.claude-go/prompt-debug-codex-20260505-003605/*.json
```
