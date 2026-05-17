# 面向 Claude Client 的自适应代码智能方案

> **版本**: 2.0（Claude Client 适配版）
> **日期**: 2026-05-17
> **范围**: Claude Code / Claude Desktop 作为消费层，支持多仓库、多分支、自适应路由
> **目标**: 一键初始化、全局存储不污染仓库、三引擎自适应路由、Token 效率最大化

---

## 1. 核心问题与设计方案

### 1.1 用户提出的五个关键问题

| # | 问题 | 设计决策 |
|---|------|---------|
| 1 | **Claude Client 如何配置** | 单一 MCP Server (`claude-code-intel`) 统一暴露所有工具；Claude Code 通过 `~/.claude/mcp-config.json` 或项目级 `.claude/mcp.json` 配置一次即可 |
| 2 | **全量初始化操作流程** | `claude-code-intel init <repo-path>` 一键初始化：检测语言 → 生成分片配置 → 构建 GitNexus 索引 → 构建 Graphify 语义图 → 注册 MCP |
| 3 | **数据存储位置** | **全局缓存** (`~/.claude-code-intel/<repo-hash>/`) 存索引大数据；**项目元数据** (`<repo>/.claude-code-intel/config.yaml`) 存轻量配置；分支通过子目录隔离 |
| 4 | **GitNexus vs Graphify 定位** | GitNexus = **精确结构引擎**（调用链、类型层级、影响分析）；Graphify = **语义社区引擎**（模块聚类、宏观理解、异常发现）；两者是上下游关系而非竞争关系 |
| 5 | **自适应三引擎路由** | Claude 调用统一 `code_query` 工具，携带自然语言意图，内部自动路由到 GitNexus / Graphify / 原生工具；精度优先、Token 预算感知、自动降级 |

### 1.2 三引擎互补架构（最终版）

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        Claude Code / Claude Desktop                          │
│                          (消费层 — 用户交互)                                  │
└─────────────────────────────────────────────────────────────────────────────┘
                                    │
                        ┌───────────┴───────────┐
                        │   MCP 统一入口          │
                        │  claude-code-intel      │
                        │  (单一 Server)          │
                        └───────────┬───────────┘
                                    │
                        ┌───────────┴───────────┐
                        │    自适应查询路由器      │
                        │  (意图分类 + 预算检查)   │
                        └───────────┬───────────┘
                                    │
        ┌───────────────────────────┼───────────────────────────┐
        ▼                           ▼                           ▼
┌───────────────┐         ┌─────────────────┐         ┌───────────────┐
│   GitNexus    │         │   Graphify      │         │  原生工具兜底   │
│  (精确结构)   │         │  (语义社区)     │         │ (Grep/Glob)   │
│  ─────────    │         │  ─────────      │         │ ─────────     │
│  调用链/依赖  │◄───────►│  Leiden 聚类    │         │ 零成本启动    │
│  类型层级     │  Graph  │  语义摘要       │         │ 实时搜索      │
│  影响半径     │  输入   │  异常发现       │         │ 最终兜底      │
│  交叉引用     │         │  God Nodes      │         │               │
└───────┬───────┘         └────────┬────────┘         └───────┬───────┘
        │                          │                          │
        └──────────────────────────┼──────────────────────────┘
                                   │
                        ┌──────────┴──────────┐
                        │   统一存储层         │
                        │  ~/.claude-code-intel/...
                        │  (全局缓存 + 分支隔离)│
                        └─────────────────────┘
