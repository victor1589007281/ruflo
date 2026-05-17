# 大型代码库知识图谱构造方案 —— Graphify + GitNexus + Native (Final v2.1)

> **版本**: 2.1
> **日期**: 2026-05-17
> **范围**: Linux Kernel / MySQL 级别超大型代码库的 LLM 知识图谱构造与消费
> **目标**: 对内作为 claude-go 内置 Tool 使用；对外通过 MCP Server 向 Claude Desktop / Cursor / Cline 提供标准化代码智能服务
> **核心约束**: 构建流水线零 LLM Token，纯静态分析 + 图算法
> **核心架构**: **三引擎互补** — GitNexus（精确结构）+ Graphify（语义图谱）+ Native（原生兜底）

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

### 1.2 三引擎互补 + 双模暴露架构

本方案的核心设计是**三引擎互补查询**，通过**两种模式**暴露：

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                     三引擎查询路由器 (pkg/codeintel/query.go)                  │
│                                                                              │
│  ┌─────────────────────────┐  ┌─────────────────────────┐                   │
│  │  GitNexus 结构引擎       │  │  Graphify 语义引擎       │                   │
│  │  (gitnexus.go)          │  │  (graphify.go)          │                   │
│  │  ─────────────────────  │  │  ─────────────────────  │                   │
│  │  • navigate   符号导航   │  │  • communities 社区检测  │                   │
│  │  • impact     影响分析   │  │  • god_nodes   枢纽节点  │                   │
│  │  • find_refs  引用查找   │  │  • path        最短路径  │                   │
│  │  • cross_shard 跨片查询  │  │  • surprises   异常边    │                   │
│  │  数据源: SQLite + JSON   │  │  数据源: graph.json      │                   │
│  └─────────────────────────┘  └─────────────────────────┘                   │
│           │                              │                                   │
│           └──────────────┬───────────────┘                                   │
│                          ▼                                                   │
│              ┌─────────────────────┐                                         │
│              │   Native 原生兜底    │                                         │
│              │   (native.go)       │                                         │
│              │   ───────────────── │                                         │
│              │   • native_grep     │  ← 索引缺失/过期时自动降级                │
│              │   • native_read     │  ← 实时文本搜索，零存储依赖               │
│              │   • fallback_search │  ← Grep → Read 自动组合                  │
│              └─────────────────────┘                                         │
└─────────────────────────────────────────────────────────────────────────────┘
         │                                    │
    ┌────┴────┐                        ┌─────┴──────┐
    ▼         ▼                        ▼            ▼
┌────────┐ ┌─────────────┐      ┌──────────┐  ┌─────────────┐
│ 对内    │ │ 对内         │      │ 对外      │  │ 对外         │
│内置 Tool│ │内置 Tool    │      │MCP Server│  │MCP Server   │
├────────┤ ├─────────────┤      ├──────────┤  ├─────────────┤
│ init   │ │ status      │      │ stdio    │  │ stdio        │
│ update │ │ query       │      │ JSON-RPC │  │ JSON-RPC     │
│ branch │ │             │      │ 2.0      │  │ 2.0          │
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

**对外协议选型结论：MCP Server（stdio）**

| 候选协议 | 支持情况 | 配置复杂度 | 结论 |
|----------|----------|-----------|------|
| **MCP Server (stdio)** | Claude Desktop ✓, Cursor ✓, Cline ✓, Windsurf ✓ | 极低（一行配置） | **选用** |
| MCP Server (HTTP/SSE) | 同上 | 需端口/防火墙 | 备选（未来扩展） |
| gRPC | 无原生支持 | 高 | 不选 |
| REST API | 需自定义客户端 | 中 | 不选 |
| CLI 子命令 | 仅限终端 | 低 | 已内置 |

MCP 是**事实标准**，Claude/Cursor/Cline 均原生支持，零网络配置（stdio 子进程通信），工具发现、调用、资源读取统一协议。

---

## 2. 对内：内置 Tool 套件

### 2.1 Tool 列表

注册于 `pkg/tool/builtin/register.go`，与 FileRead、Bash、Grep 等并列。

