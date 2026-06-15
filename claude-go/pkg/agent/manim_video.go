// manim_video.go — Phase B: 技术/算法类讲解视频走 manim 专业引擎 (genre-routed)。
//
// 动机: 一次性让 LLM 写原始 HTML/CSS 动画质量波动大; 对"算法/数据结构/协议原理"这类
// 题材, 业界做法是让 LLM 写 manim (3Blue1Brown 同款引擎) 代码, 由成熟引擎确定性渲染
// (参考 TheoremExplainAgent: planner+coder, 93.8% 成功率)。本路径:
//   1) genre 路由: 命中算法/技术原理类视频 → 走 manim;
//   2) LLM 写 manim Scene 代码 → 渲染 → 出错把"执行日志"喂回去定向修复 (有限轮);
//   3) 渲染成功后用视觉评审器对真实画面把关。
// 非算法题材或 manim 不可用时, 调用方回退到 HTML 路径。
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// manimBin 解析 manim 可执行文件 (env MANIM_BIN > PATH > ~/.local/bin/manim)。空串=不可用。
func manimBin() string {
	if b := strings.TrimSpace(os.Getenv("MANIM_BIN")); b != "" {
		if _, err := os.Stat(b); err == nil {
			return b
		}
	}
	if p, err := exec.LookPath("manim"); err == nil {
		return p
	}
	if home, _ := os.UserHomeDir(); home != "" {
		c := filepath.Join(home, ".local", "bin", "manim")
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// ManimAvailable 报告 manim 引擎是否可用。
func ManimAvailable() bool { return manimBin() != "" }

var algoVideoKeywords = []string{
	"算法", "algorithm", "数据结构", "data structure", "排序", "sort", "查找", "search",
	"raft", "paxos", "共识", "consensus", "分布式", "distributed", "协议", "protocol",
	"原理", "运行机制", "流程", "状态机", "state machine", "图解", "可视化", "visualiz",
	"递归", "recursion", "动态规划", "dynamic programming", "树", "graph", "链表", "哈希",
}

// IsAlgorithmExplainerVideo 判断目标是否为"技术/算法类讲解视频"(genre 路由)。
// 需同时像"视频"且像"技术/算法原理"题材。
func IsAlgorithmExplainerVideo(objective string) bool {
	low := strings.ToLower(objective)
	isVideo := strings.Contains(low, "视频") || strings.Contains(low, "video") ||
		strings.Contains(low, "动画") || strings.Contains(low, "mp4") || strings.Contains(low, "讲解") ||
		strings.Contains(low, "演示")
	if !isVideo {
		return false
	}
	for _, kw := range algoVideoKeywords {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

const manimSystemPrompt = `你是 manim (Community v0.18+) 动画工程师, 为"算法/技术原理讲解视频"写 Python 动画脚本。
只输出一个完整 Python 代码块, 不要任何解释文字。

【硬性结构】
- 必须: from manim import *  且类名固定为 Explainer(Scene), 实现 construct(self)。self.camera.background_color = "#0a0e17" (深色背景)。
- 严禁 Tex / MathTex / Title (无 LaTeX)。所有文字一律 Text(...)。
- 中文必须指定字体: Text("内容", font="Noto Sans CJK SC")。变量/公式用 Text 普通字符 (如 Text("n/2+1"))。
- 只用稳定 API: Text, Circle, Dot, Square, Rectangle, RoundedRectangle, Line, Arrow, DoubleArrow, VGroup, SurroundingRectangle; 方位 UP/DOWN/LEFT/RIGHT/ORIGIN; 方法 .next_to/.shift/.move_to/.to_edge/.scale/.arrange; 动画 Write/Create/FadeIn/FadeOut/Transform/GrowArrow; self.play/self.wait。

【可读性铁律 (上一类视频的真实失败教训, 必须避免)】
- 文字颜色只能用亮色: WHITE / "#e5e7eb" / BLUE_B / GREEN_B / YELLOW / ORANGE。严禁深灰/黑/低对比色 (会在深色背景上看不见)。
- 标题 font_size>=48, 正文 font_size>=30; 节点/图形用亮色描边 (color=BLUE_B/GREEN_B 等), 半径>=0.5。
- 文字要短: 每条 Text 不超过 14 个汉字; 长内容拆成多条短 Text 用 VGroup(...).arrange(DOWN, buff=0.3)。禁止超长单行 (会截断/溢出)。

【每个场景必须"图文并茂", 不能只有标题】
- 分 5-6 个小节, 每节都要有: 1 个标题(顶部) + 至少 2-3 个图形元素(节点/箭头/方框, 居中) + 1 条说明文字。只有标题的空场景不合格。
- 用 VGroup().arrange() 或 .next_to(..., buff=0.6) 布局, 不重叠、不出画面 (画面 14.2x8 单位, 元素留在 x∈[-6.5,6.5] y∈[-3.5,3.5])。

【节奏 (避免黑屏/闪现)】
- 每节展开后 self.wait(2.0) 让观众看清; 切换下一节用 self.play(FadeOut(*self.mobjects), run_time=0.5) 紧接着马上展开下一节 (不要留长黑屏)。
- 总时长约 40-60 秒。
%s
请基于以下主题输出 manim 脚本 (raft 算法: 共识问题→领导者选举→日志复制→脑裂/网络分区→解决方案):
%s`

// GenerateManimVideo 让 LLM 写 manim 代码并渲染; 出错把执行日志喂回定向修复 (最多 maxRounds 轮)。
// 成功返回 mp4 路径与最终代码; 失败返回空串。llm 用于代码生成/修复 (SimpleComplete)。
func (we *WorkflowExecutor) GenerateManimVideo(ctx context.Context, objective, workDir string, maxRounds int) (mp4Path, code string, err error) {
	bin := manimBin()
	if bin == "" {
		return "", "", fmt.Errorf("manim 不可用")
	}
	if we.llm == nil {
		return "", "", fmt.Errorf("LLM 未配置")
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", "", err
	}
	fixHint := ""
	var lastErr string
	for round := 1; round <= maxRounds; round++ {
		we.notify(we.chatID, fmt.Sprintf("  🎬 manim 第 %d/%d 轮: 生成动画脚本...", round, maxRounds))
		sys := fmt.Sprintf(manimSystemPrompt, fixHint, objective)
		reply, e := we.llm.SimpleComplete(ctx, sys, "请输出 manim 脚本 (只要代码)。")
		if e != nil {
			lastErr = "LLM 生成失败: " + e.Error()
			continue
		}
		code = extractPythonCode(reply)
		if strings.TrimSpace(code) == "" {
			lastErr = "未能从回复中提取 Python 代码"
			continue
		}
		pyPath := filepath.Join(workDir, "explainer.py")
		if e := os.WriteFile(pyPath, []byte(code), 0o644); e != nil {
			return "", code, e
		}
		we.notify(we.chatID, fmt.Sprintf("  🎞️ manim 渲染中 (第 %d 轮)...", round))
		out, renderErr := runManimRender(ctx, bin, pyPath, workDir)
		if renderErr == nil && out != "" {
			return out, code, nil
		}
		lastErr = renderErr.Error()
		// 把执行日志(截尾)喂回, 让模型定向修复
		tail := lastErr
		if len(tail) > 1500 {
			tail = tail[len(tail)-1500:]
		}
		fixHint = fmt.Sprintf("\n上一版脚本渲染失败, 请修复后重新输出完整脚本。错误日志(节选):\n%s\n", tail)
		we.notify(we.chatID, fmt.Sprintf("  ⚠️ manim 第 %d 轮渲染失败, 按错误日志修复重试...", round))
	}
	return "", code, fmt.Errorf("manim 渲染在 %d 轮内未成功: %s", maxRounds, lastErr)
}

var pyFenceRe = regexp.MustCompile("(?s)```(?:python|py)?\\s*(.*?)```")

// extractPythonCode 从 LLM 回复里取出 Python 代码 (优先代码块; 否则取含 import manim 的整体)。
func extractPythonCode(reply string) string {
	if m := pyFenceRe.FindStringSubmatch(reply); m != nil {
		return strings.TrimSpace(m[1])
	}
	if idx := strings.Index(reply, "from manim"); idx >= 0 {
		return strings.TrimSpace(reply[idx:])
	}
	if idx := strings.Index(reply, "import manim"); idx >= 0 {
		return strings.TrimSpace(reply[idx:])
	}
	return ""
}

// runManimRender 渲染 explainer.py 的 Explainer 场景, 返回输出 mp4 路径。失败返回 stderr。
func runManimRender(ctx context.Context, bin, pyPath, workDir string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	mediaDir := filepath.Join(workDir, "media")
	// -qm: 720p30 平衡质量/速度; --disable_caching 保证每轮重渲染。
	cmd := exec.CommandContext(rctx, bin, "-qm", "--format", "mp4",
		"--media_dir", mediaDir, "--disable_caching", pyPath, "Explainer")
	cmd.Dir = workDir
	combined, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v\n%s", err, string(combined))
	}
	// manim 输出在 media/videos/<script>/<quality>/Explainer.mp4
	mp4 := findNewestMP4(mediaDir)
	if mp4 == "" {
		return "", fmt.Errorf("渲染完成但未找到输出 mp4\n%s", string(combined))
	}
	return mp4, nil
}

func findNewestMP4(root string) string {
	var newest string
	var newestT time.Time
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".mp4") {
			return nil
		}
		// 跳过 manim 的中间分片 (partial_movie_files), 只取最终合成视频。
		if strings.Contains(p, "partial_movie_files") {
			return nil
		}
		if info.ModTime().After(newestT) {
			newestT = info.ModTime()
			newest = p
		}
		return nil
	})
	return newest
}

