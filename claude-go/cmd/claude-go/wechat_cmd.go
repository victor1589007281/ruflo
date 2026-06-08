package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/wechat"
	"github.com/spf13/cobra"
)

// wechatCmd 微信公众号自动排版/草稿发布。
//
//	claude-go wechat --md article.md --title "标题"            # 离线预览 (无需 IP 白名单)
//	claude-go wechat --team <团队名> --title "标题"            # 取团队 techblog 产物
//	claude-go wechat --md article.md --draft                  # 调 API 建草稿 (需 IP 白名单)
func wechatCmd() *cobra.Command {
	var mdFile, team, title, author, sourceURL, outDir, updateID string
	var draft bool
	cmd := &cobra.Command{
		Use:   "wechat",
		Short: "微信公众号自动排版 (Markdown→公众号内联HTML, mermaid→图), 可建草稿",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := resolveWechatConfigPath()
			wcfg, chromePath, err := loadWechatConfig(cfgPath)
			if err != nil {
				return err
			}
			md, t2, err := loadWechatMarkdown(mdFile, team)
			if err != nil {
				return err
			}
			if title == "" {
				title = t2
			}
			// 列表门禁 (类似 lint): 检测并修复"序号/无序列表内空行"
			if lf, n := wechat.LintLists(md); n > 0 {
				fmt.Printf("[wechat] 📋 列表门禁: 检测到并修复 %d 处列表内空行\n", n)
				md = lf
			}
			if outDir == "" {
				outDir = filepath.Join(os.TempDir(), "wechat-"+fmt.Sprint(time.Now().Unix()))
			}
			ctx := context.Background()

			if !draft && updateID == "" {
				// 离线预览 (不调 API): mermaid → 本地 PNG, 产出内联 HTML
				fmt.Printf("[wechat] 离线预览模式 (不调用公众号 API)\n")
				html, n, ok, warns, err := wechat.TypesetLocal(ctx, md, chromePath, outDir, "")
				if err != nil {
					return err
				}
				htmlPath := filepath.Join(outDir, "preview.html")
				_ = os.WriteFile(htmlPath, []byte(html), 0o644)
				fmt.Printf("[wechat] mermaid 图: %d 个, 成功渲染 %d 个\n", n, ok)
				for _, w := range warns {
					fmt.Printf("  ⚠️ %s\n", w)
				}
				fmt.Printf("[wechat] 预览 HTML: %s\n", htmlPath)
				fmt.Printf("[wechat] 图片目录: %s\n", outDir)
				fmt.Printf("[wechat] 用浏览器打开 preview.html 查看效果; 确认 IP 白名单后加 --draft 直接建草稿。\n")
				return nil
			}

			// Tier 2: 调 API 建/更新草稿
			c := wechat.NewClient(wcfg)
			opt := wechat.TypesetOptions{ChromePath: chromePath, Title: title, Author: author, SourceURL: sourceURL}
			// LLM mermaid 自动修复 (启发式修不动时兜底)
			if llm := loadLLMClient(cfgPath); llm != nil {
				opt.MermaidFixer = func(code, errMsg string) string {
					sys := "你是 mermaid 语法专家。修复给定 mermaid 图的语法错误。" +
						"只输出修正后的完整 mermaid 代码(含图类型首行), 不要任何解释、不要 markdown 围栏。" +
						"常见问题: 节点/转移标签含特殊字符(()/:,等)需加双引号; stateDiagram-v2 的中文或含空格的状态名" +
						"需用 state \"名称\" as id 先别名再引用; 标签里避免裸 < > 和 <br/>。"
					user := "错误信息:\n" + errMsg + "\n\n原始 mermaid:\n" + code
					cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
					defer cancel()
					out, e := llm.SimpleComplete(cctx, sys, user)
					if e != nil {
						fmt.Printf("  ⚠️ LLM 修复调用失败: %v\n", e)
						return ""
					}
					fixed := stripMermaidFence(out)
					fmt.Printf("  🔧 LLM 修复 mermaid (返回 %d 字符)\n", len(fixed))
					return fixed
				}
				fmt.Println("[wechat] 已启用 LLM mermaid 自动修复")
			}
			if updateID != "" {
				fmt.Printf("[wechat] 更新草稿模式 (add-new+delete-old 规避 WAF): 替换 media_id=%s\n", updateID)
				newID, res, err := c.UpdateDraftFromMarkdown(ctx, updateID, md, opt)
				if res != nil {
					fmt.Printf("[wechat] mermaid 图: %d 个, 成功 %d 个\n", res.MermaidCount, res.MermaidOK)
					for _, w := range res.Warnings {
						fmt.Printf("  ⚠️ %s\n", w)
					}
				}
				if err != nil {
					return fmt.Errorf("更新草稿失败: %w", err)
				}
				fmt.Printf("[wechat] ✅ 草稿已替换, 新 media_id=%s (旧草稿已删除)\n登录公众号草稿箱查看最新版。\n", newID)
				return nil
			}
			fmt.Printf("[wechat] 草稿模式: 渲染 mermaid + 上传图片 + 建草稿 (appid=%s)\n", wcfg.AppID)
			mediaID, res, err := c.PublishDraft(ctx, md, opt)
			if res != nil {
				fmt.Printf("[wechat] mermaid 图: %d 个, 成功 %d 个\n", res.MermaidCount, res.MermaidOK)
				for _, w := range res.Warnings {
					fmt.Printf("  ⚠️ %s\n", w)
				}
			}
			if err != nil {
				return fmt.Errorf("建草稿失败: %w", err)
			}
			fmt.Printf("[wechat] ✅ 草稿已创建 media_id=%s\n登录公众号后台「草稿箱」即可预览并群发。\n", mediaID)
			return nil
		},
	}
	cmd.Flags().StringVar(&mdFile, "md", "", "Markdown 文件路径")
	cmd.Flags().StringVar(&team, "team", "", "团队名 (从其 techblog 产物取文章)")
	cmd.Flags().StringVar(&title, "title", "", "文章标题")
	cmd.Flags().StringVar(&author, "author", "", "作者")
	cmd.Flags().StringVar(&sourceURL, "source-url", "", "原文链接")
	cmd.Flags().StringVar(&outDir, "out", "", "输出目录 (预览模式)")
	cmd.Flags().BoolVar(&draft, "draft", false, "调用公众号 API 建草稿 (需服务器 IP 在白名单)")
	cmd.Flags().StringVar(&updateID, "update", "", "更新已有草稿的 media_id (配合 --draft, 不新建)")
	return cmd
}