```

**关键设计原则**：
1. **单一 MCP Server**：Claude 只看到一组统一的 `code_*` 工具，不需要理解底层引擎差异
2. **存储外置**：所有索引数据存在用户家目录，源码仓库零污染；切换分支是 O(1) 的目录切换
3. **自适应路由**：不是让 Claude 选择"用 GitNexus 还是 Graphify"，而是 Claude 描述意图（"我想理解 TCP 模块"），路由器自动选择最佳引擎
4. **原生兜底**：任何情况下，如果索引缺失/过期/返回空，自动降级到 `grep`/`glob`，保证可用性

---

## 2. 三引擎定位与分工

### 2.1 GitNexus — 精确结构引擎（ Structural Index ）

**一句话定位**：代码的"精密地图"，回答 "X 在哪里、谁调用 X、修改 X 会影响什么"

| 维度 | 说明 |
|------|------|
| **输入** | 源码 AST（Tree-sitter）、Git 历史、文件依赖 |
| **输出** | 代码属性图：符号定义、调用链、类型继承、文件导入关系 |
| **存储** | SQLite / KuzuDB（本地嵌入式，无独立服务） |
| **构建成本** | 中等（百万行代码库 ~10-30 分钟全量） |
| **查询延迟** | < 50ms（热缓存） |
| **Token 效率** | 极高（返回符号+位置，不返内容） |

**典型查询**：
- `tcp_sendmsg` 的定义位置 + 所有调用方
- 修改 `buf_pool_t` 会影响哪些文件（影响半径）
- `struct file_operations` 的所有实现者

**不适用场景**：
- "TCP 子系统有哪些模块"（这是宏观语义问题，不是精确结构问题）
- "这段代码设计得是否合理"（需要语义理解，不是图遍历）

### 2.2 Graphify — 语义社区引擎（ Semantic Community ）

**一句话定位**：代码的"语义显微镜"，回答 "这个子系统由哪些社区组成、关键枢纽是谁、有没有架构异味"

| 维度 | 说明 |
|------|------|
| **输入** | GitNexus 的结构图 + **可选** LLM 语义增强 |
| **输出** | Leiden 社区聚类、高度数节点（God Nodes）、跨社区异常边、**可选** 自然语言摘要 |
| **存储** | NetworkX 内存图 + JSON 快照（按需加载） |
| **构建成本** | **轻量模式**：低（纯算法，百万行 ~5-10 分钟，**零 LLM Token**）；**增强模式**：中（仅 Top 节点调 LLM，成本可控） |
| **查询延迟** | < 200ms（图已加载） |
| **Token 效率** | 高（返回社区摘要，而非原始代码） |

#### 关键澄清：Graphify 的核心不依赖 LLM

**Graphify 的价值分为两层**：

| 层级 | 功能 | 是否依赖 LLM | 构建成本 | 必要性 |
|------|------|-------------|---------|--------|
| **结构层**（核心） | Leiden 社区检测、God Nodes 排名、异常边检测、跨社区调用统计 | **否**，纯图算法 | 零 Token | **必须有** |
| **语义层**（增强） | 函数自然语言摘要、社区职责描述、语义搜索 | **是**，需 LLM 调用 | 有 Token 消耗 | **可选** |

**结构层已经能解决 80% 的查询**：
- "TCP 子系统有哪些模块" → Leiden 社区检测直接给出（零 LLM）
- "哪些函数是核心枢纽" → God Nodes 按度数排名（零 LLM）
- "有没有跨层违规调用" → 比较社区归属和实际调用边（零 LLM）

**语义层的 LLM Token 控制策略**：
1. **分层提取**：只给 Top-10% God Nodes 生成 LLM 摘要，不是每个函数都调
2. **本地嵌入替代**：用本地 CodeBERT/GraphCodeBERT 生成语义向量，做自然语言到代码的匹配，不调用远程 LLM
3. **增量更新**：首次构建时 LLM 调用量最大，后续只更新变更节点的语义
4. **可跳过**：`init --light` 模式完全跳过 LLM 层，结构层立即可用

**典型查询**：
- TCP 子系统包含哪些功能社区（拥塞控制、连接管理、数据路径）—— **零 LLM**
- `net/` 分片中的 Top-10 关键函数（God Nodes）—— **零 LLM**
- 发现 `tcp_bbr.c` unexpectedly 调用了 `crypto/` 的函数（架构异味）—— **零 LLM**
- 用自然语言问 "内存分配相关逻辑集中在哪些文件" —— **轻量模式**：基于注释/God Nodes 启发式匹配；**增强模式**：LLM 语义搜索

**不适用场景**：
- "第 47 行的变量 `ret` 在哪里被使用"（太细粒度，用 GitNexus 或 Grep）
- 需要 100% 精确的调用链（Leiden 是概率聚类，可能有边界误差）

### 2.3 原生工具 — 实时兜底引擎（ Real-time Fallback ）

**一句话定位**：零索引成本的"瑞士军刀"，永远可用，用于冷启动、未索引文件、精确字符串匹配

| 维度 | 说明 |
|------|------|
| **输入** | 实时文件系统 |
| **输出** | 原始文本匹配结果 |
| **存储** | 无 |
| **构建成本** | 零 |
| **查询延迟** | 10ms-1s（视仓库大小） |
| **Token 效率** | 低（返回大量原始文本） |

**典型查询**：
- 查找 TODO/FIXME 注释
- 搜索特定的错误信息字符串
- 未索引文件的探索
- 正则模式匹配

### 2.4 配合关系：上下游流水线

```
源码仓库
    │
    ├────► Tree-sitter 解析 ──► GitNexus 结构图 ──► SQLite/KuzuDB
    │                              │
    │                              ├────► 精确查询 (navigate/impact/find_refs)
    │                              │
    │                              ▼
    │                         Graphify 语义提取
    │                         (LLM + Leiden 聚类)
    │                              │
    │                              ├────► 社区摘要 (communities/god_nodes)
    │                              ├────► 异常发现 (surprises)
    │                              └────► 语义搜索 (semantic)
    │
    └────► 原生 Grep/Glob ◄──── 兜底/冷启动/实时