// runManimVideoPath 是 creative-v2 的 Phase B 分支: 用 manim 生成算法讲解视频, 视觉把关,
// 成功返回 StageResults + true; 失败返回 (nil,false) 让调用方回退 HTML 流水线。
func (we *WorkflowExecutor) runManimVideoPath(ctx context.Context, objective string, team *ProductionTeam) ([]StageResult, bool) {
	start := time.Now()
	we.notify(team.ChatID, "🎬 **manim 路径**: 算法/技术类视频走专业动画引擎 (确定性渲染)")
	workDir := filepath.Join(team.dataDir, "manim")
	mp4, code, err := we.GenerateManimVideo(ctx, objective, workDir, 3)
	if err != nil || mp4 == "" {
		we.notify(team.ChatID, fmt.Sprintf("  ⚠️ manim 生成失败: %v", err))
		return nil, false
	}

	// 落地到团队 media 目录
	mediaDir := filepath.Join(team.dataDir, "media")
	_ = os.MkdirAll(mediaDir, 0o755)
	finalMP4 := filepath.Join(mediaDir, team.Name+".mp4")
	if data, e := os.ReadFile(mp4); e == nil {
		_ = os.WriteFile(finalMP4, data, 0o644)
	} else {
		finalMP4 = mp4
	}

	// 视觉接地把关 (复用 kimi/gemma4 评审器); 不通过则带视觉反馈再生成一轮。
	var visionRC RawVisionCompleter
	if r, ok := we.llm.(RawVisionCompleter); ok {
		visionRC = r
	}
	verdictSummary := ""
	if vb := SelectVisionBackend(ctx, visionRC); vb != nil {
		titles := []string{"开场/问题引入", "核心概念", "关键流程", "异常与边界", "解决方案/总结"}
		we.notify(team.ChatID, fmt.Sprintf("  👁️ 视觉质检 (后端 %s) manim 视频...", vb.Name()))
		if vs, e := ReviewVideoScenes(ctx, vb, finalMP4, titles, objective); e == nil {
			allPass, fb := VerdictsFeedback(vs)
			verdictSummary = formatVisionVerdicts(vs)
			if !allPass {
				we.notify(team.ChatID, "  🔁 manim 视频视觉质检发现问题, 带反馈再生成一轮:\n"+fb)
				if mp4b, code2, e2 := we.GenerateManimVideo(ctx, objective+"\n\n[上一版渲染画面的问题, 必须改进]:\n"+fb, workDir, 2); e2 == nil && mp4b != "" {
					if data, e3 := os.ReadFile(mp4b); e3 == nil {
						_ = os.WriteFile(finalMP4, data, 0o644)
						code = code2
						if vs2, e4 := ReviewVideoScenes(ctx, vb, finalMP4, titles, objective); e4 == nil {
							verdictSummary = formatVisionVerdicts(vs2)
						}
					}
				}
			} else {
				we.notify(team.ChatID, "  ✅ manim 视频视觉质检通过")
			}
		}
	}

	dur := "?"
	if mr := probeDurationSec(ctx, finalMP4); mr > 0 {
		dur = fmt.Sprintf("%.0fs", mr)
	}
	we.notify(team.ChatID, fmt.Sprintf("📎 manim 视频已生成: `%s` (时长 %s)", finalMP4, dur))

	report := fmt.Sprintf("# manim 算法讲解视频交付\n\n- 目标: %s\n- 引擎: manim (确定性渲染)\n- 视频: %s (时长 %s)\n\n## 视觉质检\n%s\n\n## 动画脚本(节选)\n```python\n%s\n```\n",
		objective, finalMP4, dur, verdictSummary, truncateResult(code, 1200))

	return []StageResult{
		{Name: "manim-generate", Role: "manim-engineer", Status: TaskCompleted, Output: truncateResult(code, 2000), Duration: time.Since(start).Round(time.Second).String()},
		{Name: "manim-render", Status: TaskCompleted, Output: fmt.Sprintf("✅ mp4: %s (时长 %s)", finalMP4, dur)},
		{Name: "final-delivery", Role: "media-producer", Status: TaskCompleted, Output: report},
	}, true
}

