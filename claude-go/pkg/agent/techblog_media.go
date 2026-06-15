// techblog_media.go — techblog 文章的可选多媒体增强 (图文配图 / 图片消息 / 视频 / 音频)。
//
// 文章写完后, 按用户勾选生成: ①配图(两种风格可选: 要点卡片 / AI插画) ②音频(朗读)
// ③视频(复用 manim/HTML 讲解)。产物供"一键发布到公众号草稿":
//   - 图文(news): 文章 markdown + 内嵌配图 + 封面
//   - 图片消息(newspic): 几张表达文章内容的图 (贴图)
//   - 视频/音频: 上传为永久素材
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/media"
)

// ttsBin 解析可用 TTS: 优先 edge-tts (免费神经网络, 中文自然), 退 piper, 再退 espeak-ng。
func ttsBin() (bin, engine string) {
	for _, c := range []struct{ name, eng string }{
		{"edge-tts", "edge"}, {"piper", "piper"}, {"espeak-ng", "espeak"}, {"espeak", "espeak"},
	} {
		if p := resolveBin(c.name); p != "" {
			return p, c.eng
		}
	}
	return "", ""
}

func resolveBin(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if home, _ := os.UserHomeDir(); home != "" {
		c := filepath.Join(home, ".local", "bin", name)
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// GenerateArticleAudio 把文章朗读为音频文件 (mp3/wav)。优先 edge-tts 中文神经语音。
// voice 可空 (默认 zh-CN-XiaoxiaoNeural)。返回输出路径。
func GenerateArticleAudio(ctx context.Context, text, outDir, voice string) (string, error) {
	bin, engine := ttsBin()
	if bin == "" {
		return "", fmt.Errorf("无可用 TTS (edge-tts/piper/espeak 均未安装)")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	// 公众号语音素材 <=2MB, 控制朗读文本长度 (取正文前 ~3000 字, 去 markdown 标记)。
	clean := stripMarkdownForTTS(text)
	if len([]rune(clean)) > 3000 {
		clean = string([]rune(clean)[:3000])
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	switch engine {
	case "edge":
		if voice == "" {
			voice = "zh-CN-XiaoxiaoNeural"
		}
		out := filepath.Join(outDir, "narration.mp3")
		cmd := exec.CommandContext(cctx, bin, "--voice", voice, "--text", clean, "--write-media", out)
		if b, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("edge-tts 失败: %v\n%s", err, truncateResult(string(b), 300))
		}
		return out, nil
	case "espeak":
		out := filepath.Join(outDir, "narration.wav")
		cmd := exec.CommandContext(cctx, bin, "-v", "zh", "-w", out, clean)
		if b, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("espeak 失败: %v\n%s", err, truncateResult(string(b), 300))
		}
		return out, nil
	default: // piper: 需 stdin 文本 + 模型, 这里简化为不支持时报错由上层回退
		return "", fmt.Errorf("piper 路径需配置中文模型, 暂未启用; 建议安装 edge-tts")
	}
}

func stripMarkdownForTTS(md string) string {
	lines := strings.Split(md, "\n")
	var out []string
	inCode := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			continue // 跳过代码块 (朗读无意义)
		}
		// 去标题井号/列表符/图片
		t = strings.TrimLeft(t, "#>-*0123456789. ")
		if strings.HasPrefix(t, "![") || strings.HasPrefix(t, "|") {
			continue
		}
		// 去行内 markdown 记号
		for _, ch := range []string{"**", "`", "*", "[", "]", "(", ")", "#"} {
			t = strings.ReplaceAll(t, ch, "")
		}
		if strings.TrimSpace(t) != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "。 ")
}

// articleImageSpec 一张配图的规格 (由 LLM 从文章提炼)。
type articleImageSpec struct {
	Title   string `json:"title"`   // 图标题/要点
	Points  []string `json:"points"` // 卡片要点 (2-4 条)
	Visual  string `json:"visual"`  // 视觉/示意描述 (AI 插画风格用)
	IsCover bool   `json:"is_cover"`
}

// ExtractImageSpecs 让 LLM 从文章提炼 n 张配图规格 (第一张作封面)。
func ExtractImageSpecs(ctx context.Context, llm LLMClient, article string, n int) ([]articleImageSpec, error) {
	if llm == nil {
		return nil, fmt.Errorf("LLM 未配置")
	}
	sys := "你是公众号视觉编辑。从技术文章中提炼用于配图的关键点。只输出 JSON 数组, 不要解释。"
	user := fmt.Sprintf(`文章:
%s

请提炼 %d 张配图 (第一张作封面, 概括全文主题)。每张输出:
{"title":"短标题(<=12字)","points":["要点1","要点2","要点3"],"visual":"一句话视觉示意描述","is_cover":true/false}
只输出 JSON 数组 [...]; points 2-4 条、每条<=16字。`, truncateResult(article, 6000), n)
	reply, err := llm.SimpleComplete(ctx, sys, user)
	if err != nil {
		return nil, err
	}
	start := strings.Index(reply, "[")
	end := strings.LastIndex(reply, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("未能解析配图规格")
	}
	var specs []articleImageSpec
	if err := json.Unmarshal([]byte(reply[start:end+1]), &specs); err != nil {
		return nil, fmt.Errorf("配图规格 JSON 解析失败: %w", err)
	}
	if len(specs) > 0 {
		specs[0].IsCover = true
	}
	return specs, nil
}