```

**关键洞察**：GitNexus 和 Graphify 不是二选一，而是**流水线关系**。
- GitNexus 提供"骨架"（结构图）
- Graphify 在骨架上做"社区划分"（Leiden 聚类、God Nodes、异常边检测）—— 这一步**零 LLM Token**
- Graphify **可选地**在骨架上附加"语义血肉"（LLM 生成摘要）—— 这一步有 Token 成本，但可跳过
- **没有 GitNexus 的结构图，Graphify 无法做社区检测**（社区检测需要图输入）；没有 Graphify，GitNexus 的图只能回答精确问题，无法提供宏观理解。

**LLM Token 消耗的真相**：
- 百万行代码库全量构建 Graphify **轻量模式**：0 LLM Token（纯算法）
- 百万行代码库全量构建 Graphify **增强模式**：约 500-2000 次 LLM 调用（仅 Top God Nodes + 社区摘要），成本可控
- **日常增量更新**：只重新提取变更文件的语义，通常 0-10 次 LLM 调用

---

## 3. 存储架构：全局缓存 + 项目元数据 + 分支隔离

### 3.1 为什么数据不能存到源码目录下

| 方案 | 优点 | 缺点 | 结论 |
|------|------|------|------|
| **源码目录内** (`<repo>/.gitnexus/`) | 跟仓库走，克隆即带索引 | 索引文件巨大（GB级），污染 git；多人协作时索引冲突；分支切换需重建 | ❌ 不可行 |
| **全局存储** (`~/.claude-code-intel/`) | 不污染仓库；多仓库隔离；分支 O(1) 切换；可被系统清理 | 需要 Repo → 存储路径的映射 | ✅ **选定** |
| **混合**（全局存数据 + 源码存配置） | 结合两者优点 | 稍复杂 | ✅ **最终方案** |

### 3.2 存储路径设计

```
~/.claude-code-intel/
├── repos.json                    # 仓库注册表: repo-path → repo-hash 映射
│
├── <repo-hash-1>/                # 例: a3f7d2e (linux kernel)
│   ├── branches.json             # 分支列表 + 当前活跃分支
│   ├── main/                     # main 分支索引
│   │   ├── gitnexus/
│   │   │   ├── symbols.db        # SQLite: 符号表
│   │   │   ├── calls.db          # KuzuDB: 调用关系图
│   │   │   └── manifest.json     # 元数据（构建时间、commit hash、文件数）
│   │   ├── graphify/
│   │   │   ├── communities.json  # Leiden 社区结果
│   │   │   ├── god_nodes.json    # 高度数节点排名
│   │   │   ├── semantic_map.json # 语义标签映射
│   │   │   └── surprises.json    # 异常跨社区边
│   │   └── cross_shard/
│   │       └── edges.kuzu        # 跨分片边（大型仓库）
│   │
│   ├── feature/tcp-optim/        # feature 分支 —— 增量覆盖层
│   │   ├── gitnexus/
│   │   │   └── delta.db          # 仅变更的符号和边
│   │   ├── graphify/
│   │   │   └── delta.json        # 仅变更的社区
│   │   └── parent                # 符号链接: -> ../main
│   │
│   └── v6.12-rc1/                # tag 也可作为"分支"索引
│       └── ...
│
├── <repo-hash-2>/                # 例: b8e1a4c (mysql)
│   └── ...
│
└── cache/
    ├── file_hashes/              # XXH3 内容寻址缓存（跨仓库去重）
    └── embeddings/               # CodeBERT 向量缓存
```

### 3.3 项目级元数据（轻量配置）

源码目录下仅存放一个 `.claude-code-intel/config.yaml`，**可选地被 git 跟踪**（推荐加入 `.gitignore`）：

```yaml
# <repo>/.claude-code-intel/config.yaml
# 此文件定义了该仓库的代码智能配置，索引数据本身不在这里

repo_name: "linux-kernel"
repo_hash: "a3f7d2e9b1c5..."  # 由 init 命令自动生成

# 分片策略（大型仓库需要）
shards:
  - name: "net"
    roots: ["net/", "include/net/"]
    max_files: 5000
  - name: "mm"
    roots: ["mm/", "include/linux/mm.h"]
    max_files: 3000
  - name: "kernel"
    roots: ["kernel/", "include/linux/sched.h"]
    max_files: 4000

# 排除规则
exclude:
  - "*.S"          # 内联汇编跳过
  - "tools/"       # 构建工具不索引
  - "Documentation/"

# 构建配置
build:
  gitnexus:
    parser: "tree-sitter"
    storage: "sqlite"       # sqlite | kuzudb
  graphify:
    leiden_resolution: 0.8
    llm_model: "claude-sonnet-4"
    semantic_extraction: true

# 自动更新触发器
auto_update:
  git_hooks: true           # 安装 post-commit / post-checkout hooks
  watch_mode: false         # 文件系统 watch（高内存消耗）
  stale_threshold_hours: 24 # 超过 24h 标记为过期
```

### 3.4 分支隔离策略：写时复制（CoW）

**核心问题**：切换分支后，索引是否应该重建？

**方案**：
1. **main/master 分支**：全量索引，作为"基础层"
2. **其他分支**：只存增量（delta），读取时合并基础层 + 增量
3. **快进（Fast-forward）**：应用 git diff，只重新解析变更文件
4. **变基/非快进**：从最近共同祖先（merge-base）重新应用补丁
5. **首次切换新分支**：自动基于当前分支 fork 一份增量层

```go
// 伪代码: 分支读取路径
func loadGraph(repoHash, branch string) Graph {
    base := loadBaseGraph(repoHash, "main")
    if branch == "main" {
        return base
    }
    delta := loadDelta(repoHash, branch)
    return merge(base, delta)  // 运行时合并，O(变更文件数)
}

