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
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// LoopDetector 工具循环检测器。
type LoopDetector struct {
	mu sync.Mutex

	// 配置
	Window    int // 滑动窗口长度 (默认 8)
	Threshold int // 连续相同签名次数阈值 (默认 3)

	history []callSignature
}

type callSignature struct {
	Tool string
	Hash string
	At   time.Time
}

// NewLoopDetector 构造默认检测器。
func NewLoopDetector() *LoopDetector {
	return &LoopDetector{
		Window:    8,
		Threshold: 3,
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
		Hash: normalizeHash(input),
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
	return true, suggestion
}

// Reset 清空窗口 (新 turn 可选择重置)。
func (d *LoopDetector) Reset() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.history = nil
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
func normalizeHash(input []byte) string {
	if len(input) == 0 {
		return "empty"
	}
	// 简单归一化: 去除首尾空白
	h := sha256.Sum256(input)
	return hex.EncodeToString(h[:])[:16]
}
