// Package wiki 实现基于 Karpathy「LLM Wiki」思想的三层知识库引擎：
// 原始层（raw，不可变来源文章）、维基层（wiki，由 LLM 维护的概念页与回链）、
// 模式层（schema，配置与约束规则）。
package wiki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultSchemaYAML = `# LLM Wiki Schema — 指导 LLM 如何维护此知识库
# 这是整个系统的关键配置文件，它让 LLM 从通用聊天模型变成有纪律的 wiki 维护者。
# 随着你在具体领域中不断实践，此文件也会与你和 LLM 一起持续演化。
version: 3

# === 三层架构 ===
# Raw 层: 原始资料集合（文章、论文、笔记等）— 不可变的事实来源，LLM 只读不写
# Wiki 层: LLM 生成的 Markdown 概念页 — LLM 负责创建、更新、维护交叉引用
# Schema 层: 本文件 — 指导 LLM 的工作流程和规范

# ============================================================
# 页面模板 (Page Templates)
# ============================================================
# 所有新建 wiki 页面必须遵循此模板，保证结构一致性。
# LLM 在整理(Organize)和摄取(Ingest)时必须严格按此结构输出。
page_templates:
  concept: |
    # {title}
    
    > **分类**: {category}  |  **标签**: {tags}
    > **摘要**: 一句话概括此概念
    > **更新时间**: {date}
    
    ## 核心内容
    (主体知识，保留关键事实，简洁可检索)
    
    ## 关键要点
    - 要点1
    - 要点2
    - 要点3
    
    ## 关联概念
    - [[related-slug-1]] — 关联原因
    - [[related-slug-2]] — 关联原因
    
    ## 来源
    - 来自: raw/{source-file}
    - 原始链接: {url}
  
  comparison: |
    # {title}: A vs B
    
    > **分类**: {category}  |  **对比主题**: {topic}
    
    | 维度 | A | B |
    |------|---|---|
    | ... | ... | ... |
    
    ## 结论
    (对比总结)
    
    ## 来源
    - [[source-a]], [[source-b]]
  
  timeline: |
    # {title} 时间线
    
    > **分类**: {category}
    
    ## 时间线
    - **{date1}**: 事件1
    - **{date2}**: 事件2
    
    ## 关联概念
    - [[related-slug]]

# ============================================================
# 分类系统 (Classification System)
# ============================================================
# 每个页面必须归属一个主分类，可有多个标签。
# LLM 在创建/整理页面时必须从此列表中选择分类和标签。
categories:
  - name: "技术"
    slug: "tech"
    tags: ["programming", "ai", "ml", "system", "devops", "database", "security", "algorithm"]
    description: "技术相关知识：编程、AI、系统架构、安全等"
  - name: "产品"
    slug: "product"
    tags: ["product", "design", "ux", "feature", "roadmap", "user-research"]
    description: "产品设计、用户体验、功能规划"
  - name: "商业"
    slug: "business"
    tags: ["business", "finance", "market", "strategy", "investment", "startup"]
    description: "商业模式、金融、市场分析、投资"
  - name: "研究"
    slug: "research"
    tags: ["research", "paper", "experiment", "methodology", "data-analysis"]
    description: "学术研究、论文、实验方法"
  - name: "人文"
    slug: "humanities"
    tags: ["history", "philosophy", "culture", "education", "language"]
    description: "人文社科、历史、哲学、文化"
  - name: "通用"
    slug: "general"
    tags: ["general", "note", "todo", "misc"]
    description: "未分类或通用内容"

wiki:
  link_syntax: "[[slug]]"
  max_concept_body_chars: 32000
  index_page: "_index"
  # 命名规范: slug 使用小写字母 + 短横线，支持中文拼音
  slug_convention: "lowercase-hyphenated"
  # 页面元数据: 每个页面顶部必须包含 YAML frontmatter
  frontmatter_required:
    - "title"
    - "category"
    - "tags"
    - "created"
    - "updated"
    - "sources"

# ============================================================
# 摄取工作流 (Ingest Workflow)
# ============================================================
# LLM 摄取原始资料时必须严格按此步骤执行。
ingest:
  merge_strategy: "llm_full_replace"
  workflow:
    step_1: "通读原始资料，理解主题和核心内容"
    step_2: "抽取 3-10 个关键概念和实体（人名、技术名词、事件等）"
    step_3: "为每个概念确定 slug（检查是否已有同名页面，有则更新）"
    step_4: "从 categories 列表中为每个概念选择最合适的分类和标签"
    step_5: "使用 page_templates.concept 模板撰写/更新每个概念的 wiki 页面"
    step_6: "建立充分的 [[slug]] 交叉引用（一篇资料通常影响 5-15 个页面）"
    step_7: "更新 _index.md 目录（按分类分组列出所有页面）"
    step_8: "检查已有页面是否需要因新资料而更新（补充关联、修正信息）"
  quality_rules:
    - "每个概念页不少于 200 字核心内容"
    - "每个概念页至少 2 个 [[交叉引用]]"
    - "严禁编造原始资料中不存在的事实"
    - "保留原始数据（数字、日期、引用）的精确性"
    - "分类和标签必须从 categories 列表中选择"

# ============================================================
# 查询配置 (Query Configuration)
# ============================================================
query:
  navigation_mode: "snapshot"
  auto_archive: true
  flexible_format: true
  # 查询回答的质量要求
  answer_rules:
    - "仅基于 wiki 现有内容回答，不臆造"
    - "不足时明确说明哪些信息缺失"
    - "引用具体页面: 参见 [[slug]]"
    - "如果查询具有归档价值，自动生成概念页"
  # 归档条件: 满足以下任一条件时将查询结果归档
  archive_triggers:
    - "包含深度分析或推理"
    - "包含对比或总结"
    - "用户明确要求保存"
    - "涉及多个概念的综合"

# ============================================================
# Lint 检查项 (Quality Checks)
# ============================================================
# 定期运行的健康检查，每项有明确的修复建议。
lint:
  checks:
    broken_links:
      description: "检测 [[slug]] 指向不存在的页面"
      severity: "error"
      auto_fix: "创建缺失的目标页面（包含占位内容）"
    orphaned_pages:
      description: "没有任何其他页面引用的孤立页"
      severity: "warning"
      auto_fix: "在相关主题页面中添加引用"
    missing_frontmatter:
      description: "缺少必要的 YAML frontmatter 字段"
      severity: "error"
      auto_fix: "根据内容推断并补充 frontmatter"
    missing_category:
      description: "页面未分类或使用了不存在的分类"
      severity: "error"
      auto_fix: "根据内容自动分类到最合适的类别"
    missing_cross_refs:
      description: "内容提到了其他概念但未使用 [[slug]] 引用"
      severity: "warning"
      auto_fix: "自动添加交叉引用链接"
    contradictions:
      description: "不同页面中的矛盾信息"
      severity: "critical"
      auto_fix: "标记矛盾并在两个页面中添加警告"
    outdated_content:
      description: "超过 90 天未更新且有新相关资料的页面"
      severity: "info"
      auto_fix: "标记为待更新"
    empty_sections:
      description: "页面中存在空的章节（只有标题没有内容）"
      severity: "warning"
      auto_fix: "填充内容或删除空章节"
    duplicate_concepts:
      description: "不同 slug 描述相同概念"
      severity: "warning"
      auto_fix: "合并为一个页面，另一个重定向"
    research_gaps:
      description: "概念提及但缺乏深入内容的领域"
      severity: "info"
      auto_fix: "标记为待研究"

# ============================================================
# 整理任务 (Organize Tasks)
# ============================================================
# LLM 执行全量/增量整理时必须按此优先级执行。
organize:
  triggers:
    - "new_raw_files"      # 新增 raw 文件时自动触发增量整理
    - "manual"             # 手动触发
    - "scheduled"          # 定时触发（默认每日一次）
  # 整理任务按优先级排序（从高到低）
  tasks:
    - name: "classify"
      priority: 1
      description: "为所有缺少分类的页面分配 category 和 tags"
    - name: "apply_template"
      priority: 2
      description: "检查页面结构，不符合 page_template 的重新格式化"
    - name: "cross_reference"
      priority: 3
      description: "扫描所有页面内容，补充缺失的 [[slug]] 交叉引用"
    - name: "update_index"
      priority: 4
      description: "重建 _index.md，按分类分组列出所有页面及摘要"
    - name: "summarize"
      priority: 5
      description: "为缺少摘要的页面生成一句话概括"
    - name: "deduplicate"
      priority: 6
      description: "合并重复概念页面"
    - name: "fill_gaps"
      priority: 7
      description: "基于现有页面间的关联，识别并标记知识空缺"
    - name: "archive_stale"
      priority: 8
      description: "将长期未更新且无引用的页面移入 wiki/_archive/"
  # 整理质量检查: 每次整理后自动运行
  post_organize_lint: true
  # 增量整理: 每批处理的 raw 文件数
  incremental_batch_size: 5
  # 全量整理: 每批处理的 wiki 页面数
  full_batch_size: 20
`

	ingestSystemPrompt = `你是「LLM Wiki」的策展助手，严格遵循 Schema 层规范。
用户会提供一篇原始资料（可能附带标题与来源）和 schema.yaml 配置。

按 schema.ingest.workflow 步骤执行:
1. 通读原始资料，理解主题和核心内容
2. 抽取 3-10 个关键概念和实体（人名、技术名词、事件等）
3. 为每个概念确定 slug（小写短横线，检查是否已有同名页面）
4. 从 schema.categories 中为每个概念选择最合适的分类和标签
5. 使用 schema.page_templates.concept 结构撰写 wiki 页面:
   - # {title}
   - > **分类**: {category}  |  **标签**: {tags}
   - > **摘要**: 一句话概括此概念
   - ## 核心内容 (不少于200字，保留关键事实)
   - ## 关键要点 (3-5个要点)
   - ## 关联概念 (使用 [[slug]]，至少2个交叉引用)
   - ## 来源 (标注来自哪个 raw 文件)
6. 建立充分的 [[slug]] 交叉引用（一个来源通常影响 5-15 个 wiki 页面）
7. 如果概念属于已有的相关页面，也输出更新后的该页面
8. 检查已有页面是否需要因新资料而更新

输出必须是单一 JSON 对象，不要代码围栏：
{"pages":[{"slug":"...","title":"...","body_markdown":"..."}]}

强制质量规则:
- pages 非空
- body_markdown 使用 [[slug]] 表示指向 wiki/<slug>.md 的链接
- 不同 page 的 slug 必须唯一
- 正文简洁、可检索，保留关键事实，严禁编造
- 分类和标签必须从 schema.categories 中选择
- 尽量多地建立概念间的关联`

	querySystemPrompt = `你是「LLM Wiki」的问答助手。下面提供 wiki 目录中页面列表及部分正文摘录。
请仅依据这些内容回答用户问题；若信息不足请明确说明缺口，不要臆造来源中不存在的事实。
回答使用用户提问语言（若用户用中文则中文答）。`
)

