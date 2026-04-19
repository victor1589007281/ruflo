# Web 信息提取增强方案 (V2)

> 版本: 2026-04-19 | 状态: 设计+实施中

## 一、现状诊断

### 当前架构

```
pkg/browser/browser.go       Headless Chrome --dump-dom (单次渲染)
  └→ extractMainText()        CETR 文本密度分析
pkg/wiki/engine.go           浏览器优先 → HTTP 降级
pkg/tool/builtin/webfetch.go LLM 工具: web_fetch (HTTP only)
```

### 核心问题

| **问题** | **影响** | **优先级** |
|:---|:---|:---|
| `--dump-dom` 单次渲染无法处理动态页面 | 金融K线/SPA无法获取 | **P0** |
| 无 Cookie/Session 管理 | 登录态页面无法访问 | **P0** |
| 被 Cloudflare/WAF 拦截 | 大量网站返回验证页 | **P0** |
| 无 MediaWiki API 专用通道 | Wiki 页面提取低效 | **P1** |
| 无金融数据 API 集成 | K线只能爬HTML | **P1** |
| 正文提取算法无法处理表格/结构化数据 | 数据丢失 | **P1** |
| 无请求重试/退避/代理轮换 | 不稳定 | **P2** |

## 二、业界调研

### 2.1 大模型 Web 能力对比

| **模型/产品** | **Web 能力** | **参考价值** |
|:---|:---|:---|
| **Anthropic Computer Use** | 截图→模型输出动作→执行闭环 | GUI Agent 范式参考 |
| **BrowserQwen (Qwen-Agent)** | 本地浏览器插件 + 服务协同 | 端+云架构参考 |
| **Gemini Grounding** | 模型触发搜索 + 引用元数据 | 可审计引用机制 |
| **OpenAI Web Search** | API 内置搜索工具 | 搜索聚合模式 |
| **WebArena** (论文) | 真实网站评测基准 | 失败模式分类 |
| **SeeAct** (ICML 2024) | 视觉+DOM 多模态 grounding | 截图+选择器双通道 |
| **Mind2Web** (NeurIPS 2023) | HTML 元素过滤+操作序列 | DOM 裁剪策略 |

### 2.2 反拦截方案

| **方案** | **原理** | **适用性** |
|:---|:---|:---|
| `--disable-blink-features=AutomationControlled` | 隐藏 headless 标记 | 基础防护 ✅ |
| Stealth 参数集 (CDP flags) | 修补 `navigator.webdriver` 等 | 中级防护 ✅ |
| 真实浏览器 Profile + Cookie | 模拟真实用户 Session | 高级防护 ✅ |
| 代理池 + IP 轮换 | 分散请求源 | 大规模采集 |
| Playwright stealth 插件 | 指纹伪装 | Go 需替代方案 |

### 2.3 场景化最佳方案

#### 金融 K 线数据

**推荐**: API 优先 + 页面抓取兜底

| **数据源** | **方式** | **适用** |
|:---|:---|:---|
| 东方财富 HTTP API | `push2his.eastmoney.com/api/qt/stock/kline` | A股日/周/月K |
| 新浪财经 API | `money.finance.sina.com.cn/quotes_service/api` | A股实时+历史 |
| Yahoo Finance | `query1.finance.yahoo.com/v8/finance/chart` | 美股/港股 |
| Alpha Vantage | REST API (需 key) | 全球市场 |

#### Wiki 文章

**推荐**: MediaWiki API 优先 + 页面抓取兜底

```
GET https://en.wikipedia.org/w/api.php?action=parse&page=XXX&prop=text&format=json
GET https://zh.wikipedia.org/w/api.php?action=query&titles=XXX&prop=extracts&exintro&format=json
```

## 三、技术方案

### 3.1 三层路由架构

```
                    ┌─────────────┐
                    │  URL 分类器  │ ← 域名规则 + 场景检测
                    └──────┬──────┘
                ┌──────────┼──────────┐
                ▼          ▼          ▼
        ┌──────────┐ ┌──────────┐ ┌──────────┐
        │ API 通道 │ │ 浏览器渲染│ │ HTTP 直连│
        │(金融/Wiki)│ │(CDP 增强) │ │(Readability)│
        └──────────┘ └──────────┘ └──────────┘
                ┌──────────┼──────────┐
                ▼          ▼          ▼
            ┌─────────────────────────────┐
            │     统一 FetchResult        │
            │  URL, Title, Text, HTML,    │
            │  Tables[], Images[],        │
            │  Metadata{source, ts, hash} │
            └─────────────────────────────┘
```

### 3.2 增强模块

#### A. Stealth 浏览器增强 (`pkg/browser/stealth.go`)

- Chrome 启动参数 stealth 集合 (20+ 反检测 flags)
- `navigator.webdriver` 属性覆盖
- WebGL/Canvas 指纹随机化
- 等待策略: networkIdle + selector + timeout 三选最快

#### B. MediaWiki API 客户端 (`pkg/browser/mediawiki.go`)

- 自动检测 Wikipedia/Fandom/MediaWiki 站点
- 使用 `action=parse` API 获取 HTML 渲染内容
- `action=query&prop=extracts` 获取纯文本摘要
- 自动处理分页 (`continue` token)

#### C. 金融数据 API (`pkg/browser/finance_api.go`)

- 东方财富 K 线 API (免费, 无需 key)
- 新浪财经行情 API
- 输出标准化 OHLCV 结构体
- 支持日/周/月级别

#### D. 智能正文提取增强 (`extractMainText` 改进)

- 表格提取: `<table>` → Markdown 表格
- 列表提取: `<ul>/<ol>` → 层级文本
- 代码块提取: `<pre>/<code>` → 保留格式

### 3.3 统一 FetchResult

```go
type FetchResult struct {
    URL       string
    Title     string
    Text      string            // 正文 (纯文本)
    HTML      string            // 原始 HTML
    Tables    []TableData       // 结构化表格
    Metadata  FetchMetadata     // 来源、时间戳、内容哈希
    Source    string            // "api"/"browser"/"http"
}

type FetchMetadata struct {
    FetchedAt   time.Time
    ContentHash string   // SHA256 用于去重
    StatusCode  int
    ContentType string
    Canonical   string   // <link rel="canonical">
}
```

## 四、实施计划

| **阶段** | **内容** | **优先级** |
|:---|:---|:---|
| Phase 1 | Stealth 参数增强 + 等待策略 | P0 |
| Phase 2 | MediaWiki API 客户端 | P1 |
| Phase 3 | 金融数据 API (东方财富K线) | P1 |
| Phase 4 | 正文提取增强 (表格/列表) | P1 |
| Phase 5 | 代理池 + 请求退避 | P2 |

## 五、指标

| **指标** | **当前** | **目标** |
|:---|:---|:---|
| WAF 拦截率 | ~40% | <10% |
| Wiki 提取成功率 | ~60% | >95% |
| 金融 K 线获取延迟 | N/A (不支持) | <2s |
| 正文提取准确率 | ~70% | >85% |
| 表格/列表保留率 | ~20% | >80% |
