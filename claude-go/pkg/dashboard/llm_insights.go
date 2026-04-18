package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
)

// buildLLMSummary 使用 Claude 为当前的规则洞察做二次压缩/聚合, 返回一段 Markdown.
// 需要环境变量 ANTHROPIC_API_KEY (或 ANTHROPIC_AUTH_TOKEN), 否则返回错误.
func buildLLMSummary(ctx context.Context, resp *InsightsResp) (string, error) {
	key := firstNonEmpty(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("ANTHROPIC_AUTH_TOKEN"))
	if key == "" {
		return "", fmt.Errorf("未设置 ANTHROPIC_API_KEY, LLM 诊断不可用")
	}
	model := os.Getenv("DASHBOARD_LLM_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	base := firstNonEmpty(os.Getenv("ANTHROPIC_BASE_URL"), "https://api.anthropic.com")
	client := api.NewClient(base, key, model)

	payload := map[string]interface{}{
		"insights": resp.Insights,
		"total":    resp.Total,
	}
	b, _ := json.Marshal(payload)
	sys := strings.TrimSpace(`你是 Claude-Go 的系统诊断助手。你会收到一组基于规则产生的 insights（每条含 module/severity/title/suggestion）。请:
1. 合并重复/同源问题
2. 根据 severity 排优先级 (critical > warn > info)
3. 给出 3-5 条高概括性的"下一步建议" (markdown 列表)
4. 语言使用简体中文, 语气专业、克制, 不使用 emoji
5. 输出 ≤ 220 字`)

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := client.SimpleComplete(cctx, sys, "规则产出的 JSON 数据如下:\n"+string(b))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
