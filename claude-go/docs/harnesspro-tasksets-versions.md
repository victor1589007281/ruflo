# HarnessPro：外部任务集、版本轨迹与效果比较

本文对应 `pkg/harnesspro` 的轻量任务集接入和跨运行版本仓库。当前执行体仍是**一次单发补全**：`system=harness instructions`、`user=task prompt`。它不是 Terminal-Bench 2 的工具型 agent 执行器，也不会产生论文完整形态的 shell/tool/hook 轨迹。

## 1. 准备并注册任务集

一个任务集目录包含 `taskset.json`；任务可内联在 `tasks`，也可放在同目录的 `tasks_file`：

```json
{
  "name": "support-json",
  "version": "v1",
  "description": "两种不同输出契约，避免把单题字段固化成全局 schema",
  "tasks_file": "tasks.json"
}
```

`tasks.json` 示例：

```json
[
  {
    "id": "incident-summary",
    "category": "json",
    "difficulty": 0.3,
    "prompt": "把以下故障记录整理成 JSON 对象，字段必须是 service、severity、summary，不要输出其他文字。记录：payment 服务发生高优先级超时。"
  },
  {
    "id": "refund-advice",
    "category": "json",
    "difficulty": 0.4,
    "prompt": "把以下退款请求整理成 JSON 对象，字段必须是 order_id、eligible、reason，不要输出其他文字。订单 A17 已签收 20 天，超过 7 天退货期。"
  }
]
```

至少两题，任务 ID 不得重复。`name`/`version` 只允许字母数字和 `._-`；`tasks_file` 只能是当前目录下的简单文件名，不能用 `../` 逃逸。任务顺序也是考场定义的一部分。

```bash
# 只校验并计算 hash，不注册
claude-go harness-evo taskset validate ./support-json

# CLI 默认注册到 ~/.claude-go/harnesspro/tasksets
claude-go harness-evo taskset import ./support-json
claude-go harness-evo taskset list
claude-go harness-evo taskset show support-json@v1

# 若要让 dashboard/webapp 使用同一注册表，显式指向其 StateDir 下的目录
claude-go harness-evo taskset import ./support-json \
  --dir /mnt/data/claude-go/harness-evo/tasksets
```

同一 `name@version` 不允许换成不同任务内容；修改题目后应提升 `version`。注册后的 manifest 自包含，不再依赖源目录。

也可通过 dashboard 注册（请求体是完整 TaskSet JSON）：

```text
POST /api/harness-evo/tasksets
GET  /api/harness-evo/tasksets
GET  /api/harness-evo/tasksets/support-json@v1
```

webapp 对应代理为 `GET/POST /api/workflows/evo/tasksets`，需要 webapp 登录。

## 2. 运行多轮进化

```bash
claude-go --model opencode:deepseek-v4.1-flash harness-evo run \
  --taskset support-json@v1 \
  --taskset-dir /mnt/data/claude-go/harness-evo/tasksets \
  --version-dir /mnt/data/claude-go/harness-evo/versions \
  --run-id evo-manual-001 \
  --epochs 3 --k 3 --train 1 \
  --workdir /mnt/data/claude-go/harness-evo/runs/evo-manual-001/workdir \
  --json
```

`--taskset` 与 `--pool` 互斥。任务集 hash 始终从实际任务重新计算，不信任外部申报值。

在 webapp 的 Harness 自动进化运行弹窗中，打开“高级”，从“任务集 taskset”下拉选择 `support-json@v1`，设置 epochs、k、train 后启动；格式表单和 JSON 模式都可使用。

## 3. 版本与轨迹如何关联

每个 harness 有两类 ID：

- `content_id = hc-sha256(instructions)`：相同指令跨运行的共同坐标。
- `version_id = hash(run_id, candidate_id, instructions)`：一次运行中的唯一谱系节点。

版本还记录 `parent_version_id`、origin、epoch、model、task_set、created_at 和 instructions。rollout、floor、eval、diagnosis、plan、survivor 和 report 中的候选都携带版本坐标；题级输出和失败断言仍留在该 run 的 journal/report，聚合观测进入版本仓库：

- `train_rollout`：训练集多次 rollout 的 pass_rate、断言级 pass_frac、cost、n。
- `floor`：h0 在验证集的逐题评测聚合。
- `eval`：候选在验证集的效果及 frontier/filtered/survivor_tiers。

同一版本、同一 phase+epoch 的观测在重试/resume 时覆盖，避免重复计数。全灭后的工程兜底会显式记录 `fallback=true`，不能当作正常过地板晋级。

```text
GET /api/harness-evo/versions?run_id=evo-manual-001
GET /api/harness-evo/versions?content_id=hc-...
GET /api/harness-evo/versions?ids=hv-...,hv-...
GET /api/harness-evo/versions/{version_id}
```

webapp 代理为 `GET /api/workflows/evo/versions`，只返回当前账号拥有的 harness-evo 运行版本。

## 4. 在 webapp 比较版本

打开 Harness 自动进化的运行详情和产物阅读器：

1. “版本与效果”（手机为“版本”tab）默认显示当前运行。
2. 切到“全部运行”可跨 run 勾选版本。
3. 勾选两个或更多版本，对比各 phase/epoch 的 pass_rate、pass_frac、cost、n、frontier、filtered、fallback 和 survivor_tiers。
4. 恰好勾选两个版本时显示带双侧行号的 instructions diff。

模型或任务集 hash 不同的版本会显示“非严格同条件比较”提示；即使模型和任务集相同，也应结合样本数 n 与运行条件解读，不能把单次 pass_rate 当成统计显著提升。

## 5. P0 评估口径

- 任务描述及公开文件是字段名、JSON schema、输出格式和验收契约的唯一来源；Planner/Editor 不得用 harness 覆盖任务契约。
- `RunTests` 的失败断言不再被 rollout/eval/floor 丢弃；失败断言进入逐题明细和 Analyzer 输入。
- 一题全部断言通过才算 pass；`pass_frac` 是断言级通过比例。
- infra error 不进入 pass 率分母，但保留在 Details，不会被伪装成普通产品失败。