// GenerateArticleImages 按风格把配图规格渲染成 PNG (1080x1080 公众号方图)。返回图片路径 (第一张=封面)。
// style: "card"(要点卡片, 稳定专业) | "illustration"(AI 插画, 更视觉)。
func GenerateArticleImages(ctx context.Context, llm LLMClient, specs []articleImageSpec, style, outDir string) ([]string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	eng := media.NewEngine(outDir)
	eng.Width, eng.Height = 1080, 1080
	var paths []string
	for i, s := range specs {
		var htmlDoc string
		if style == "illustration" {
			svg := genIllustrationSVG(ctx, llm, s)
			htmlDoc = wrapIllustrationHTML(s, svg)
		} else {
			htmlDoc = wrapCardHTML(s, i+1, len(specs))
		}
		name := fmt.Sprintf("img_%s_%02d", style, i+1)
		results := eng.RenderAll(ctx, htmlDoc, name, []string{"png"})
		if p := mediaFilePath(results, "png"); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("未能渲染任何配图")
	}
	return paths, nil
}

// genIllustrationSVG 让 LLM 为该要点生成一段概念示意 SVG (插画风格)。失败返回空串 (回退纯标题)。
func genIllustrationSVG(ctx context.Context, llm LLMClient, s articleImageSpec) string {
	if llm == nil {
		return ""
	}
	sys := "你是技术插画师。用 SVG 画一张简洁的概念示意图 (扁平风、深色背景适配)。只输出 <svg>...</svg>, 不要解释。"
	user := fmt.Sprintf("主题: %s\n视觉示意: %s\n要求: viewBox=\"0 0 900 520\", 用节点/箭头/方框等几何元素表达概念, 亮色线条(#60a5fa/#34d399/#fbbf24), 不要写大段文字。只输出 SVG。", s.Title, s.Visual)
	reply, err := llm.SimpleComplete(ctx, sys, user)
	if err != nil {
		return ""
	}
	a := strings.Index(reply, "<svg")
	b := strings.LastIndex(reply, "</svg>")
	if a >= 0 && b > a {
		return reply[a : b+len("</svg>")]
	}
	return ""
}

func cardPointsHTML(points []string) string {
	var sb strings.Builder
	for _, p := range points {
		sb.WriteString(`<li>` + html.EscapeString(p) + `</li>`)
	}
	return sb.String()
}

// wrapCardHTML 风格A: 干净的要点卡片 (深色科技风, 标题+要点)。
func wrapCardHTML(s articleImageSpec, idx, total int) string {
	return fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="utf-8"><style>
  html,body{margin:0;width:1080px;height:1080px;background:linear-gradient(145deg,#0a0e17,#111827);
    font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;color:#e5e7eb}
  .wrap{box-sizing:border-box;height:100%%;padding:90px 80px;display:flex;flex-direction:column;justify-content:center}
  .tag{color:#60a5fa;font-size:30px;letter-spacing:4px;margin-bottom:24px}
  h1{font-size:72px;line-height:1.25;margin:0 0 50px;color:#fff}
  ul{list-style:none;padding:0;margin:0}
  li{font-size:40px;line-height:1.7;padding-left:48px;position:relative;margin-bottom:22px;color:#cbd5e1}
  li::before{content:"";position:absolute;left:0;top:18px;width:22px;height:22px;border-radius:6px;
    background:linear-gradient(135deg,#60a5fa,#34d399)}
  .ft{margin-top:auto;color:#475569;font-size:26px}
</style></head><body><div class="wrap">
  <div class="tag">%02d / %02d</div>
  <h1>%s</h1>
  <ul>%s</ul>
  <div class="ft">技术解读</div>
</div></body></html>`, idx, total, html.EscapeString(s.Title), cardPointsHTML(s.Points))
}

// wrapIllustrationHTML 风格B: AI 插画 (LLM 生成的 SVG 概念图 + 标题)。
func wrapIllustrationHTML(s articleImageSpec, svg string) string {
	if strings.TrimSpace(svg) == "" {
		svg = `<svg viewBox="0 0 900 520"><circle cx="450" cy="260" r="120" fill="none" stroke="#60a5fa" stroke-width="4"/></svg>`
	}
	return fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="utf-8"><style>
  html,body{margin:0;width:1080px;height:1080px;background:radial-gradient(circle at 50%% 35%%,#1e293b,#0a0e17);
    font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;color:#fff}
  .wrap{box-sizing:border-box;height:100%%;padding:80px;display:flex;flex-direction:column;align-items:center;justify-content:center}
  .art{width:820px;height:480px;display:flex;align-items:center;justify-content:center}
  .art svg{max-width:100%%;max-height:100%%}
  h1{font-size:64px;margin:40px 0 0;text-align:center;line-height:1.3}
  .sub{color:#94a3b8;font-size:32px;margin-top:18px;text-align:center}
</style></head><body><div class="wrap">
  <div class="art">%s</div>
  <h1>%s</h1>
  <div class="sub">%s</div>
</div></body></html>`, svg, html.EscapeString(s.Title), html.EscapeString(s.Visual))
}