// 存储优化: 内容寻址去重
// 两个分支中完全相同的 AST 节点，在 SQLite 中只存一份
```

**存储成本**：
- 一个 feature 分支通常只修改 1%-5% 的文件，增量层仅占用基础层的 1%-5%
- 100 个 feature 分支的存储成本 ≈ 1 个全量 + 5 个增量 ≈ 1.2 个全量

---

## 4. Claude Client 配置方案

### 4.1 Claude Code 的 MCP 配置

Claude Code 支持两种 MCP 配置方式：

**方式 A: 全局配置（推荐）**

```json
// ~/.claude/mcp-config.json
{
  "mcpServers": {
    "code-intel": {
      "command": "claude-code-intel",
      "args": ["mcp"],
      "env": {
        "CODE_INTEL_DATA_DIR": "~/.claude-code-intel",
        "CODE_INTEL_LOG_LEVEL": "info"
      }
    }
  }
}
```

**方式 B: 项目级配置**

```json
// <repo>/.claude/mcp.json
{
  "mcpServers": {
    "code-intel": {
      "command": "claude-code-intel",
      "args": ["mcp", "--project", "."],
      "env": {
        "CODE_INTEL_CONFIG": ".claude-code-intel/config.yaml"
      }
    }
  }
}
```

**Claude Desktop** 在 `claude_desktop_config.json` 中同理配置。

### 4.2 MCP 暴露的统一工具集

Claude 不需要知道底层是 GitNexus 还是 Graphify，只调用以下统一工具：

#### 核心查询工具

| 工具名 | 输入 | 输出 | 内部路由 |
|--------|------|------|---------|
| `code_navigate` | `symbol`: 符号名<br>`direction`: "callers" / "callees" / "both"<br>`depth`: 1-3 | 定义位置 + 调用链 | **GitNexus** |
| `code_impact` | `file_path`: 文件路径<br>`transitive`: bool | 直接影响文件列表 + 传递影响文件列表 | **GitNexus** + Graphify surprises |
| `code_communities` | `scope`: "repo" / "shard" / "file"<br>`shard`: 分片名（可选） | 社区列表 + 核心节点 + 职责摘要 | **Graphify** |
| `code_god_nodes` | `shard`: 分片名<br>`top_n`: 10-50 | 高度数节点排名 + 职责说明 | **Graphify** |
| `code_semantic` | `query`: 自然语言查询<br>`top_k`: 5-20 | 相关符号/文件列表 + 相关性分数 | **Graphify** + 向量检索 |
| `code_surprises` | `shard`: 分片名 | 异常跨社区调用 + 架构异味描述 | **Graphify** |
| `code_status` | 无 | 索引状态（新鲜度、覆盖率、上次构建时间） | 元数据查询 |

#### 原生兜底工具（与现有工具共存）

| 工具名 | 说明 |
|--------|------|
| `code_grep` | 增强版 Grep，返回文件路径 + 行号 + 1 行上下文（不返完整内容） |
| `code_glob` | 增强版 Glob，支持按社区过滤（如 "只返回 net/ 社区内的文件"） |

### 4.3 系统提示词注入（自动上下文增强）

MCP Server 可以在 `code_navigate` 等工具调用时，自动在返回结果中附加社区上下文：

```json
// code_navigate 返回示例
{
  "symbol": "tcp_sendmsg",
  "definition": {"file": "net/ipv4/tcp.c", "line": 842},
  "callers": [...],
  "callees": [...],
  "_context": {
    "community": "TCP Data Path",
    "community_description": "负责 TCP 数据包的发送和接收路径，包括窗口管理、拥塞控制交互",
    "god_node_rank": 3,
    "cross_shard_calls": ["kmalloc (mm)", "spin_lock (kernel)"]
  }
}
```

Claude 在收到结果后，不需要额外调用 `code_communities`，就已经知道 `tcp_sendmsg` 属于"TCP Data Path"社区，且是第 3 重要的节点。

---

## 5. 全量初始化操作流程

### 5.1 一键初始化命令

```bash
# 安装（一次）
pip install claude-code-intel

# 初始化仓库（每个仓库执行一次）
cd /path/to/linux
claude-code-intel init

# 输出:
# [INFO] Detected C codebase, ~3.2M lines, 52K files
# [INFO] Generated shard config: arch, kernel, mm, net, fs, drivers
# [INFO] Building GitNexus index for shard: net (4,231 files)...
# [INFO] Building GitNexus index for shard: mm (2,891 files)...
# ...
# [INFO] Building Graphify semantic map...
# [INFO] Detected 47 communities in net/, 32 in mm/, ...
# [INFO] Index stored at: ~/.claude-code-intel/a3f7d2e/main/
# [INFO] MCP config snippet:
# {
#   "mcpServers": {
#     "code-intel": {
#       "command": "claude-code-intel",
#       "args": ["mcp", "--project", "/path/to/linux"]
#     }
#   }
# }
# [INFO] Run 'claude-code-intel doctor' to verify
```

### 5.2 初始化内部流程

```
init 命令执行流程:
│
├─ 1. 探测阶段 (Detect)
│   ├─ 检测语言分布 (C/C++/Go/Rust/...)
│   ├─ 统计代码行数、文件数
│   ├─ 检测 .git 存在性、当前分支
│   └─ 生成 repo-hash (基于 remote URL + 绝对路径)
│
├─ 2. 配置阶段 (Configure)
│   ├─ 自动生成分片策略（按目录结构启发式）
│   ├─ 生成 .claude-code-intel/config.yaml
│   └─ 询问用户确认/调整分片
│
├─ 3. 结构索引阶段 (GitNexus)
│   ├─ 并行构建各分片的 AST
│   ├─ 提取符号表、调用链、类型层级
│   ├─ 写入 SQLite/KuzuDB
│   └─ 提取跨分片边
│
├─ 4. 语义索引阶段 (Graphify)
│   ├─ 基于 GitNexus 图加载各分片
│   ├─ Leiden 社区检测（纯算法，零 LLM Token）
│   ├─ 计算 God Nodes、异常边（纯算法，零 LLM Token）
│   ├─ 【可选】LLM 语义提取：仅 Top God Nodes + 社区边界
│   │   → 轻量模式 (--light)：跳过此步，结构层立即可用
│   │   → 增强模式 (默认)：生成语义摘要，提升自然语言查询效果
│   └─ 写入 JSON 快照
│
├─ 5. 注册阶段 (Register)
│   ├─ 可选: 安装 git hooks (post-commit, post-checkout)
│   ├─ 可选: 注册到 ~/.claude/mcp-config.json
│   └─ 生成 CLAUDE.md 上下文摘要
│
└─ 6. 验证阶段 (Verify)
    ├─ 执行 smoke test 查询
    ├─ 检查索引覆盖率
    └─ 输出状态报告
