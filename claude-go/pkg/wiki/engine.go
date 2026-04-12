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
version: 2

# === 三层架构 ===
# Raw 层: 原始资料集合（文章、论文、笔记等）— 不可变的事实来源，LLM 只读不写
# Wiki 层: LLM 生成的 Markdown 概念页 — LLM 负责创建、更新、维护交叉引用
# Schema 层: 本文件 — 指导 LLM 的工作流程和规范

wiki:
  link_syntax: "[[slug]]"
  max_concept_body_chars: 32000
  # 页面模板: 每个 wiki 页面的推荐结构
  page_template: |
    # {title}
    
    > 摘要: 一句话概括此概念
    
    ## 核心内容
    (主体知识)
    
    ## 关联概念
    - [[related-slug-1]]
    - [[related-slug-2]]
    
    ## 来源
    - 来自: raw/{source-file}
  # 索引页: wiki/_index.md 作为知识库总入口
  index_page: "_index"
  # 分类: 按主题对页面进行逻辑分组
  categories:
    - name: "技术"
      tags: ["programming", "ai", "ml", "system"]
    - name: "产品"
      tags: ["product", "design", "ux"]
    - name: "商业"
      tags: ["business", "finance", "market"]
    - name: "通用"
      tags: ["general"]

ingest:
  merge_strategy: "llm_full_replace"
  # 摄取工作流: LLM 读取资料 → 抽取概念 → 写摘要页 → 更新索引 → 更新关联页 → 追加日志
  workflow:
    - "读取原始资料"
    - "抽取关键概念和实体"
    - "为每个概念撰写或更新 wiki 页面"
    - "使用 [[slug]] 建立交叉引用"
    - "更新 _index.md 目录"
    - "在变更日志中追加记录"

query:
  navigation_mode: "snapshot"
  # 查询结果如果具有归档价值（分析、对比、推理），自动归档到 wiki
  auto_archive: true
  # 回答格式: 可以是 markdown、对比表、图表描述等
  flexible_format: true

lint:
  # 定期健康检查项目
  checks:
    - "broken_links"       # 坏链检测
    - "orphaned_pages"     # 孤立页检测  
    - "contradictions"     # 矛盾数据检测
    - "outdated_content"   # 过时内容检测
    - "missing_concepts"   # 缺失概念检测
    - "missing_cross_refs" # 缺失交叉引用
    - "research_gaps"      # 研究空缺
  
organize:
  # 整理触发条件
  triggers:
    - "new_raw_files"      # 新增 raw 文件时自动触发增量整理
    - "manual"             # 手动触发
    - "scheduled"          # 定时触发
  # 整理任务
  tasks:
    - "summarize"          # 补充摘要
    - "cross_reference"    # 建立交叉引用
    - "categorize"         # 分类归档
    - "update_index"       # 更新索引
    - "deduplicate"        # 去重
    - "fill_gaps"          # 填补空缺
`

	ingestSystemPrompt = `你是「LLM Wiki」的策展助手，遵循三层架构(Raw/Wiki/Schema)规范。
用户会提供一篇刚从网页摘录的纯文本（可能附带标题与来源）。

你的任务（参照 Karpathy LLM Wiki 摄取工作流）：
1. 读取原始资料，抽取关键概念和实体
2. 每个概念对应一个 slug（小写、短横线、英文或拼音均可，需稳定、可复用）
3. 为每个概念撰写或更新 Markdown 正文，遵循以下页面结构:
   - # 标题
   - > 摘要: 一句话概括
   - ## 核心内容 (主体知识)
   - ## 关联概念 (使用 [[slug]] 交叉引用)
   - ## 来源 (标注来自哪个 raw 文件)
4. 建立充分的 [[slug]] 交叉引用（一个来源通常影响 10-15 个 wiki 页面）
5. 如果概念属于已有的相关页面，也输出更新后的该页面

输出必须是单一 JSON 对象，不要代码围栏：
{"pages":[{"slug":"...","title":"...","body_markdown":"..."}]}

要求：
- pages 非空
- body_markdown 使用 [[slug]] 表示指向 wiki/<slug>.md 的链接
- 不同 page 的 slug 必须唯一
- 正文简洁、可检索，保留关键事实，不要编造
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

