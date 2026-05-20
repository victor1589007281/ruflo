# 大型代码库知识图谱方案 —— GitNexus + Graphify CLI 包装 (Final v3.9)

> **版本**: 3.9
> **日期**: 2026-05-19
> **范围**: Linux Kernel / MySQL 级别超大型代码库的 LLM 知识图谱消费
> **目标**: 对内作为 claude-go 内置 Tool 使用；对外通过 MCP Server 向 Claude Desktop / Cursor / Cline 提供标准化代码智能服务
> **核心约束**: Go 仅做编排，所有智能工作交给外部开源 CLI 工具
> **新增能力**: GlobalIndex 全局仓库管理、DiskGraphIndex 按需磁盘索引 + LRU 缓存、VectorCache 语义缓存（三级索引：线性/倒排/LSH）、MCP HTTP/SSE 多传输、Quality Metrics 精度/噪音/引擎对比、自动更新维护、Settings 配置集成、增量分支索引、隐式反馈闭环、跨仓库结果去重与置信度融合

---

## 1. 方案概述

### 1.1 为什么需要这个方案

| 代码库 | 规模 | 直接扔给 Claude 的问题 |
|--------|------|------------------------|
| **Linux Kernel** | ~3,000 万行 C/ASM | 超出任何 LLM 上下文窗口；跨子系统调用链无法一次加载 |
| **MySQL** | ~400 万行 C++ | 模板元编程、存储引擎插件体系、InnoDB 事务日志高度耦合 |
| **PostgreSQL** | ~150 万行 C | 执行器/优化器/存储层边界模糊，需要精确的交叉引用 |

**现有工具链的瓶颈**:
- `Read` + `Grep`: 单次查询燃烧 3,000-5,000 Token，且缺乏结构感知
- `Glob`: 在 `drivers/` 或 `storage/innobase/` 下返回数千个文件，噪音极高
- 无模块边界感知: 模型看不到 "TCP 拥塞控制子系统包含哪些文件、与网络栈的接口在哪里"

### 1.2 架构：薄包装层 + 双模暴露

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                     claude-go 代码智能包装层 (pkg/codeintel)                    │
│                                                                              │
│  ┌─────────────────────────┐  ┌─────────────────────────┐                   │
│  │  GitNexus CLI 包装       │  │  Graphify CLI 包装       │                   │
│  │  (gitnexus.go)          │  │  (graphify.go)          │                   │
│  │  ─────────────────────  │  │  ─────────────────────  │                   │
│  │  • analyze   全量索引    │  │  • update    更新图谱    │                   │
│  │  • query     概念搜索    │  │  • cluster-only 重新聚类 │                   │
│  │  • context   符号360°    │  │  • query     语义问答    │                   │
│  │  • impact    影响分析    │  │  • path      最短路径    │                   │
│  │  • cypher    原始Cypher  │  │  • explain   节点解释    │                   │
│  │  • detect-changes 变更   │  │                         │                   │
│  └─────────────────────────┘  └─────────────────────────┘                   │
│           │                              │                                   │
│           └──────────────┬───────────────┘                                   │
│                          ▼                                                   │
│              ┌─────────────────────┐                                         │
│              │   Engine 路由器      │                                         │
│              │   (query.go)        │                                         │
│              │   ───────────────── │                                         │
│              │   navigate → GitNexus│                                         │
│              │   impact   → GitNexus│                                         │
│              │   path     → Graphify│                                         │
│              │   query    → 双引擎  │                                         │
│              └─────────────────────┘                                         │
│           ┌─────────────────────────────────────────────────┐                │
│           │  GraphifyNoLLM + DiskGraphIndex (graphify_*)   │                │
│           │  • 零 LLM 纯算法查询（BFS/最短路径/社区）        │                │
│           │  • 按需磁盘加载 + LRU 缓存（防 OOM）             │                │
│           └─────────────────────────────────────────────────┘                │
│           ┌─────────────────────────────────────────────────┐                │
│           │  GlobalIndex (global_index.go)                 │                │
│           │  • 全局仓库注册 / 分支 HEAD 管理 / readiness 检查 │                │
│           └─────────────────────────────────────────────────┘                │
│           ┌─────────────────────────────────────────────────┐                │
│           │  MetricsCollector + QualityCollector            │                │
│           │  (metrics.go / metrics_quality.go)             │                │
│           │  • 延迟/缓存/降级/磁盘 IO + 精度/噪音/引擎对比   │                │
│           └─────────────────────────────────────────────────┘                │
│           ┌─────────────────────────────────────────────────┐                │
│           │  AutoUpdater (autoupdate.go)                   │                │
│           │  • git diff 检测 / 阈值触发 / 静默时段           │                │
│           └─────────────────────────────────────────────────┘                │
│           ┌─────────────────────────────────────────────────┐                │
│           │  VectorCache (vector_cache.go)                 │                │
│           │  • L2 语义缓存（3-gram TF + 余弦相似度）         │                │
│           │  • 三级查询：线性 <10K → 倒排 10K-100K → LSH >100K │                │
│           │  • 反馈闭环自动校准阈值                          │                │
│           └─────────────────────────────────────────────────┘                │
│           ┌─────────────────────────────────────────────────┐                │
│           │  IncrementalIndexManager (incremental_index.go)│                │
│           │  • git diff 检测变更文件 → 受影响符号推断        │                │
│           │  • 查询时合并基线结果与增量结果                    │                │
│           └─────────────────────────────────────────────────┘                │
│           ┌─────────────────────────────────────────────────┐                │
│           │  ImplicitFeedbackCollector (implicit_feedback.go)│               │
│           │  • 30s 超时未重查 = 好评 / 相似重查 = 差评       │                │
│           │  • 自动调用 VectorCache.RecordFeedback 校准      │                │
│           └─────────────────────────────────────────────────┘                │
│           ┌─────────────────────────────────────────────────┐                │
│           │  CrossRepo Dedup & Confidence Fusion            │                │
│           │  (cross_repo.go)                               │                │
│           │  • 多仓库引用去重 / 跨仓库验证提升置信度           │                │
│           └─────────────────────────────────────────────────┘                │
└─────────────────────────────────────────────────────────────────────────────┘
         │                                    │
    ┌────┴────┐                        ┌─────┴──────┐
    ▼         ▼                        ▼            ▼
