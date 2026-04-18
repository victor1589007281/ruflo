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
	sys := strings.TrimSpace(`你是 Claude-Go 的系统诊断助手。你会收到一组基于规则产生的 insights（每条含 module/severity/title/suggestion）。请:
1. 合并重复/同源问题
2. 根据 severity 排优先级 (critical > warn > info)
3. 给出 3-5 条高概括性的"下一步建议" (markdown 列表)
4. 语言使用简体中文, 语气专业、克制, 不使用 emoji
5. 输出 ≤ 220 字`)

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
