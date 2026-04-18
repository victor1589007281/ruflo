# Orchestrator V2 优化方案

> 基于 go-dev-768 运行分析 + 业界大模型调研 (2025-2026)
> 日期: 2026-04-18

## 1. 问题诊断

### 1.1 运行时间瓶颈分析

| 任务 | 耗时 | 最佳评分(C/Co/S/Q) | 编译 | 达标 | 问题 |
|:---|:---|:---|:---|:---|:---|
| 项目脚手架 | **16m8s** | 9/7/6/8 | ✅ | ✅ | 中等偏长, R4才达标 |
| WAL+mmap 封装 | **25m47s** | 6/7/7/6 | ✅ | ⚠️ | 过大, 低级基础设施难生成 |
| 内存KV (TTL+LRU) | **4m45s** | 8/8/8/6 | ✅ | ✅ | **R1即达标, 理想粒度** |
| 向量索引 HNSW | **24m11s** | 3/4/6/5 | ✅ | ❌ | 算法复杂度超出LLM能力 |
| 文件分块存储 | **21m51s** | 6/7/8/7 | ✅ | ✅ | 中等复杂 |
| 知识图谱 | **24m5s** | 8/7/7/8 | ✅ | ✅ | R5才稳定 |
| DSL 解析器 | **22m5s** | 5/6/8/8 | ✅ | ❌ | 正确性始终<6 |
| 计划器+执行器 | **27m36s** | 6/6/6/6 | ✅ | ✅ | 最长任务 |
| 集成测试 | **22m18s** | 8/8/8/7 | ✅ | ✅ | 测试设计文本为主 |
| 性能基准 | **20m44s** | 6/5/7/7 | ✅ | ❌ | 完整度不足 |

**关键发现:**
- 总耗时 **135min**, 仅内存KV(4m45s)在R1达标
- 8/10任务超20min — **粒度过大**
- 编译全部通过(L2硬门禁有效), 但5轮对抗占用大量时间
- HNSW、DSL两个任务从未达标 — **超出模型单轮能力**

### 1.2 与设计偏差

设计→实现对齐度: **~55-65%**

| 设计模块 | 实现状态 | 偏差原因 |
|:---|:---|:---|
| 六边形架构 | 接口层实现, 端口适配器部分 | 代码截断 |
| WAL 日志 | 骨架, 非生产级 | 底层OS操作难自动生成 |
| HNSW 索引 | 简化版, 非标准HNSW | 算法复杂度 |
| SQL-like DSL | Tokenizer+Parser部分 | 正确性始终不足 |
| 统一查询接口 | 计划器骨架 | 依赖前置模块 |
| HTTP/gRPC API | **缺失** | 未在WBS中 |

### 1.3 核心问题总结

```
P1: 任务粒度过大 → 单个Coder在5轮内无法收敛
P2: 对抗轮次固定5轮 → 简单任务浪费, 复杂任务不够
P3: 算法密集型任务(HNSW/DSL) → LLM单轮生成能力不足
P4: Go-only编译门禁 → 不支持C++/Rust等语言
P5: 无自适应粒度 → Planner一次性输出, 不能运行时拆分
P6: 无文件物化 → 代码只在REPORT.md中, 不在磁盘上
```

## 2. 业界大模型调研综述

### 2.1 主要模型代码生成策略对比

| 模型 | 核心策略 | 粒度 | 自修复 | 多Agent |
|:---|:---|:---|:---|:---|
| **Claude 4.6/4.7** | 长horizon agentic, 动态工具注册 | 文件级 | 工具反馈驱动 | Team Lead+Workers |
| **GLM 5.1** | Benchmark-driven路由, CodeGeeX4 | 函数级 | 执行反馈 | 单Agent |
| **Kimi K2** | 256K长上下文, MoE | 模块级 | Thinking模式 | 单Agent |
| **MiniMax M2.7** | MoE, 状态演化 | Patch级 | 假设→验证 | 单Agent |
| **DeepSeek V4** | FIM, Self-Debugging | 补全级 | 执行→解释→修复 | 单Agent |
| **Qwen 3.6** | Coder-Next, MCP集成 | 文件级 | MCP工具链 | Agent协作 |
| **GPT 5.6** | Codex演化, Compaction | 仓库级 | 操作+研究分离 | 多Worker |
| **Gemini Γ4** | 多模态证据, Code Assist | 文件级 | 截图+测试 | IDE集成 |

### 2.2 关键论文策略

| 方法 | 核心思想 | 应用到Orchestrator |
|:---|:---|:---|
| **SWE-agent** | LM友好命令接口+守护 | 标准化edit/test/build命令 |
| **Agentless** | 定位→修复→验证, 无Agent | 简单任务快速通道 |
| **MapCoder** | 检索/规划/编码/调试Agent | 角色图+回退到规划 |
| **Self-Debugging** | 执行→解释→修复 | 总是捕获stdout/stderr |
| **Reflexion** | 语言强化学习, 情景记忆 | 每任务持久化postmortem |
| **CodeAct** | 代码作为动作空间 | REPL驱动的工具编排 |
| **准确-纠正悖论** | 强模型不一定更好自纠正 | 交叉模型审查+静态检查 |
| **CodePlan** | 多步依赖编辑 | 编辑图(有序patch) |

### 2.3 关键洞察

1. **默认patch级, 按需升级** — Agentless+SWE-agent哲学
2. **验证驱动而非vibes驱动** — 外部验证器(test/typecheck/lint)
3. **粒度匹配风险** — 小patch+宽验证 > 大重写无门禁
4. **角色分离** — 作者 vs 评审 vs 测试, 准确-纠正悖论

## 3. 优化方案