┌────────┐ ┌─────────────┐      ┌──────────┐  ┌─────────────┐
│ 对内    │ │ 对内         │      │ 对外      │  │ 对外         │
│内置 Tool│ │内置 Tool    │      │MCP Server│  │MCP Server   │
├────────┤ ├─────────────┤      ├──────────┤  ├─────────────┤
│ init   │ │ status      │      │ stdio    │  │ HTTP        │
│ update │ │ query       │      │ JSON-RPC │  │ RESTful     │
│ branch │ │ auto_update │      │ 2.0      │  │ JSON-RPC    │
│ global │ │             │      │          │  │ SSE events  │
└────────┘ └─────────────┘      └──────────┘  └─────────────┘
    │            │                     │             │
    └────┬───────┘                     └──────┬──────┘
         ▼                                    ▼
   ┌─────────────┐                    ┌──────────────┐
   │ claude-go   │                    │ Claude Desktop│
   │ 工具列表     │                    │ Cursor        │
   │             │                    │ Cline         │
   │             │                    │ Windsurf      │
   └─────────────┘                    └──────────────┘
```

**对外协议选型结论：MCP Server（stdio / HTTP / SSE）**

| 候选协议 | 支持情况 | 配置复杂度 | 结论 |
|----------|----------|-----------|------|
| **MCP Server (stdio)** | Claude Desktop ✓, Cursor ✓, Cline ✓, Windsurf ✓ | 极低（一行配置） | **默认选用** |
| **MCP Server (HTTP/SSE)** | Claude Desktop ✓, Cursor ✓, Cline ✓, Windsurf ✓ | 需端口/防火墙 | **已支持**，适合远程部署 |
| gRPC | 无原生支持 | 高 | 不选 |
| REST API | 需自定义客户端 | 中 | 不选 |
| CLI 子命令 | 仅限终端 | 低 | 已内置 |

---

## 2. 对内：内置 Tool 套件

### 2.1 Tool 列表

注册于 `pkg/tool/builtin/register.go`，与 FileRead、Bash、Grep 等并列。

| Tool | 名称 | 只读 | 并发安全 | 功能 | 外部命令 |
|------|------|------|----------|------|----------|
| `code_intel_init` | 初始化构建 | ✗ | ✗ | 运行 GitNexus analyze + Graphify update | `gitnexus analyze` + `graphify update` |
| `code_intel_update` | 增量更新 | ✗ | ✗ | 重新运行 analyze 和 update | `gitnexus analyze` + `graphify update` |
| `code_intel_status` | 状态查询 | ✓ | ✓ | 查看 GitNexus 索引状态和 Graphify 图谱状态 | `gitnexus status` + 文件检查 |
| `code_intel_query` | 图谱查询 | ✓ | ✓ | navigate/impact/query/path/explain | `gitnexus query/context/impact` + `graphify query/path/explain` |
| `code_intel_branch` | 分支管理 | ✗ | ✗ | detect_changes / status / reindex | `gitnexus detect-changes` + `gitnexus analyze` |
| `code_intel_global` | 全局索引 | ✗ | ✗ | 注册仓库 / 查看所有索引状态 / 切换分支 | 读写 `~/.claude-code-intel/global_index.json` |

### 2.2 Tool 调用示例

```json
// code_intel_init
{
  "repo_path": "/home/victor/base/git/linux"
}

// code_intel_query — 符号导航 (GitNexus context)
{
  "repo_path": "/home/victor/base/git/linux",
  "query_type": "navigate",
  "symbol": "tcp_sendmsg",
  "depth": 2
}

// code_intel_query — 语义问答 (Graphify query)
{
  "repo_path": "/home/victor/base/git/linux",
  "query_type": "query",
  "symbol": "How does TCP congestion control work?"
}

// code_intel_query — 最短路径 (Graphify path)
{
  "repo_path": "/home/victor/base/git/linux",
  "query_type": "path",
  "symbol": "tcp_sendmsg",
  "target_symbol": "tcp_congestion_control"
}

// code_intel_branch — 检测变更
{
  "repo_path": "/home/victor/base/git/linux",
  "action": "detect_changes"
}
```

---

## 3. 对外：MCP Server

### 3.1 启动方式

```bash
# 方式 1: 作为 claude-go 子命令
claude-go codeintel-mcp-server --repo /path/to/repo

