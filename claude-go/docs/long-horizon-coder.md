# 长程自主编码运行时 (`claude-go coder`)

把一次性的 agent run 串成**有状态、可恢复、有客观终态**的长程任务。

一句话定位: `claude-go run` 是"做一轮", `claude-go coder` 是"做到过为止, 而且中途
被杀掉也能接着做"。

---

## 1. 为什么需要它

单轮的 `claude-go run` 有一个实测过的失败形态:

> 进程 `exit=0`、`is_error=false`、模型输出了一段很像样的总结, 而工作区**一个字节
> 都没改**。

只要判据取自 agent 的自我报告, 这种失败永远是绿的。`coder` 的全部设计都围绕这一条:

| 关心的性质 | 由什么保证 |
|---|---|
| 做完了没有 | **驱动侧**跑 `--verify`, 退出码 0 才算完。模型自称不算 |
| 断点能否恢复 | 每轮开头**先测量世界**, 而不是相信落盘的自述 |
| 是否在空转 | 工作区指纹 + 归一化后的验证输出, 两者都不变才算"没动手" |
| 程序能否独立运行 | 终态在一个**排除运行时目录的干净副本**里重新验一遍 |

---

## 2. 快速上手

```bash
claude-go coder \
  --goal "实现 Greet 函数, 让 go test ./... 通过" \
  --verify "go test ./..." \
  --workspace /tmp/demo
```

三个必填参数就是全部心智负担: **做什么**(`--goal`)、**怎么算做完**(`--verify`)、
**在哪做**(`--workspace`)。

`--verify` 是唯一的成功判据。它由命令行给出而不是写在项目里 —— agent 改不到它,
也就无法把"判据"改成自己能过的东西。

### 经网关调用非 Claude 模型

```bash
claude-go coder \
  --goal-file goal.txt \
  --verify "go test ./..." \
  --workspace /tmp/demo \
  --model opencode:space-bunny-free \
  --config /mnt/data/claude-go/config/config.json \
  --gateway http://127.0.0.1:18989
```

`--gateway` 会写进 `CLAUDE_GO_LLM_GATEWAY`。`opencode:` 系模型**必须**经网关 ——
直连不认带前缀的名字。判据要落在响应体的 `model` 字段上, 而不是 HTTP 200:
网关会接受未声明的模型名并静默换成别的模型来服务。

---

## 3. 一轮里发生了什么

```
┌─ 每轮 ────────────────────────────────────────────────┐
│ 1. 驱动侧跑 --verify         ← 先测量世界, 不信任任何自述 │
│    若通过 → 直接进终态验收 (不烧这一轮)                  │
│ 2. 组装 prompt (目标 + 上轮原始报错) 交给 agent          │
│ 3. 有界地跑一个 claude-go 子进程                        │
│ 4. 驱动侧**再**跑 --verify   ← 唯一成功判据              │
│ 5. 算工作区指纹, 判这一轮动没动手                        │
│ 6. 状态原子落盘 (.coder/state.json)                     │
└───────────────────────────────────────────────────────┘
        ↓ 通过
   导出干净副本 (排除 .coder/) → 在副本里再验一次
```

第 1 步放在 agent 动手**之前**是刻意的, 它同时买到两件事:

- **恢复天然正确**: 上一轮无论怎么死的 (崩溃/被杀/断电), 新进程第一件事就是重新
  测量世界。实测过: 一个已经做完但被挂死的任务, 重启后**一轮都没烧**就判过了。
- **空转可判**: 有"这轮改动前"和"这轮结束后"两个读数才能比。

---

## 4. 它在 `<workspace>/.coder/` 里留下什么

| 文件 | 用途 |
|---|---|
| `state.json` | 状态与逐轮读数。**断点恢复的唯一依据** |
| `PLAN.md` / `PROGRESS.md` | 让 agent 自己维护的计划与进度 (跨进程的记忆) |
| `prompt.md` | 本轮实际喂进去的 prompt 正文 (取证: 它到底被要求做什么) |
| `logs/iter-NNN/{stdout,stderr}.log` | 每轮 agent 的完整输出 |

