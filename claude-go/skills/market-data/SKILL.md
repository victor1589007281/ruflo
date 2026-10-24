---
name: market-data
description: Query point-in-time equity, crypto and macro data with explicit as_of, adjustment and source. Use for stock quotes, K-lines, fundamentals, filings and macro series instead of web search.
allowed-tools: market_snapshot, market_quote, market_history, market_fundamentals, market_filings, market_news, market_macro, WebSearch
---

# Market Data

结构化数字只来自 `market_*` 工具，**不要用 `WebSearch` 的结果充当价格或财务数字**。
搜索只用于"为什么涨/跌""产业链位置""管理层表态"这类叙述性问题。

## 先取快照

任何涉及个股的分析，第一步调 `market_snapshot`（一次），拿到：

- `quote`：现价、昨收、`session`（pre/regular/post/closed/halted）、`as_of`、`delay`
- `bars`：带 `adjust` 口径的 K 线
- `facts`：估值与财务，缺失字段在 `missing` 里
- `warnings`：停牌、跨源价差、非交易日回退等

同一轮里的多个分析角色应共享这份快照，不要各自再查一次——不同时点的价格会让后续推理建立在流沙上。

## 三条硬规则

1. **时点**：用户问历史时，把 `as_of` 设成当时；问"现在"才用最新交易日。两者不能混。
2. **复权**：比较收益率必须同口径（`none` / `qfq` / `hfq` 三者不可比）。工具返回的 `adjust` 要原样带进结论。
3. **缺失**：字段返回 `null` 就写"未披露"，不要用同业均值、新闻里的"约 30 倍"或模型记忆补齐。

## 输出

每个数字标注 `source` 与 `as_of`；跨源分歧（`unverified`）时把两个值都列出来，不要取平均。
本 skill 只提供数据与可复核推理，不构成投资建议，也不执行任何交易。