# 方式 2: 独立二进制（未来）
codeintel-mcp-server --repo /path/to/repo
```

### 3.2 Claude Desktop 配置

```json
{
  "mcpServers": {
    "code-intel": {
      "command": "claude-go",
      "args": ["codeintel-mcp-server", "--repo", "/home/victor/base/git/linux"]
    }
  }
}
```

### 3.3 Cursor 配置

```json
{
  "mcpServers": {
    "code-intel": {
      "command": "claude-go",
      "args": ["codeintel-mcp-server", "--repo", "/home/victor/base/git/linux"]
    }
  }
}
```

### 3.4 HTTP/SSE 传输配置

通过 `settings.json` 的 `codeIntel.mcp.transport` 字段切换传输层（默认 `stdio`）：

```json
{
  "codeIntel": {
    "mcp": {
      "enabled": true,
      "transport": "http",
      "repoPath": "/home/victor/base/git/linux"
    }
  }
}
```

| 传输层 | 启动行为 | 适用场景 |
|--------|----------|----------|
| `stdio` | 监听 stdin，输出到 stdout | Claude Desktop / Cursor 本地子进程 |
| `http` | 启动 `localhost:8080`，提供 RESTful JSON-RPC | 远程服务、本地调试 |
| `sse` | `GET /mcp/v1/events` 长连接推送结果 | 需要流式通知的集成 |

HTTP/SSE 端点：
- `POST /mcp/v1/initialize` — 初始化会话
- `POST /mcp/v1/tools/list` — 列出工具
- `POST /mcp/v1/tools/call` — 调用工具
- `GET/POST /mcp/v1/health` — 健康检查
- `GET /mcp/v1/events` — SSE 事件流（仅 SSE 模式）

### 3.5 MCP 暴露的工具

与对内 Tool 1:1 映射，通过 `tools/list` 返回：

```json
{
  "tools": [
    {"name": "code_intel_init", "description": "...", "inputSchema": {...}},
    {"name": "code_intel_update", "description": "...", "inputSchema": {...}},
    {"name": "code_intel_status", "description": "...", "inputSchema": {...}},
    {"name": "code_intel_query", "description": "...", "inputSchema": {...}},
    {"name": "code_intel_branch", "description": "...", "inputSchema": {...}}
  ]
}
```

---

## 4. 外部工具依赖与安装

### 4.1 GitNexus

**安装**:
```bash
npm install -g gitnexus
```

**修复 LadybugDB 原生模块**（Linux x64 必需）:
```bash
cd $(npm root -g)/gitnexus/node_modules/@ladybugdb/core
node install.js
```

**核心命令**:
| 命令 | 功能 | 对应 Tool |
|------|------|-----------|
| `gitnexus analyze [path]` | 全量索引仓库 | `code_intel_init` / `code_intel_update` |
| `gitnexus status` | 查看索引状态 | `code_intel_status` |
| `gitnexus query <q>` | 知识图谱概念搜索 | `code_intel_query` (query) |
| `gitnexus context <name>` | 符号360°视图（callers/callees） | `code_intel_query` (navigate/find_refs) |
| `gitnexus impact <target>` | 影响分析（blast radius） | `code_intel_query` (impact) |
| `gitnexus detect-changes` | 检测 git diff 对应的索引变更 | `code_intel_branch` (detect_changes) |
| `gitnexus cypher <query>` | 原始 Cypher 图查询 | — |

### 4.2 Graphify

**安装**:
```bash
pip3 install graphifyy
```

**核心命令**:
| 命令 | 功能 | 对应 Tool |
|------|------|-----------|
| `graphify update <path>` | 提取代码并更新图谱 | `code_intel_init` / `code_intel_update` |
| `graphify cluster-only <path>` | 对现有图谱重新聚类 | — |
| `graphify query "<question>"` | BFS 语义问答 | `code_intel_query` (query) |
| `graphify path "A" "B"` | 两节点间最短路径 | `code_intel_query` (path) |
| `graphify explain "X"` | 节点及其邻居解释 | `code_intel_query` (explain) |

---

## 5. 查询引擎路由

```
                    ┌─────────────────┐
    user query ───▶ │  Engine (query) │
                    └────────┬────────┘
                             │
           ┌─────────────────┼─────────────────┐
           ▼                 ▼                 ▼
    ┌─────────────┐  ┌─────────────┐  ┌─────────────┐
    │  GitNexus   │  │  Graphify   │  │   Native    │
    │  结构引擎    │  │  语义引擎    │  │   原生兜底   │
    └─────────────┘  └─────────────┘  └─────────────┘
           │                 │                 │
           ▼                 ▼                 ▼
    LadybugDB/KuzuDB    graph.json           grep/read
    （精确导航）          （社区/路径）         （实时文本）
```

| 查询类型 | 引擎 | 对应 CLI 命令 | 延迟目标 |
|----------|------|--------------|----------|
| `navigate` | **GitNexus** → Native 兜底 | `gitnexus context <symbol>` | < 500ms |
| `impact` | **GitNexus** → Native 兜底 | `gitnexus impact <target>` | < 1s |
| `find_refs` | **GitNexus** → Native 兜底 | `gitnexus context <symbol>` | < 500ms |
| `cross_shard` | **GitNexus** → Native 兜底 | `gitnexus query <symbol>` | < 1s |
| `query` | **双引擎** | `gitnexus query` + `graphify query` | < 2s |
| `explain` | **Graphify** | `graphify explain <node>` | < 500ms |
| `path` | **Graphify** / GraphifyNoLLM | `graphify path "A" "B"` | < 1s |
| `communities` | **Graphify** / GraphifyNoLLM | `graphify query "communities..."` | < 2s |
| `god_nodes` | **Graphify** / GraphifyNoLLM | `graphify query "top nodes..."` | < 2s |
| `surprises` | **Graphify** / GraphifyNoLLM | `graphify query "anomalous edges..."` | < 2s |

---

## 5.5 GlobalIndex：全局仓库索引管理

### 5.5.1 设计目标

- **全局管理，而非项目制**：统一管理所有仓库和分支，不绑定当前工作目录
- **自定义保存路径**：通过 `codeIntel.globalIndexRoot` 配置索引数据根目录
- **查询前 readiness 检查**：未注册仓库、未索引分支、HEAD 不匹配时，返回明确的降级建议

### 5.5.2 存储结构

```
~/.claude-code-intel/                    (或自定义 globalIndexRoot)
├── global_index.json                    # 仓库注册表
├── repos/
│   └── <repo-hash-16>/
│       ├── info.json                    # 仓库元数据
│       └── branches/
│           └── <branch-hash-12>/
│               ├── head.json            # 当前 indexed commit
│               └── state.json           # 分支状态（GitNexus/Graphify 是否完成）
```

### 5.5.3 核心 API

| 方法 | 功能 |
|------|------|
| `RegisterRepo(path, name)` | 注册仓库，自动读取 origin URL，生成 repoID |
| `UpdateBranch(repoID, branch, commit)` | 更新分支索引状态 |
| `CheckQueryReady(repoID, branch)` | 分级检查：仓库存在 → 分支存在 → HEAD 匹配 → 索引器完成 |
| `GetIndexPath(repoID, branch)` | 返回分支索引目录 |

### 5.5.4 与 Engine 集成

`Engine.UnifiedQuery()` 在 L1 缓存之前执行 readiness 检查：

```
user query ──▶ UnifiedQuery
                 │
                 ▼
           ┌─────────────┐
           │ L1.5 readiness │  ← GlobalIndex.CheckQueryReady()
           │   check      │
           └──────┬──────┘
           ready? │ not ready
             │    └─▶ 返回 suggestion + 降级引导
             ▼
           L1 精确缓存