var (
	reScript = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyle  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reTags   = regexp.MustCompile(`<[^>]+>`)
	reWS     = regexp.MustCompile(`\s+`)
	reTitle  = regexp.MustCompile(`(?is)<title[^>]*>([^<]*)</title>`)
	reH1     = regexp.MustCompile(`(?is)<h1[^>]*>([^<]*)</h1>`)
	// wikilink 匹配 [[slug]] 或 [[alias|slug]]，取最后一个竖线后的 slug 段。
	reWikiLink = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
	// slugify 用：将非字母数字序列（含 Unicode 字母数字）折叠为短横线。
	reSlugUnsafe = regexp.MustCompile(`[^\p{L}\p{N}]+`)
)

// LLMClient 定义 Wiki 引擎对 LLM 的最小依赖，与 api.Client.SimpleComplete 兼容。
type LLMClient interface {
	SimpleComplete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
	RawComplete(ctx context.Context, contentJSON json.RawMessage, maxTokens int) (string, error)
}

// BrowserFetcher 浏览器抓取接口，用于绕过防爬虫。
type BrowserFetcher interface {
	Available() bool
	Fetch(ctx context.Context, url string) (title, text, html string, err error)
}

// Engine 是 LLM Wiki 知识库的运行时入口，负责摄取、查询与质检。
// 同一 Engine 实例上的公开方法使用互斥锁序列化，避免并发写盘与 git 提交交错。
type Engine struct {
	RepoDir    string // Git 仓库根目录，例如 ~/knowledge-wiki
	APIKey     string // 保留用于 HTTP 拉取 URL (非 LLM)
	BaseURL    string // 保留向后兼容
	Model      string // 保留向后兼容
	llm        LLMClient
	browser    BrowserFetcher
	httpClient *http.Client
	mu         sync.Mutex
}

// SetBrowser 设置浏览器抓取器（用于绕过防爬虫）。
func (e *Engine) SetBrowser(b BrowserFetcher) {
	e.browser = b
}

// BrokenLink 表示从源页指向不存在目标页的坏链。
type BrokenLink struct {
	SourcePage string // 含有错误链接的 wiki 页面文件名（不含路径）
	TargetPage string // 被引用但缺失的目标 slug
}

