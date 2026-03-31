# Ruflo V3 自动进化机制详解

## 1. 进化机制总览

Ruflo V3 实现了多层次的 **自进化能力**，核心思路是：**从 Agent 执行结果中学习模式，跨会话积累经验，自动优化后续行为**。

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '14px', 'fontFamily': 'Arial', 'primaryColor': '#e8f4fd', 'primaryTextColor': '#1a1a2e'}}}%%
graph TB
    subgraph LAYER1["<b>🔄 层 1: 运行时学习</b>"]
        LLM_HOOK["<b>LLM 钩子</b><br/>缓存 + 模式提取"]
        PATTERN["<b>模式存储</b><br/>ReasoningBank"]
        ROUTE["<b>智能路由</b><br/>Agent/Model 选择"]
    end

    subgraph LAYER2["<b>🧠 层 2: SONA 自适应</b>"]
        TRAJ["<b>轨迹记录</b><br/>观察→思考→行动→结果"]
        VERDICT["<b>判定奖励</b><br/>success/failure 信号"]
        LORA["<b>LoRA 蒸馏</b><br/>置信度增量更新"]
        EWC["<b>EWC++ 整合</b><br/>防止灾难性遗忘"]
    end

    subgraph LAYER3["<b>📜 层 3: 治理进化</b>"]
        LEDGER["<b>运行账本</b><br/>记录每次执行"]
        EVAL["<b>评估器</b><br/>测试/违规/质量打分"]
        OPTIMIZER["<b>优化器循环</b><br/>提升胜出规则"]
    end

    subgraph LAYER4["<b>🔧 层 4: 后台 Worker</b>"]
        ULTRALEARN["ultralearn"]
        OPTIMIZE["optimize"]
        CONSOLIDATE["consolidate"]
        PREDICT["predict"]
    end

    LLM_HOOK --> PATTERN
    PATTERN --> ROUTE
    ROUTE -->|反馈| LLM_HOOK

    TRAJ --> VERDICT --> LORA --> EWC
    EWC -->|持久化| PATTERN

    LEDGER --> EVAL --> OPTIMIZER
    OPTIMIZER -->|规则更新| ROUTE

    ULTRALEARN --> PATTERN
    OPTIMIZE --> ROUTE
    CONSOLIDATE --> EWC
    PREDICT --> ROUTE

    style LAYER1 fill:#e8f5e9,stroke:#4caf50,stroke-width:2px
    style LAYER2 fill:#e3f2fd,stroke:#2196f3,stroke-width:2px
    style LAYER3 fill:#f3e5f5,stroke:#9c27b0,stroke-width:2px
    style LAYER4 fill:#fff3e0,stroke:#ff9800,stroke-width:2px
```

---

## 2. 层 1: 运行时学习 — LLM 钩子

### 2.1 Pre-LLM 钩子

每次 LLM API 调用前执行：

```
1. 生成缓存键 = base64(provider + model + messages + temp + maxTokens)
2. 查询缓存 (TTL: 1小时, 最大 1000 条)
   - 命中 → 跳过 LLM 调用，返回缓存响应 ($0 成本!)
   - 未命中 → 继续
3. 加载提供商优化参数:
   - Anthropic: temperature=0.7, "Be concise and direct"
   - OpenAI: temperature=0.8, "Respond in structured format"
4. 应用优化到请求
5. 记录指标: llm.calls.{provider}.{model}
6. 存储请求到内存: llm:request:{correlationId}
```

### 2.2 Post-LLM 钩子

每次 LLM API 响应后执行：

```
1. 缓存响应 (LRU 淘汰)
2. 记录指标: 延迟、token 使用、成本
3. 如果响应较长:
   - 提取有价值的模式 (extractPatternFromResponse)
   - 存入 ReasoningBank 向量数据库
4. 记录到内存: llm:response:{correlationId}
```

### 2.3 ReasoningBank 模式学习

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '14px', 'fontFamily': 'Arial'}}}%%
graph LR
    INPUT["<b>输入文本</b>"]
    EMBED["<b>嵌入</b><br/>MiniLM-L6<br/>384 维向量"]
    HNSW["<b>HNSW 搜索</b><br/>M=16, ef=100"]
    MATCH["<b>匹配判定</b>"]
    STORE["<b>存储</b><br/>短期 → 长期"]

    INPUT --> EMBED --> HNSW --> MATCH
    MATCH -->|新模式| STORE
    MATCH -->|已存在| UPDATE["<b>更新</b><br/>usageCount++<br/>quality 调整"]

    subgraph PROMOTION["<b>晋升机制</b>"]
        SHORT["短期记忆<br/>(max 1000)"]
        LONG["长期记忆<br/>(max 5000)"]
        SHORT -->|usageCount ≥ 3<br/>quality ≥ 0.6| LONG
    end

    STORE --> SHORT

    style INPUT fill:#e8f5e9,stroke:#4caf50,stroke-width:2px
    style HNSW fill:#e3f2fd,stroke:#2196f3,stroke-width:2px
    style PROMOTION fill:#fff3e0,stroke:#ff9800,stroke-width:2px
```