| Tool | 名称 | 只读 | 并发安全 | 功能 |
|------|------|------|----------|------|
| `code_intel_init` | 初始化构建 | ✗ | ✗ | 仓库全量索引，自动/手动分片 |
| `code_intel_update` | 增量更新 | ✗ | ✗ | git diff 检测变更，仅重建受影响分片 |
| `code_intel_status` | 状态查询 | ✓ | ✓ | 查看分片、分支、索引时间、文件数 |
| `code_intel_query` | 图谱查询 | ✓ | ✓ | navigate / impact / communities / god_nodes / cross_shard |
| `code_intel_branch` | 分支管理 | ✗ | ✗ | switch / create (CoW) / delete / list |

### 2.2 Tool 调用示例

```json
// code_intel_init
{
  "repo_path": "/home/victor/base/git/linux",
  "auto_shard": true
}

// code_intel_query — 符号导航
{
  "repo_path": "/home/victor/base/git/linux",
  "query_type": "navigate",
  "symbol": "tcp_sendmsg",
  "shard": "net",
  "depth": 2
}

// code_intel_query — 社区概览
{
  "repo_path": "/home/victor/base/git/linux",
  "query_type": "communities",
  "shard": "net"
}

// code_intel_branch — 创建 feature 分支索引
{
  "repo_path": "/home/victor/base/git/linux",
  "action": "create",
  "branch_name": "feature-tcp-optim",
  "base_branch": "main"
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

### 3.4 MCP 暴露的工具

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

### 3.5 MCP Server 实现

位于 `pkg/codeintel/mcpserver.go`，JSON-RPC 2.0 over stdio：

```
stdin  → [JSON-RPC Request]  → handleRequest()
                                    │
                                    ▼
                         ┌──────────────────┐
                         │ initialize       │
                         │ tools/list       │
                         │ tools/call       │──→ executeTool() ──→ Engine/Builder/BranchMgr
                         │ resources/list   │
                         └──────────────────┘
                                    │