```

### 5.3 增量更新（日常维护）

```bash
# 手动触发增量更新
cd /path/to/linux
git checkout feature/new-tcp-algo
claude-code-intel sync

# 输出:
# [INFO] Switched to branch: feature/new-tcp-algo
# [INFO] Base: main (a3f7d2e), Delta from: e4c8b91..f2d9a34
# [INFO] Changed files: 12 (net/ipv4/tcp_bbr.c, include/net/tcp.h, ...)
# [INFO] Re-indexing changed files...
# [INFO] Incremental update complete in 4.2s
```

**自动触发方式**：
1. **Git Hooks**（推荐）：安装 `post-commit`、`post-checkout`、`post-merge` hooks，自动调用 `claude-code-intel sync --background`
2. **Watch 模式**：`claude-code-intel watch` 监控文件系统变化（适合活跃开发，内存占用较高）
3. **惰性更新**：查询时检测索引新鲜度，若过期自动触发增量更新（查询延迟 +2-5s）

---

## 6. 自适应查询路由（核心创新）

### 6.1 为什么需要自适应路由

**Claude 不应该做这些决策**：
- ❌ "我应该用 GitNexus 还是 Graphify？"
- ❌ "这个仓库有没有索引？"
- ❌ "这个查询会消耗多少 Token？"

**Claude 应该只说**：
- ✅ "我想知道 `tcp_sendmsg` 的调用方"
- ✅ "TCP 子系统有哪些核心模块？"
- ✅ "修改这个文件会影响什么？"

### 6.2 意图分类器

`code_query` 工具内部的路由逻辑：

```python
def route_query(intent: str, query: str, context: dict) -> EngineChoice:
    """
    意图 → 引擎路由决策
    """

    # 精确导航类意图 → GitNexus
    if intent in ["find_definition", "find_callers", "find_callees",
                  "type_hierarchy", "find_refs", "cross_reference"]:
        return EngineChoice.GITNEXUS

    # 影响分析类意图 → GitNexus + Graphify
    if intent in ["impact_analysis", "what_breaks", "dependency_check"]:
        return EngineChoice.HYBRID_GITNEXUS_FIRST

    # 宏观理解类意图 → Graphify
    if intent in ["understand_module", "community_overview", "architecture",
                  "onboarding", "key_components"]:
        return EngineChoice.GRAPHIFY

    # 异常发现类意图 → Graphify
    if intent in ["find_anomaly", "architecture_smell", "cross_layer_violation"]:
        return EngineChoice.GRAPHIFY

    # 语义搜索类意图 → Graphify + 向量
    if intent in ["semantic_search", "find_logic", "where_is_feature"]:
        return EngineChoice.GRAPHIFY_SEMANTIC

    # 文本搜索类意图 → 原生 Grep
    if intent in ["text_search", "find_string", "regex_search", "grep"]:
        return EngineChoice.NATIVE_GREP

    # 兜底：如果索引存在且新鲜，优先 GitNexus；否则原生
    if index_is_fresh():
        return EngineChoice.GITNEXUS
    return EngineChoice.NATIVE_GREP
```

### 6.3 Token 预算感知

每个查询在执行前预估 Token 成本，超出预算时自动剪枝或拒绝：

```go
type QueryBudget struct {
    MaxTokens   int     // 默认 800
    MaxDepth    int     // 调用图深度，默认 2
    MaxResults  int     // 返回结果数，默认 20
}

func executeWithBudget(query Query, budget QueryBudget) Result {
    // 1. 预估成本
    estimated := estimator.Estimate(query)
    if estimated > budget.MaxTokens {
        // 自动剪枝：降低深度、限制结果数
        query = prune(query, budget)
    }

    // 2. 执行查询
    result := execute(query)

    // 3. 后处理剪枝
    result = contextPrune(result, budget)
    return result
}
```

### 6.4 降级策略

```
查询执行链:
│
├─ 1. 检查索引新鲜度
│   └─ 过期? → 触发后台增量更新 + 继续（使用旧索引）
│
├─ 2. 执行首选引擎查询
│   └─ 返回空? → 尝试备用引擎
│       ├─ GitNexus 空 → 降级到 Graphify 语义搜索
│       ├─ Graphify 空 → 降级到向量搜索
│       └─ 全部空 → 降级到原生 Grep
│
├─ 3. 检查 Token 预算
│   └─ 超出? → 结果剪枝（限制深度/行数/结果数）
│
└─ 4. 返回结果 + 元数据（使用了哪个引擎、置信度、是否降级）
```

---

### 6.5 混合查询准确性保障（核心机制）

上一节的"降级策略"是**单引擎的备用切换**，但真正的准确性保障需要**多引擎协同验证**。

#### 核心原则：图谱导航 + 原生验证

| 引擎角色 | 职责 | 准确性 |
|---------|------|--------|
| **GitNexus / Graphify** | **导航/推荐**：快速缩小搜索范围，指出"应该看哪里" | 高概率正确，但可能因索引过期而偏差 |
| **Grep** | **存在性验证**：确认图谱返回的符号/文件在当前代码中确实存在 | 100% 准确（实时文件系统） |
| **Read** | **事实确认**：读取精确行号周围的上下文，作为最终事实来源 | 100% 准确（直接读取源码） |

**关键认知**：
- 图谱查询的**召回率**风险（索引过期导致遗漏新符号）> **精确率**风险（返回错误位置）
- Grep 的**召回率**高（不漏匹配），但**精确率**低（大量噪音）
- **组合使用**：图谱提供高精确率的候选集，Grep 兜底召回率，Read 确认最终事实

#### 索引新鲜度检查

每次查询前，MCP Server 自动检查索引新鲜度：

```go
type FreshnessCheck struct {
    IndexedCommit   string    // 索引构建时的 commit hash
    CurrentCommit   string    // 当前 HEAD commit hash
    StaleFiles      []string  // 自索引以来变更的文件列表（git diff）
    StaleThreshold  int       // 容忍阈值：变更文件数 > 10 视为"过期"
}

