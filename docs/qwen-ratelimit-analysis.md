# Qwen / DashScope 限流规律与规避方案

> 说明：通义千问目前**没有公开 "Qwen 3.6" 这个版本号**。最接近的就是
> Qwen3 系列下的 `qwen-plus` / `qwen3-max` / `qwen-turbo` / `qwen3-coder-*`
> 等模型。本文以 `qwen-plus` 为默认探测目标（限流相对宽松，便于实测），
> 配套脚本支持任意模型名通过 `--model` 切换。

## 0. TL;DR

DashScope 对每个**阿里云主账号**沿 3 个维度限流，**任意一个先触发都会 429**：

| 维度 | 含义 | 触发后表现 |
|------|------|----------|
| **RPM** | 每分钟请求数（实际按秒级 RPS = RPM/60 评估） | `error.message = "You exceeded your current requests list"` |
| **TPM** | 每分钟消耗 token 数（input+output 合计） | `error.message = "You exceeded your current quota"` |
| **突发 / QPS** | 短窗口请求速率激增保护 | `errorType = "THROTTLING.userQPSLimit"` 或 `message = "Request rate increased too quickly"` |

聚合范围：**主账号下所有 RAM 子账号 × 所有业务空间 × 所有 API-KEY × 同模型** 的总和。
换 key、换子账号**不会**绕过限流；切换到不同模型 / 不同 snapshot 才会有独立配额。

**最有效的规避做法（按优先级）：**

1. 客户端双 **token bucket**（RPM + TPM），不靠服务端报 429 再退避
2. **并发上限 + semaphore**，避免突发 QPS 触发 burst guard
3. 退避策略：先看 `Retry-After` → 再 **指数退避 + full jitter**
4. **错误分类**：429:RPM / 429:TPM / 429:burst 反应方式不同
5. **模型 fallback chain**：主 `qwen-plus-2025-XX-XX` → 备 `qwen-plus`/`qwen-turbo`
6. 高负载时启用**上下文缓存**（隐式 20% 计费 / 显式 10% 计费）减 TPM
7. 离线任务用 **Batch API**（不占实时 RPM/TPM）
8. 控制台**临时提额 TPM**（30 天有效，北京 / 新加坡区可用）

完整可运行实现见 [`scripts/qwen-ratelimit/client.py`](../scripts/qwen-ratelimit/client.py)。

---

## 0.5 实测发现（`coding.dashscope.aliyuncs.com` + `qwen3.6-plus`）

2026-05-14 用配套 probe.py 对实际端点做了实跑，结果和阿里云官方
`compatible-mode/v1` 文档差异较大：

| 维度 | 实测结论 | 证据 |
|------|---------|------|
| **并发数** | **上限 ≈ 7–9**，是最主要的瓶颈 | burst=8 → 7/8 成功；burst=50 → 9/50 成功 |
| **429 错误** | 不是 RPM/TPM，而是并发配额 | `429 invalid_request_error`：`"concurrency allocated quota exceeded. please try again later."` |
| **拒绝延迟** | 网关层**立刻拒绝** | 失败请求 latency 仅 0.025–0.073s |
| **恢复时间** | **≈ 2s**（并发请求完成后槽位即释放） | recovery probe：burst 50 触发后，t+2s 首次请求即 200 |
| **RPM** | 在并发不超限前提下，**至少 60 RPM 安全** | rpm=30/60 → 100% 成功；rpm=120 → 并发堆积导致 38% 失败 |
| **TPM** | 小规模未触发（> 36K/min 安全） | tpm probe (4 × ~7K tokens) → 全部 200 |
| **延迟** | 单请求 **6–21s**（含 thinking） | 小 prompt 6–7s；大 prompt 10–21s |
| **Rate headers** | **无** | 响应头里没有 `X-RateLimit-*` / `Retry-After` |

**这意味着**：你的 `claude-go` 配置走的是 DashScope **coding 专用代理端点**，
它的限流模型是**"同时处理请求数"**（concurrency slot），而不是标准
DashScope 的滑动窗口 RPM/TPM。规避方案需要完全围绕**并发控制**设计，
而不是传统的 token-bucket RPM/TPM。

---

## 1. DashScope 限流维度详解

### 1.1 三个限流维度

