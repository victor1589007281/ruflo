// loop_detector.go — 工具调用循环检测 (G3)。
//
// 对标:
//   - Kimi K2.5 "防伪并行": 要求每个 worker 产出非平凡 artifact (diff/test/file)
//   - GLM 5.1 Agentic 信用分配: 按 tool span 分段归因, 识别无效重复
//   - MiniMax M2.7 Keep/Revert: 检测无效反复
//
// 当前 QueryEngine 不识别 "同工具同参数连续调用"。
// 模型偶尔会卡在 Read→Grep→Read→Grep 同一文件的死循环里, 吞 token 却无进展。
//
// 本组件维护一个滑动窗口, 记录 (toolName, normalizedInputHash)。
// 连续 N 次命中相同签名 → 判定死循环, 返回建议文本由上层注入 assistant 提示。
//
// 产出级检测 (V1.1 新增):
//   - 编辑震荡检测: 追踪同一文件的修改历史, 若内容在有限状态间循环 (A→B→A)
//     判定为无效震荡, 提示模型换策略。
//   - 编译错误指纹检测: 对 Bash 错误结果提取规范化指纹, 若同一指纹连续 >=2 次出现,
//     判定为"此路不通", 提示模型停止重复尝试。
package internal_hook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LoopDetector 工具循环检测器。
type LoopDetector struct {
	mu sync.Mutex

	// 配置
	Window    int // 滑动窗口长度 (默认 8)
	Threshold int // 连续相同签名次数阈值 (默认 3)

	// 输入级循环检测
	history []callSignature

	// 产出级检测配置
	Cwd string // 工作目录, 用于解析相对路径

	// 文件编辑历史: 文件路径 -> 最近编辑记录
	fileEdits map[string][]fileEditRecord

	// 编译错误指纹: 规范化指纹 -> 连续出现次数
	errorFingerprints map[string]int
	lastErrorFP       string

	// triggerCount 累计触发次数 (输入级 + 产出级)。
	// AdvisorCheckpointHook 通过增量判断"本轮是否检测到循环"。
	triggerCount int
}

type callSignature struct {
	Tool string
	Hash string
	At   time.Time
}

type fileEditRecord struct {
	ContentHash string
	InputHash   string
	At          time.Time
}

var (
	errDigitRe = regexp.MustCompile(`\d+`)
	errPathRe  = regexp.MustCompile(`(/[a-zA-Z0-9_.-]+)+`)
)

// NewLoopDetector 构造默认检测器。
func NewLoopDetector() *LoopDetector {
	return &LoopDetector{
		Window:            8,
		Threshold:         3,
		fileEdits:         make(map[string][]fileEditRecord),
		errorFingerprints: make(map[string]int),
	}
}

// Observe 记录一次工具调用, 返回是否触发循环 + 建议文本。
//
// 参数:
//   - tool: 工具名
//   - input: 工具输入 (json.RawMessage)
//
// 返回:
//   - looped: 是否触发循环
//   - suggestion: 建议注入给模型的提示 (触发时非空)
func (d *LoopDetector) Observe(tool string, input []byte) (looped bool, suggestion string) {
	if d == nil {
		return false, ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	sig := callSignature{
		Tool: tool,
		Hash: NormalizeHash(input),
		At:   time.Now(),
	}

	d.history = append(d.history, sig)
	if len(d.history) > d.Window {
		d.history = d.history[len(d.history)-d.Window:]
	}

	threshold := d.Threshold
	if threshold < 2 {
		threshold = 3
	}

	// 扫描: 尾部 threshold 个签名是否全部相同
	if len(d.history) < threshold {
		return false, ""
	}
	tail := d.history[len(d.history)-threshold:]
	first := tail[0]
	for _, s := range tail[1:] {
		if s.Tool != first.Tool || s.Hash != first.Hash {
			return false, ""
		}
	}

	suggestion = fmt.Sprintf(
		"系统检测到你连续 %d 次调用 `%s`, 参数完全相同, 未取得新信息。请从以下 3 个选项中任选其一:\n"+
			"1. 说明你从已有结果中学到了什么, 然后采取【不同策略】继续。\n"+
			"2. 承认当前信息不足以解决问题, 直接向用户反馈卡点。\n"+
			"3. 如果信息其实已足够, 直接给出最终答复, 不再继续搜索。\n"+
			"注意: 再次以相同参数调用同一工具将不会带来进展。",
		threshold, first.Tool,
	)
	d.triggerCount++
	return true, suggestion
}

// TriggerCount 返回累计触发次数 (输入级 + 产出级)。
func (d *LoopDetector) TriggerCount() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.triggerCount
}