func checkFreshness(repoHash, branch string) FreshnessLevel {
    diff := gitDiff(indexedCommit, currentCommit)
    if len(diff) == 0 {
        return FRESH       // 完全新鲜，直接信任图谱结果
    }
    if len(diff) <= 10 {
        return MOSTLY_FRESH // 少量变更，图谱结果可信，附加变更文件列表
    }
    if len(diff) <= 100 {
        return STALE       // 明显过期，返回图谱结果 + 过期警告 + 建议验证
    }
    return VERY_STALE      // 严重过期，仅将图谱作为"参考方向"，强制原生验证
}
```

#### 三引擎协同工作流（以 "tcp_sendmsg 的调用方" 为例）

```
Step 1: GitNexus 提供候选集（高效、可能遗漏最新变更）
─────────────────────────────────────────
→ code_navigate(symbol="tcp_sendmsg", direction="callers", depth=2)
→ 返回 5 个调用方候选：
    [inet_sendmsg@af_inet.c:800, sock_sendmsg@socket.c:654,
     tcp_sendmsg_nocheck@tcp.c:900, ...]
Token: ~150，延迟: ~50ms

Step 2: Grep 验证存在性（零遗漏、确认召回率）
─────────────────────────────────────────
→ 对每个候选文件执行：grep -n "tcp_sendmsg" <file> | head -5
→ 验证结果：
    ✓ af_inet.c:800   — 存在
    ✓ socket.c:654    — 存在
    ✗ tcp.c:900       — 不存在（函数已被重构删除）
    ✓ tcp_ipv4.c:432  — 存在（GitNexus 遗漏，Grep 发现新增调用）
Token: ~100，延迟: ~200ms

Step 3: Read 精确读取（最终事实来源）
─────────────────────────────────────────
→ 对验证通过的每个调用方，Read 调用点周围 ±5 行
→ Read af_inet.c offset=795 limit=12
→ Read socket.c offset=649 limit=12
→ Read tcp_ipv4.c offset=427 limit=12
Token: ~180，延迟: ~100ms

Step 4: 聚合返回
─────────────────────────────────────────
→ 返回给 Claude 的结果：
    - 确认的调用方（带精确上下文）
    - 图谱遗漏但 Grep 发现的新调用方
    - 图谱过期的条目（已标记删除）
    - _freshness: "MOSTLY_FRESH"（变更文件: 3个）

Total: ~430 Token（vs 纯 Read 读 3 个完整文件的 ~3600 Token，8.4x 节省）
```

#### 不同查询类型的混合策略

| 查询类型 | GitNexus 作用 | Grep 验证策略 | Read 策略 | 准确性保障 |
|----------|--------------|--------------|-----------|-----------|
| **定义位置查询** | 返回精确行号 | `grep -n "symbol" file` 验证行号 | 读取该行 ±3 行确认 | **100%**（Read 是事实来源） |
| **调用链查询** | 返回候选调用方列表 | 对每个候选文件 grep 验证存在性 | 读取每个验证通过的调用点 ±5 行 | **~98%**（Grep 兜底遗漏，Read 确认） |
| **影响分析** | 返回影响半径文件列表 | 对边界文件 grep 验证关键符号 | 读取 Top-5 影响文件的入口点 | **~95%**（边界可能因增量更新遗漏） |
| **社区理解** | 返回社区划分 + God Nodes | 无需验证（结构属性，非事实断言） | 按需 Read 社区代表文件 | **N/A**（宏观理解，无精确性要求） |
| **语义搜索** | 返回候选符号/文件 | grep 验证查询关键词在候选中存在 | Read 最相关的 1-2 个文件 | **~90%**（语义匹配本身有概率性） |

#### 为什么这个混合模式不会降低准确性

**纯原生工具的问题**：
- `grep "tcp_sendmsg"` → 200 个匹配，Claude 需要读 10+ 个文件筛选 → **高 Token、高噪音**
- 人/模型容易遗漏（200 个匹配看不全）→ **召回率实际更低**

**纯图谱工具的问题**：
- 索引过期 → 返回已删除的函数位置 → **精确率下降**
- 新增函数未索引 → 遗漏调用方 → **召回率下降**

**混合模式的优势**：
- 图谱把 200 个匹配筛成 5 个高置信候选 → **Token 节省 95%**
- Grep 验证确保 5 个候选都是真实存在的 → **精确率 100%**
- Grep 同时扫描发现图谱遗漏的新调用 → **召回率不低于纯 Grep**
- Read 只读验证通过的精确位置 → **最终事实 100% 准确**

#### 乐观读取模式（默认）vs 严格验证模式

```yaml
# .claude-code-intel/config.yaml
accuracy_mode: "optimistic"  # optimistic | strict

# optimistic（默认，推荐）：
#   - 索引 FRESH 时：直接返回图谱结果，不做 Grep/Read 验证
#   - 索引 MOSTLY_FRESH 时：返回图谱结果 + 过期警告，Claude 自行决定是否验证
#   - 索引 STALE 时：自动触发 Grep 验证
#   适用：日常开发，索引通过 git hooks 保持新鲜