// LintReport 汇总回链健康度与库规模统计。
type LintReport struct {
	BrokenLinks   []BrokenLink
	OrphanedPages []string
	TotalPages    int
	TotalRaw      int
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type ingestLLMPage struct {
	Slug          string `json:"slug"`
	Title         string `json:"title"`
	BodyMarkdown  string `json:"body_markdown"`
	BodyMarkdown2 string `json:"body"` // 兼容 LLM 偶发使用 body 字段
}

type ingestLLMResult struct {
	Pages []ingestLLMPage `json:"pages"`
}

// NewEngine 构造引擎实例，不自动初始化仓库；首次操作前可调用 EnsureRepo。
// 保留旧签名的向后兼容：若不传 LLM client，Engine 将在调用 LLM 时报错。
func NewEngine(repoDir, apiKey, baseURL, model string) *Engine {
	return &Engine{
		RepoDir: strings.TrimSpace(repoDir),
		APIKey:  strings.TrimSpace(apiKey),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Model:   strings.TrimSpace(model),
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// NewEngineWithLLM 构造引擎实例，使用已有的 LLM client (推荐)。
func NewEngineWithLLM(repoDir string, llm LLMClient) *Engine {
	return &Engine{
		RepoDir: strings.TrimSpace(repoDir),
		llm:     llm,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// SetLLM 设置 LLM 客户端。
func (e *Engine) SetLLM(llm LLMClient) {
	e.llm = llm
}

// completeLLM 统一 LLM 调用入口：优先用注入的 LLMClient，降级用旧的 HTTP callLLM。
func (e *Engine) completeLLM(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	if e.llm != nil {
		return e.llm.SimpleComplete(ctx, systemPrompt, userPrompt)
	}
	return callLLM(ctx, e.httpClient, e.APIKey, e.BaseURL, e.Model, systemPrompt, userPrompt)
}

// completeLLMWithMaxTokens 使用指定的 maxTokens 调用 LLM。
func (e *Engine) completeLLMWithMaxTokens(ctx context.Context, systemPrompt, userPrompt string, maxTokens int) (string, error) {
	if e.llm != nil {
		contentJSON := json.RawMessage(`[{"type":"text","text":` + string(mustMarshalString(userPrompt)) + `}]`)
		return e.llm.RawComplete(ctx, contentJSON, maxTokens)
	}
	return callLLM(ctx, e.httpClient, e.APIKey, e.BaseURL, e.Model, systemPrompt, userPrompt)
}

func mustMarshalString(s string) json.RawMessage {
	data, _ := json.Marshal(s)
	return data
}

// schemaVersion 内置 Schema 版本号，升级时递增。
// EnsureRepo 会比较文件中的 version 字段，若低于此值则自动升级。
const schemaVersion = 3

// EnsureRepo 若目录或 Git 结构不完整则初始化：创建 raw/wiki/schema、写入默认 schema.yaml，并在需要时 git init。
// 如果 schema.yaml 已存在但版本低于内置版本，会自动升级。
func EnsureRepo(repoDir string) error {
	if repoDir == "" {
		return fmt.Errorf("wiki: repoDir 为空")
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return fmt.Errorf("wiki: 创建仓库根目录: %w", err)
	}
	for _, sub := range []string{"raw", "wiki", "schema"} {
		p := filepath.Join(repoDir, sub)
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fmt.Errorf("wiki: 创建子目录 %s: %w", sub, err)
		}
	}
	schemaPath := filepath.Join(repoDir, "schema", "schema.yaml")
	needWrite := false
	if _, err := os.Stat(schemaPath); os.IsNotExist(err) {
		needWrite = true
	} else if err != nil {
		return fmt.Errorf("wiki: 检查 schema: %w", err)
	} else {
		// 检查版本号，低于内置版本则升级
		if existing, err := os.ReadFile(schemaPath); err == nil {
			if !strings.Contains(string(existing), fmt.Sprintf("version: %d", schemaVersion)) {
				needWrite = true
			}
		}
	}
	if needWrite {
		if err := os.WriteFile(schemaPath, []byte(defaultSchemaYAML), 0o644); err != nil {
			return fmt.Errorf("wiki: 写入默认 schema: %w", err)
		}
	}
	gitDir := filepath.Join(repoDir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		cmd := exec.Command("git", "init")
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("wiki: git init 失败: %w: %s", err, strings.TrimSpace(string(out)))
		}
	} else if err != nil {
		return fmt.Errorf("wiki: 检查 .git: %w", err)
	}
	return nil
}

// Ingest 从 URL 拉取 HTML，抽取正文写入 raw/，调用 LLM 更新 wiki 概念页，并尝试 git 提交。
func (e *Engine) Ingest(ctx context.Context, url string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.RepoDir == "" {
		return fmt.Errorf("wiki: RepoDir 未设置")
	}
	if err := EnsureRepo(e.RepoDir); err != nil {
		return err
	}
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("wiki: URL 为空")
	}

	var title, text string

	// 优先使用浏览器抓取（绕过防爬虫/JS渲染）
	if e.browser != nil && e.browser.Available() {
		bTitle, bText, _, bErr := e.browser.Fetch(ctx, url)
		if bErr == nil && strings.TrimSpace(bText) != "" {
			title = bTitle
			text = bText
		}
	}

	// 降级: HTTP 直接抓取
	if text == "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("wiki: 构造请求: %w", err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

		resp, err := e.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("wiki: 获取 URL: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("wiki: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		htmlBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("wiki: 读取响应体: %w", err)
		}
		htmlStr := string(htmlBytes)
		title = pickTitle(htmlStr)
		text = extractContent(htmlStr)
	}

	if strings.TrimSpace(title) == "" {
		title = "untitled"
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("wiki: 未能从页面抽取有效正文")
	}
	dateStr := time.Now().UTC().Format("2006-01-02")
	slug := slugify(title)
	rawName := fmt.Sprintf("%s_%s.md", dateStr, slug)
	rawPath := filepath.Join(e.RepoDir, "raw", rawName)
	frontmatter := fmt.Sprintf("---\nsource: %q\ndate: %q\ntitle: %q\n---\n\n", url, dateStr, title)
	if err := os.WriteFile(rawPath, []byte(frontmatter+text+"\n"), 0o644); err != nil {
		return fmt.Errorf("wiki: 写入 raw 文件: %w", err)
	}

	userPrompt := fmt.Sprintf("来源 URL: %s\n标题: %s\n\n正文:\n%s", url, title, truncateRunes(text, 24000))
	rawJSON, err := e.completeLLM(ctx, ingestSystemPrompt, userPrompt)
	if err != nil {
		return fmt.Errorf("wiki: LLM 摄取: %w", err)
	}
	pages, err := parseIngestJSON(rawJSON)
	if err != nil {
		return fmt.Errorf("wiki: 解析摄取 JSON: %w", err)
	}
	wikiDir := filepath.Join(e.RepoDir, "wiki")
	for _, p := range pages {
		s := strings.TrimSpace(p.Slug)
		if s == "" {
			continue
		}
		body := strings.TrimSpace(p.BodyMarkdown)
		if body == "" {
			body = strings.TrimSpace(p.BodyMarkdown2)
		}
		header := ""
		if t := strings.TrimSpace(p.Title); t != "" {
			header = "# " + t + "\n\n"
		}
		wp := filepath.Join(wikiDir, s+".md")
		if err := os.WriteFile(wp, []byte(header+body+"\n"), 0o644); err != nil {
			return fmt.Errorf("wiki: 写入 wiki/%s.md: %w", s, err)
		}
	}
	if err := gitCommit(e.RepoDir, fmt.Sprintf("wiki: ingest %s (%s)", slug, dateStr)); err != nil {
		return err
	}
	return nil
}

// Query 将当前 wiki 快照提供给 LLM，由其「导航」推理后返回答案文本。
func (e *Engine) Query(ctx context.Context, question string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.RepoDir == "" {
		return "", fmt.Errorf("wiki: RepoDir 未设置")
	}
	if err := EnsureRepo(e.RepoDir); err != nil {
		return "", err
	}
	q := strings.TrimSpace(question)
	if q == "" {
		return "", fmt.Errorf("wiki: 问题为空")
	}
	bundle, err := buildWikiContextBundle(e.RepoDir, 12000)
	if err != nil {
		return "", err
	}
	user := fmt.Sprintf("用户问题:\n%s\n\n---\n目录与摘录:\n%s", q, bundle)
	return e.completeLLM(ctx, querySystemPrompt, user)
}

// Lint 检查 wiki 页之间的 [[slug]] 回链是否指向存在的文件，并找出无任何入链的孤立页。
func (e *Engine) Lint(ctx context.Context) (*LintReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.RepoDir == "" {
		return nil, fmt.Errorf("wiki: RepoDir 未设置")
	}
	if err := EnsureRepo(e.RepoDir); err != nil {
		return nil, err
	}
	report := &LintReport{}
	rawDir := filepath.Join(e.RepoDir, "raw")
	wikiDir := filepath.Join(e.RepoDir, "wiki")

	rawEntries, err := os.ReadDir(rawDir)
	if err != nil {
		return nil, fmt.Errorf("wiki: 读取 raw: %w", err)
	}
	for _, ent := range rawEntries {
		if ent.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(ent.Name()), ".md") {
			report.TotalRaw++
		}
	}

	wikiFiles, err := collectWikiFiles(wikiDir)
	if err != nil {
		return nil, err
	}
	report.TotalPages = len(wikiFiles)
	if report.TotalPages == 0 {
		return report, nil
	}

	existing := make(map[string]struct{}, len(wikiFiles))
	incoming := make(map[string]int, len(wikiFiles))
	for _, wf := range wikiFiles {
		existing[wf.slug] = struct{}{}
		incoming[wf.slug] = 0
	}

	var broken []BrokenLink
	for _, wf := range wikiFiles {
		for _, target := range wf.links {
			if _, ok := existing[target]; !ok {
				broken = append(broken, BrokenLink{SourcePage: wf.name, TargetPage: target})
				continue
			}
			incoming[target]++
		}
	}
	report.BrokenLinks = broken

	var orphans []string
	for slug, cnt := range incoming {
		if cnt == 0 {
			orphans = append(orphans, slug+".md")
		}
	}
	report.OrphanedPages = orphans

	return report, nil
}