**关键配置**:
- `dimensions`: 384 (MiniLM-L6-v2)
- `hnswM`: 16 (每层最大连接数)
- `hnswEfConstruction`: 200 (构建精度)
- `hnswEfSearch`: 100 (搜索精度)
- `promotionThreshold`: 3 (使用 3 次晋升)
- `qualityThreshold`: 0.6 (质量阈值)
- `dedupThreshold`: 0.95 (去重阈值)

---

## 3. 层 2: SONA 自适应神经架构

### 3.1 SONA 四步流水线

SONA (Self-Optimizing Neural Architecture) 实现了一个本地模式学习系统：

| 步骤 | 操作 | 实现细节 |
|------|------|---------|
| **RETRIEVE** | 检索相关模式 | HNSW O(log n) 搜索，150x-12,500x 加速 |
| **JUDGE** | 评估判定 | success/failure verdict + 奖励信号 shaping |
| **DISTILL** | 提炼学习 | LoRA 风格置信度更新 |
| **CONSOLIDATE** | 防止遗忘 | EWC++ 弹性权重整合 |

### 3.2 轨迹记录

```typescript
interface TrajectoryStep {
  type: 'observation' | 'thought' | 'action' | 'result';
  content: string;
  embedding?: number[];
  metadata?: Record<string, unknown>;
  timestamp?: number;
}
```

每个任务执行被记录为一系列 **观察→思考→行动→结果** 的轨迹步骤。

### 3.3 LoRA 风格蒸馏

置信度更新公式：

```
pattern.confidence += learningRate × reward × (1 - confidence)
```

- `learningRate` — 默认 0.01 (LoRA rank)
- `reward` — 来自 verdict 的奖励信号 [-1, 1]
- `(1 - confidence)` — 确保置信度趋近但不超过 1.0

### 3.4 EWC++ 防遗忘

`EWCConsolidator` 使用弹性权重整合防止新学习覆盖旧的重要模式：

```
新权重 = 学习权重 + λ × F_i × (θ_old - θ_new)²
```

- `λ` (ewcLambda) — 正则化强度
- `F_i` — Fisher 信息矩阵对角线（重要性权重）
- 重要的旧模式受到更强的保护

### 3.5 持久化

所有学习结果持久化到：
- `.claude-flow/neural/patterns.json` — 模式库
- `.claude-flow/neural/stats.json` — 学习统计
- `.claude-flow/memory.db` — AgentDB 向量数据库

---

## 4. 层 3: 治理进化 — Guidance Optimizer

### 4.1 进化流水线

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '14px', 'fontFamily': 'Arial'}}}%%
graph TB
    subgraph COMPILE["<b>编译阶段</b>"]
        ROOT["CLAUDE.md<br/>(根宪法)"]
        LOCAL["CLAUDE.local.md<br/>(本地实验)"]
        BUNDLE["PolicyBundle<br/>(编译产物)"]
        ROOT --> BUNDLE
        LOCAL --> BUNDLE
    end

    subgraph RUNTIME["<b>运行时</b>"]
        EXECUTE["任务执行"]
        GATE["门控检查<br/>block/warn/allow"]
        RECORD["记录违规"]
    end

    subgraph EVAL["<b>评估阶段</b>"]
        TP["TestsPassEvaluator"]
        FC["ForbiddenCommandEvaluator"]
        FD["ForbiddenDependencyEvaluator"]
        VR["ViolationRateEvaluator"]
        DQ["DiffQualityEvaluator"]
    end

    subgraph EVOLVE["<b>进化阶段</b>"]
        RANK["违规排名分析"]
        AB["A/B 测试<br/>规则对比"]
        PROMOTE["规则提升<br/>local → root"]
        ADR["生成 ADR<br/>记录变更理由"]
    end

    BUNDLE --> GATE
    GATE --> EXECUTE
    EXECUTE --> RECORD
    RECORD --> TP & FC & FD & VR & DQ
    TP & FC & FD & VR & DQ --> RANK
    RANK --> AB --> PROMOTE --> ADR
    ADR -->|更新| ROOT

    style COMPILE fill:#e8f5e9,stroke:#4caf50,stroke-width:2px
    style RUNTIME fill:#e3f2fd,stroke:#2196f3,stroke-width:2px
    style EVAL fill:#fff3e0,stroke:#ff9800,stroke-width:2px
    style EVOLVE fill:#f3e5f5,stroke:#9c27b0,stroke-width:2px
