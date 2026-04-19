package dashboard

// LLM 驱动的二次诊断摘要。
// 本文件仅负责 "把规则洞察压缩成给人看的建议", 底层 API client 由
// llm_client.go 统一提供 (GetSharedLLMClient), 与飞书 bot 使用同一份
// claude-go.json#ai 配置。

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// buildLLMSummary 使用 Claude/兼容模型把 rule-based InsightsResp 压缩成
// 一段面向运维的 Markdown 建议。失败时返回原始错误, 调用方决定是否在
// response 中显示为 LLMError。
func buildLLMSummary(ctx context.Context, resp *InsightsResp) (string, error) {
	sys := strings.TrimSpace(`# 角色
你是 claude-go 全局诊断总控。前置的规则引擎已经扫出若干 insight (每条含 module/severity/title/suggestion). 你的任务是把它们浓缩成一份"运维看一眼就能动手"的简报, 而不是复述.

# 硬要求
1. 首行必须是 TL;DR 一句话: 总体状态 [OK / 关注 / 严重] + 主要风险来自哪个 module
2. 第 2~N 行: markdown 无序列表, 3~5 条"下一步动作"
   - 每条格式: "[Px][module] 动作" (Px ∈ {P0,P1,P2})
   - 必须具体可执行 (改哪个配置/命令/阈值); 不要出现 "建议加强"、"建议监控"、"尽快处理" 等空话
   - 同源问题必须合并, 不要罗列多条重复建议
3. 优先级排序: critical > warn > info; 若 critical 为 0, TL;DR 不得写 "严重"
4. 全文 ≤ 240 字, 简体中文, 无 emoji, 不要复读 JSON 字段名
5. 若 insights 为空或全是 info, 输出: "TL;DR: 系统状态良好, 无需干预." 然后给 1 条 "保持 X 次/周巡检" 的短建议即可`)

	payload := map[string]interface{}{
		"insights": resp.Insights,
		"total":    resp.Total,
	}
	b, _ := json.Marshal(payload)
	user := "规则产出的 JSON 数据如下:\n" + string(b)

	out, _, err := LLMComplete(ctx, sys, user, 30*time.Second)
	if err != nil {
		return "", err
	}
	return out, nil
}