// ExtractContentForTest 导出 extractContent 用于测试。
func ExtractContentForTest(html string) string { return extractContent(html) }

// extractContent 从 HTML 中提取正文 (v2: 文本密度分析 + 噪声过滤)。
// 对动态渲染页面 (今日头条等) 过滤评论、推荐、热榜等噪声区域。
func extractContent(htmlBody string) string {
	// 移除 script/style
	s := reScript.ReplaceAllString(htmlBody, "")
	s = reStyle.ReplaceAllString(s, "")
	// 移除已知噪声容器
	s = reNoiseContainer.ReplaceAllString(s, "")
	// 移除导航/页脚
	s = reNavFooter.ReplaceAllString(s, "")

	// 尝试 <article> 精确提取
	if m := reArticle.FindStringSubmatch(s); len(m) > 1 {
		text := reTags.ReplaceAllString(m[1], " ")
		text = reWS.ReplaceAllString(text, " ")
		text = strings.TrimSpace(text)
		if len([]rune(text)) > 200 {
			return text
		}
	}

	// 降级: 全文剥离
	s = reTags.ReplaceAllString(s, " ")
	s = reWS.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

var (
	reNoiseContainer = regexp.MustCompile(`(?is)<(?:div|section)[^>]*(?:class|id)\s*=\s*"[^"]*(?:comment|recommend|sidebar|related|trending|hotlist|ad-|advertisement|social-share|breadcrumb)[^"]*"[^>]*>.*?</(?:div|section)>`)
	reNavFooter      = regexp.MustCompile(`(?is)<(?:nav|header|footer|aside)[^>]*>.*?</(?:nav|header|footer|aside)>`)
	reArticle        = regexp.MustCompile(`(?is)<article[^>]*>(.*?)</article>`)
)

// slugify 将标题转为适合文件名的短横线 slug（Unicode 友好，小写、空白与标点转为 -）。
func slugify(title string) string {
	s := strings.TrimSpace(title)
	s = strings.ToLower(s)
	s = reSlugUnsafe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = fmt.Sprintf("page-%d", time.Now().UnixNano())
	}
	return s
}

