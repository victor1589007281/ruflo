# 大型代码库知识图谱构造方案 —— Graphify + GitNexus + Claude

> **版本**: 1.0
> **日期**: 2026-05-16
> **范围**: Linux Kernel / MySQL 级别超大型代码库的 LLM 知识图谱构造与消费
> **目标**: 在单台工作站上完成千万行级代码库的多模态图谱构建，通过 MCP 向 Claude 提供 71.5x Token 效率的代码智能查询

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

### 1.2 双引擎互补架构

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Claude Code / Claude Desktop                       │
│                              (消费者层)                                       │
└─────────────────────────────────────────────────────────────────────────────┘
                                    │
                        ┌───────────┴───────────┐
                        │      MCP Router        │
                        │   (claude-go 工具表)   │
                        └───────────┬───────────┘
                                    │
        ┌───────────────────────────┼───────────────────────────┐
        ▼                           ▼                           ▼
┌───────────────┐         ┌─────────────────┐         ┌───────────────┐
│  GitNexus     │         │  Graphify       │         │ 原生工具       │
│  (结构索引)   │         │  (语义图谱)     │         │ (Grep/Read)   │
│  ─────────    │         │  ─────────      │         │ ─────────     │
│  LadybugDB    │         │  NetworkX       │         │ 实时搜索       │
│  Tree-sitter  │         │  Leiden 聚类    │         │ 精确读取       │
│  SCIP/LSP     │         │  LLM 语义提取   │         │ 兜底回退       │
└───────────────┘         └─────────────────┘         └───────────────┘
        │                           │                           │
        └───────────────────────────┴───────────────────────────┘
                                    │
                        ┌───────────┴───────────┐
                        │   统一查询编排器        │
                        │   (Query Orchestrator) │
                        └───────────┬───────────┘
                                    │
        ┌───────────────────────────┼───────────────────────────┐
        ▼                           ▼                           ▼
┌───────────────┐         ┌─────────────────┐         ┌───────────────┐
│ Linux Kernel  │         │ MySQL           │         │ PostgreSQL    │
│ graph-main/   │         │ graph-mysql/    │         │ graph-pg/     │
│ (分片存储)    │         │ (分片存储)      │         │ (分片存储)    │
└───────────────┘         └─────────────────┘         └───────────────┘
```

**分工原则**:

| 维度 | GitNexus | Graphify |
|------|----------|----------|
| **核心能力** | 精确的代码属性图（调用链、依赖、类型层级） | 语义社区检测、多模态提取、可视化 |
| **查询类型** | "谁调用了 `tcp_sendmsg`" / "InnoDB 缓冲池的所有引用" | "TCP 子系统的核心模块是什么" / "异常跨层调用" |
| **存储引擎** | LadybugDB（本地持久化，极速查询） | NetworkX + 增量缓存 |
| **更新策略** | Git hooks 自动增量重索引 | Watch 模式实时更新 |
| **MCP 暴露** | `gitnexus_mcp`（工具集 + Hooks） | `graphify_mcp`（查询 + 报告） |
| **最佳场景** | 精确导航、影响分析、PR Review | 宏观理解、新人 onboarding、架构审计 |

---

## 2. 大型代码库分片构造策略

千万行代码无法一次性加载到内存构建单张图。必须按**子系统分片**构建，再通过**跨片边**连接。

### 2.1 Linux Kernel 分片方案

```
linux-graph/
├── arch/                    # 架构抽象层 (x86, arm64, riscv)
│   ├── graph.json
│   └── manifest.json        # 跨片接口符号列表
├── kernel/                  # 核心调度、同步原语
├── mm/                      # 内存管理子系统
├── net/                     # 网络栈 (TCP/IP, socket)
│   ├── tcp/                 # TCP 拥塞控制可再分片
│   └── ipv4/
├── fs/                      # 文件系统 VFS + 具体实现
├── drivers/                 # 驱动框架 + 总线 (PCI, USB)
├── crypto/                  # 加密子系统
└── cross_edges.db           # 跨片子图边 (KuzuDB/SQLite)
```

**分片边界定义** (以 `net/` 为例):

```yaml
# net/manifest.yaml
shard_name: "net"
root_dirs: ["net/", "include/net/"]
interface_symbols:
  exports:          # 本片区对外暴露的符号
    - "tcp_sendmsg"
    - "tcp_recvmsg"
    - "sock_init_data"
    - "inet_ioctl"
  imports:          # 本片区依赖的外部符号
    - "kmalloc"          # 来自 mm/
    - "spin_lock"        # 来自 kernel/
    - "schedule"         # 来自 kernel/
    - "pci_register_driver"  # 来自 drivers/