# strict：
#   - 所有精确导航查询都走 "GitNexus → Grep → Read" 完整验证链
#   - Token 成本 +30%，但绝对准确性最高
#   适用：代码审查、安全审计、需要 100% 准确的场景
```

### 6.6 查询示例与路由决策

| 用户意图 | 路由 | 工具 | Token 成本 | 效率提升 |
|----------|------|------|-----------|---------|
| "`tcp_sendmsg` 在哪里定义" | **GitNexus** | `code_navigate` | ~150 | 30x vs Grep+Read |
| "修改 VFS 会影响什么" | **GitNexus → Graphify** | `code_impact` + `code_surprises` | ~380 | 8x vs 手工 grep |
| "TCP 子系统有哪些模块" | **Graphify** | `code_communities` | ~120 | 40x vs 读目录+文件 |
| "找出所有内存分配调用" | **GitNexus 跨分片** | `code_navigate` (cross-shard) | ~250 | 20x vs grep |
| "这段认证逻辑合理吗" | **Graphify 语义** | `code_semantic` + `code_surprises` | ~500 | 新增能力 |
| "搜所有 TODO" | **原生 Grep** | `code_grep` | ~200 | 1x（索引无优势） |
| "未索引文件里的内容" | **原生 Read** | `Read` (现有工具) | 视文件大小 | 1x |

---

## 7. 多仓库管理

### 7.1 仓库注册表

```json
// ~/.claude-code-intel/repos.json
{
  "repos": {
    "a3f7d2e9": {
      "path": "/home/victor/base/git/linux",
      "name": "linux",
      "remote_url": "https://github.com/torvalds/linux.git",
      "default_branch": "main",
      "current_branch": "feature/tcp-optim",
      "last_indexed": "2026-05-17T08:30:00Z",
      "size_mb": 11500,
      "shards": ["arch", "kernel", "mm", "net", "fs", "drivers"]
    },
    "b8e1a4c2": {
      "path": "/home/victor/base/git/mysql-server",
      "name": "mysql",
      "remote_url": "https://github.com/mysql/mysql-server.git",
      "default_branch": "main",
      "current_branch": "main",
      "last_indexed": "2026-05-16T22:00:00Z",
      "size_mb": 3200,
      "shards": ["sql", "innodb", "myisam", "replication"]
    }
  }
}
```

### 7.2 跨仓库查询（高级场景）

```bash
# 查询: "Linux 的 TCP 实现和 MySQL 的网络层有什么相似之处"
# 这需要同时加载两个仓库的 Graphify 结果，做跨仓库语义对比

code_cross_repo --repos linux,mysql --query "network layer architecture comparison"
```

**实现方式**：
- 每个仓库独立索引，Graphify 的社区摘要用统一 schema
- 跨仓库查询时，加载各仓库的 `communities.json`，用 LLM 做对比分析
- 不涉及跨仓库的精确图遍历（无意义），只做语义层对比

---

## 8. 与现有 Claude Code 工作流的融合

### 8.1 不是替代现有工具，而是增强

| 现有工具 | 何时仍用现有工具 | 何时用 code-intel |
|----------|----------------|------------------|
| `Read` | 读取特定文件的特定行；未索引文件 | 读取前先用 `code_navigate` 定位精确行号 |
| `Grep` | 快速文本搜索；正则匹配；临时查询 | 结构性查询用 `code_navigate`；语义查询用 `code_semantic` |
| `Glob` | 目录枚举；找配置文件 | 按社区过滤的文件枚举用 `code_glob` |
| `Bash` | 编译、测试、git 操作 | 索引进度监控用 `code_status` |

### 8.2 典型工作流对比

**Before（纯原生工具）**：
```
User: "TCP 拥塞控制是怎么实现的？"
Claude:
  1. Glob net/ipv4/tcp* → 20 个文件
  2. Grep "congestion" → 200 个匹配
  3. Read tcp_cong.c → 350 行
  4. Read tcp_bbr.c → 420 行
  5. Grep "struct tcp_congestion_ops" → 12 个匹配
  6. Read include/net/tcp.h → 80 行
Total: ~4,800 Token，6 步交互
```

**After（自适应 code-intel，乐观模式）**：
```
User: "TCP 拥塞控制是怎么实现的？"
Claude:
  1. code_communities shard="net" → 返回"Congestion Control"社区
     Token: ~120，延迟: ~150ms

  2. code_navigate symbol="tcp_congestion_control" depth=2
     → 定义 + 调用方 + 被调用方（精确位置）
     _freshness: FRESH（索引完全新鲜，无需验证）
     Token: ~150，延迟: ~50ms

  3. Read 3 个文件的特定行（80 行）
     Token: ~240
Total: ~510 Token，3 步交互，9.4x 节省
```

**After（自适应 code-intel，严格模式 / 索引过期时）**：
```
User: "tcp_sendmsg 有哪些调用方？"
Claude:
  1. code_navigate symbol="tcp_sendmsg" direction="callers" depth=2
     → 返回 5 个候选调用方 + 位置
     _freshness: MOSTLY_FRESH（3 个文件自索引后变更）
     Token: ~150，延迟: ~50ms

  2. code_grep pattern="tcp_sendmsg" files=[候选文件列表]
     → 验证每个候选是否存在 + 发现 1 个新增调用方
     → 过滤掉 1 个已删除的候选
     Token: ~100，延迟: ~200ms

  3. Read 验证通过的 4 个调用点（每个 ±5 行，共 ~60 行）
     Token: ~180，延迟: ~150ms

  4. 返回聚合结果：确认的 4 个调用方 + 上下文 + 1 个新增调用方
     Token: ~50（结果格式化）