// gitCommit 在仓库中执行 git add -A 与 git commit；若无变更则静默成功。
func gitCommit(repoDir, message string) error {
	st := exec.Command("git", "-C", repoDir, "status", "--porcelain")
	out, err := st.Output()
	if err != nil {
		return fmt.Errorf("wiki: git status: %w", err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil
	}
	add := exec.Command("git", "-C", repoDir, "add", "-A")
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("wiki: git add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	commit := exec.Command("git", "-C", repoDir, "commit", "-m", message)
	cout, cerr := commit.CombinedOutput()
	if cerr != nil {
		// 无变更或其他非致命情况
		if strings.Contains(string(cout), "nothing to commit") {
			return nil
		}
		return fmt.Errorf("wiki: git commit: %w: %s", cerr, strings.TrimSpace(string(cout)))
	}
	return nil
}

// callLLM 调用 OpenAI 兼容的 chat/completions 接口（含 DashScope 兼容模式）。
func callLLM(ctx context.Context, client *http.Client, apiKey, baseURL, model, systemPrompt, userPrompt string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if apiKey == "" || baseURL == "" || model == "" {
		return "", fmt.Errorf("wiki: LLM 配置不完整 (apiKey/baseURL/model)")
	}
	endpoint := baseURL + "/chat/completions"
	body := chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("wiki: 序列化请求: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("wiki: 构造 LLM 请求: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("wiki: LLM 请求失败: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("wiki: 读取 LLM 响应: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("wiki: LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("wiki: 解析 LLM 响应 JSON: %w", err)
	}
	if parsed.Error != nil && strings.TrimSpace(parsed.Error.Message) != "" {
		return "", fmt.Errorf("wiki: LLM API 错误: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("wiki: LLM 返回空内容")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}

type wikiFileInfo struct {
	name  string
	slug  string
	links []string
}

func collectWikiFiles(wikiDir string) ([]wikiFileInfo, error) {
	ents, err := os.ReadDir(wikiDir)
	if err != nil {
		return nil, fmt.Errorf("wiki: 读取 wiki 目录: %w", err)
	}
	var out []wikiFileInfo
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		data, err := os.ReadFile(filepath.Join(wikiDir, name))
		if err != nil {
			return nil, fmt.Errorf("wiki: 读取 %s: %w", name, err)
		}
		links := parseWikiLinks(string(data))
		out = append(out, wikiFileInfo{name: name, slug: base, links: links})
	}
	return out, nil
}

func parseWikiLinks(md string) []string {
	found := reWikiLink.FindAllStringSubmatch(md, -1)
	seen := make(map[string]struct{})
	var slugs []string
	for _, m := range found {
		if len(m) < 2 {
			continue
		}
		raw := strings.TrimSpace(m[1])
		if raw == "" {
			continue
		}
		part := raw
		if idx := strings.LastIndex(raw, "|"); idx >= 0 && idx+1 < len(raw) {
			part = strings.TrimSpace(raw[idx+1:])
		}
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		slugs = append(slugs, part)
	}
	return slugs
}

func pickTitle(html string) string {
	if m := reTitle.FindStringSubmatch(html); len(m) > 1 {
		t := strings.TrimSpace(stripInnerTags(m[1]))
		if t != "" {
			return t
		}
	}
	if m := reH1.FindStringSubmatch(html); len(m) > 1 {
		t := strings.TrimSpace(stripInnerTags(m[1]))
		if t != "" {
			return t
		}
	}
	return ""
}

func stripInnerTags(s string) string {
	return strings.TrimSpace(reTags.ReplaceAllString(s, " "))
}

func parseIngestJSON(raw string) ([]ingestLLMPage, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	var res ingestLLMResult
	if err := json.Unmarshal([]byte(s), &res); err != nil {
		return nil, err
	}
	if len(res.Pages) == 0 {
		return nil, fmt.Errorf("pages 为空")
	}
	return res.Pages, nil
}

func buildWikiContextBundle(repoDir string, maxRunes int) (string, error) {
	wikiDir := filepath.Join(repoDir, "wiki")
	ents, err := os.ReadDir(wikiDir)
	if err != nil {
		return "", fmt.Errorf("wiki: 读取 wiki: %w", err)
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "（当前 wiki 目录为空）", nil
	}
	var b strings.Builder
	b.WriteString("页面列表:\n")
	for _, n := range names {
		b.WriteString("- ")
		b.WriteString(n)
		b.WriteByte('\n')
	}
	b.WriteString("\n---\n摘录:\n")
	used := 0
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(wikiDir, n))
		if err != nil {
			return "", fmt.Errorf("wiki: 读取 %s: %w", n, err)
		}
		chunk := fmt.Sprintf("\n### %s\n%s\n", n, string(data))
		runes := []rune(chunk)
		if used+len(runes) > maxRunes {
			remain := maxRunes - used
			if remain <= 0 {
				break
			}
			b.WriteString(string(runes[:remain]))
			b.WriteString("\n…(已截断)…\n")
			break
		}
		b.WriteString(chunk)
		used += len(runes)
	}
	return b.String(), nil
}

// buildWikiIndexOnly 只返回 wiki 页面目录列表，不包含内容。
func buildWikiIndexOnly(repoDir string) (string, error) {
	wikiDir := filepath.Join(repoDir, "wiki")
	ents, err := os.ReadDir(wikiDir)
	if err != nil {
		return "", fmt.Errorf("wiki: 读取 wiki: %w", err)
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "（当前 wiki 目录为空）", nil
	}
	var b strings.Builder
	b.WriteString("页面列表:\n")
	for _, n := range names {
		b.WriteString("- ")
		b.WriteString(n)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…(正文已截断用于提示)…"
}

// ══════════════════════════════════════════════════════════
// 扩展能力: Karpathy LLM Wiki 完整操作集
// ══════════════════════════════════════════════════════════

const (
	organizeSystemPrompt = `你是「LLM Wiki」的策展维护者。你必须严格遵循下方 schema.yaml 中定义的所有规则。

## 整理任务（按 organize.tasks 优先级从高到低执行）:

1. **分类(classify)** [P1]: 为每个页面分配 category（从 categories 列表选择）和 tags
2. **套用模板(apply_template)** [P2]: 每个页面必须符合 page_templates.concept 结构:
   - 必须有 > **分类** 和 > **摘要** 行
   - 必须有 ## 核心内容、## 关键要点、## 关联概念、## 来源 四个章节
   - 不符合的页面必须重写为正确结构
3. **交叉引用(cross_reference)** [P3]: 扫描页面正文中提到的概念，添加 [[slug]] 链接
   - 每个页面至少 2 个交叉引用
   - 一个概念通常影响 5-15 个相关页面
4. **更新索引(update_index)** [P4]: 创建/更新 _index.md，按分类分组列出所有页面
5. **补充摘要(summarize)** [P5]: 缺摘要的页面生成一句话概括
6. **去重(deduplicate)** [P6]: 高度相似的页面合并
7. **填补空缺(fill_gaps)** [P7]: 被 [[slug]] 引用但不存在的概念，创建框架页
8. **归档(archive_stale)** [P8]: 长期无更新无引用的页面标记为归档

## 强制质量规则:
- 每个 slug 全局唯一，小写短横线格式
- 核心内容不少于 200 字
- 不编造 raw 中不存在的事实
- 分类和标签只能从 schema.categories 中选择
- 如果页面过多，分批返回最需要更新的（优先级: P1 > P2 > P3）`

	incrementalOrganizePrompt = `你是「LLM Wiki」的增量维护者。你必须严格遵循 schema.yaml 中的所有规则。

## 增量更新任务（按 ingest.workflow 步骤执行）:

1. **通读原始资料**，理解主题和核心内容
2. **抽取 3-10 个关键概念和实体**（人名、技术名词、事件等）
3. **确定每个概念的 slug**（检查是否已有同名页面，有则更新）
4. **从 categories 列表中选择**最合适的分类和标签
5. **使用 page_templates.concept 模板**撰写/更新每个概念页:
   - 必须有 > **分类**: {category}  |  **标签**: {tags}
   - 必须有 > **摘要**: 一句话概括
   - 必须有 ## 核心内容、## 关键要点、## 关联概念、## 来源
6. **建立充分的 [[slug]] 交叉引用**（每个页面至少 2 个）
7. **更新 _index.md** 目录
8. **检查已有页面**是否需要补充关联或修正

## 强制质量规则:
- 每个概念页核心内容不少于 200 字
- 每个页面至少 2 个 [[交叉引用]]
- 严禁编造原始资料中不存在的事实
- 分类和标签必须从 categories 列表中选择
- 保留原始数据（数字、日期、引用）的精确性

仅输出需要创建或更新的页面。
按以下格式输出每个页面：

---PAGE: <slug>
---TITLE: <页面标题>
---BODY:
<完整的 markdown 页面内容>
===END===`

	healthCheckPrompt = `你是「LLM Wiki」的健康检查专家。以下是当前 wiki 的全部页面目录与部分内容。
请进行全面的健康检查:

1. **矛盾检测**: 页面之间是否存在相互矛盾的信息
2. **过时检测**: 是否有被新资料取代的过时结论
3. **缺失检测**: 被提及但尚未建立页面的重要概念
4. **孤立检测**: 没有任何入链的孤立页面
5. **交叉引用**: 缺失的交叉引用
6. **新研究方向**: 基于现有内容，建议新的研究问题和信息来源

输出 JSON:
{
  "contradictions": [{"page1":"...","page2":"...","issue":"..."}],
  "outdated": [{"page":"...","issue":"..."}],
  "missing_concepts": ["concept1","concept2"],
  "orphaned_pages": ["page1","page2"],
  "missing_refs": [{"source":"...","should_link_to":"..."}],
  "research_suggestions": ["suggestion1","suggestion2"],
  "summary": "健康检查总结"
}`
)

// HealthReport 健康检查结果。
type HealthReport struct {
	Contradictions      []Contradiction    `json:"contradictions"`
	Outdated            []OutdatedItem     `json:"outdated"`
	MissingConcepts     []string           `json:"missing_concepts"`
	OrphanedPages       []string           `json:"orphaned_pages"`
	MissingRefs         []MissingRef       `json:"missing_refs"`
	ResearchSuggestions []string           `json:"research_suggestions"`
	Summary             string             `json:"summary"`
}

type Contradiction struct {
	Page1 string `json:"page1"`
	Page2 string `json:"page2"`
	Issue string `json:"issue"`
}

type OutdatedItem struct {
	Page  string `json:"page"`
	Issue string `json:"issue"`
}

type MissingRef struct {
	Source       string `json:"source"`
	ShouldLinkTo string `json:"should_link_to"`
}

// OrganizeResult 整理操作的结果。
type OrganizeResult struct {
	UpdatedPages int    `json:"updated_pages"`
	Log          string `json:"log"`
}

// IngestText 直接摄取纯文本内容（非 URL），用于飞书卡片/文件等场景。
func (e *Engine) IngestText(ctx context.Context, title, text, source string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.RepoDir == "" {
		return fmt.Errorf("wiki: RepoDir 未设置")
	}
	if err := EnsureRepo(e.RepoDir); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("wiki: 文本内容为空")
	}

	dateStr := time.Now().UTC().Format("2006-01-02")
	slug := slugify(title)
	if slug == "" {
		slug = fmt.Sprintf("text-%d", time.Now().UnixNano())
	}
	rawName := fmt.Sprintf("%s_%s.md", dateStr, slug)
	rawPath := filepath.Join(e.RepoDir, "raw", rawName)
	frontmatter := fmt.Sprintf("---\nsource: %q\ndate: %q\ntitle: %q\n---\n\n", source, dateStr, title)
	if err := os.WriteFile(rawPath, []byte(frontmatter+text+"\n"), 0o644); err != nil {
		return fmt.Errorf("wiki: 写入 raw 文件: %w", err)
	}

	userPrompt := fmt.Sprintf("来源: %s\n标题: %s\n\n正文:\n%s", source, title, truncateRunes(text, 24000))
	rawJSON, err := e.completeLLM(ctx, ingestSystemPrompt, userPrompt)
	if err != nil {
		return fmt.Errorf("wiki: LLM 摄取: %w", err)
	}
	pages, err := parseIngestJSON(rawJSON)
	if err != nil {
		return fmt.Errorf("wiki: 解析摄取 JSON: %w", err)
	}
	wikiDir := filepath.Join(e.RepoDir, "wiki")
	for _, p := range pages {
		s := strings.TrimSpace(p.Slug)
		if s == "" {
			continue
		}
		body := strings.TrimSpace(p.BodyMarkdown)
		if body == "" {
			body = strings.TrimSpace(p.BodyMarkdown2)
		}
		header := ""
		if t := strings.TrimSpace(p.Title); t != "" {
			header = "# " + t + "\n\n"
		}
		wp := filepath.Join(wikiDir, s+".md")
		if err := os.WriteFile(wp, []byte(header+body+"\n"), 0o644); err != nil {
			return fmt.Errorf("wiki: 写入 wiki/%s.md: %w", s, err)
		}
	}
	return gitCommit(e.RepoDir, fmt.Sprintf("wiki: ingest-text %s (%s)", slug, dateStr))
}

// loadSchemaContext 读取 schema.yaml 内容，供 LLM 参考。
func (e *Engine) loadSchemaContext() string {
	schemaPath := filepath.Join(e.RepoDir, "schema", "schema.yaml")
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		return ""
	}
	return truncateRunes(string(data), 4000)
}

// Organize 对整个 wiki 进行全量整理: 总结、建立关联、归档、维护结构。
// 当页面数量较多时分批处理，每批最多 20 个页面。
func (e *Engine) Organize(ctx context.Context) (*OrganizeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.RepoDir == "" {
		return nil, fmt.Errorf("wiki: RepoDir 未设置")
	}
	if err := EnsureRepo(e.RepoDir); err != nil {
		return nil, err
	}

	schemaCtx := e.loadSchemaContext()
	wikiDir := filepath.Join(e.RepoDir, "wiki")
	pages, _ := filepath.Glob(filepath.Join(wikiDir, "*.md"))

	totalUpdated := 0
	var logBuf strings.Builder

	const batchSize = 20

	if len(pages) <= batchSize {
		bundle, err := buildWikiContextBundle(e.RepoDir, 16000)
		if err != nil {
			return nil, err
		}
		userPrompt := "Schema 配置:\n" + schemaCtx + "\n---\n当前 wiki:\n" + bundle + "\n\n---\n## 输出格式（严格按此格式输出每个需要更新的页面）\n对每个需要更新/创建的页面，按以下格式输出：\n\n---PAGE: <slug>\n---TITLE: <页面标题>\n---BODY:\n<完整的 markdown 页面内容>\n===END===\n\n规则：\n- 每个页面以 ---PAGE: 开头，以 ===END=== 结尾\n- 页面内容必须包含完整正文，不要省略号\n- 不需要更新的页面不要输出\n- 页面之间可以有任意数量的空行和说明文字\n- 最后可输出 ===LOG=== 后跟整理日志摘要"
		resp, err := e.completeLLMWithMaxTokens(ctx, organizeSystemPrompt, userPrompt, 16384)
		if err != nil {
			return nil, fmt.Errorf("wiki: LLM 整理: %w", err)
		}
		return e.applyOrganizeResult(resp)
	}

	// 分批处理
	for i := 0; i < len(pages); i += batchSize {
		select {
		case <-ctx.Done():
			return &OrganizeResult{UpdatedPages: totalUpdated, Log: logBuf.String() + "\n(中断)"}, ctx.Err()
		default:
		}

		end := i + batchSize
		if end > len(pages) {
			end = len(pages)
		}
		batch := pages[i:end]

		var batchContent strings.Builder
		batchContent.WriteString(fmt.Sprintf("Schema:\n%s\n---\n批次 %d/%d 的页面:\n\n", schemaCtx, i/batchSize+1, (len(pages)+batchSize-1)/batchSize))
		for _, p := range batch {
			data, _ := os.ReadFile(p)
			batchContent.WriteString(fmt.Sprintf("### %s\n%s\n\n", filepath.Base(p), truncateRunes(string(data), 4000)))
		}
		batchContent.WriteString("\n---\n## 输出格式（严格按此格式输出每个需要更新的页面）\n对每个需要更新/创建的页面，按以下格式输出：\n\n---PAGE: <slug>\n---TITLE: <页面标题>\n---BODY:\n<完整的 markdown 页面内容>\n===END===\n\n规则：\n- 每个页面以 ---PAGE: 开头，以 ===END=== 结尾\n- 页面内容必须包含完整正文，不要省略号\n- 不需要更新的页面不要输出\n- 最后可输出 ===LOG=== 后跟整理日志摘要")

		resp, err := e.completeLLMWithMaxTokens(ctx, organizeSystemPrompt, batchContent.String(), 16384)
		if err != nil {
			logBuf.WriteString(fmt.Sprintf("批次 %d 失败: %v\n", i/batchSize+1, err))
			continue
		}

		result, err := e.applyOrganizeResult(resp)
		if err != nil {
			logBuf.WriteString(fmt.Sprintf("批次 %d 应用失败: %v\n", i/batchSize+1, err))
			continue
		}
		totalUpdated += result.UpdatedPages
		logBuf.WriteString(result.Log + "\n")
	}

	return &OrganizeResult{UpdatedPages: totalUpdated, Log: logBuf.String()}, nil
}

// IncrementalOrganize 增量整理: 仅处理新增 raw 文件影响的页面。
// 当 raw 文件较多时并发分批处理，每批最多 5 个 raw 文件。
func (e *Engine) IncrementalOrganize(ctx context.Context) (*OrganizeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.RepoDir == "" {
		return nil, fmt.Errorf("wiki: RepoDir 未设置")
	}
	if err := EnsureRepo(e.RepoDir); err != nil {
		return nil, err
	}

	recentRaw, err := e.listRecentRawFiles(7 * 24 * time.Hour)
	if err != nil {
		return nil, err
	}
	if len(recentRaw) == 0 {
		return &OrganizeResult{Log: "无近期新增 raw 文件，跳过增量整理"}, nil
	}

	schemaCtx := e.loadSchemaContext()
	wikiIndex, _ := buildWikiIndexOnly(e.RepoDir)

	const batchSize = 5
	totalUpdated := 0
	var logBuf strings.Builder

	// 分批处理 raw 文件
	for i := 0; i < len(recentRaw); i += batchSize {
		select {
		case <-ctx.Done():
			return &OrganizeResult{UpdatedPages: totalUpdated, Log: logBuf.String() + "\n(中断)"}, ctx.Err()
		default:
		}

		end := i + batchSize
		if end > len(recentRaw) {
			end = len(recentRaw)
		}
		batch := recentRaw[i:end]

		var rawContent strings.Builder
		rawContent.WriteString(fmt.Sprintf("Schema:\n%s\n---\n新增 raw 文件 (批次 %d):\n\n", schemaCtx, i/batchSize+1))
		for _, rf := range batch {
			data, _ := os.ReadFile(rf)
			rawContent.WriteString(fmt.Sprintf("### %s\n%s\n\n", filepath.Base(rf), truncateRunes(string(data), 8000)))
		}
		rawContent.WriteString("---\n当前 wiki 索引:\n" + wikiIndex)

		resp, err := e.completeLLMWithMaxTokens(ctx, incrementalOrganizePrompt, rawContent.String(), 16384)
		if err != nil {
			logBuf.WriteString(fmt.Sprintf("批次 %d 失败: %v\n", i/batchSize+1, err))
			continue
		}

		result, err := e.applyOrganizeResult(resp)
		if err != nil {
			logBuf.WriteString(fmt.Sprintf("批次 %d 应用失败: %v\n", i/batchSize+1, err))
			continue
		}
		totalUpdated += result.UpdatedPages
		logBuf.WriteString(result.Log + "\n")
	}

	return &OrganizeResult{UpdatedPages: totalUpdated, Log: logBuf.String()}, nil
}

// HealthCheck 对 wiki 进行 LLM 驱动的健康检查。
func (e *Engine) HealthCheck(ctx context.Context) (*HealthReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.RepoDir == "" {
		return nil, fmt.Errorf("wiki: RepoDir 未设置")
	}
	if err := EnsureRepo(e.RepoDir); err != nil {
		return nil, err
	}

	bundle, err := buildWikiContextBundle(e.RepoDir, 4000)
	if err != nil {
		return nil, err
	}
	resp, err := e.completeLLMWithMaxTokens(ctx, healthCheckPrompt, bundle, 8192)
	if err != nil {
		return nil, fmt.Errorf("wiki: LLM 健康检查: %w", err)
	}

	s := strings.TrimSpace(resp)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	var report HealthReport
	if err := json.Unmarshal([]byte(s), &report); err != nil {
		return &HealthReport{Summary: "解析失败，原始响应: " + truncateRunes(resp, 2000)}, nil
	}
	return &report, nil
}

// QueryAndArchive 查询 wiki 并自动将高质量回答归档为 wiki 页面。
func (e *Engine) QueryAndArchive(ctx context.Context, question string) (string, bool, error) {
	answer, err := e.Query(ctx, question)
	if err != nil {
		return "", false, err
	}

	if len([]rune(answer)) < 200 {
		return answer, false, nil
	}

	archivePrompt := `判断以下回答是否值得归档到 wiki（包含有价值的分析、对比、推理或综合）。
如果值得归档，输出 JSON: {"archive":true,"slug":"...","title":"..."}
如果不值得，输出: {"archive":false}
仅输出 JSON。`

	resp, err := e.completeLLM(ctx, archivePrompt, fmt.Sprintf("问题: %s\n\n回答:\n%s", question, answer))
	if err != nil {
		return answer, false, nil
	}

	var decision struct {
		Archive bool   `json:"archive"`
		Slug    string `json:"slug"`
		Title   string `json:"title"`
	}
	s := strings.TrimSpace(resp)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	if json.Unmarshal([]byte(strings.TrimSpace(s)), &decision) == nil && decision.Archive {
		slug := decision.Slug
		if slug == "" {
			slug = slugify(decision.Title)
		}
		wikiDir := filepath.Join(e.RepoDir, "wiki")
		header := "# " + decision.Title + "\n\n"
		footer := fmt.Sprintf("\n\n---\n_来源: 查询归档 | 问题: %s | 日期: %s_\n", question, time.Now().Format("2006-01-02"))
		wp := filepath.Join(wikiDir, slug+".md")
		if err := os.WriteFile(wp, []byte(header+answer+footer+"\n"), 0o644); err == nil {
			_ = gitCommit(e.RepoDir, fmt.Sprintf("wiki: archive query %s", slug))
			return answer, true, nil
		}
	}

	return answer, false, nil
}

// Status 返回 wiki 的当前状态统计。
func (e *Engine) Status() map[string]interface{} {
	result := map[string]interface{}{
		"repoDir": e.RepoDir,
		"ready":   false,
	}
	if e.RepoDir == "" {
		return result
	}

	rawDir := filepath.Join(e.RepoDir, "raw")
	wikiDir := filepath.Join(e.RepoDir, "wiki")

	rawCount := 0
	if entries, err := os.ReadDir(rawDir); err == nil {
		for _, ent := range entries {
			if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".md") {
				rawCount++
			}
		}
	}

	wikiCount := 0
	if entries, err := os.ReadDir(wikiDir); err == nil {
		for _, ent := range entries {
			if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".md") {
				wikiCount++
			}
		}
	}

	result["ready"] = true
	result["rawCount"] = rawCount
	result["wikiCount"] = wikiCount
	result["hasLLM"] = e.llm != nil
	return result
}

// WatchRaw 监控 raw 目录变化，发现新文件自动触发增量整理。
// 返回一个 stop 函数用于停止监控。
func (e *Engine) WatchRaw(ctx context.Context, onChange func(newFiles []string)) (stop func()) {
	ticker := time.NewTicker(60 * time.Second)
	known := make(map[string]struct{})

	rawDir := filepath.Join(e.RepoDir, "raw")
	if entries, err := os.ReadDir(rawDir); err == nil {
		for _, ent := range entries {
			known[ent.Name()] = struct{}{}
		}
	}

	stopCh := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				entries, err := os.ReadDir(rawDir)
				if err != nil {
					continue
				}
				var newFiles []string
				for _, ent := range entries {
					if _, ok := known[ent.Name()]; !ok {
						known[ent.Name()] = struct{}{}
						newFiles = append(newFiles, filepath.Join(rawDir, ent.Name()))
					}
				}
				if len(newFiles) > 0 && onChange != nil {
					onChange(newFiles)
				}
			case <-stopCh:
				ticker.Stop()
				return
			case <-ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()

	return func() { close(stopCh) }
}

// listRecentRawFiles 列出最近 duration 内修改的 raw 文件。
func (e *Engine) listRecentRawFiles(duration time.Duration) ([]string, error) {
	rawDir := filepath.Join(e.RepoDir, "raw")
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return nil, fmt.Errorf("wiki: 读取 raw: %w", err)
	}
	cutoff := time.Now().Add(-duration)
	var result []string
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			result = append(result, filepath.Join(rawDir, ent.Name()))
		}
	}
	return result, nil
}