`state.json` 是原子落盘的 (先写 `.tmp` 再 `rename`)。直接 `WriteFile` 会在"写到一半
断电"时留下截断的 JSON —— 而**读不出来**与"从没跑过"在下游是同一个分支, 断点恢复
会**静默**退化成重跑且不报错。

### 逐轮读数怎么读

```
逐轮读数:
  #1   验证=false 工作区变化=false 退出码=0   空转=是
  #2   验证=true  工作区变化=true  退出码=0   空转=否
```

- `验证` —— 这一轮**结束时**验证命令过没过 (本轮唯一成功判据)。
- `工作区变化` —— 指纹变没变。`false` 意味着 agent 一个字节没改。
- `退出码` —— agent 子进程的退出码。它**不是**判据: 退出码 0 而什么都没干, 是常态。
- `空转` —— `验证=false 且 工作区变化=false` 且验证输出也没变。连续这么多轮就止损。

---

## 5. 断点恢复

**恢复就是原样再跑一次同一条命令。** 没有单独的 `--resume`。

```bash
# 第一次 (中途被 Ctrl-C / OOM / kill -9 打断都算)
claude-go coder --goal-file goal.txt --verify "go test ./..." --workspace /tmp/demo

# 恢复: 一模一样的命令, 什么都不用加
claude-go coder --goal-file goal.txt --verify "go test ./..." --workspace /tmp/demo
```

启动时会打印 `[coder] 从断点恢复: 已完成 N 轮`, 逐轮读数里的历史是**累积**的 ——
被杀之前那几轮的读数原样保留, 接着往下编号。