// ObserveResult 在工具执行后调用, 检测产出级循环 (编辑震荡 / 编译错误重复)。
//
// 参数:
//   - tool: 工具名
//   - input: 工具输入
//   - result: 工具结果文本摘要
//   - isError: 是否为错误结果
//
// 返回:
//   - stuck: 是否检测到无效产出循环
//   - suggestion: 建议注入给模型的提示
func (d *LoopDetector) ObserveResult(tool string, input []byte, result string, isError bool) (stuck bool, suggestion string) {
	if d == nil {
		return false, ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	// 1. 编辑震荡检测
	if stuck, suggestion = d.checkEditOscillationLocked(tool, input); stuck {
		d.triggerCount++
		return true, suggestion
	}

	// 2. 编译错误指纹检测
	if stuck, suggestion = d.checkCompileErrorLoopLocked(tool, result, isError); stuck {
		d.triggerCount++
		return true, suggestion
	}

	return false, ""
}

// checkEditOscillationLocked 检测同一文件是否在有限内容状态间循环。
// 要求调用方已持有 d.mu。
func (d *LoopDetector) checkEditOscillationLocked(tool string, input []byte) (bool, string) {
	if tool != "StrReplace" && tool != "Write" {
		return false, ""
	}

	// 提取文件路径
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return false, ""
	}
	p := in.Path
	if p == "" {
		return false, ""
	}
	if !filepath.IsAbs(p) && d.Cwd != "" {
		p = filepath.Join(d.Cwd, p)
	}

	// 读取文件当前内容并计算哈希
	content, err := os.ReadFile(p)
	if err != nil {
		return false, ""
	}
	contentHash := hashBytes(content)

	// 记录本次编辑
	record := fileEditRecord{
		ContentHash: contentHash,
		InputHash:   NormalizeHash(input),
		At:          time.Now(),
	}
	d.fileEdits[p] = append(d.fileEdits[p], record)
	// 保留最近 5 条
	if len(d.fileEdits[p]) > 5 {
		d.fileEdits[p] = d.fileEdits[p][len(d.fileEdits[p])-5:]
	}

	records := d.fileEdits[p]
	if len(records) < 3 {
		return false, ""
	}

	// 规则 1: ABA 震荡 — 最近 3 次内容呈 A→B→A 模式
	if len(records) >= 3 {
		a, b, c := records[len(records)-3], records[len(records)-2], records[len(records)-1]
		if a.ContentHash == c.ContentHash && a.ContentHash != b.ContentHash {
			return true, fmt.Sprintf(
				"系统检测到你对 `%s` 的编辑呈震荡模式 (内容在状态 A→B→A 间反复)。这通常意味着修改策略在原地打转, 无法收敛到正确解。请:\n"+
					"1. 立即停止对该文件的微编辑。\n"+
					"2. 回顾整体方案, 确认当前改动的逻辑是否与目标一致。\n"+
					"3. 如果编译/测试错误反复出现, 分析错误根因而非反复尝试表面修复。\n"+
					"4. 必要时向用户说明卡点并请求指导。",
				filepath.Base(p),
			)
		}
	}

	// 规则 2: 强震荡 — 最近 4 次编辑中不同内容的种类 <= 2
	if len(records) >= 4 {
		unique := make(map[string]struct{}, len(records))
		for _, r := range records {
			unique[r.ContentHash] = struct{}{}
		}
		if len(unique) <= 2 {
			return true, fmt.Sprintf(
				"系统检测到你对 `%s` 的编辑在有限内容状态间循环 (%d 次编辑仅产生 %d 种不同内容)。这表明修改未推进实质性进展。建议:\n"+
					"1. 暂停修改, 重新审视整体方案和数据流。\n"+
					"2. 检查是否有隐藏依赖或接口契约被忽视。\n"+
					"3. 如果问题超出当前信息范围, 直接向用户反馈。",
				filepath.Base(p), len(records), len(unique),
			)
		}
	}

	return false, ""
}