// Engine 是 LLM Wiki 知识库的运行时入口，负责摄取、查询与质检。
// 同一 Engine 实例上的公开方法使用互斥锁序列化，避免并发写盘与 git 提交交错。
type Engine struct {
	RepoDir    string // Git 仓库根目录，例如 ~/knowledge-wiki
	APIKey     string // 保留用于 HTTP 拉取 URL (非 LLM)
	BaseURL    string // 保留向后兼容
	Model      string // 保留向后兼容
	llm        LLMClient
	httpClient *http.Client
	mu         sync.Mutex
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

// EnsureRepo 若目录或 Git 结构不完整则初始化：创建 raw/wiki/schema、写入默认 schema.yaml，并在需要时 git init。
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
	if _, err := os.Stat(schemaPath); os.IsNotExist(err) {
		if err := os.WriteFile(schemaPath, []byte(defaultSchemaYAML), 0o644); err != nil {
			return fmt.Errorf("wiki: 写入默认 schema: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("wiki: 检查 schema: %w", err)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("wiki: 构造请求: %w", err)
	}
	req.Header.Set("User-Agent", "claude-go-wiki/1.0")

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
	title := pickTitle(htmlStr)
	if strings.TrimSpace(title) == "" {
		title = "untitled"
	}
	text := extractContent(htmlStr)
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

// extractContent 从 HTML 字符串中剥离 script/style 与标签，保留主要可读文本。
func extractContent(htmlBody string) string {
	s := reScript.ReplaceAllString(htmlBody, " ")
	s = reStyle.ReplaceAllString(s, " ")
	s = reTags.ReplaceAllString(s, " ")
	s = reWS.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

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
	organizeSystemPrompt = `你是「LLM Wiki」的策展维护者。下面是当前 wiki 的全部页面目录与部分内容。
你的任务是对整个 wiki 进行全面整理和优化:

1. **总结与摘要**: 检查每个页面是否有清晰的摘要段落，没有的补充
2. **建立关联**: 审查所有页面之间的概念关联，添加缺失的 [[slug]] 交叉引用
3. **归档整理**: 按主题对页面进行逻辑分组，在每个页面添加相关页面链接
4. **维护结构**: 创建或更新 _index.md 作为 wiki 的目录入口页面
5. **去重与合并**: 如果有重复或高度相似的页面，标注合并建议
6. **填补空缺**: 识别被引用但不存在的概念页，为其创建基础框架

重要：请输出紧凑的 JSON（无多余空格和换行），确保能在输出限制内完成：
{"pages":[{"slug":"...","title":"...","body_markdown":"..."}],"log":"整理日志摘要"}`

	incrementalOrganizePrompt = `你是「LLM Wiki」的增量维护者。以下是最近新增的 raw 文件和当前 wiki 的页面目录。
你的任务是仅针对新增内容进行增量更新:

1. 将新增 raw 内容与现有 wiki 概念页关联
2. 更新受影响的现有页面（添加新的交叉引用）
3. 如果新内容引入了新概念，创建对应的 wiki 页面
4. 更新 _index.md 目录

仅输出需要创建或更新的页面（不要输出未变更的页面）。
输出 JSON:
{"pages":[{"slug":"...","title":"...","body_markdown":"..."}],"log":"增量更新日志"}`

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

// Organize 对整个 wiki 进行全量整理: 总结、建立关联、归档、维护结构。
func (e *Engine) Organize(ctx context.Context) (*OrganizeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.RepoDir == "" {
		return nil, fmt.Errorf("wiki: RepoDir 未设置")
	}
	if err := EnsureRepo(e.RepoDir); err != nil {
		return nil, err
	}

	// 使用仅目录模式减少输入，给 LLM 输出留更多空间
	bundle, err := buildWikiIndexOnly(e.RepoDir)
	if err != nil {
		return nil, err
	}
	resp, err := e.completeLLMWithMaxTokens(ctx, organizeSystemPrompt, bundle, 16384)
	if err != nil {
		return nil, fmt.Errorf("wiki: LLM 整理: %w", err)
	}

	return e.applyOrganizeResult(resp)
}

// IncrementalOrganize 增量整理: 仅处理新增 raw 文件影响的页面。
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

	var rawContent strings.Builder
	rawContent.WriteString("近期新增 raw 文件:\n\n")
	for _, rf := range recentRaw {
		data, _ := os.ReadFile(rf)
		rawContent.WriteString(fmt.Sprintf("### %s\n%s\n\n", filepath.Base(rf), truncateRunes(string(data), 6000)))
	}

	bundle, _ := buildWikiContextBundle(e.RepoDir, 12000)
	userPrompt := rawContent.String() + "\n---\n当前 wiki 状态:\n" + bundle

	resp, err := e.completeLLM(ctx, incrementalOrganizePrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("wiki: LLM 增量整理: %w", err)
	}

	return e.applyOrganizeResult(resp)
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

// applyOrganizeResult 解析 LLM 整理结果并写入 wiki。
func (e *Engine) applyOrganizeResult(resp string) (*OrganizeResult, error) {
	s := strings.TrimSpace(resp)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	var parsed struct {
		Pages []ingestLLMPage `json:"pages"`
		Log   string          `json:"log"`
	}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return nil, fmt.Errorf("wiki: 解析整理结果: %w: %s", err, truncateRunes(resp, 500))
	}

	wikiDir := filepath.Join(e.RepoDir, "wiki")
	count := 0
	for _, p := range parsed.Pages {
		slug := strings.TrimSpace(p.Slug)
		if slug == "" {
			continue
		}
		body := strings.TrimSpace(p.BodyMarkdown)
		if body == "" {
			body = strings.TrimSpace(p.BodyMarkdown2)
		}
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
		Log:          parsed.Log,
	}, nil
}