判"接着跑"还是"从头跑"不能看轮次编号 (两者都是 #1,#2,#3), 要看:
`IterationRecord.Resumed` 标记 (只有本次进程跑的那几轮才是 `true`),
以及 agent 实际被调用的次数。

**目标变了等于换了任务**: `--goal` 与落盘状态不一致时会按新目标重开 (工作区代码
保留)。改了 `--goal` 又想接着以前的进度, 那不是恢复, 是换任务。

---

## 6. 把 agent 写在哪里: `PLAN.md` / `PROGRESS.md`

运行时会在 prompt 里要求 agent 维护这两份文件。它们不是装饰 —— 它们是**跨进程的
记忆**: 一个被 `kill -9` 的进程没有机会做任何交待, 下一个进程只能靠工作区里的
痕迹知道"上次做到哪了"。实测里的效果是 agent 会在 `PROGRESS.md` 写下"已知坑",
包括它踩过的报错和下一步该做什么, 恢复后确实按那份记录继续。

---

## 7. 常用参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--goal` / `--goal-file` | — | 任务目标。长目标用文件 |
| `--verify` | — | **必填**。完成判据命令, 退出码 0 = 完成 |
| `--workspace` | — | 工作目录 (agent 改这里, 验证也在这里跑) |
| `--model` | 配置默认 | 模型别名 |
| `--gateway` | — | LLM 网关地址 |
| `--config` | — | claude-go 配置文件 |
| `--allowed-tools` | `Read,Write,StrReplace,Bash,Glob,Grep` | 工具白名单。名字必须是注册表里的**真名** |
| `--max-iterations` | 20 | 最多迭代几轮 |
| `--max-turns` | 40 | 单轮 agent 的回合上限 (必须有限) |
| `--no-progress-limit` | 3 | 连续多少轮毫无改动就止损 |
| `--iteration-timeout` | 20m | 单轮墙钟上限 |
| `--verify-timeout` | 10m | 验证命令墙钟上限 |
| `--reset` | false | 丢弃已有状态从头开始 (工作区代码保留) |
| `--json` | false | 以 JSON 输出结果 |

工具白名单里的名字容易写错的两个: 是 **`StrReplace`** 不是 `Edit`, 是 **`Bash`**
不是 `Shell`。写错的名字会静默地从可见集里消失 —— 表现成"agent 什么都不做",
而没有任何报错。

---

## 8. 失败时会怎样

`--verify` 之外, 结束原因 (`Reason`) 有五种:

| Reason | 含义 | 该怎么办 |
|---|---|---|
| `done` | 通过, 且干净副本里也通过 | — |
| `no_progress` | 连续 N 轮工作区和验证输出都没变 | 看 `.coder/logs/` 里 agent 在干什么, 通常是 prompt 没说清或工具没给对 |
| `max_iterations` | 迭代额度用尽仍未通过 | 加额度, 或把任务拆小 |
| `verify_error` | **验证命令本身**跑不起来 (超时/找不到命令) | 这是环境问题, 不是代码问题。先修验证命令 |
| `clean_verify_failed` | 原目录过了, 但干净副本里没过 | 代码依赖了 `.coder/` 里的东西, 这个程序不能独立运行 |

`verify_error` 与 `no_progress` 必须分开: 把"环境坏了"归成"代码还没写好"会让运行时
拿着一条永远跑不完的验证命令, 每轮白烧一整轮额度, 最后报"没通过"而真凶是超时。

**单轮 agent 失败不致命**: agent 崩了 (SIGKILL / 非零退出) 只是这一轮失败, 下一轮会
带着原始报错重来。**驱动进程**被杀才是恢复路径。两者的正确反应完全不同。

---

## 9. 作为库使用

```go
import "github.com/anthropic/claude-go/pkg/coder"

r := &coder.Runner{
    Config: coder.Config{
        Goal:        "实现 Greet",
        Workspace:   "/tmp/demo",
        VerifyCmd:   []string{"go", "test", "./..."},
        MaxIterations: 20,
    },
    Agent:  &coder.ClaudeGoAgent{Bin: "/path/to/claude-go", Cwd: "/tmp/demo"},
    Verify: coder.CommandVerifier{Cmd: []string{"go", "test", "./..."}, Timeout: 10 * time.Minute},
}
res, err := r.Run(ctx)
```

`Agent` 与 `Verifier` 都是接口, 可以替换 —— 换一个 CLI、换一套验证方式都不必动驱动。

---

## 10. 设计上的几个取舍

**为什么不用 agent 的自述当判据。** 见 §1。这是全篇唯一不能被推翻的前提。

**为什么每轮重新测, 而不是缓存上轮结论。** 缓存会让"上一轮结束"与"这一轮开始"
之间发生的事情失明 —— 而那正是被杀掉时发生的事。

**为什么空转判据是两维的。** 只看指纹会漏掉"改了又改回去"; 只看验证输出会漏掉
"改了但报错一样"。二维都不动才算没干活。

**为什么验证输出要先归一化。** 测试输出带耗时 (`ok smoke 0.002s` / `FAIL\tsmoke\t
0.002s`), 每轮都不同。不抹掉耗时, "输出没变"永远为假, 止损形同虚设。注意要同时认
空格和**制表符** —— `go test` 的汇总行用的是 tab。

**为什么终态要在干净副本里验。** 原目录里有 `.coder/` (会话、日志、状态)。依赖它
的验证会假装通过。副本里只有 agent 实际留下的代码 —— 那才是"这个程序能独立运行"
的证据。

---

## 11. 已知边界

- **验证命令必须是确定性的。** 一个时快时慢、结果时对时错的验证命令会让空转判据
  反复抖动, 也会让终态验收不可复现。
- **agent 的回合上限必须有限。** `--max-turns` 是硬边界, 别设成 0 或负数期望
  "无限" —— 一个卡住的子进程会一直占着这一轮。
- **`.coder/` 不能被验证命令依赖。** 依赖了的话 `clean_verify_failed` 会把它抓出来。
- **工作区指纹会跳过 `.coder/`、`.claude-go/`、`.git/`、`node_modules/`、`vendor/`。**
  判据是"这个目录会不会在 agent 什么都没干的情况下也变" —— 会变的必须跳过, 否则
  "工作区动没动"恒为真, 止损永不触发。新增这类目录时要一起加进去。