stdout ← [JSON-RPC Response] ←──────┘
```

---

## 4. 构建流水线（零 LLM Token）

### 4.1 设计原则

**构建过程不使用 LLM**。所有阶段均为纯静态分析或图算法：

| 阶段 | 技术 | Token 消耗 | 说明 |
|------|------|-----------|------|
| 1. 文件枚举 | filepath.Walk | 0 | 按分片配置遍历源码 |
| 2. AST 解析 | Tree-sitter (占位) | 0 | 提取符号、类型、调用关系 |
| 3. 调用图构建 | 内存图结构 | 0 | 函数调用、类型继承、引用关系 |
| 4. 社区检测 | Leiden 算法 (占位) | 0 | 基于边权重划分语义社区 |
| 5. God Node | 度数/中心性统计 | 0 | 识别高度数枢纽节点 |
| 6. 跨片边 | 符号解析 | 0 | 导出/导入符号匹配 |
| 7. 语义摘要 | LLM (可选) | 可调 | **默认关闭**，仅增强模式启用 |

### 4.2 分片构建流水线

```go
// builder.go — BuildAll 全量构建
func (b *Builder) BuildAll(branchName string, progress chan<- BuildProgress) (*BranchIndex, error) {
    for _, shard := range b.Config.Shards {
        // 1. 枚举文件
        files := enumerateFiles(shard)
        
        // 2. AST 解析 + 符号提取
        symbols, edges := parseAST(files)
        
        // 3. 写入 SQLite 索引
        insertSymbols(db, symbols)
        insertEdges(db, edges)
        
        // 4. 调用图 JSON
        saveJSON(callgraphPath, buildCallgraph(symbols, edges))
        
        // 5. Leiden 社区检测
        communities := detectCommunities(symbols, edges)
        
        // 6. God Node 识别
        godNodes := identifyGodNodes(symbols, edges)
        
        // 7. 清单生成
        manifest := buildManifest(symbols, edges)
    }
    // 8. 跨片边提取
    extractCrossEdges(branchIndex)
}
```

### 4.3 增量更新

```go
// builder.go — IncrementalUpdate
func (b *Builder) IncrementalUpdate(branchName string, ...) (*BranchIndex, error) {
    changedFiles := gitDiff()           // git diff --name-only
    affectedShards := mapFilesToShards(changedFiles)
    for _, shard := range affectedShards {
        rebuildShard(shard)              // 仅重建受影响分片
    }
    updateCrossEdges()                   // 增量更新跨片边
}
```

---

## 5. 存储与分支隔离

### 5.1 存储布局

```
~/.claude-code-intel/
├── <repo-hash>/                       # 以仓库路径哈希为根
│   ├── config.yaml                    # 仓库配置（分片边界、构建参数）
│   ├── branches/
│   │   ├── main/                      # main 分支索引
│   │   │   ├── branch.json            # 分支元数据（Commit、Shards 列表）
│   │   │   ├── shards/
│   │   │   │   ├── net/
│   │   │   │   │   ├── index.json     # 分片元数据（文件数、符号数、社区）
│   │   │   │   │   ├── ast.db         # SQLite AST 索引
│   │   │   │   │   ├── callgraph.json # 调用图
│   │   │   │   │   ├── graphify/      # 社区检测结果
│   │   │   │   │   │   └── graph.json # NetworkX 风格图
│   │   │   │   │   └── manifest.yaml  # 跨片接口符号（exports/imports）
│   │   │   │   ├── mm/
│   │   │   │   ├── kernel/
│   │   │   │   └── ...
│   │   │   └── cross_edges.db         # KuzuDB/SQLite 跨片边
│   │   └── feature-tcp-optim/         # feature 分支
│   │       ├── branch.json            # CoW 复制的元数据
│   │       ├── shards/
│   │       │   ├── net/               # 若 net/ 被修改，独立存储
│   │       │   │   ├── index.json
│   │       │   │   ├── ast.db
│   │       │   │   └── ...
│   │       │   ├── mm/
│   │       │   │   └── .ref           # 引用文件 → 指向 main/mm/（共享）
│   │       │   └── ...
│   │       └── cross_edges.db
│   └── shared/                        # 分支间共享的只读数据（如语言模型缓存）
```

### 5.2 分支隔离策略：CoW 元数据 + 共享只读数据

| 操作 | 实现 |
|------|------|
| **创建分支** | 复制 `branch.json` 元数据；各分片目录写 `.ref` 文件指向 base 分支 |
| **切换分支** | 修改活跃分支指针（未来扩展：config.yaml 中的 `active_branch`） |
| **修改分片** | 若某分片在新分支有变更，创建独立的 `ast.db`/`callgraph.json`；无变更则通过 `.ref` 共享 |
| **删除分支** | 仅删除分支目录中的元数据和独立分片；共享数据保留（由引用计数管理，未来扩展） |

```go
// branch.go — CreateBranch (CoW)
func (bm *BranchManager) CreateBranch(baseBranch, newBranch string) error {
    baseIdx := loadBranchIndex(baseBranch)
    newIdx := &BranchIndex{
        BranchName: newBranch,
        Shards:     copyMetadata(baseIdx.Shards),  // 元数据复制
    }
    for name := range baseIdx.Shards {
        shardDir := bm.Store.ShardDir(newBranch, name)
        baseShardDir := bm.Store.ShardDir(baseBranch, name)
        os.MkdirAll(shardDir, 0755)
        os.WriteFile(filepath.Join(shardDir, ".ref"), []byte(baseShardDir), 0644)
    }
}
```

---

## 6. 查询引擎（三引擎互补）

### 6.1 查询路由

```
                    ┌─────────────────┐
    user query ───▶ │  Router (query) │
                    └────────┬────────┘
                             │
           ┌─────────────────┼─────────────────┐
           ▼                 ▼                 ▼
    ┌─────────────┐  ┌─────────────┐  ┌─────────────┐
    │  GitNexus   │  │  Graphify   │  │   Native    │
    │  结构引擎    │  │  语义引擎    │  │  原生兜底    │
    └─────────────┘  └─────────────┘  └─────────────┘
           │                 │                 │
           ▼                 ▼                 ▼
    SQLite + JSON     graph.json          grep/read
    （精确导航）        （社区/路径）        （实时文本）
