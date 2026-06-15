// vision_critic.go — 视觉接地 (vision-grounded) 的创意质检。
//
// 背景: creative-v2 的视觉审查历来只读 HTML 源码文本, 看不到真实渲染像素, 因此无法可靠
// 区分"好视频"和"坏视频"(实践已证明: 只有真的看渲染帧才能判定)。本组件把渲染出来的
// 场景帧喂给多模态模型, 让它评审"实际看到的画面", 产出结构化裁决 + 可执行修改建议。
//
// 参考业界做法 (vision-grounded critic loop): ReLook (MLLM 视觉评审打分) /
// Design2Code (基于自身渲染截图自修) / VASCAR。
//
// 视觉后端 (两路, 自动择优):
//  1. Kimi 等 Anthropic 兼容多模态: 复用 claude-go 现有 api.Client(RawComplete, 已带正确
//     的 coding-agent 头 + anthropic-version), 走 /messages 图像块。实测 ~3.5s/帧, 首选。
//  2. 本地 ollama gemma4 (OpenAI 兼容 /chat/completions image_url): 离线兜底, CPU ~70s/帧。
//     可配置: CREATIVE_VISION_BASE_URL / _MODEL / _API_KEY。
package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SceneVerdict 单个场景的视觉评审裁决。
type SceneVerdict struct {
	Scene    string   `json:"scene"`
	Score    float64  `json:"score"`    // 0-10
	Pass     bool     `json:"pass"`     // 是否达标
	Issues   []string `json:"issues"`   // 具体问题 (供下一轮针对性修复)
	Readable bool     `json:"readable"` // 文字是否清晰可读 (截断/重叠/溢出=false)
	Raw      string   `json:"-"`        // 模型原始回复 (诊断用)
}

// RawVisionCompleter 是 Anthropic 兼容多模态能力接口; *api.Client 满足 (RawComplete 发
// Anthropic 内容块, 已带 kimi coding 端点所需的 x-api-key/anthropic-version 头)。
type RawVisionCompleter interface {
	RawComplete(ctx context.Context, contentJSON json.RawMessage, maxTokens int) (string, error)
}

// VisionBackend 描述一张图。两种实现见下。
type VisionBackend interface {
	Describe(ctx context.Context, imgBase64, prompt string) (string, error)
	Name() string
}

// --- 后端1: Anthropic 兼容 (kimi 等, 复用 api.Client) ---
type rawVisionBackend struct{ rc RawVisionCompleter }

func (b rawVisionBackend) Name() string { return "anthropic(kimi)" }
func (b rawVisionBackend) Describe(ctx context.Context, imgB64, prompt string) (string, error) {
	blocks := []map[string]any{
		{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": imgB64}},
		{"type": "text", "text": prompt},
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return "", err
	}
	return b.rc.RawComplete(ctx, raw, 1024)
}

// --- 后端2: OpenAI 兼容 (本地 ollama gemma4 等) ---
type httpVisionBackend struct{ base, model, key string }

func (b httpVisionBackend) Name() string { return "openai(" + b.model + ")" }
func (b httpVisionBackend) Describe(ctx context.Context, imgB64, prompt string) (string, error) {
	body := map[string]any{
		"model": b.model, "stream": false,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": prompt},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + imgB64}},
		}}},
	}
	raw, _ := json.Marshal(body)
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, strings.TrimSuffix(b.base, "/")+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.key != "" {
		req.Header.Set("Authorization", "Bearer "+b.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("视觉后端无返回 (status=%d)", resp.StatusCode)
	}
	return out.Choices[0].Message.Content, nil
}

func visionEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// SelectVisionBackend 择优: 有 Anthropic 兼容客户端(kimi)就用它(快); 否则用本地 ollama(如可达)。
// rc 可为 nil。返回 nil 表示无可用视觉后端 (调用方应跳过视觉质检)。
func SelectVisionBackend(ctx context.Context, rc RawVisionCompleter) VisionBackend {
	if rc != nil {
		return rawVisionBackend{rc: rc}
	}
	base := visionEnv("CREATIVE_VISION_BASE_URL", "http://localhost:11434/v1")
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/models", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil
	}
	return httpVisionBackend{
		base:  base,
		model: visionEnv("CREATIVE_VISION_MODEL", "gemma4:26b-a4b-it-qat"),
		key:   visionEnv("CREATIVE_VISION_API_KEY", "ollama"),
	}
}