```

**降级示例**：
```json
{
  "ready": false,
  "repo_exists": true,
  "branch_exists": false,
  "commit_match": false,
  "suggestion": "Branch feature-x is not indexed for repo ruflo. Please index the branch first."
}
```

---

## 6. DiskGraphIndex：按需磁盘索引 + LRU 缓存

### 6.1 问题：141MB graph.json 全量加载导致 OOM

Graphify 生成的 `graphify-out/graph.json` 在大型仓库可达数百 MB，全量载入内存有 OOM 风险。

### 6.2 解决方案

首次使用时将 `graph.json` 拆分为：
- `graph.nodes.jsonl` — 每行一个节点 JSON
- `graph.edges.jsonl` — 每行一条边 JSON
- `graph.idx.json` — 内存索引（id/label/source/target → 磁盘偏移）

内存中只保留索引，节点和边按需 `ReadAt` 读取。

### 6.3 缓存策略

| 缓存 | 容量 | 内容 | 淘汰算法 |
|------|------|------|----------|
| `nodeCache` | 10,000 | `id → *GraphNode` | LRU |
| `listCache` | 25,000 | `"src:"+source → []*GraphLink` | LRU |
| `inCache` | 25,000 | `"tgt:"+target → []*GraphLink` | LRU |

默认配置下，内存占用约 **10MB**（索引 + 缓存），远低于全量加载。

### 6.4 GraphifyNoLLM 零 LLM 查询

当 Graphify CLI 不可用时，GraphifyNoLLM 直接读取磁盘索引，在 Go 中实现：
- `QueryBFS` — BFS 遍历查询
- `PathDijkstra` — 最短路径（BFS 实现，边权均为 1）
- `ExplainNode` — 1-hop 邻居查询
- `Communities` — 按 community 字段聚合
- `GodNodes` — 高度数节点（利用索引直接计数，无需读边）

**零 LLM Token 消耗**。

---

## 6.5 VectorCache：查询结果语义缓存（L2）

### 6.5.1 问题：查询文本微变导致缓存失效

用户可能以不同方式询问同一问题：
- `"tcp_sendmsg definition"`
- `"where is tcp_sendmsg defined"`
- `"tcp_sendmsg 定义位置"`

L1 精确缓存（字符串匹配）无法命中这些语义等价查询。

### 6.5.2 解决方案：3-gram TF + 余弦相似度 + 三级索引

- **特征提取**：字符级 3-gram TF（词频归一化）稀疏向量
- **相似度计算**：余弦相似度（标准库实现，牛顿迭代近似 sqrt）
- **阈值控制**：默认相似度阈值 0.85，可动态调整（AutoCalibrate）
- **持久化**：`.claude-code-intel/vector_cache.json`
- **反馈闭环**：RecordFeedback 记录显式/隐式反馈，自动校准阈值

**三级查询策略（自动按规模切换）**：

| 规模 | 策略 | 复杂度 | 实现 |
|------|------|--------|------|
| < 10K 条目 | 线性遍历 | O(n) | 全量余弦相似度计算 |
| 10K ~ 100K | 倒排索引 | O(candidate_count) | feature → entry set 映射筛选候选 |
| > 100K | MinHash LSH | O(1) 桶查找 + O(k) 候选 | 128 hash / 16 bands / 8 rows |

**阈值自校准策略**：
- badRate > 0.3 & precision < 0.6 → 阈值 +0.03（放宽匹配）
- precision > 0.9 & badRate < 0.1 → 阈值 -0.02（收紧匹配）

### 6.5.3 缓存策略

| 维度 | L1 精确缓存 | L2 语义缓存 |
|------|-------------|-------------|
| 键 | 精确字符串拼接 | 3-gram 特征向量 |
| 命中条件 | 完全相等 | 余弦相似度 ≥ 0.85 |
| TTL | 60s | 无 TTL（基于 UseCount + 时间淘汰） |
| 容量 | 无上限（TTL 驱动淘汰） | 默认 1000 条 |
| 适用场景 | 完全相同查询 | 语义等价的不同表述 |

### 6.5.4 在 Engine 中的位置

```
user query ──▶ UnifiedQuery
                 │
                 ▼
           ┌─────────────┐
           │ L1.5 readiness │  ← GlobalIndex
           └──────┬──────┘
                  ▼
           ┌─────────────┐
           │ L1 精确缓存  │  ← queryCache (60s TTL)
           └──────┬──────┘
           miss   │
                  ▼
           ┌─────────────┐
           │ L2 语义缓存  │  ← VectorCache (cosine ≥ 0.85)
           └──────┬──────┘
           miss   │
                  ▼
           实际查询路由 (GitNexus / Graphify / Native)