```

**跨片边存储**:

```cypher
// cross_edges.db (KuzuDB)
(:符号 {名称, 类型, shard})-[:跨片调用]->(:符号 {名称, 类型, shard})
(:符号 {名称, 类型, shard})-[:跨片导入]->(:符号 {名称, 类型, shard})

// 示例: net/ 调用 mm/ 的 kmalloc
CREATE (s:符号 {名称: "tcp_sendmsg", shard: "net", 类型: "function"})
CREATE (t:符号 {名称: "kmalloc", shard: "mm", 类型: "function"})
CREATE (s)-[:跨片调用]->(t)
```

### 2.2 MySQL 分片方案

```
mysql-graph/
├── sql/                     # 解析器、优化器、执行器
├── storage/innobase/        # InnoDB 存储引擎
├── storage/myisam/          # MyISAM 引擎
├── storage/ndb/             # NDB Cluster
├── client/                  # 客户端协议
├── replication/             # 主从复制
├── backup/                  # 备份恢复
├── plugin/                  # 插件框架
└── cross_edges.db
```

**MySQL 特有的分层边界**:

| 层级 | 目录 | 关键跨片接口 |
|------|------|-------------|
| SQL 层 | `sql/` | `handler::` 虚函数接口 |
| 存储引擎 | `storage/*/`| `handlerton` 结构体 |
| 日志系统 | `log/` | `mysql_bin_log` 全局实例 |
| 复制 | `replication/` | `Binlog_sender`, `Relay_log_info` |

### 2.3 分片构建流水线

```bash
# 1. 安装双引擎
npm install -g gitnexus
pip install graphifyy

# 2. 配置分片边界
# linux-shards.yml (用户自定义)
shards:
  - name: net
    roots: ["net/", "include/net/"]
    max_files: 5000
  - name: mm
    roots: ["mm/", "include/linux/mm.h"]
    max_files: 3000
  # ...

# 3. GitNexus 分片索引
for shard in net mm kernel fs drivers; do
  gitnexus analyze \
    --shard $shard \
    --root linux/ \
    --include "$(yq ".shards[] | select(.name==\"$shard\") | .roots[]" linux-shards.yml)" \
    --output "linux-graph/$shard/"
done

# 4. Graphify 语义聚类 (在每个分片上运行)
for shard in net mm kernel fs drivers; do
  graphify ./linux-graph/$shard/ \
    --mode shard \
    --leiden-resolution 0.8 \
    --output ./linux-graph/$shard/graphify-out/
done

# 5. 跨片边提取
python3 extract_cross_edges.py \
  --manifest linux-shards.yml \
  --gitnexus-dir linux-graph/ \
  --output linux-graph/cross_edges.db
```

---

## 3. MCP 服务设计与 Claude 集成

### 3.1 GitNexus MCP 服务 (`gitnexus mcp`)

GitNexus 原生支持 MCP。对大型代码库，需要扩展配置以支持分片路由。

```json
// ~/.claude-go/mcp/gitnexus-mcp.json
{
  "mcpServers": {
    "gitnexus-linux": {
      "command": "gitnexus",
      "args": ["mcp", "--project", "/home/victor/base/git/linux", "--shard-dir", "/home/victor/base/git/linux-graph"],
      "env": {
        "GITNEXUS_LADYBUG_PATH": "/home/victor/base/git/linux-graph/.ladybug"
      }
    },
    "gitnexus-mysql": {
      "command": "gitnexus",
      "args": ["mcp", "--project", "/home/victor/base/git/mysql-server", "--shard-dir", "/home/victor/base/git/mysql-graph"]
    }
  }
}
```

**暴露的 MCP 工具** (GitNexus 原生):

| 工具名 | 输入 | 输出 | Token 成本 |
|--------|------|------|-----------|
| `gitnexus_navigate` | `symbol`, `depth` | 定义位置 + 调用方/被调用方 | ~150 Token |
| `gitnexus_impact` | `file_path` | 影响半径（依赖文件列表 + 依赖数） | ~200 Token |
| `gitnexus_type_hierarchy` | `type_name` | 继承链 + 实现者 | ~180 Token |
| `gitnexus_find_refs` | `symbol` | 所有引用位置 | ~250 Token |
| `gitnexus_cross_shard` | `symbol`, `target_shard` | 跨片调用关系 | ~120 Token |

### 3.2 Graphify MCP 服务 (`graphify serve`)

Graphify 提供 `serve.py` 模块，需包装为 MCP 服务。

```python
# graphify_mcp_bridge.py
from mcp.server import Server
from mcp.types import TextContent
import json

app = Server("graphify-bridge")

@app.call_tool()
async def graphify_query(name: str, arguments: dict):
    if name == "graphify_communities":
        shard = arguments["shard"]
        with open(f"{shard}/graphify-out/graph.json") as f:
            graph = json.load(f)
        # Leiden 社区摘要
        communities = extract_community_summary(graph)
        return [TextContent(type="text", text=json.dumps(communities, indent=2))]
    
    elif name == "graphify_god_nodes":
        shard = arguments["shard"]
        top_n = arguments.get("top_n", 10)
        gods = extract_highest_degree_nodes(shard, top_n)
        return [TextContent(type="text", text=json.dumps(gods, indent=2))]
    
    elif name == "graphify_explain":
        shard = arguments["shard"]
        node = arguments["node"]
        explanation = generate_node_explanation(shard, node)
        return [TextContent(type="text", text=explanation)]
    
    elif name == "graphify_surprises":
        shard = arguments["shard"]
        surprises = detect_cross_community_edges(shard)
        return [TextContent(type="text", text=json.dumps(surprises, indent=2))]
```

**Graphify MCP 工具**:

| 工具名 | 输入 | 输出 | 用途 |
|--------|------|------|------|
| `graphify_communities` | `shard` | 社区列表 + 核心节点 + 职责摘要 | 宏观理解子系统 |
| `graphify_god_nodes` | `shard`, `top_n` | 高度数节点排名 | 识别关键枢纽 |
| `graphify_explain` | `shard`, `node` | 自然语言解释该节点的职责 | 快速理解陌生符号 |
| `graphify_surprises` | `shard` | 异常跨社区连接 | 发现架构异味 |
| `graphify_path` | `shard`, `from`, `to` | 两节点间的最短路径 | 理解调用链 |

### 3.3 统一查询编排器 (Integration with claude-go)

在 `claude-go` 的现有四层混合架构基础上，将 Graphify + GitNexus 作为**新的第 2.5 层**插入：

```
┌────────────────────────────────────────────┐
│  第 4 层: 编排器 (claude-go orchestrator)   │
│  - 意图分类                                │
│  - Token 预算检查                          │
│  - 多层结果 RRF 排序                       │
│  - 上下文剪枝                              │
└────────────────────────────────────────────┘
                     │
    ┌────────────────┼────────────────┐
    ▼                ▼                ▼
┌────────┐    ┌────────────┐    ┌──────────┐
│ 第一层 │    │ 第 2.5 层  │    │ 第三层   │
│ Zoekt  │    │ GitNexus   │    │ SCIP/LSP │
│ (文本) │    │ +Graphify  │    │ (精确)   │
└────────┘    │ (结构+语义)│    └──────────┘
              └────────────┘
```

**意图路由规则**:

| 用户意图 | 首选层 | 工具 | 降级 |
|----------|--------|------|------|
| "`tcp_sendmsg` 在哪里定义" | GitNexus | `gitnexus_navigate` | Zoekt |
| "修改 VFS 会影响什么" | GitNexus | `gitnexus_impact` | 手工 grep |
| "TCP 子系统有哪些模块" | Graphify | `graphify_communities` | 读 Kconfig |
| "InnoDB 缓冲池的核心文件" | Graphify | `graphify_god_nodes` | 读目录 |
| "这个结构体有哪些实现" | SCIP/LSP | LSP `textDocument/implementation` | ctags |
| "认证逻辑在哪里" | 向量层 | CodeBERT + Qdrant | Zoekt |

**Token 预算检查**:

```go
// pkg/tool/builtin/code_intel.go (新增)
type CodeIntelBudget struct {
    MaxTokensPerQuery int // 默认 800
}

func (b *CodeIntelBudget) Check(queryType string, estimatedTokens int) error {
    if estimatedTokens > b.MaxTokensPerQuery {
        return fmt.Errorf("查询 %s 预估消耗 %d Token, 超出预算 %d", 
            queryType, estimatedTokens, b.MaxTokensPerQuery)
    }
    return nil
}
```

---

## 4. 与 claude-go 工作流的深度集成

### 4.1 Agent Skills 自动注入

GitNexus 的 `analyze` 命令会自动生成 `CLAUDE.md` / `AGENTS.md`。对于分片架构，需要扩展为按子系统生成。

```bash
# 为 Linux 网络子系统生成上下文
graphify ./linux-graph/net/ --output-context ./linux-graph/net/CLAUDE.md

# 内容示例 (自动生成的 CLAUDE.md)
# === TCP Subsystem Context ===
# Core Files: net/ipv4/tcp.c, net/ipv4/tcp_input.c, net/ipv4/tcp_output.c
# God Nodes: tcp_sendmsg (degree 142), tcp_recvmsg (degree 128), tcp_v4_do_rcv (degree 95)
# Communities:
#   - Congestion Control (tcp_cong.c, tcp_bbr.c, tcp_cubic.c)
#   - Connection Management (tcp_timer.c, tcp_fastopen.c)
#   - Data Path (tcp_input.c, tcp_output.c)
# Cross-Shard Interfaces:
#   - Calls mm/: kmalloc, kfree, page_frag_alloc
#   - Calls kernel/: spin_lock, rcu_read_lock
# Surprises: tcp_bbr.c has unexpected calls to crypto/ (should be net-only)
```

### 4.2 PreToolUse Hook (GitNexus 原生支持)

在 Claude 执行 `Read` 或 `Grep` 前，自动注入图谱上下文：

```typescript
// GitNexus PreToolUse Hook 伪代码
onPreToolUse(tool, args) {
  if (tool === "Read" && args.file.includes("net/ipv4/tcp")) {
    // 自动附加 TCP 子系统的社区摘要
    const context = gitnexus.getShardContext("net");
    return { 
      augmentedPrompt: `You are reading a file in the TCP subsystem.\n${context.communities}\nProceed with reading.` 
    };
  }
  if (tool === "Grep" && args.pattern === "tcp_congestion_control") {
    // 替换为图谱导航，节省 Token
    return { 
      redirect: { tool: "gitnexus_navigate", args: { symbol: "tcp_congestion_control", depth: 2 } }
    };
  }
}
```

### 4.3 PostToolUse Hook (自动重索引)

代码提交后自动检测索引过期：

```typescript
onPostToolUse(tool, args, result) {
  if (tool === "Bash" && args.command.includes("git commit")) {
    const staleShards = gitnexus.checkStaleShards();
    if (staleShards.length > 0) {
      return {
        suggestion: `Index stale for shards: ${staleShards.join(", ")}. Run reindex?`,
        autoAction: () => gitnexus.incrementalReindex(staleShards)
      };
    }
  }
}
```

---

## 5. 部署手册

### 5.1 环境准备

```bash
# 系统要求
# - RAM: 32GB+ (Linux Kernel 全量索引峰值占用 ~24GB)
# - Disk: 100GB+ SSD (索引文件约为源码的 3-5 倍)
# - OS: Linux (推荐 Ubuntu 22.04+)

# 安装依赖
sudo apt-get install -y ripgrep nodejs npm python3-pip

# 安装双引擎
npm install -g gitnexus
pip install graphifyy yq

# 配置 GitNexus (自动检测编辑器)
gitnexus setup
```

### 5.2 Linux Kernel 索引

```bash
# 1. 获取源码
git clone --depth=1 https://github.com/torvalds/linux.git /data/codebases/linux
cd /data/codebases/linux

# 2. 生成分片配置
python3 << 'EOF'
import os, yaml

subsystems = ['arch', 'kernel', 'mm', 'net', 'fs', 'drivers', 'crypto', 'lib']
shards = []
for s in subsystems:
    shards.append({
        'name': s,
        'roots': [f'{s}/'] if s != 'arch' else [f'{s}/x86/', f'{s}/arm64/', f'{s}/include/'],
        'max_files': 5000
    })

with open('linux-shards.yml', 'w') as f:
    yaml.dump({'shards': shards}, f)
EOF

# 3. 分片索引 (并行)
mkdir -p /data/graphs/linux
export GITNEXUS_LADYBUG_PATH=/data/graphs/linux/.ladybug

parallel -j 4 '
  shard={}
  echo "Indexing shard: $shard"
  gitnexus analyze \
    --shard $shard \
    --include "$(yq ".shards[] | select(.name==\"$shard\") | .roots[]" linux-shards.yml)" \
    --output /data/graphs/linux/$shard/
' ::: arch kernel mm net fs drivers crypto lib

# 4. Graphify 语义增强
parallel -j 4 '
  shard={}
  graphify /data/graphs/linux/$shard/ \
    --mode shard \
    --leiden-resolution 0.8 \
    --output /data/graphs/linux/$shard/graphify/
' ::: arch kernel mm net fs drivers crypto lib

# 5. 提取跨片边
python3 extract_cross_edges.py \
  --manifest linux-shards.yml \
  --ladybug-path /data/graphs/linux/.ladybug \
  --output /data/graphs/linux/cross_edges.kuzu

# 6. 注册 MCP
cat >> ~/.claude-go/mcp.json << 'MCP'
{
  "gitnexus-linux": {
    "command": "gitnexus",
    "args": ["mcp", "--shard-dir", "/data/graphs/linux"]
  },
  "graphify-linux": {
    "command": "python3",
    "args": ["/data/graphs/linux/graphify_mcp_bridge.py"]
  }
}
MCP
```

### 5.3 MySQL 索引

```bash
git clone --depth=1 https://github.com/mysql/mysql-server.git /data/codebases/mysql
cd /data/codebases/mysql

# MySQL 分片更简单：按存储引擎 + SQL 层分片
mkdir -p /data/graphs/mysql

gitnexus analyze --shard sql --include "sql/" --output /data/graphs/mysql/sql/
gitnexus analyze --shard innodb --include "storage/innobase/" --output /data/graphs/mysql/innodb/
gitnexus analyze --shard replication --include "replication/" --output /data/graphs/mysql/replication/

# Graphify
for shard in sql innodb replication; do
  graphify /data/graphs/mysql/$shard/ --mode shard --output /data/graphs/mysql/$shard/graphify/
done
```

---

## 6. 查询示例与 Token 效率对比

### 6.1 场景: "理解 TCP 拥塞控制子系统"

**Before (纯 Read/Grep)**:
```
1. Grep "tcp_congestion_control" → 18 个文件, 200 个匹配
2. Read net/ipv4/tcp_cong.c → 350 行
3. Read net/ipv4/tcp_bbr.c → 420 行
4. Read net/ipv4/tcp_cubic.c → 380 行
5. Grep "struct tcp_congestion_ops" → 12 个匹配
6. Read include/net/tcp.h → 相关 80 行
Total: ~1,600 行 → ~4,800 Token
```

**After (Graphify + GitNexus)**:
```
1. graphify_communities shard="net"
   → 返回 TCP Congestion Control 社区: {files: [tcp_cong.c, tcp_bbr.c, tcp_cubic.c], core_nodes: ["tcp_congestion_control", "bbr_main"], summary: "拥塞控制算法框架 + BBR/Cubic 实现"}
   Token: ~120

2. gitnexus_navigate symbol="tcp_congestion_control" depth=2
   → 定义 + 调用方 + 被调用方 (精确位置)
   Token: ~150

3. 按需 Read 3 个文件的特定行 (80 行)
   Token: ~240
Total: ~510 Token (9.4x 节省)
```

### 6.2 场景: "修改 `innobase/buf` 会影响什么"

**Before**:
```
1. Grep "buf_pool_t" → 47 个文件
2. 手动筛选相关文件
3. Read 10 个文件的关键部分
Total: ~3,000 Token
```

**After**:
```
1. gitnexus_impact file_path="storage/innobase/buf/buf0buf.cc"
   → 影响半径: {direct: 12 files, transitive: 34 files, top_callers: ["buf_page_get_gen", "buf_pool_init"]}
   Token: ~200

2. graphify_surprises shard="innodb"
   → 发现 buf/ 社区与 log/ 社区有异常密集的跨社区调用
   Token: ~180
Total: ~380 Token (7.9x 节省)
```

### 6.3 场景: "找出 Linux 中所有内存分配相关调用"

**Before**:
```
Grep "kmalloc\|kzalloc\|vmalloc" → 5,000+ 匹配
无法直接消费
```

**After**:
```
1. gitnexus_cross_shard symbol="kmalloc" target_shard="*"
   → 返回跨片调用统计: {net: 142, fs: 98, drivers: 523, ...}
   Token: ~250

2. graphify_god_nodes shard="mm" top_n=5
   → 发现 "kmalloc" 是 mm/ 子系统的最高度节点 (degree 1,247)
   Token: ~100
Total: ~350 Token + 可操作性结果
```

---

## 7. 存储与性能优化

### 7.1 存储预算

| 代码库 | 源码大小 | GitNexus 索引 | Graphify 输出 | 跨片边 | 总计 |
|--------|----------|--------------|--------------|--------|------|
| Linux Kernel | ~3GB | ~8GB | ~2GB | ~500MB | ~11GB |
| MySQL | ~800MB | ~2GB | ~600MB | ~100MB | ~3GB |
| PostgreSQL | ~300MB | ~800MB | ~250MB | ~50MB | ~1.1GB |

### 7.2 查询延迟 SLA

| 操作 | 延迟 | 条件 |
|------|------|------|
| GitNexus `navigate` | < 50ms | LadybugDB 热缓存 |
| GitNexus `impact` | < 100ms | 2 跳内 |
| Graphify `communities` | < 200ms | graph.json 已加载 |
| Graphify `god_nodes` | < 50ms | NetworkX 内存图 |
| 跨片边查询 | < 30ms | KuzuDB 本地 |

### 7.3 内存管理

```python
# graphify_mcp_bridge.py 中的内存控制
import resource

# 限制每个分片的内存占用
MAX_SHARD_MEMORY_MB = 4096

def load_shard_graph(shard_path):
    """按需加载分片图，LRU 淘汰"""
    if shard_path in _graph_cache:
        return _graph_cache[shard_path]
    
    # 检查内存压力
    usage_mb = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024
    while usage_mb > MAX_SHARD_MEMORY_MB * 0.8:
        # LRU 淘汰
        evict_oldest_shard()
    
    graph = nx.readwrite.json_graph.node_link_graph(
        json.load(open(f"{shard_path}/graphify-out/graph.json"))
    )
    _graph_cache[shard_path] = graph
    return graph
```

---

## 8. 与现有 claude-go 架构的融合点

### 8.1 复用组件

| claude-go 已有组件 | 复用方式 |
|-------------------|----------|
| `modelconfig.ResolvedConfig` | 扩展 `CodeIntelShardConfig` 字段，支持分片参数 |
| `api.RateLimitGuard` | MCP 工具调用也走 Guard，防止 Graphify LLM 调用超限 |
| `orchestrator.Engine` | 分片构建流水线本身是一个 DAG，可用 Engine 调度 |
| `hooks.Hooks` | PreToolUse/PostToolUse 接入 GitNexus 原生 hooks |
| `feishu.Bot` | 索引进度通知、长时间构建的飞书状态推送 |

### 8.2 新增组件

```
claude-go/pkg/codeintel/
├── shard_manager.go         # 分片生命周期管理
├── cross_edge_store.go      # 跨片边存储 (KuzuDB 封装)
├── query_router.go          # 意图 → 层 路由
├── token_budget.go          # Token 预算检查
├── mcp_bridge_gitnexus.go   # GitNexus MCP 客户端
├── mcp_bridge_graphify.go   # Graphify MCP 客户端
└── context_pruner.go        # 结果剪枝 (去重/截断/社区过滤)
```

---

## 9. 风险与缓解

| 风险 | 影响 | 缓解 |
|------|------|------|
| Linux Kernel 索引耗时过长 | 高 | 首次全量夜间跑；后续增量 < 5 分钟 |
| LadybugDB 单文件过大 | 中 | 按分片拆分；> 2GB 自动分卷 |
| Tree-sitter 解析内联 ASM 失败 | 低 | 跳过 `.S` 文件；记录警告 |
| Graphify Leiden 聚类粒度不当 | 中 | 可调 resolution 参数；人工 review 社区摘要 |
| MCP 工具过多导致 Claude 困惑 | 中 | 工具命名规范 (`gitnexus_*`, `graphify_*`)；系统提示词中提供使用示例 |
| 跨片边遗漏 | 中 | 定期全量校验 (CI  nightly)；遗漏时降级到 Zoekt |

---

## 10. 实施路线图

### Phase 1: 单分片验证 (1 周)
- [ ] 选 Linux `net/` 或 MySQL `storage/innobase/` 作为试点
- [ ] 跑通 `gitnexus analyze` + `graphify` 完整流水线
- [ ] 验证 MCP 工具在 Claude 中的可用性
- [ ] 测量 Token 节省率 (目标 > 5x)

### Phase 2: 多分片 + 跨片 (1 周)
- [ ] 完成全部分片索引
- [ ] 实现 `extract_cross_edges.py`
- [ ] 集成 `cross_shard` 查询工具
- [ ] 性能基准测试 (延迟 < 100ms P99)

### Phase 3: claude-go 深度集成 (1 周)
- [ ] 实现 `pkg/codeintel/` 模块
- [ ] 接入 `PreToolUse` / `PostToolUse` hooks
- [ ] Token 预算强制检查
- [ ] 飞书通知集成 (索引进度、过期告警)

### Phase 4: 自动化运维 (持续)
- [ ] Git hooks 自动增量重索引
- [ ] CI nightly 全量校验
- [ ] 社区摘要自动生成与更新
- [ ] 基于实际查询日志优化意图路由模型

---

## 11. 参考

- [GitNexus](https://github.com/abhigyanpatwari/GitNexus) — Zero-Server Code Intelligence Engine
- [Graphify](https://graphify.net/) — Knowledge Graphs for AI Coding Assistants
- [code-retrieval-design.md](./code-retrieval-design.md) — 本仓库原有的代码检索四层架构设计
- [Zoekt](https://github.com/sourcegraph/zoekt) — Google 代码搜索引擎
- [KuzuDB](https://github.com/kuzudb/kuzu) — 嵌入式图数据库
- [Tree-sitter](https://tree-sitter.github.io/tree-sitter/) — 增量解析器
- [Leiden Algorithm](https://arxiv.org/abs/1810.08473) — 社区检测
- [Zhao et al., ICSE 2023](https://ieeexplore.ieee.org/abstract/document/10172761) — 增量调用图构建
