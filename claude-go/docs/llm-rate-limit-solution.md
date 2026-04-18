# LLM 限流治理方案 — 全局准入控制 + 自适应退避

> **版本**: 1.0 | **日期**: 2026-04-18
> **问题**: 团队长时间运行遇 LLM 限流后编排停止工作
> **适用**: 所有团队 (研发/金融/研究/创意/蜂群)

---

## 1. 问题诊断

### 当前架构缺陷

| 缺陷 | 严重度 | 说明 |
|:---|:---:|:---|
| **无全局限速器** | 🔴 | 每个 Agent 独立调 API, 无跨 Agent 协调 |
| **无 RPM/TPM 追踪** | 🔴 | 不知道当前消耗速率, 无法预判限流 |
| **Retry-After 未解析** | 🔴 | 忽略服务端建议的等待时间 |
| **双层重试叠加** | 🟡 | api.Client 5次 × Coordinator 4次 = 最多 20 次 HTTP |
| **4xx 错误污染熔断器** | 🟡 | 400/401/403 不应计入 consecutiveFails |
| **workflow 无并发上限** | 🟡 | executeParallel 不经过 AgentPool |
| **退避无抖动 (coordinator)** | 🟡 | 多 Agent 同时退避后同时重试 (雷群效应) |

### 限流场景 (DashScope/阿里百炼)

| 错误码 | 含义 | 当前处理 | 应对策略 |
|:---|:---|:---|:---|
| `429-Throttling.RateQuota` | RPM/RPS 超限 | 重试 3s×3 退避 | **全局 RPM 令牌桶** |
| `429-Throttling.BurstRate` | 突发速率过快 | 同上 | **请求平滑 + 启动预热** |
| `429-Throttling.AllocationQuota` | TPM/TPS 超限 | 同上 | **全局 TPM 令牌桶** |
| `429-CommodityNotPurchased` | 计费问题 | 当作可重试 ❌ | **立即失败, 不重试** |

---

## 2. 解决方案架构

```
┌────────────────────────────────────────────────────────┐
│                   所有 Agent / Team                     │
│  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐    │
│  │ A1  │ │ A2  │ │ A3  │ │ A4  │ │ A5  │ │ ... │    │
│  └──┬──┘ └──┬──┘ └──┬──┘ └──┬──┘ └──┬──┘ └──┬──┘    │
│     └───────┴───────┴───┬───┴───────┴───────┘         │
│                         ▼                              │
│  ┌──────────────────────────────────────────────────┐  │
│  │         RateLimitGuard (全局准入控制器)            │  │
│  │                                                    │  │
│  │  ┌─────────────┐  ┌─────────────┐  ┌──────────┐  │  │
│  │  │ RPM 令牌桶   │  │ 并发信号量   │  │ 退避状态 │  │  │
│  │  │ (请求/分钟)  │  │ (maxInFlight)│  │ (全局)   │  │  │
│  │  └─────────────┘  └─────────────┘  └──────────┘  │  │
│  │                                                    │  │
│  │  ┌──────────────────────────────────────────────┐  │  │
│  │  │ Retry-After 解析 + 自适应 AIMD 并发控制       │  │  │
│  │  └──────────────────────────────────────────────┘  │  │
│  └──────────────────────────────────────────────────┘  │
│                         ▼                              │
│  ┌──────────────────────────────────────────────────┐  │
│  │              api.Client (HTTP 层)                  │  │
│  │  重试: 仅 1 次快速重试 (非 429) + Retry-After     │  │
│  │  熔断: 仅计数 5xx/429, 不计 4xx 参数错误          │  │
│  └──────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────┘
```

### 核心组件

| 组件 | 职责 | 算法 |
|:---|:---|:---|
| **RateLimitGuard** | 全局准入控制, 所有 LLM 调用必经 | 令牌桶 + 信号量 + AIMD |
| **RPM Bucket** | 限制每分钟请求数 | 令牌桶 (capacity=RPM, refill=RPM/60/s) |
| **Concurrency Sem** | 限制同时在途 LLM 请求数 | 加权信号量 (初始 maxInFlight) |
| **AIMD Controller** | 自适应调节 maxInFlight | 成功: +1/round; 429: ×0.5 |
| **Retry-After Parser** | 解析服务端建议等待时间 | HTTP header + JSON body |

---

## 3. 关键算法

### 3.1 令牌桶 (RPM)

```
capacity = provider_rpm_limit (如 60)
tokens = capacity
refill_rate = capacity / 60.0 (每秒补充)

Acquire():
  while tokens < 1:
    wait(refill)
  tokens -= 1

每秒 tick:
  tokens = min(capacity, tokens + refill_rate)
```

### 3.2 AIMD 并发控制

```
maxInFlight = initial (如 6)

on_success():
  if all_recent_ok:
    maxInFlight = min(maxInFlight + 1, hard_max)

on_429():
  maxInFlight = max(maxInFlight / 2, 1)
  global_pause(retry_after 或 10s)
```

### 3.3 Retry-After 解析

```
解析优先级:
1. HTTP Retry-After header (秒数或日期)
2. JSON body "retry_after" / "retryAfter" 字段
3. 匹配 "请在 Xs 后重试" 中文模式
4. 回退: 基于 attempt 的指数退避
```

---

## 4. 参考论文与业界方案

| 来源 | 核心思想 | 引用 |
|:---|:---|:---|
| OpenAI Rate Limits 官方文档 | x-ratelimit-* headers + 指数退避 | platform.openai.com |
| Anthropic Rate Limits 文档 | 令牌桶模型 + ITPM/OTPM 分离 | docs.anthropic.com |
| 阿里百炼错误码文档 | BurstRate/RateQuota/AllocationQuota 三类限流 | alibabacloud.com |
| LangChain InMemoryRateLimiter | requests_per_second + 检查间隔 | docs.langchain.com |
| Justitia (arXiv:2510.17015) | 任务并行 LLM Agent 公平调度 | arXiv |
| PecSched (arXiv:2409.15104) | 抢占式 LLM 集群调度 | arXiv |

---

## 5. 实现计划

### 新增文件

| 文件 | 内容 |
|:---|:---|
| `pkg/api/ratelimit.go` | RateLimitGuard + RPM 令牌桶 + AIMD |

### 修改文件

| 文件 | 变更 |
|:---|:---|
| `pkg/api/client.go` | 集成 Guard; 解析 Retry-After; 修复熔断器 |
| `pkg/agent/workflow.go` | executeParallel 经过 Guard 信号量 |
| `pkg/agent/coordinator.go` | 退避加抖动; 识别更多限流模式 |