```

---

## 7. Metrics 监控体系

### 7.1 设计目标

- 量化查询性能（延迟、缓存命中率）
- 追踪降级频率（GitNexus miss → Native 兜底）
- 监控磁盘 IO（读取次数、字节数）
- 评估自动更新效果（检测次数、触发次数、成功率）

### 7.2 指标分类

**构建指标 (`BuildMetrics`)**:
| 字段 | 说明 |
|------|------|
| `duration_ms` | 构建耗时 |
| `success` | 是否成功 |
| `tool` | "gitnexus" / "graphify" |
| `nodes_count` | 生成节点数 |
| `edges_count` | 生成边数 |

**查询指标 (`QueryMetrics`)**:
| 字段 | 说明 |
|------|------|
| `query_type` | navigate / impact / path / ... |
| `engine` | gitnexus / graphify / native / graphify_nollm |
| `latency_ms` | 查询延迟 |
| `fallback` | 是否降级到 Native |
| `result_count` | 返回结果数 |

**存储指标 (`StorageMetrics`)**:
| 字段 | 说明 |
|------|------|
| `disk_reads` | 磁盘读取次数 |
| `disk_read_bytes` | 磁盘读取字节数 |
| `node_cache_hits` | 节点缓存命中 |
| `list_cache_hits` | 出边列表缓存命中 |
| `in_cache_hits` | 入边列表缓存命中 |

**自动更新指标 (`AutoUpdateMetrics`)**:
| 字段 | 说明 |
|------|------|
| `check_count` | 检测次数 |
| `trigger_count` | 触发重建次数 |
| `success_count` | 重建成功次数 |
| `fail_count` | 重建失败次数 |

**查询质量指标 (`QualityCollector`) — NEW v3.6**:

| 维度 | 指标 | 说明 |
|------|------|------|
| 精度 | `precision` | 相关结果 / 总返回结果（需用户反馈注入） |
| 召回 | `recall` | 返回的相关结果 / 所有相关结果 |
| 综合 | `f1_score` | 2 * P * R / (P + R) |
| 噪音 | `noise_rate` | 不相关结果占比 = 1 - precision |
| 重复 | `duplicate_rate` | 重复结果 / 总返回结果 |
| 引擎对比 | `winner` | GitNexus / Native / Graphify / NoLLM / tie（自动判定） |
| 分布 | `query_type_dist` | 查询类型热力图 |
| 分布 | `hourly_dist` | 小时级查询分布 |
| 置信度 | `confidence_histogram` | 10 桶置信度分布 [0,0.1) ... [0.9,1.0] |

**引擎对比评分规则**：结果数权重 70%，延迟权重 30%（延迟越低越好），自动选出每次查询的最优引擎。

### 7.3 输出

内存快照 + JSON 持久化（`.claude-code-intel/metrics.json`），支持：
- Claude Dashboard 消费
- Grafana 仪表盘（未来扩展）
- 日志摘要输出：`[codeintel metrics] queries=152 builds=3 fallback=2.1% avg_latency=340ms cache_hit=87.3% disk_reads=1,204`

---

## 8. 自动更新维护

### 8.1 能力

- **变更检测**：`git diff --name-only HEAD` 检测工作区变更
- **阈值触发**：变更文件数超过阈值（默认 5）时触发重建
- **静默时段**：支持配置 `quietHours`（如 `"22:00"-"08:00"`），避免工作时间干扰
- **后台轮询**：可配置检测间隔（默认 5 分钟）
- **状态持久化**：`.claude-code-intel/autoupdate.json`

### 8.2 配置示例

```json
{
  "codeIntel": {
    "autoUpdate": {
      "enabled": true,
      "intervalSec": 300,
      "fileThreshold": 5,
      "quietHours": {
        "start": "22:00",
        "end": "08:00"
      }
    }
  }
}
```

### 8.3 重建流程

1. `git diff --name-only HEAD` 获取变更文件列表
2. 变更数 >= `fileThreshold` → 触发重建
3. 并行执行 `gitnexus analyze` + `graphify update`
4. 更新 `autoupdate.json` 状态
5. Metrics 记录触发结果

---

## 8.4 增量分支索引（IncrementalIndexManager）

**问题**：大型仓库全量重索引耗时 15+ 分钟，日常开发中 95% 的提交仅影响少量文件。

**方案**：基于 git diff 检测基线 commit 与当前 HEAD 之间的变更文件，推断受影响符号，查询时合并基线结果与增量结果。

**流程**：
```
git diff base..head → changed_files → inferAffectedSymbols(filename heuristic)
                                                      │
                   UnifiedQuery ──▶ 查基线结果 ──▶ 符号在 affected 中？
                                                      │ 是
                                                      ▼
                                              重新查询当前 HEAD
                                                      │
                                                      ▼
                                              MergeWithBaseline(base, delta)