```

| 查询类型 | 引擎 | 对应能力 | 数据源 | 延迟目标 |
|----------|------|----------|--------|----------|
| `navigate` | **GitNexus** | 符号定义 + 调用方/被调用方 | SQLite AST | < 50ms |
| `impact` | **GitNexus** | 文件依赖半径 | callgraph.json | < 100ms |
| `find_refs` | **GitNexus** | 符号所有引用位置 | SQLite edges | < 50ms |
| `cross_shard` | **GitNexus** | 跨片引用 | cross_edges.json | < 30ms |
| `communities` | **Graphify** | Leiden 社区 + God Nodes | graphify/graph.json | < 200ms |
| `god_nodes` | **Graphify** | 高度数节点排名 | index.json | < 50ms |
| `path` | **Graphify** | 两符号间最短路径 | callgraph.json (BFS) | < 200ms |
| `surprises` | **Graphify** | 异常边（跨社区/高权重） | callgraph + community | < 200ms |
| `native_grep` | **Native** | 实时文本搜索 | 文件系统 | < 500ms |
| `native_read` | **Native** | 实时文件读取 | 文件系统 | < 50ms |

### 6.2 降级策略

当索引引擎（GitNexus / Graphify）查询失败时，**自动降级**到 Native 引擎：

1. **优先索引**：navigate → GitNexus SQLite 查询
2. **索引缺失**：SQLite 未找到 → Native.Grep(symbol) + Native.ReadFile()
3. **返回标记**：降级结果带 `"_fallback": true` 和 `"_original_error"` 字段

```go
// query.go — 自动降级示例
func (e *Engine) Navigate(branchName string, q NavigateQuery) (*QueryResult, error) {
    qr, err := e.GitNexus.Navigate(branchName, q)
    if err != nil {
        return e.nativeFallback("navigate", q.Symbol, err)  // 自动降级
    }
    return qr, nil
}
```

### 6.2 Token 效率对比

**场景: "理解 TCP 拥塞控制子系统"**

**Before (纯 Read/Grep)**:
```
1. Grep "tcp_congestion_control" → 18 个文件, 200 个匹配
2. Read net/ipv4/tcp_cong.c → 350 行
3. Read net/ipv4/tcp_bbr.c → 420 行
4. Grep "struct tcp_congestion_ops" → 12 个匹配
Total: ~1,600 行 → ~4,800 Token
```

**After (三引擎互补查询)**:
```
1. code_intel_query query_type="communities" shard="net"
   → Graphify: {communities: [{id: 3, label: "Congestion Control", files: [...], core_nodes: ["tcp_congestion_control", "bbr_main"]}]}
   Token: ~120

2. code_intel_query query_type="navigate" symbol="tcp_congestion_control" depth=2
   → GitNexus: {def: "net/ipv4/tcp_cong.c:142", callers: [...], callees: [...]}
   Token: ~150

3. code_intel_query query_type="path" symbol="tcp_sendmsg" target_symbol="tcp_congestion_control"
   → Graphify: {path: ["tcp_sendmsg", "tcp_transmit_skb", "tcp_congestion_control"], length: 3}
   Token: ~80

4. 按需 native_read 2 个文件的特定行 (60 行)
   Token: ~180
Total: ~530 Token (9x 节省)
```

---

## 7. 实现文件清单

### 7.1 新增文件

```
pkg/codeintel/
├── types.go           # 核心类型：RepoConfig, ShardConfig, ShardIndex, BranchIndex, QueryResult...
├── store.go           # 存储抽象：SQLite/JSON/YAML 读写，目录管理
├── shard.go           # 分片管理与自动检测（按顶层目录分组）
├── builder.go         # 构建流水线：全量构建 + 增量更新（零 LLM）
├── gitnexus.go        # GitNexus 结构引擎：navigate/impact/find_refs/cross_shard
├── graphify.go        # Graphify 语义引擎：communities/god_nodes/path/surprises
├── native.go          # Native 原生兜底：native_grep/native_read/fallback_search
├── query.go           # 三引擎查询路由器：自动路由 + 降级策略
├── branch.go          # 分支隔离：CoW 元数据 + 共享只读数据
└── mcpserver.go       # MCP Server：JSON-RPC 2.0 over stdio