func resolveWechatConfigPath() string {
	if flagConfig != "" {
		return flagConfig
	}
	if env := os.Getenv("CLAUDE_GO_CONFIG"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-go", "config", "config.json")
}

func loadWechatConfig(path string) (wechat.Config, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return wechat.Config{}, "", fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}
	var raw struct {
		WechatMP wechat.Config `json:"wechatMP"`
		Browser  struct {
			ChromePath string `json:"chromePath"`
		} `json:"browser"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return wechat.Config{}, "", fmt.Errorf("解析配置失败: %w", err)
	}
	if raw.WechatMP.AppID == "" {
		return wechat.Config{}, "", fmt.Errorf("配置缺少 wechatMP.appId (请在 %s 配置)", path)
	}
	return raw.WechatMP, raw.Browser.ChromePath, nil
}

// loadWechatMarkdown 从 --md 文件或 --team 的 techblog 产物取 Markdown, 返回 (md, 推断标题)。
func loadWechatMarkdown(mdFile, team string) (string, string, error) {
	if mdFile != "" {
		b, err := os.ReadFile(mdFile)
		if err != nil {
			return "", "", fmt.Errorf("读取 %s 失败: %w", mdFile, err)
		}
		return string(b), inferTitle(string(b)), nil
	}
	if team == "" {
		return "", "", fmt.Errorf("需指定 --md 或 --team")
	}
	home, _ := os.UserHomeDir()
	stateDir := basedir.ResolveDefault("", "")
	bbPath := filepath.Join(stateDir, "teams", team, "blackboard.json")
	if _, err := os.Stat(bbPath); err != nil {
		bbPath = filepath.Join(home, ".claude-go", "teams", team, "blackboard.json")
	}
	data, err := os.ReadFile(bbPath)
	if err != nil {
		return "", "", fmt.Errorf("读取团队 blackboard 失败: %w", err)
	}
	var entries []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return "", "", fmt.Errorf("解析 blackboard 失败: %w", err)
	}
	// 优先用 article-writing-result (干净 Markdown 含 mermaid), 退而用 self-critique-result
	pick := map[string]string{}
	for _, e := range entries {
		pick[e.Key] = e.Value
	}
	for _, k := range []string{"article-writing-result", "self-critique-result", "formatting-result"} {
		if v := strings.TrimSpace(pick[k]); v != "" {
			return v, inferTitle(v), nil
		}
	}
	return "", "", fmt.Errorf("团队 %s 未找到文章产物", team)
}

// loadLLMClient 从 config.json 解析默认模型 (ai.modelAlias + providers) 构造 LLM 客户端; 失败返回 nil。
func loadLLMClient(path string) *api.Client {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var raw struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
		} `json:"providers"`
		AI struct {
			ModelAlias string `json:"modelAlias"`
		} `json:"ai"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	parts := strings.SplitN(raw.AI.ModelAlias, ":", 2)
	if len(parts) != 2 {
		return nil
	}
	prov, ok := raw.Providers[parts[0]]
	if !ok || prov.BaseURL == "" {
		return nil
	}
	return api.NewClient(prov.BaseURL, prov.APIKey, parts[1])
}

// stripMermaidFence 去掉 LLM 返回里可能包裹的 ```mermaid ... ``` 围栏。
func stripMermaidFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSpace(s)
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}

func inferTitle(md string) string {
	for _, ln := range strings.Split(md, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "# "))
		}
	}
	return ""
}