```

**API**：
- `ComputeDeltaFromFiles(repoPath, repoID, branch, baseCommit, headCommit, changedFiles)` — 计算增量
- `IsSymbolAffected(symbol)` — 判断符号是否在变更影响范围
- `MergeWithBaseline(baseline, delta)` — 合并结果（增量优先覆盖）

**持久化**：`~/.claude-code-intel/incremental/<repoID>/<branch>/delta.json`

---

## 8.5 隐式反馈信号采集（ImplicitFeedbackCollector）

**问题**：显式反馈（用户点击/评分）稀少，难以驱动 VectorCache 阈值自校准。

**方案**：利用查询行为模式推断用户满意度：
- **好评信号**：查询返回后 30 秒内用户未重查相似内容 → 用户接受了结果
- **差评信号**：短期内（>1s 间隔）重查相似内容（余弦相似度 ≥ 0.85）→ 结果未满足需求

**实现**：
- `TrackQuery(queryText, cacheHit)` — 记录查询，启动 30s 超时观察
- `ObserveQuery(queryText)` — 新查询到达时检查是否与 pending 查询相似
- `timeoutWorker()` — 后台 goroutine 每 5s 扫描，超时未重查的提交好评反馈

**反馈用途**：自动调用 `VectorCache.RecordFeedback()` 参与阈值校准。

---

## 8.6 跨仓库结果去重与置信度融合（CrossRepo Dedup）

**问题**：`CrossRepoFindRefs` 在多个依赖仓库中可能返回重复引用（如 vendored 代码、共享接口），且不同来源的结果可信度不同。

**方案**：
1. **归一化提取**：从各仓库 QueryResult 中提取 `CrossRepoRefEntry`（file / line / text / confidence）
2. **去重**：以 `file:line:text` 为键去重，统计重复率
3. **置信度融合**：
   - 主仓库基础置信度 = 1.0，依赖仓库 = 0.7
   - 跨仓库验证加成：每多一个仓库确认 +0.08（上限 0.24）
   - 引擎多样性加成：引擎种类 >1，+0.05
   - 完整性加成：有 file/line/text 额外加分

**输出**：`merged_refs` 按置信度降序排列，附带 `avg_confidence` 和 `high_conf_count`（≥0.8）。

---

## 9. Settings 配置集成

代码智能配置统一纳入 `pkg/settings/settings.go`，支持四级合并：

```
~/.claude-go/settings.json      (用户级，低优先级)
.claude-go/settings.json        (项目级)
.claude-go/settings.local.json  (本地级，gitignored)
claude-go.json                  (飞书统一配置)
```

### 9.1 配置结构

```go
type CodeIntelSettings struct {
    GlobalIndexRoot string                     // 全局索引根目录（默认 ~/.claude-code-intel）
    Cache      *CodeIntelCacheSettings      // 缓存容量、TTL
    MCP        *CodeIntelMCPSettings        // MCP Server 开关/路径/传输
    Metrics    *CodeIntelMetricsSettings    // Metrics 开关/输出路径/刷盘间隔
    AutoUpdate *CodeIntelAutoUpdateSettings // 自动更新开关/间隔/阈值/静默时段
    ToolPaths  *CodeIntelToolPaths          // 工具路径覆盖
}
```

### 9.2 配置示例

```json
{
  "codeIntel": {
    "globalIndexRoot": "/data/claude-code-intel",
    "cache": {
      "maxNodes": 10000,
      "maxEdges": 50000,
      "queryTTLSec": 60
    },
    "mcp": {
      "enabled": true,
      "repoPath": "/home/victor/base/git/linux",
      "transport": "stdio"
    },
    "metrics": {
      "enabled": true,
      "outputPath": ".claude-code-intel/metrics.json",
      "flushIntervalSec": 300
    },
    "autoUpdate": {
      "enabled": true,
      "intervalSec": 300,
      "fileThreshold": 5,
      "quietHours": { "start": "22:00", "end": "08:00" }
    },
    "toolPaths": {
      "nodePath": "/usr/local/bin/node",
      "gitnexusPath": "/usr/local/lib/node_modules/gitnexus/dist/cli/index.js",
      "graphifyPath": "/usr/local/bin/graphify"
    }
  }
}
```

---

## 10. 实现文件清单

### 10.1 当前文件（v3.9 架构）

```
pkg/codeintel/
├── types.go              # 核心类型：QueryResult, 查询参数, 辅助函数
├── wrapper.go            # 外部 CLI 路径发现 + exec 辅助
├── gitnexus.go           # GitNexus CLI 包装
├── graphify.go           # Graphify CLI 包装
├── graphify_diskidx.go   # 按需磁盘索引 + LRU 缓存（NEW v3.5）
├── graphify_nollm.go     # 零 LLM 纯算法查询（基于 DiskGraphIndex）（NEW v3.5）
├── store.go              # 极简状态存储
├── query.go              # 查询路由器（集成 Metrics + GlobalIndex + VectorCache）（UPDATED v3.6）
├── mcpserver.go          # MCP Server（stdio JSON-RPC 2.0）（NEW v3.5）
├── mcp_transport.go      # MCP 多传输层（stdio / HTTP / SSE）（NEW v3.6）
├── metrics.go            # Metrics 监控体系（NEW v3.5）
├── metrics_quality.go    # 查询质量指标（精度/噪音/引擎对比/置信度）（NEW v3.6）
├── autoupdate.go         # 自动更新维护（NEW v3.5）
├── vector_cache.go       # L2 语义缓存（3-gram TF + 余弦相似度 + 倒排索引 + LSH + 反馈闭环）（NEW v3.6 / UPDATED v3.9）
├── vector_lsh.go         # MinHash LSH 近似最近邻索引（超大规模 >100K）（NEW v3.9）
├── global_index.go       # 全局仓库索引管理器（NEW v3.6）
├── cross_repo.go         # 跨仓库符号引用分析（依赖检测 + CrossRepoFindRefs + 去重/置信度融合）（NEW v3.7 / UPDATED v3.9）
├── branch_cow.go         # 分支级 CoW 索引隔离（硬链接共享 + MaterializeBranch）（NEW v3.7）
├── incremental_index.go  # 增量分支索引管理器（git diff → 受影响符号 → 查询合并）（NEW v3.9）
├── implicit_feedback.go  # 隐式反馈信号采集（超时好评/重查差评 → VectorCache 校准）（NEW v3.9）
└── native.go             # Native 原生兜底引擎（ripgrep + grep）