Total: ~480 Token，4 步交互，准确性 100%

对比纯 Grep 方案：
  grep "tcp_sendmsg" → 200 个匹配 → Read 10+ 文件筛选 → ~3000 Token
对比纯图谱方案（无验证）：
  可能返回已删除的调用方，遗漏新增的调用方
```

---

## 9. 实施路线图

### Phase 1: 单仓库验证（2 周）
- [ ] 实现 `claude-code-intel init/sync` CLI
- [ ] 集成 Tree-sitter + SQLite（GitNexus 核心）
- [ ] 集成 Leiden 社区检测 + God Nodes（Graphify 轻量核心，零 LLM）
- [ ] 集成可选 LLM 语义增强层（Graphify 增强模式）
- [ ] 实现统一 MCP Server + `code_navigate` / `code_communities`
- [ ] 在一个中型仓库（如 claude-go 自身）验证 Token 节省率

### Phase 2: 大型仓库 + 分片（2 周）
- [ ] 实现分片构建流水线
- [ ] 在 Linux Kernel / MySQL 上验证
- [ ] 实现跨分片边提取
- [ ] 实现分支增量更新（CoW）

### Phase 3: 自适应路由 + 降级（1 周）
- [ ] 实现意图分类器
- [ ] 实现 Token 预算感知
- [ ] 实现自动降级链
- [ ] A/B 测试：对比纯原生工具 vs code-intel

### Phase 4: 多仓库 + 自动化（1 周）
- [ ] 多仓库注册表管理
- [ ] Git hooks 自动安装
- [ ] Watch 模式
- [ ] 索引过期告警 + 自动后台更新

---

## 10. 风险与缓解

| 风险 | 影响 | 缓解 |
|------|------|------|
| 大型仓库首次索引耗时过长 | 高 | 夜间跑首次全量；提供进度条和预估时间 |
| 索引占用磁盘过大 | 中 | 压缩（SQLite VACUUM、JSON gzip）；定期清理旧分支 |
| Graphify LLM 调用费用过高 | 低 | 默认轻量模式零 LLM Token；增强模式仅 Top God Nodes 调 LLM；本地 CodeBERT 替代远程 LLM |
| Claude 不理解新工具 | 中 | 系统提示词中提供工具使用示例；返回结果带 `_context` 辅助理解 |
| 分支增量更新遗漏跨文件影响 | 中 | 保守策略：变更文件 + 直接调用方都标记为需要重新索引 |
| 多仓库存储爆炸 | 低 | 内容寻址去重；30 天未访问的旧分支自动归档 |

---

## 11. 附录：配置文件完整示例

### 11.1 MCP 配置（~/.claude/mcp-config.json）

```json
{
  "mcpServers": {
    "code-intel": {
      "command": "claude-code-intel",
      "args": ["mcp"],
      "env": {
        "CODE_INTEL_DATA_DIR": "~/.claude-code-intel",
        "CODE_INTEL_LOG_LEVEL": "info",
        "CODE_INTEL_MAX_TOKENS_PER_QUERY": "800"
      }
    }
  }
}
```

### 11.2 项目级配置（<repo>/.claude-code-intel/config.yaml）

```yaml
repo_name: "my-project"
repo_hash: "auto"  # init 时自动生成

shards:
  - name: "core"
    roots: ["src/core/", "include/core/"]
    max_files: 1000
  - name: "api"
    roots: ["src/api/"]
    max_files: 500

exclude:
  - "*.pb.go"
  - "vendor/"
  - "*_test.go"

build:
  gitnexus:
    parser: "tree-sitter"
    storage: "sqlite"
    parallel_jobs: 4
  graphify:
    leiden_resolution: 0.8
    llm_model: "claude-sonnet-4"
    max_llm_calls_per_shard: 200

auto_update:
  git_hooks: true
  stale_threshold_hours: 24
```

### 11.3 查询示例（Claude 实际调用）

```json
// Claude → MCP: code_navigate
{
  "tool": "code_navigate",
  "arguments": {
    "symbol": "tcp_sendmsg",
    "direction": "both",
    "depth": 2
  }
}

// MCP → Claude: 返回
{
  "definition": {"file": "net/ipv4/tcp.c", "line": 842, "column": 1},
  "callers": [
    {"symbol": "inet_sendmsg", "file": "net/ipv4/af_inet.c", "line": 800},
    {"symbol": "sock_sendmsg", "file": "net/socket.c", "line": 654}
  ],
  "callees": [
    {"symbol": "tcp_write_xmit", "file": "net/ipv4/tcp_output.c", "line": 2400},
    {"symbol": "sk_stream_wait_memory", "file": "net/core/stream.c", "line": 140}
  ],
  "_context": {
    "community": "TCP Data Path",
    "community_rank": 3,
    "shard": "net"
  }
}
```

---

## 参考

- [GitNexus](https://github.com/abhigyanpatwari/GitNexus) — Zero-Server Code Intelligence Engine
- [Graphify](https://graphify.net/) — Knowledge Graphs for AI Coding Assistants
- [code-retrieval-design.md](./code-retrieval-design.md) — claude-go 四层混合检索架构
- [large-codebase-graphify-gitnexus-design.md](./large-codebase-graphify-gitnexus-design.md) — 本方案的前一版本（面向 claude-go 引擎）