// checkCompileErrorLoopLocked 检测同一编译/命令错误是否连续重复出现。
// 要求调用方已持有 d.mu。
func (d *LoopDetector) checkCompileErrorLoopLocked(tool string, result string, isError bool) (bool, string) {
	if (tool != "Bash" && tool != "Shell") || !isError {
		return false, ""
	}

	fp := normalizeErrorFingerprint(result)
	if fp == "" {
		return false, ""
	}

	if fp == d.lastErrorFP {
		d.errorFingerprints[fp]++
	} else {
		d.errorFingerprints = map[string]int{fp: 1}
		d.lastErrorFP = fp
	}

	if d.errorFingerprints[fp] >= 2 {
		return true, fmt.Sprintf(
			"系统检测到同一编译/命令错误已连续 %d 次出现。这通常意味着当前修改方向无法解决根本问题 (可能是接口漂移、类型不匹配、依赖缺失或隐藏约束)。请:\n"+
				"1. 立即停止重复相同的修复尝试。\n"+
				"2. 深入阅读报错涉及的相关代码, 理解错误的真实根因。\n"+
				"3. 检查是否有未被注意到的接口变更或跨文件依赖。\n"+
				"4. 如果根因不明, 向用户说明具体错误信息并请求指导。",
			d.errorFingerprints[fp],
		)
	}
	return false, ""
}

// normalizeErrorFingerprint 对命令错误输出做规范化, 生成可比较的错误指纹。
// 去掉数字和绝对路径, 保留错误类型和消息骨架。
func normalizeErrorFingerprint(result string) string {
	if len(result) > 2000 {
		result = result[:2000]
	}
	// 去掉所有数字
	s := errDigitRe.ReplaceAllString(result, "N")
	// 将绝对路径替换为 PATH
	s = errPathRe.ReplaceAllString(s, "PATH")
	// 去掉多余空白
	lines := strings.Split(s, "\n")
	var sb strings.Builder
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}
	return strings.TrimSpace(sb.String())
}

// Reset 清空窗口 (新 turn 可选择重置)。
func (d *LoopDetector) Reset() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.history = nil
	d.fileEdits = make(map[string][]fileEditRecord)
	d.errorFingerprints = make(map[string]int)
	d.lastErrorFP = ""
	d.mu.Unlock()
}

// Size 返回当前窗口已记录条数 (用于测试)。
func (d *LoopDetector) Size() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.history)
}

// normalizeHash 对 input 做轻量归一化 (去空白) 后 hash, 避免 trivial 差异导致误判。
// 不做 JSON 字段排序 — 代价高且边际收益低, 真要误放过就留给下一轮 human-in-loop。
func NormalizeHash(input []byte) string {
	if len(input) == 0 {
		return "empty"
	}
	// 简单归一化: 去除首尾空白
	h := sha256.Sum256(input)
	return hex.EncodeToString(h[:])[:16]
}

// hashBytes 计算字节切片的短哈希。
func hashBytes(b []byte) string {
	if len(b) == 0 {
		return "empty"
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}