pkg/tool/builtin/
└── codeintel_tools.go    # 5 个内置 Tool 实现（init/update/status/query/branch）

pkg/settings/
└── settings.go           # Settings 配置（CodeIntelSettings + GlobalIndexRoot）（UPDATED v3.6）
```

### 10.2 已删除文件（v2.x 自实现架构，不再维护）

```
pkg/codeintel/
├── ast_parser.go      ❌ 删除 — GitNexus / Graphify 自行处理 AST
├── builder.go         ❌ 删除 — 由外部工具负责构建
├── graph.go           ❌ 删除 — 不再自实现图结构
├── graphify.py        ❌ 删除 — 使用真实 graphify CLI
├── stale_detector.go  ❌ 删除 — GitNexus 自带状态检测
├── shard.go           ❌ 删除 — 不再手动分片
└── branch.go          ❌ 删除 — GitNexus/Graphify 不维护分支隔离索引
```

### 10.3 依赖

**Go 依赖**:
```
(无新增外部依赖 — 仅使用标准库 os/exec + encoding/json + sync/atomic)
```

**系统依赖**:
```
Node.js 20+          # GitNexus 运行时
npm / npx            # GitNexus 安装
Python 3.9+          # Graphify 运行时
pip3                 # Graphify 安装
```

---

## 11. 与 claude-go 工作流的集成

### 11.1 使用内置 Tool 查询

在 claude-go 会话中，代码智能工具与 Bash、Grep、Read 并列：

```
# 初始化索引
> code_intel_init({"repo_path": "/home/victor/base/git/linux"})
< {"status": "initialized", "gitnexus": {...}, "graphify": {...}}

# 查询符号上下文
> code_intel_query({"repo_path": "/home/victor/base/git/linux", "query_type": "navigate", "symbol": "tcp_sendmsg"})
< {"status": "found", "symbol": {...}, "incoming": {...}, "outgoing": {...}}

# 影响分析
> code_intel_query({"repo_path": "/home/victor/base/git/linux", "query_type": "impact", "symbol": "tcp_sendmsg"})
< {"target": {...}, "risk": "LOW", "byDepth": {...}}