// CritiqueSceneImage 让多模态后端评审一张渲染好的场景图。
func CritiqueSceneImage(ctx context.Context, vb VisionBackend, imgPath, sceneTitle, objective string) (SceneVerdict, error) {
	data, err := os.ReadFile(imgPath)
	if err != nil {
		return SceneVerdict{}, fmt.Errorf("读取场景图失败: %w", err)
	}
	b64 := base64.StdEncoding.EncodeToString(data)

	prompt := fmt.Sprintf(`你是严格的视觉总监, 审查一段讲解视频里某一场景的真实渲染画面。只依据你实际看到的像素判断, 不要臆测代码。

整体视频目标: %s
本场景应表达: %s

审查重点:
1) 文字是否清晰完整 (无截断/重叠/溢出/空白占位)
2) 内容是否与"本场景应表达"一致、信息完整
3) 排版构图与可读性

只输出以下 JSON (不要多余文字):
{"score": 1-10整数, "pass": true/false, "readable": true/false, "issues": ["具体问题"]}
通过标准: score>=7 且 readable=true 且内容与目标一致。`, objective, sceneTitle)

	content, err := vb.Describe(ctx, b64, prompt)
	if err != nil {
		return SceneVerdict{}, err
	}
	v := parseSceneVerdict(content)
	v.Scene = sceneTitle
	v.Raw = content
	return v, nil
}

// ReviewVideoScenes 对一段已渲染视频做视觉接地评审: 在每个场景中心抽 1 帧, 逐帧评审真实画面。
func ReviewVideoScenes(ctx context.Context, vb VisionBackend, mp4Path string, sceneTitles []string, objective string) ([]SceneVerdict, error) {
	if vb == nil {
		return nil, fmt.Errorf("无可用视觉后端")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg 未安装")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return nil, fmt.Errorf("ffprobe 未安装")
	}
	dur := probeDurationSec(ctx, mp4Path)
	if dur <= 0 {
		return nil, fmt.Errorf("无法获取视频时长")
	}
	n := len(sceneTitles)
	if n == 0 {
		n = 1
		sceneTitles = []string{"整体画面"}
	}
	tmp, err := os.MkdirTemp("", "visionreview-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	verdicts := make([]SceneVerdict, 0, n)
	for i := 0; i < n; i++ {
		t := (float64(i) + 0.5) * dur / float64(n)
		frame := filepath.Join(tmp, fmt.Sprintf("scene_%02d.png", i+1))
		cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-y",
			"-ss", fmt.Sprintf("%.2f", t), "-i", mp4Path,
			"-frames:v", "1", "-vf", "scale=960:-1", frame)
		if err := cmd.Run(); err != nil {
			verdicts = append(verdicts, SceneVerdict{Scene: sceneTitles[i], Score: 0, Pass: false, Issues: []string{"抽帧失败"}})
			continue
		}
		v, err := CritiqueSceneImage(ctx, vb, frame, sceneTitles[i], objective)
		if err != nil {
			verdicts = append(verdicts, SceneVerdict{Scene: sceneTitles[i], Score: 6, Pass: false, Readable: true, Issues: []string{"评审调用失败: " + err.Error()}})
			continue
		}
		verdicts = append(verdicts, v)
	}
	return verdicts, nil
}

// VerdictsFeedback 把不达标场景汇总为可执行的修改反馈 (喂回 html-developer); 全通过返回空串。
func VerdictsFeedback(verdicts []SceneVerdict) (allPass bool, feedback string) {
	var b strings.Builder
	allPass = true
	for _, v := range verdicts {
		if v.Pass && v.Readable {
			continue
		}
		allPass = false
		b.WriteString(fmt.Sprintf("- 场景「%s」(评分%.0f", v.Scene, v.Score))
		if !v.Readable {
			b.WriteString(", 文字不清晰/截断")
		}
		b.WriteString("): ")
		if len(v.Issues) > 0 {
			b.WriteString(strings.Join(v.Issues, "; "))
		} else {
			b.WriteString("需提升内容完整性与可读性")
		}
		b.WriteString("\n")
	}
	return allPass, b.String()
}

func probeDurationSec(ctx context.Context, mp4Path string) float64 {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", mp4Path).Output()
	if err != nil {
		return 0
	}
	var d float64
	fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &d)
	return d
}

// parseSceneVerdict 从模型回复中提取 JSON 裁决 (容错: 提取首个 {...} 块)。
func parseSceneVerdict(s string) SceneVerdict {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	v := SceneVerdict{Score: 6, Pass: false, Readable: true} // 解析失败时的保守默认
	if start >= 0 && end > start {
		var raw struct {
			Score    float64  `json:"score"`
			Pass     bool     `json:"pass"`
			Readable bool     `json:"readable"`
			Issues   []string `json:"issues"`
		}
		if json.Unmarshal([]byte(s[start:end+1]), &raw) == nil {
			return SceneVerdict{Score: raw.Score, Pass: raw.Pass, Readable: raw.Readable, Issues: raw.Issues}
		}
	}
	low := strings.ToLower(s)
	if strings.Contains(low, "\"pass\": true") || strings.Contains(low, "\"pass\":true") {
		v.Pass = true
	}
	return v
}