pkg/tool/builtin/
└── codeintel_tools.go # 5 个内置 Tool 实现（init/update/status/query/branch）
```

### 7.2 修改文件

```
pkg/tool/builtin/register.go    # 注册 5 个 codeintel tools
```

### 7.3 依赖

```
github.com/mattn/go-sqlite3    # 已有（AST 索引）
gopkg.in/yaml.v3               # 新增（配置/清单序列化）
```

**占位待集成（未来扩展，不影响编译）**:
- `github.com/smacker/go-tree-sitter` — AST 解析
- `gonum/graph` 或自实现 — Leiden 社区检测
- KuzuDB Go driver — 跨片图边存储

---

## 8. 与 claude-go 工作流的集成

### 8.1 PreToolUse Hook 增强（未来扩展）

```go
// 在 Claude 执行 Read/Grep 前，自动重定向为图谱查询
if tool == "Grep" && args.pattern == "tcp_congestion_control" {
    return { redirect: { tool: "code_intel_query", args: {
        query_type: "navigate",
        symbol: "tcp_congestion_control",
        shard: "net",
        depth: 2
    }}}
}
```

### 8.2 PostToolUse Hook 自动重索引（未来扩展）

```go
if tool == "Bash" && strings.Contains(args.command, "git commit") {
    staleShards := detectStaleShards()
    if len(staleShards) > 0 {
        suggest: fmt.Sprintf("Index stale for shards: %v. Run code_intel_update?", staleShards)
    }
}
```

---

## 9. 风险与缓解

| 风险 | 影响 | 缓解 |
|------|------|------|
| Tree-sitter Go binding 集成复杂度 | 中 | 当前为占位实现，不影响整体架构；可渐进替换 |
| Leiden 算法 Go 实现 | 中 | 先用 NetworkX 风格图 + 简化社区检测；后续替换为完整 Leiden |
| KuzuDB Go driver 成熟度 | 低 | 先用 SQLite 存储跨片边；KuzuDB 作为未来优化 |
| 千万行代码首次构建耗时 | 高 | 夜间全量跑；日常增量 < 5 分钟 |
| 存储空间占用 | 中 | 按分片拆分；CoW 分支共享未变更数据 |
| MCP 工具命名冲突 | 低 | 统一 `code_intel_*` 前缀命名 |

---

## 10. 实施路线图

### Phase 1: 骨架可用（已完成）
- [x] 核心类型与接口（types.go）
- [x] 存储抽象（store.go）
- [x] 分片自动检测（shard.go）
- [x] 构建流水线骨架（builder.go）
- [x] 查询引擎骨架（query.go）
- [x] 分支 CoW 管理（branch.go）
- [x] MCP Server（mcpserver.go）
- [x] 5 个内置 Tool（codeintel_tools.go）
- [x] Tool 注册（register.go）

### Phase 2: AST 解析集成（已完成）
- [x] 集成 `go-tree-sitter` 实现真实 AST 解析（8 种语言：C/C++/Go/Java/JS/TS/Python/Rust）
- [x] 符号提取（函数、类型、变量、宏）
- [x] 调用关系提取

### Phase 3: 图算法集成（已完成）
- [x] 实现简化版 Leiden 社区检测（模块度优化 + 社区聚合）
- [x] God Node 识别（PageRank × log(degree+1)）
- [x] 跨片边精确提取
- [x] 新增：最短路径（BFS）、异常边检测（surprises）

### Phase 4: 三引擎架构重构（已完成）
- [x] 拆分 GitNexus 结构引擎（gitnexus.go）
- [x] 拆分 Graphify 语义引擎（graphify.go）
- [x] 新增 Native 原生兜底引擎（native.go）
- [x] 查询路由器 + 自动降级策略（query.go）
- [x] 并行分片构建
- [x] 增量更新性能优化（变更文件精准映射）

### Phase 5: 性能与运维（未来）
- [ ] 索引过期检测 + 自动重索引 Hook
- [ ] 存储压缩（大仓库分卷）
- [ ] 完整 Leiden 算法替换（当前为简化版）
- [ ] KuzuDB 跨片图边存储（当前为 JSON）

---

## 11. 参考

- [GitNexus](https://github.com/abhigyanpatwari/GitNexus) — Zero-Server Code Intelligence Engine
- [Tree-sitter](https://tree-sitter.github.io/tree-sitter/) — 增量解析器
- [Leiden Algorithm](https://arxiv.org/abs/1810.08473) — 社区检测
- [MCP Specification](https://modelcontextprotocol.io/) — Model Context Protocol
- [KuzuDB](https://github.com/kuzudb/kuzu) — 嵌入式图数据库
- [Zhao et al., ICSE 2023](https://ieeexplore.ieee.org/abstract/document/10172761) — 增量调用图构建