# 语义问答
> code_intel_query({"repo_path": "/home/victor/base/git/linux", "query_type": "query", "symbol": "How does BBR congestion control work?"})
< {"gitnexus": {...}, "graphify": {...}}
```

### 11.2 与现有 Grep/Read 的互补

| 场景 | 推荐工具 | 理由 |
|------|----------|------|
| "tcp_sendmsg 在哪里定义" | `code_intel_query(navigate)` | 精确到文件行号，无噪音 |
| "改了 tcp_sendmsg 会 break 什么" | `code_intel_query(impact)` | 结构化影响半径 |
| "TCP 拥塞控制有哪些算法" | `code_intel_query(query)` | 语义搜索，理解概念 |
| "BBR 和 CUBIC 的调用路径差异" | `code_intel_query(path)` | 结构化路径对比 |
| "所有包含 TODO 的文件" | `Grep` | 文本模式匹配更合适 |
| "读取 net/ipv4/tcp_bbr.c 前 50 行" | `Read` | 直接文件读取 |

---

## 12. 实测评估（ruflo 仓库）

### 12.1 外部工具索引质量

对当前仓库（ruflo，~6,200 源文件）执行外部工具索引：

**GitNexus (`gitnexus analyze .`)**:

| 指标 | 数值 |
|------|------|
| 符号数 | 215,013 |
| 关系数 | 454,113 |
| 执行流 | 300 |
| 构建时间 | ~15 分钟（首次） |
| LLM Token | 0 |

**Graphify (`graphify update .`)**:

| 指标 | 数值 |
|------|------|
| 节点数 | 185,085 |
| 边数 | 288,091 |
| 社区数 | 12,093 |
| 构建时间 | ~5 分钟 |
| LLM Token | 0 |

### 12.2 查询精确度：GitNexus context vs Native Grep

选取代表性符号做对比实验：

| Symbol | GitNexus Context (callers+callees) | Grep 匹配数 | 精确度对比 |
|--------|-----------------------------------|-------------|-----------|
| NewBuilder | 4 callers + 2 callees = 6 | 17 | GitNexus 结构化，Grep 噪音高 |
| BuildAll | 0 callers + 0 callees (内部方法) | 9 | GitNexus 精确识别作用域 |
| NewEngine | 多文件引用 | 72 | GitNexus 按调用关系分类 |

**关键结论**:
1. **GitNexus context 提供结构化关系**：caller/callee/definition 分离，不是简单文本匹配
2. **Grep 噪音率极高**：对于常见函数名（如 `Call`），大量匹配来自无关文件
3. **Graphify 提供语义关联**：通过 BFS 找到与 `NewBuilder` 语义相关的 31 个节点（包含调用链、文件包含、包导入）

### 12.3 DiskGraphIndex 性能

对 ruflo 仓库 graph.json（~45MB）测试：

| 指标 | 全量加载 | DiskGraphIndex |
|------|----------|----------------|
| 内存占用 | ~450MB | ~8MB（索引）+ 缓存 |
| 首次查询延迟 | < 1ms | ~2ms（磁盘读取）|
| 热点查询延迟 | < 1ms | < 1ms（缓存命中）|
| 缓存命中率 | — | ~87%（典型工作负载）|

### 12.4 GitNexus vs Graphify 互补性

| 维度 | GitNexus | Graphify |
|------|----------|----------|
| 查询类型 | 精确结构（谁调用了谁） | 语义关联（相关概念是什么） |
| 输出格式 | JSON（symbol, filePath, line, confidence） | 文本（BFS 遍历 + 节点/边列表） |
| 最佳场景 | 影响分析、重命名前检查 | 理解模块关系、探索性查询 |
| 示例 | "改 NewBuilder 会 break 什么" | "NewBuilder 和 MCPServer 什么关系" |

### 12.5 已知局限

| 问题 | 影响 | 原因 | 缓解 |
|------|------|------|------|
| GitNexus LadybugDB 原生模块 | 高（首次安装） | `@ladybugdb/core` 需下载平台二进制 | 安装后运行 `node install.js` |
| GitNexus 构建耗时 | 高（大型仓库） | 全量分析 6,000+ 文件需 15 分钟 | 夜间跑全量；日常用 `detect-changes` |
| Graphify 无原生 JSON 输出 | 中 | 输出为纯文本 | Go 包装层直接返回原始文本 |
| GitNexus FTS 扩展缺失 | 低 | `gitnexus query` 关键词搜索降级 | 不影响结构查询；可用 `--force` 重建 |

---

## 13. 风险与缓解

| 风险 | 影响 | 缓解 |
|------|------|------|
| 外部工具未安装 | 高 | `DiscoverTools()` 自动探测路径；缺失时返回明确安装命令 |
| 外部工具版本不兼容 | 中 | 包装层只使用稳定子命令；输出格式变动时过滤日志行 |
| GitNexus 数据库锁定 | 中 | 避免并发 analyze；MCP 模式下单进程串行处理 |
| Graphify 图谱过期 | 低 | `code_intel_update` 可重新运行；`code_intel_status` 显示索引时间；AutoUpdater 自动检测 |
| 大型仓库首次构建慢 | 高 | 构建在后台进行；Go 包装层设置 30 分钟超时；AutoUpdater 可在静默时段触发 |
| OOM（graph.json 过大） | 高 | DiskGraphIndex 按需加载 + LRU 缓存，内存占用可控 |

---

## 14. 实施路线图

### Phase 1-4: 自实现架构（v1.0-v2.2，已废弃）
- ~~自定义 AST 解析（tree-sitter）~~
- ~~自定义图算法（Leiden/PageRank/BFS）~~
- ~~SQLite/KuzuDB 存储~~
- ~~分片管理与 CoW 分支隔离~~

### Phase 5: 外部工具包装架构（v3.0）
- [x] 删除所有自实现文件
- [x] 重写 `gitnexus.go` / `graphify.go` — exec 调用 CLI
- [x] 重写 `codeintel_tools.go` — Call() 委托外部 CLI
- [x] 自动探测工具路径
- [x] ruflo 仓库端到端测试通过

### Phase 6: 优化与扩展（v3.5）
- [x] MCP Server 实现（stdio JSON-RPC 2.0）
- [x] GraphifyNoLLM 零 LLM 查询（BFS/最短路径/Explain/Communities/GodNodes）
- [x] DiskGraphIndex 按需磁盘索引 + LRU 缓存（解决 OOM）
- [x] 并发查询优化（GitNexus + Graphify 并行执行）
- [x] Native 兜底引擎恢复（ripgrep 路径发现 + 类型修复）
- [x] Metrics 监控体系（查询延迟、缓存命中率、降级率、磁盘 IO）
- [x] 自动更新维护（git diff 检测、阈值触发、静默时段）
- [x] Settings 配置集成（四级配置合并）

### Phase 7: 全局化与质量量化（v3.6）
- [x] GlobalIndex 全局仓库索引管理（统一管理所有仓库/分支/HEAD）
- [x] MCP Server HTTP/SSE 多传输层支持（stdio / HTTP / SSE）
- [x] VectorCache L2 语义缓存（3-gram TF + 余弦相似度）
- [x] Quality Metrics 查询质量指标（精度/召回/F1/噪音/引擎对比/置信度分布）
- [x] Engine readiness 检查与降级引导（L1.5 层）
- [x] Settings 支持 globalIndexRoot 自定义索引保存路径

### Phase 8: 高级优化（v3.7）
- [x] 倒排索引加速向量缓存查询（>10K 条目时启用 inverted index 筛选候选集）
- [x] 查询结果自动反馈闭环（RecordFeedback + AutoCalibrate 动态调整相似度阈值）
- [x] 跨仓库符号引用分析（DetectRepoDependencies + CrossRepoFindRefs 遍历依赖链）
- [x] 分支级 CoW 索引隔离（CreateBranchWithCow 硬链接共享大文件 + MaterializeBranch 独立化）

### Phase 9: 超大规模与智能反馈（v3.9，当前）
- [x] 增量分支索引（IncrementalIndexManager：git diff → 受影响符号 → 查询合并）
- [x] MinHash LSH 替代倒排索引（超大规模缓存 >100K：128 hash / 16 bands / 8 rows）
- [x] 隐式反馈信号采集（ImplicitFeedbackCollector：30s 超时好评 / 相似重查差评）
- [x] 跨仓库结果去重与置信度融合（CrossRepoRefEntry 归一化 → 去重 → 置信度融合）

### Phase 10: 未来方向
- [ ] Grafana 仪表盘模板（导出 metrics.json 到 Prometheus/Grafana）
- [ ] 查询结果排序学习（Learning to Rank）：基于反馈训练结果排序模型
- [ ] 分布式索引支持（多机并行构建/查询，通过 gRPC 聚合）
- [ ] 自然语言到 Cypher 的 LLM 转换层（可选增强，默认关闭）

---

## 15. 参考

- [GitNexus](https://github.com/abhigyanpatwari/GitNexus) — Zero-Server Code Intelligence Engine (Node.js)
- [Graphify](https://github.com/safishamsi/graphify) — Knowledge Graph Engine (Python)
- [MCP Specification](https://modelcontextprotocol.io/) — Model Context Protocol
- [LadybugDB](https://github.com/ladybugdb/core) — GitNexus 图数据库后端