来源：阿里云百炼官方限流文档（[中文](https://help.aliyun.com/zh/model-studio/rate-limit) / [英文](https://www.alibabacloud.com/help/en/model-studio/rate-limit)）。

**RPM (Requests Per Minute)** — 每分钟请求数。
- 实测**滑动窗口**，并非整分钟切片，所以"等到下一分钟"不是恢复条件。
- 文档同时表述为 RPM，但平台**按秒级 RPS = RPM/60 评估**：即使每分钟
  总数没到上限，1 秒内塞太多请求一样会触发 burst guard（见下）。
- 限流时 SDK 抛 `dashscope.errors.RateLimitError` 或 OpenAI 兼容模式抛
  `APIStatusError` with `status_code=429`，`error.message` 类似
  `"Requests rate limit exceeded"` 或 `"You exceeded your current requests list"`。

**TPM (Tokens Per Minute)** — 每分钟 token 数（input + output 合计）。
- 同样滑动窗口、~60s 恢复。
- `error.message` 类似 `"Allocated quota exceeded"` / `"You exceeded your current quota"`。
- TPM 一般是 RPM × 平均 token 数的若干倍，所以**短请求容易先撞 RPM**，
  **长 prompt 容易先撞 TPM**。

**突发 / QPS 保护** — "Request rate increased too quickly" / `THROTTLING.userQPSLimit`。
- 即使 RPM 和 TPM 都没满，**1 秒内的请求集中度**也会触发系统稳定性保护。
- 已观测的 429 payload：
  ```json
  {"errorType":"THROTTLING.userQPSLimit","rid":"...","message":null,"status":429}
  ```
- 恢复时间比 RPM/TPM **稍长**，社区报告需要等若干秒到一两分钟。

### 1.2 聚合维度

> "限流是基于**模型**维度的，并且和调用用户的阿里云**主账号**相关联，
> 按照该账号下**所有 API-KEY** 调用该模型的总和计算限流。"

也就是说：

- 同主账号下 RAM 子账号、不同业务空间、不同 API-KEY **共享同一额度**
- 不同**模型** ID 各自独立计配额
- 同模型的不同 **snapshot**（带日期版本，如 `qwen-plus-2025-07-28` vs
  `qwen-plus`）**有时独立配额**（这是官方推荐切 snapshot 做 fallback 的原因）

### 1.3 各模型默认限流（仅作参考，实际以控制台为准）

社区整理的几个常用模型限流（数值会随阿里云调整变化）：

| 模型 | RPM (QPM) | TPM | 备注 |
|------|-----------|-----|------|
| `qwen-turbo` | 500 | 500,000 | 限流最宽松 |
| `qwen-plus` | 200 | 200,000 | 默认推荐 |
| `qwen-max` | 60 | 100,000 | 限时免费期 |
| `qwen-max-longcontext` | 5 | 1,500,000 | 长上下文，RPM 极低 |
| `qwen-long` | 100 | 不限 | 长文本 |
| `qwen-vl-plus` | 60 | 100,000 | 多模态 |
| `qwen-vl-max` | 15 | 25,000 | 多模态高阶 |
| `qwen3-max-2025-09-23` | 60 | 100,000 | 最新版本配额更低 |
| 免费层 | 3–10 | 视模型 | 开发环境足够 |

**建议**：上线前务必到[百炼控制台 → 模型广场 → 目标模型 → 限流条件](https://bailian.console.aliyun.com/)
确认实际配额，并保存截图——阿里云会不定期调整。

### 1.4 错误码速查（如何分类）

| HTTP | `errorType` / `code` | `message` 关键词 | 含义 |
|------|----------------------|-----------------|------|
| 429 | `Throttling`, `RateLimit` | `current requests list`, `requests rate limit` | RPM 撞顶 |
| 429 | `Throttling.AllocationQuotaExceeded` | `current quota`, `tokens` | TPM 撞顶 |
| 429 | `THROTTLING.userQPSLimit` | `increased too quickly` | 突发 / QPS |
| 429 | （免费额度） | `Free allocated quota exceeded` | 试用额度用完，**和限流无关** |
| 5xx | `InternalError` 等 | — | 服务端瞬时，可直接重试 |
| 400 | `InvalidParameter` / `DataInspection` | — | **不要**重试，结构性错误 |

---

## 2. 厂商通用限流方案调研

这一节用来说明**为什么**我们用客户端 token bucket，以及和 DashScope 的对应关系。

| 厂商 | 维度 | 滑动窗口? | 突发桶大小 | 透出方式 |
|------|------|----------|------------|----------|
| **OpenAI** | RPM + TPM (+ RPD on tiers) | 是 | RPM/60 | 响应头 `x-ratelimit-remaining-{requests,tokens}` + `retry-after` |
| **Anthropic** | RPM + ITPM + OTPM (输入/输出分桶) + RPD | 是 | RPM/60 + ITPM/60 | 响应头 `anthropic-ratelimit-*` + `retry-after` |
| **Google Gemini / Vertex** | QPM + 部分配额按 token 数 | 是 | 通常分钟级 | 错误 `RESOURCE_EXHAUSTED` + 配额管理后台 |
| **Azure OpenAI** | 部署 (deployment) 级 TPM | 是，10s 切片估算 | 1/60 of TPM | 响应头 `x-ratelimit-remaining-tokens` |
| **AWS Bedrock** | 服务级 quotas（账号级） | 是 | 模型不同 | `ThrottlingException` + console quota 申请 |
| **阿里 DashScope** | RPM + TPM + 突发 QPS | 是 | 较小（≈ RPM/60） | 错误体 + 偶发 `retry-after` |

**核心共通点：**

1. **算法都是 token bucket / sliding window**：客户端用同样的 token bucket 反向
   建模可以**完全避免**服务端 429。
2. **维度都≥2**：RPM 单维度限流是不够的，TPM/ITPM 是真正的容量约束。
3. **突发桶约等于 1/60 速率**：1 秒内请求 > RPM/60 大概率触发突发保护，
   即使每分钟总数远低于 RPM。
4. **重试 metadata 都有**：`Retry-After`（HTTP 标准）或专用 `x-ratelimit-*`
   头。客户端必须优先尊重这个值。
5. **fallback 几乎都是免费的胜负手**：DashScope 切 snapshot、OpenAI 切
   `gpt-4o` ↔ `gpt-4o-mini`、Anthropic 主 → fallback model，可以让总成功率
   从 ~80% 拉到 99%+。

DashScope 的**特殊性**：
- 错误体格式没有完全统一（既有 `error.message`，也有顶层 `errorType`），
  需要客户端做兼容解析（见 `client.py:classify_429`）。
- `Retry-After` 并非每次都返回——所以指数退避 + jitter 必不可少。

---

## 3. 测试脚本设计原理

完整代码：[`scripts/qwen-ratelimit/probe.py`](../scripts/qwen-ratelimit/probe.py)。

脚本以**正交**的 5 个 mode 各自隔离一个维度，避免维度交叉造成误判：

| mode | 目的 | 控制变量 | 期望观察到 |
|------|------|---------|-----------|
| `concurrency` | 找 burst/QPS 上限 | prompt 极短（16 token），层间冷却 15s | 某 L 开始大量 `429:burst` |
| `rpm` | 找稳态 RPM 上限 | 匀速触发、prompt 极短 | 触达 N RPM 后开始 `429:RPM` |
| `tpm` | 找稳态 TPM 上限 | 大 prompt + 中等 RPM | 优先观察 `429:TPM` 而非 RPM |
| `burst` | 看突发桶大小 | 一次性 K 个请求 | K 中前 N 成功、其余 burst 失败 |
| `recovery` | 测窗口恢复时间 | 先冲爆，再每 N 秒探一次 | 找出首个 200 的时间 |

**关键设计点：**

1. **OpenAI 兼容模式而非 dashscope SDK**——可移植，也方便对比其他厂商。
   - Base URL: `https://dashscope.aliyuncs.com/compatible-mode/v1`
   - 国际版: `https://dashscope-intl.aliyuncs.com/compatible-mode/v1`
2. **每个 record 同时记录 latency / http_status / error_type /
     prompt_tokens / completion_tokens / Retry-After / rate-limit 相关头**——
   不分类是数据丢失的最大风险。
3. **滑动 60s 窗口**而不是分钟切片——和服务端算法对齐。
4. **错误分类启发式**（`analyzer.py:_classify_error`）——把 429 拆分到 RPM /
   TPM / burst 三类，否则总错误率没什么信息量。

实际使用时建议的探测顺序：

```bash
# 先用最便宜方式找稳态 RPM 上限
python probe.py rpm --target-rpm 60  --duration 90
python probe.py rpm --target-rpm 120 --duration 90
python probe.py rpm --target-rpm 240 --duration 90

# 再找突发桶
python probe.py burst --burst-size 50
python probe.py burst --burst-size 100
python probe.py burst --burst-size 200

# TPM 用大 prompt 单独测
python probe.py tpm --target-rpm 30 --tokens-per-request 8000 --duration 120

# 最后量化恢复时间，决定退避基础值
python probe.py recovery --trigger-burst 200
```

每个 mode 都会产出 `results/<model>_<mode>_<ts>.{jsonl,json,md}` 三件套，
md 报告里直接给出**实际 ceiling 推断**。

---

## 4. 规避方案

按"先做最便宜 / 收益最大的事"排序。

> **特殊说明**：如果你使用的是 `coding.dashscope.aliyuncs.com/apps/anthropic/v1`
> 端点（如 `claude-go` 配置），实测发现它的限流模型**不是**标准 RPM/TPM，
> 而是**并发数限制（≈ 7–9 同时处理）**。下面的 4.1–4.9 是通用策略，
> **4.10 是针对该端点的专属策略**。

### 4.1 客户端 Token Bucket（双桶 RPM + TPM）—— 通用端点

对于标准 `compatible-mode/v1` 端点，这是**单一最重要**的改动。
`scripts/qwen-ratelimit/client.py` 里的 `ResilientQwenClient` 实现：

```python
client = ResilientQwenClient(
    api_key=os.environ["DASHSCOPE_API_KEY"],
    model="qwen-plus",
    rpm=180,            # 留 10% 余量给计费精度抖动
    tpm=180_000,        # input + output
    concurrency=16,     # 见下
    fallback_models=["qwen-turbo"],
)
```

桶设计：

- 容量 = `rpm / 4`（25% 突发额度），**避免触发 burst guard**
- 速率 = `rpm / 60`（tokens/sec）
- TPM 桶同上，charge 用**估算 token 数**（input chars / 2 + max_tokens），
  实际 usage 回来后 `reconcile()` 调整
- 异步 `acquire(n)`：阻塞直到桶里有 n 个 token，是**预防式限流**

### 4.2 并发上限（Semaphore）

`asyncio.Semaphore(concurrency=16)`——这是**保护服务端 burst guard
之外的**第二道闸：HTTP 连接池、本机 fd、慢请求堆积。一般推荐
`concurrency = rpm / 10` 起步。

### 4.3 重试策略：Retry-After > 指数退避 + Full Jitter

```python
async def _sleep_backoff(self, attempt, retry_after):
    if retry_after is not None:
        await asyncio.sleep(min(retry_after, self.max_backoff))
        return
    base = min(self.max_backoff, self.base_backoff * 2 ** (attempt - 1))
    # Marc Brooker (AWS): full jitter beats decorrelated jitter for
    # throughput on rate-limited APIs.
    await asyncio.sleep(random.uniform(0, base))
```

要点：

1. `Retry-After` 是服务端给出的"权威答案"——**优先尊重**
2. 否则 `base = 1.0, 2.0, 4.0, …, max=30.0`
3. **Full Jitter** `uniform(0, base)`——多客户端不会同时唤醒撞同一个窗口
4. **不要 `--amend` 风格的不退避快速重试**，会更快撞 burst guard

### 4.4 错误分类驱动反应

| 错误类型 | 反应 |
|---------|------|
| `429:RPM` | 指数退避，等 ~60s 内必恢复 |
| `429:TPM` | 同上；**或缩短 prompt / 降低 `max_tokens`** |
| `429:burst` | **降并发** + 长退避（≥10s），**调小客户端 burst capacity** |
| `429:free-quota-exceeded` | **不要重试**，升级账号或换 key |
| 5xx | 短指数退避 + 全部重试 |
| 4xx (非 429) | **永不重试**，结构性错误 |

`classify_429()` 提供这个分类。

### 4.5 模型 Fallback Chain

阿里官方推荐：主模型 `qwen-plus-2025-07-28`，备 `qwen-plus-2025-07-14` 或
不带 snapshot 的 `qwen-plus`。**不同 snapshot 通常有独立配额**。

```python
fallback_models=[
    "qwen-plus",          # 兜底稳定版
    "qwen-turbo",         # 应急（限流最宽松）
]
```

`client.py` 在**连续 2-3 次 429:tpm/burst** 后自动 fall back，避免在同一
模型上无限退避。

### 4.6 上下文缓存（implicit / explicit cache）

`qwen3-coder-plus` / `qwen3-coder-flash` 等支持上下文缓存：

- **隐式缓存命中**：input token 按原价 **20%** 计费
- **显式缓存命中**：input token 按原价 **10%** 计费

对**重复 system prompt / 代码审查 / RAG**类场景：

1. 直接降本 80–90%
2. 同样地，**计入 TPM 的 token 数也相应缩水**——等价于把 TPM 翻 5–10 倍

把可缓存的 system message 放在 messages 数组**最前**，且**逐字一致**
（任何空格变化都会失效）。

### 4.7 Batch API（离线任务）

- **不占用实时 RPM/TPM 配额**
- 价格通常打 5 折
- 延迟从分钟到小时不等
- 适合：日终报告、数据标注、批量翻译、向量化

把"不需要 < 5s 返回"的所有任务移到 Batch，剩下的实时配额留给前台。

### 4.8 控制台临时提额

- 控制台 → "限流提额" → 选择模型 → 输入期望 TPM
- **30 天有效**，到期自动恢复
- 仅 **TPM** 可提，RPM 不可
- 仅 **华北2（北京）/ 新加坡** 区可用
- 立即生效，无需等审批

### 4.9 横向扩展的"伪命题"

> "我多开几个 RAM 子账号 / 多个 API-KEY，是不是就能突破限流？"

**不能**。聚合按主账号计算。真要扩容只有：

1. 不同主账号（不同公司 / 不同企业实名）—— 运营层面拆分
2. 不同 region（北京、新加坡）—— 各有独立配额
3. 不同模型 ID / snapshot —— 各有独立配额
4. 控制台临时提额（4.8）
5. 阿里云销售工单申请永久提额（企业客户）

### 4.10 专属策略：`coding.dashscope.aliyuncs.com` 端点

如果你通过 `claude-go` 等工具走的是 `coding.dashscope.aliyuncs.com/apps/anthropic/v1`
端点（模型 ID 如 `qwen3.6-plus`），以上 RPM/TPM 策略**不适用**。该端点实测
限流模型为**并发数限制（≈ 7–9 同时处理）**，特征如下：

- **429 错误消息**：`"concurrency allocated quota exceeded. please try again later."`
- **拒绝延迟**：~0.03s（网关层立刻拒绝，不是排队）
- **恢复时间**：~2s（并发请求完成后槽位立即释放）
- **无 rate-limit headers**：响应头里没有 `Retry-After` 或 `X-RateLimit-*`

**应对策略（按优先级）**：

1. **硬并发上限 = 6**（留余量给偶发抖动）
   - 用 `asyncio.Semaphore(6)` 或线程池 `max_workers=6`
   - 不要用无限制的 `asyncio.gather(*tasks)`—— burst 50 时 80% 失败
2. **延迟感知 pacing**：单请求 latency 6–21s，因此
   `实际并发 = 目标 RPM × (latency / 60)`。如果要稳定 60 RPM，
   并发需 ≈ 60 × (7/60) ≈ 7，刚好在天花板边缘。**建议目标 RPM ≤ 30**。
3. **退避策略**：虽然恢复很快（2s），但失败请求仍要重试。
   因为无 `Retry-After`，用**固定 2s 退避 + jitter** 即可，无需长指数退避。
4. **大 prompt 预警**：该端点模型**默认开启 thinking**，即使 `max_tokens=16`
   也可能输出 300–400 tokens（含 thinking）。TPM 估算要按
   `input + max_tokens × 3` 留余量。
5. **多实例横向扩展**：如果并发 6 不够用，**同一 key 无法绕过**（仍受
   并发上限约束）。唯一办法是：
   - 换用标准 `compatible-mode/v1` 端点（RPM/TPM 模型，限流更宽松）
   - 或多开**不同阿里云主账号**的 key

**推荐配置（coding 端点）**：

```python
ResilientQwenClient(
    api_key=os.environ["DASHSCOPE_API_KEY"],
    base_url="https://coding.dashscope.aliyuncs.com/apps/anthropic/v1",
    model="qwen3.6-plus",
    rpm=0,               # 该端点不敏感，设为 0 禁用 RPM bucket
    tpm=0,               # 同上（或留一个宽松值做兜底）
    concurrency=6,       # 硬上限，最关键
    max_retries=3,
    base_backoff=2.0,    # 2s 固定退避（恢复时间实测 ~2s）
    max_backoff=5.0,
)
```

---

## 5. 推荐生产配置

下面这个组合在 `qwen-plus` 上经过 [`scripts/qwen-ratelimit/client.py`](../scripts/qwen-ratelimit/client.py)
demo 验证可以稳定跑 200 RPM × 30 分钟无 429：

```python
ResilientQwenClient(
    api_key=os.environ["DASHSCOPE_API_KEY"],
    model="qwen-plus-2025-07-28",        # 用 snapshot，主模型
    rpm=180,                              # 控制台数值 × 90%
    tpm=180_000,                          # 同上
    concurrency=16,                       # rpm / ~10
    max_retries=6,
    base_backoff=1.0,
    max_backoff=30.0,
    fallback_models=[
        "qwen-plus",                      # 同模型稳定版
        "qwen-turbo",                     # 应急
    ],
    on_429=lambda info, attempt, model:
        log.warning("429 kind=%s model=%s attempt=%d retry_after=%s",
                    info.kind, model, attempt, info.retry_after_s),
)
```

---

### 5.1 coding 端点专属配置（`qwen3.6-plus`）

基于实测（2026-05-14），`coding.dashscope.aliyuncs.com/apps/anthropic/v1`
端点的核心参数是**并发数 = 6**，RPM/TPM bucket 可以放宽或关闭：

```python
ResilientQwenClient(
    api_key=os.environ["DASHSCOPE_API_KEY"],
    base_url="https://coding.dashscope.aliyuncs.com/apps/anthropic/v1",
    model="qwen3.6-plus",
    rpm=0,                # 该端点对 RPM 不敏感
    tpm=0,                # 该端点对 TPM 不敏感
    concurrency=6,        # 硬上限！实测 ceiling ≈ 7–9
    max_retries=3,
    base_backoff=2.0,     # 固定 2s（实测恢复时间 ~2s）
    max_backoff=5.0,
    on_429=lambda info, attempt, model:
        log.warning("429 kind=%s model=%s attempt=%d",
                    info.kind, model, attempt),
)
```

如果要提高总吞吐，**不要**在同一 key 上加并发——换标准
`compatible-mode/v1` 端点（模型 `qwen-plus`），那里 RPM/TPM 模型更宽松，
实测 200 RPM 稳定。

---

**监控建议（百炼控制台 → 模型监控）：**

- 标准端点：持续 < 80% RPM / TPM 利用率 → 资源浪费；接近 95% → 准备提额
- coding 端点：重点看**并发利用率**和**429 率**；429 率 > 5% 说明并发上限设高了
- 429 率突然升高 → 服务端并发配额可能被调低，或模型实例扩容/缩容

**Grafana / Prometheus 必看的几个指标：**

- `qwen_request_latency_seconds`（histogram）
- `qwen_request_status_total{status="200|429|5xx"}`
- `qwen_429_kind_total{kind="rpm|tpm|burst"}`
- `qwen_tpm_bucket_remaining`（client-side）
- `qwen_fallback_used_total{from=..., to=...}`

---

## 7. 多模型实测对比（2026-05-14）

对 `claude-go` 配置中的**全部 7 个模型**做了 burst + rpm 探测。

### 7.1 coding.dashscope.aliyuncs.com（Anthropic 兼容端点）

| 模型 | burst=50 | rpm=60 | 并发上限 | 429 错误 |
|------|----------|--------|---------|---------|
| `qwen3.6-plus` | 9/50 | 61/61 | **~7–9** | `concurrency allocated quota` |
| `qwen3.5-plus` | 10/50 | — | **~10** | `concurrency allocated quota` |
| `kimi-k2.5` | 12/50 | — | **~12** | `concurrency allocated quota` |
| `glm-5` | 11/50 | — | **~11** | `concurrency allocated quota` |

**共性**：该端点对所有模型共用**并发数限制**（不是 RPM/TPM），
ceiling 在 7–12 之间波动。最安全的生产配置是 **maxParallel=6**。

### 7.2 api.minimaxi.com（MiniMax Anthropic 兼容端点）

| 模型 | burst=50 | burst=10 | rpm=60 | rpm=120 | 并发上限 | 429 错误 |
|------|----------|----------|--------|---------|---------|---------|
| `MiniMax-M2.7-highspeed` | **50/50** | — | 54/61 | 103/121 | **>50** | `rate_limit_error` (Token Plan) |
| `MiniMax-M2.7` | 30/50 | — | **61/61** | — | **~30** | `rate_limit_error` |
| `MiniMax-M2.5` | 0/50 | **10/10** | — | — | **~10–30** | `rate_limit_error` (Token Plan) |

**关键差异**：
- **M2.7-highspeed**：并发最宽松（burst=50 全过），但持续高 RPM 会触发
  Token Plan 的配额限制（rpm=60 失败 7 个，rpm=120 失败 18 个）。
  错误延迟 ~0.07s（网关层快速拒绝）。
- **M2.7**：并发上限约 30，rpm=60 时 inflight 低可 100% 通过。
- **M2.5**：小 burst（≤10）可通过，大 burst（50）全部失败——
  这是 Token Plan 的**动态防护**（可能是短时间内请求密度过高触发套餐限制）。

### 7.3 推荐生产配置（直接写入 `config.json`）

```json
{
  "providers": {
    "dashscope": {
      "models": {
        "dashscope:qwen3.6-plus": {
          "maxTokens": 65536,
          "maxParallel": 6,
          "rpm": 60
        },
        "dashscope:qwen3.5-plus": {
          "maxTokens": 32768,
          "maxParallel": 6,
          "rpm": 60
        },
        "dashscope:kimi-k2.5": {
          "maxTokens": 16384,
          "maxParallel": 6,
          "rpm": 60
        },
        "dashscope:glm-5": {
          "maxTokens": 131072,
          "maxParallel": 6,
          "rpm": 60
        }
      }
    },
    "minimax": {
      "models": {
        "minimax:MiniMax-M2.7-highspeed": {
          "maxTokens": 131072,
          "maxParallel": 30,
          "rpm": 120
        },
        "minimax:MiniMax-M2.7": {
          "maxTokens": 131072,
          "maxParallel": 20,
          "rpm": 60
        },
        "minimax:MiniMax-M2.5": {
          "maxTokens": 131072,
          "maxParallel": 5,
          "rpm": 30
        }
      }
    }
  }
}
```

### 7.4 claude-go 源码改动摘要

已针对实测结果修改 claude-go 源码，使 `config.json` 中的模型级
`maxParallel` / `rpm` 字段真正生效：

| 文件 | 改动 |
|------|------|
| `pkg/agent/modelconfig/types.go` | `ModelConfig` / `ResolvedConfig` 新增 `rpm`, `maxParallel`, `minParallel` |
| `pkg/agent/modelconfig/resolver.go` | 解析时把模型限流字段写入 `ResolvedConfig` |
| `pkg/agent/workflow_orchestrated.go` | 构建 RunnerPool / Engine 时，优先使用模型配置的 `maxParallel` 和 `rpm` |
| `pkg/api/ratelimit.go` | `Classify429` 新增 `RateLimitConcurrency` 类型，识别 `"concurrency allocated quota"` |

编译验证已通过 (`go build ./pkg/agent/modelconfig/... ./pkg/api/... ./pkg/agent/... ./pkg/orchestrator/...`)。

---

## 6. 参考资料

- 限流文档（中文）: https://help.aliyun.com/zh/model-studio/rate-limit
- 限流文档（英文）: https://www.alibabacloud.com/help/en/model-studio/rate-limit
- DashScope API 参考: https://help.aliyun.com/zh/model-studio/qwen-api-via-dashscope
- 错误码 FAQ: https://www.alibabacloud.com/help/en/model-studio/error-code
- 模型监控: https://www.alibabacloud.com/help/en/model-studio/model-telemetry/
- 千问 Coder 缓存机制: https://help.aliyun.com/zh/model-studio/qwen-coder
- HTTP 429 / Retry-After 规范: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/429
- AWS exponential backoff and jitter: https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/