// applyOrganizeResult 解析 LLM 整理结果（分隔符格式）并写入 wiki。
// 格式: ---PAGE: slug / ---TITLE: title / ---BODY: / ...内容... / ===END===
func (e *Engine) applyOrganizeResult(resp string) (*OrganizeResult, error) {
	type wikiPage struct {
		Slug       string
		Title      string
		Body       string
	}

	var pages []wikiPage
	var logText string

	// 提取 ===LOG=== 后的日志摘要
	if logIdx := strings.Index(resp, "===LOG==="); logIdx >= 0 {
		logText = strings.TrimSpace(resp[logIdx+9:])
		resp = resp[:logIdx]
	}

	// 按 ---PAGE: 分割
	const pageStart = "---PAGE:"
	const bodyStart = "---BODY:"
	const pageEnd = "===END==="

	pos := 0
	for {
		idx := strings.Index(resp[pos:], pageStart)
		if idx < 0 {
			break
		}
		pagePos := pos + idx + len(pageStart)
		lineEnd := strings.Index(resp[pagePos:], "\n")
		if lineEnd < 0 {
			lineEnd = len(resp) - pagePos
		}
		slug := strings.TrimSpace(resp[pagePos : pagePos+lineEnd])
		if slug == "" {
			pos = pagePos
			continue
		}

		// 找 ---TITLE:
		title := ""
		titleIdx := strings.Index(resp[pagePos:], "---TITLE:")
		if titleIdx >= 0 {
			tStart := pagePos + titleIdx + 9
			tEnd := strings.Index(resp[tStart:], "\n")
			if tEnd < 0 {
				tEnd = len(resp) - tStart
			}
			title = strings.TrimSpace(resp[tStart : tStart+tEnd])
		}

		// 找 ---BODY:
		bodyIdx := strings.Index(resp[pagePos:], bodyStart)
		if bodyIdx < 0 {
			pos = pagePos
			continue
		}
		bodyStartPos := pagePos + bodyIdx + len(bodyStart)
		// 跳过 ---BODY: 后面的换行
		for bodyStartPos < len(resp) && resp[bodyStartPos] == '\n' {
			bodyStartPos++
		}

		// 找 ===END===
		endIdx := strings.Index(resp[bodyStartPos:], pageEnd)
		var body string
		if endIdx >= 0 {
			body = resp[bodyStartPos : bodyStartPos+endIdx]
			pos = bodyStartPos + endIdx + len(pageEnd)
		} else {
			// 没有 ===END===，取到末尾
			body = resp[bodyStartPos:]
			pos = len(resp)
		}
		body = strings.TrimRight(body, "\n\r")

		pages = append(pages, wikiPage{Slug: slug, Title: title, Body: body})
	}

	if len(pages) == 0 {
		// 没有页面需要更新，但仍返回日志
		return &OrganizeResult{UpdatedPages: 0, Log: logText}, nil
	}

	wikiDir := filepath.Join(e.RepoDir, "wiki")
	count := 0
	for _, p := range pages {
		slug := strings.TrimSpace(p.Slug)
		if slug == "" {
			continue
		}
		body := strings.TrimSpace(p.Body)
		if body == "" {
			continue
		}
		header := ""
		if t := strings.TrimSpace(p.Title); t != "" {
			header = "# " + t + "\n\n"
		}
		wp := filepath.Join(wikiDir, slug+".md")
		if err := os.WriteFile(wp, []byte(header+body+"\n"), 0o644); err != nil {
			continue
		}
		count++
	}

	dateStr := time.Now().Format("2006-01-02 15:04")
	_ = gitCommit(e.RepoDir, fmt.Sprintf("wiki: organize %d pages (%s)", count, dateStr))

	return &OrganizeResult{
		UpdatedPages: count,
		Log:          logText,
	}, nil
}
