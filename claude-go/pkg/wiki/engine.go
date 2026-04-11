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
	defaultSchemaYAML = `# LLM Wiki 默认模式配置
# 描述维基链接、摄取与查询行为的约定，供人类与 LLM 共同遵守。
version: 1

wiki:
  # 概念页之间的链接使用双括号包裹的目标 slug，例如：[[machine-learning]]
  link_syntax: "[[slug]]"
  # 单页正文建议上限（字符），避免无限膨胀
  max_concept_body_chars: 32000

ingest:
  # 摄取后由 LLM 给出完整概念页正文（create/update）
  merge_strategy: "llm_full_replace"

query:
  # 查询阶段由 LLM 在提供的目录快照与文件片段上「导航」推理
  navigation_mode: "snapshot"
`

	ingestSystemPrompt = `你是「LLM Wiki」的策展助手。用户会提供一篇刚从网页摘录的纯文本（可能附带标题与来源）。
你的任务：
1. 从中抽取关键概念，每个概念对应一个 slug（小写、短横线、英文或拼音均可，需稳定、可复用）。
2. 为每个概念撰写或更新一段 Markdown 正文，正文中使用 [[other-slug]] 形式建立与其他概念页的回链；至少包含与本批概念相关的交叉引用。
3. 输出必须是单一 JSON 对象，不要 Markdown 代码围栏，不要额外解释。JSON 结构如下：
{"pages":[{"slug":"...","title":"...","body_markdown":"..."}]}
要求：
- pages 非空。
- body_markdown 使用 [[slug]] 表示指向 wiki/<slug>.md 的链接。
- 不同 page 的 slug 必须唯一。
- 正文简洁、可检索，保留与来源一致的关键事实，不要编造引用。`

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

// Engine 是 LLM Wiki 知识库的运行时入口，负责摄取、查询与质检。
// 同一 Engine 实例上的公开方法使用互斥锁序列化，避免并发写盘与 git 提交交错。
type Engine struct {
	RepoDir    string // Git 仓库根目录，例如 ~/knowledge-wiki
	APIKey     string // OpenAI 兼容 API 的 Bearer Token
	BaseURL    string // API 根路径，例如 https://api.openai.com/v1 或 DashScope 兼容地址
	Model      string // 模型名称，写入 chat/completions 请求体
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
	rawJSON, err := callLLM(ctx, e.httpClient, e.APIKey, e.BaseURL, e.Model, ingestSystemPrompt, userPrompt)
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
	return callLLM(ctx, e.httpClient, e.APIKey, e.BaseURL, e.Model, querySystemPrompt, user)
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

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…(正文已截断用于提示)…"
}