```

### 4.2 评估器

| 评估器 | 检查内容 |
|-------|---------|
| `TestsPassEvaluator` | 测试是否通过 |
| `ForbiddenCommandEvaluator` | 是否使用了禁止命令 |
| `ForbiddenDependencyEvaluator` | 是否引入了禁止依赖 |
| `ViolationRateEvaluator` | 违规率趋势 |
| `DiffQualityEvaluator` | diff 质量评分 |

### 4.3 规则进化路径

```
1. CLAUDE.local.md 中添加实验性规则
2. 运行时收集该规则的执行数据
3. OptimizerLoop 分析违规排名
4. 效果好的规则 → 提升到 CLAUDE.md (根宪法)
5. 效果差的规则 → 废弃或修改
6. 每次变更生成 ADR (Architecture Decision Record)
```

---

## 5. 层 4: 后台 Worker 持续优化

### 5.1 Worker 触发机制

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '14px', 'fontFamily': 'Arial'}}}%%
graph LR
    subgraph TRIGGERS["<b>触发条件</b>"]
        REFACTOR["重大重构后"]
        FEATURE["新功能后"]
        SECURITY["安全变更后"]
        API["API 变更后"]
        FILES["5+ 文件变更后"]
        DEBUG["复杂调试"]
    end

    subgraph WORKERS["<b>Worker 调度</b>"]
        W_OPT["optimize (高)"]
        W_TEST["testgaps (正常)"]
        W_AUDIT["audit (关键)"]
        W_DOC["document (正常)"]
        W_MAP["map (正常)"]
        W_DEEP["deepdive (正常)"]
    end

    subgraph RESULTS["<b>进化结果</b>"]
        PERF["性能优化建议"]
        COVERAGE["缺失测试覆盖"]
        VULNS["安全漏洞报告"]
        DOCS["文档更新"]
        CODEMAP["代码库映射"]
        ANALYSIS["深度分析报告"]
    end

    REFACTOR --> W_OPT --> PERF
    FEATURE --> W_TEST --> COVERAGE
    SECURITY --> W_AUDIT --> VULNS
    API --> W_DOC --> DOCS
    FILES --> W_MAP --> CODEMAP
    DEBUG --> W_DEEP --> ANALYSIS

    style TRIGGERS fill:#e8f5e9,stroke:#4caf50,stroke-width:2px
    style WORKERS fill:#e3f2fd,stroke:#2196f3,stroke-width:2px
    style RESULTS fill:#fff3e0,stroke:#ff9800,stroke-width:2px
```

### 5.2 Worker 执行模式

```bash
# 列出可用 Worker
npx claude-flow hooks worker list

# 手动触发
npx claude-flow hooks worker dispatch --trigger audit
npx claude-flow hooks worker dispatch --trigger optimize

# Worker 守护进程
npx claude-flow daemon start  # 启动后台 Worker 守护进程
npx claude-flow daemon status # 查看运行状态
```

---

## 6. 跨会话学习持久化

### 6.1 会话生命周期

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {'fontSize': '14px', 'fontFamily': 'Arial'}}}%%
sequenceDiagram
    participant S1 as 🔵 会话 1
    participant MEM as 🧠 Memory DB
    participant PAT as 📁 patterns.json
    participant S2 as 🟢 会话 2

    rect rgb(232, 245, 233)
    Note over S1: 会话开始
    S1->>MEM: session-start<br/>恢复上次上下文
    MEM-->>S1: 历史模式 + 统计
    end

    S1->>S1: 执行任务...
    S1->>MEM: memory_store(pattern-auth,<br/>"JWT + refresh token 方案")
    S1->>PAT: SONA distill → 模式持久化

    rect rgb(255, 243, 224)
    Note over S1: 会话结束
    S1->>MEM: session-end<br/>--export-metrics true
    S1->>PAT: EWC++ consolidate
    end

    Note over MEM, PAT: 数据在磁盘上持久化<br/>.claude-flow/memory.db<br/>.claude-flow/neural/patterns.json

    rect rgb(227, 242, 253)
    Note over S2: 新会话开始
    S2->>MEM: session-restore --latest
    MEM-->>S2: 上次会话的上下文
    S2->>MEM: memory_search("认证模式")
    MEM-->>S2: pattern-auth (相似度: 0.92)<br/>"JWT + refresh token 方案"
    Note over S2: 利用历史经验!
    end
```

### 6.2 模式传输

支持跨项目的模式传输（通过 IPFS）：

```bash
# 导出当前项目的模式到 IPFS
npx claude-flow hooks transfer store

# 从其他项目导入模式
npx claude-flow hooks transfer from-project --source /path/to/other
```

---

## 7. 进化机制总结

| 维度 | 机制 | 存储位置 | 生效范围 |
|------|------|---------|---------|
| **LLM 缓存** | 响应缓存 + 模式提取 | 内存 Map (运行时) | 当前会话 |
| **ReasoningBank** | HNSW 向量搜索 + 模式晋升 | .claude-flow/memory.db | 跨会话 |
| **SONA** | 轨迹 → 判定 → 蒸馏 → 整合 | .claude-flow/neural/ | 跨会话 |
| **治理进化** | 评估器 → 优化器 → 规则提升 | CLAUDE.md / CLAUDE.local.md | 永久 |
| **后台 Worker** | 定时/触发式优化分析 | 各自输出 | 持续 |
| **模式传输** | IPFS 跨项目共享 | IPFS 网络 | 跨项目 |
