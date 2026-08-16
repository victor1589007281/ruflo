# harness-evo —— harness 层进化效果持续跟进 (手册 7.5)

方案三（冻结权重、harness 层增强）的**评测方法与数据台账**所在目录。
模型权重一个 bit 不动；这里度量的是"关键功能点改造前后，同一批弱模型在同一批
确定性任务上的表现变化"。效果展示页：docforge claude-go 手册 **7.5**。

## 两条评测轨

| 轨 | 测什么 | 任务集 | 判分 | 运行方式 |
|---|---|---|---|---|
| **A 引擎级 e2e A/B** | 完整 agent 循环（工具调用/编辑/跑测试） | t1 写文件 · t2 改 bug 跑测试 · t3 多步计算 · t4 死循环抵抗（权威定义在 `deploy/k8s-e2e-weakmodel.sh` 的 T1_P..T4_C） | 确定性 checker（文件内容/测试通过），零模型自评 | `run-k8s-ab.sh`（真集群 kind-dbk8s-e2e，集群内 Ollama，逐 run `CLAUDE_GO_WEAK_MODEL=0|1` 切换基线/方案三） |
| **B replay 单发** | 单发提示词质量（格式纪律/指令遵循/编码判断/agent 常识） | `regression/evalenv/coding-basic.jsonl`（12 任务，Expect 子串断言） | `evo replay`，judge=nil | `run-replay.sh` |

GEPA prompt 进化（L5）的候选打分流在轨 B 上跑：`evo gepa-loop` 自动 replay 入
Pareto 池；手工复测用 `run-replay.sh ... candidate`。

## 评测纪律（踩过的坑都在这里）

1. **确定性判分**：能用 checker/Expect 判的绝不叫模型自评（judge=nil）。
2. **A/B 同体切换**：同一 Deployment、逐 run 用环境变量切基线/增强，排除环境差。
3. **提示词不嵌引号**：prompt 经 `sh -c` 两层展开会变形（曾把 t2 带偏，记为评测
   缺陷而非被测物缺陷）。
4. **ConfigMap 顺序**：`monolith.yaml` 内嵌旧版 ConfigMap，必须先 apply yaml 再建
   ConfigMap 并 rollout restart。
5. **H3 不同源**：GEPA 反思器必须与被评分模型不同源（CLI 强制报错）。
6. **无效轮留痕不上图**：基础设施失败（全 rc≠0）的行记 `valid=0`，入台账但
   `xychart` 子命令不采。
7. **镜像唯一标签**：每次构建用时间戳标签导入 kind，防验到陈旧二进制。

## 台账格式（ledger.jsonl, v1, append-only, 随仓提交）

```json
{"v":1,"ts":"2026-08-12T14:23:00+08:00","commit":"abc1234","change":"L2 修复环上线",
 "track":"k8s-ab","suite":"engine-basic-v1","model":"lfm2.5:2.6b-q4_k_m",
 "mode":"baseline|plan3|candidate","task":"t1","pass":1,"wall_s":26,
 "valid":1,"note":"","source":"/tmp/wml-e2e-results-….jsonl"}
```

## 一次跟进的标准动作（SOP）

```bash
# 1. 改功能点 → 提交 (台账行自动记 commit)
# 2. 跑电池 (轨 A 约 40-90 分钟; 可只跑单模型 lfm|gemma)
bash regression/harness-evo/run-k8s-ab.sh all full "本次变更一句话说明"
# 2b. (可选) 轨 B 单发复测
bash regression/harness-evo/run-replay.sh ollama:lfm2.5:2.6b-q4_k_m \
     /tmp/prompt.txt baseline "说明"
# 3. 看文本汇总
python3 regression/harness-evo/ledger.py report
# 4. 生成 7.5 页面图表数据块, 贴回 docforge (LEDGER 标记区间)
python3 regression/harness-evo/ledger.py xychart
```

## 文件

| 文件 | 作用 |
|---|---|
| `run-k8s-ab.sh` | 轨 A 电池 + 记账（包裹 `deploy/k8s-e2e-weakmodel.sh`） |
| `run-replay.sh` | 轨 B replay + 记账（包裹 `claude-go evo replay`） |
| `ledger.py` | 台账追加（幂等去重/无效轮标记）· `report` 汇总 · `xychart` 出图数据 |
| `ledger.jsonl` | 数据台账（唯一真相源） |