### 3.1 L1: 自适应任务粒度 (核心改进)

**问题**: Planner一次性输出10个固定任务, 无法运行时调整。

**方案**: 引入 **Planner WBS 粒度策略** — 根据任务复杂度动态选择:

```
简单模块(如配置/工具) → 1个任务, 2轮对抗上限
中等模块(如存储引擎) → 接口+实现 2个任务, 3轮对抗
复杂模块(如HNSW/DSL) → 拆为3-5个子任务, 每个5轮对抗
```

**具体**: Planner输出任务时标注复杂度(simple/medium/complex), 
Orchestrator根据复杂度设置不同的 `AdversarialRound`。

**目标任务粒度**: 14-18个任务, 每个任务8-15min。

### 3.2 L2: 多语言编译门禁 (C++/Rust/Python支持)

**问题**: `runBuildCheck` 硬编码 `go build ./...`。

**方案**: 引入 **LanguageToolchain** 抽象:

```go
type LanguageToolchain struct {
    Language    string   // "go", "cpp", "rust", "python"
    BuildCmd    []string // ["go","build","./..."] / ["cargo","build"] / ["cmake","--build","build"]
    LintCmd     []string // ["go","vet","./..."] / ["cargo","clippy"] / ["cppcheck","src/"]
    TestCmd     []string // ["go","test","./..."] / ["cargo","test"] / ["ctest"]
    InitCmd     []string // ["go","mod","init"] / ["cargo","init"] / ["cmake","-B","build"]
    FileExt     string   // ".go", ".cpp", ".rs", ".py"
    ProjectFile string   // "go.mod", "Cargo.toml", "CMakeLists.txt"
}
```

### 3.3 L3: 早期终止 + 快速通道

**问题**: 简单任务也跑5轮对抗。

**方案**: 
- R1达标 → 立即终止(已有, 但需要更宽松的"快速通道"阈值)
- 连续2轮评审分数无改善 → 提前终止(max改进轮 = 3而非5)
- 复杂度="simple"的任务 → maxRounds=2

### 3.4 L4: 文件物化引擎

**问题**: 代码只存在于LLM输出(REPORT.md), 不在磁盘上。

**方案**: 从Coder输出中提取代码块, 写入工作区文件系统:
1. 解析 ```lang\n...``` 代码块
2. 匹配 `// File: path/to/file.go` 注释
3. 写入 `team.Cwd/` 目录
4. 执行 `buildCheck` 验证

### 3.5 L5: Planner prompt 优化 (粒度控制)

**问题**: Planner输出的WBS任务粒度不可控。

**方案**: 在Planner prompt中注入:
- 每个任务的**预估token上限**(8K tokens)
- **文件数上限**(每个任务最多3个文件)
- **复杂度标签**(simple/medium/complex)
- **验收标准必须包含编译命令**

## 4. 多语言支持评估

### 4.1 C++ 支持可行性

| 维度 | Go(当前) | C++ | 差异 |
|:---|:---|:---|:---|
| 编译命令 | `go build ./...` | `cmake --build build` | 需要CMakeLists.txt |
| 包管理 | `go.mod` | vcpkg/conan/CMake | 更复杂 |
| 项目文件 | `go.mod` | `CMakeLists.txt` | 需要初始化 |
| 编译速度 | 快 | 慢(头文件) | 对抗轮次受影响 |
| LLM生成质量 | 好 | 中等(模板/内存管理) | 更多编译错误 |
| Lint | `go vet` | `cppcheck`/`clang-tidy` | 工具需预装 |

**结论**: C++ 可支持, 但编译速度和LLM生成质量会影响效率。建议使用CMake+vcpkg。

### 4.2 Rust 支持可行性

| 维度 | Go(当前) | Rust | 差异 |
|:---|:---|:---|:---|
| 编译命令 | `go build ./...` | `cargo build` | 更统一 |
| 包管理 | `go.mod` | `Cargo.toml` | 更强大 |
| 项目文件 | `go.mod` | `Cargo.toml` | `cargo init` |
| 编译速度 | 快 | 慢(borrow checker) | 更多编译轮次 |
| LLM生成质量 | 好 | 中等(生命周期) | lifetime错误常见 |
| Lint | `go vet` | `cargo clippy` | 内置 |

**结论**: Rust 的 cargo 工具链统一度更高, 编译错误信息更友好, 但 borrow checker 和 lifetime 是LLM的难点。

### 4.3 100% 复现能力评估

| 能力 | Go | C++ | Rust |
|:---|:---|:---|:---|
| DAG编排 | ✅ | ✅ | ✅ |
| 对抗循环 | ✅ | ✅ | ✅ |
| 编译硬门禁 | ✅ | ✅ | ✅ |
| 上下文压缩 | ✅ | ✅ | ✅ |
| 重采样 | ✅ | ✅ | ✅ |
| best-of-N | ✅ | ✅ | ✅ |
| 文件物化 | ⚠️ | ✅ | ✅ |
| LLM代码质量 | 高 | 中 | 中 |
| 编译速度 | 快 | 慢 | 中 |
| **预期效率** | **100%** | **~70%** | **~75%** |

## 5. 实施计划

| 优先级 | 改动 | 文件 | 预估 |
|:---|:---|:---|:---|
| P0 | L2 多语言编译门禁 | workflow.go, orchestrator.go | 必须(启动C++/Rust前) |
| P0 | L4 文件物化引擎 | orchestrator.go (新函数) | 必须 |
| P1 | L1 自适应粒度 | orchestrator.go | 高收益 |
| P1 | L5 Planner prompt优化 | workflow.go | 高收益 |
| P2 | L3 早期终止优化 | orchestrator.go | 中等收益 |
