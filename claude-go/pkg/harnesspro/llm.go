package harnesspro

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/types"
)

// llm.go —— 进化侧 LLM 调用的唯一出口。
//
// 接口形态刻意取 (text, usage, error) 三元组而不是 api.Client.SimpleComplete:
// 论文 4.1 要求 cost 从轨迹机械统计 (token 用量 × 价目表), 拿不到 usage 的调用
// 形态会让 cost 退化成"估算输入 + 未知输出", 帕累托前沿就糊了。

// Usage 一次调用的 token 用量 (cost 的机械来源)。
type Usage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// Total 总 token。
func (u Usage) Total() int64 { return int64(u.Input + u.Output) }

// LLM 进化侧 LLM 客户端 (生产 = APILLM; 测试 = 桩)。
type LLM interface {
	// Complete 一次 system+user 补全, 返回文本与用量。
	Complete(ctx context.Context, system, user string) (string, Usage, error)
	// Model 当前模型别名 (报告/日志归因)。
	Model() string
}

// maxCompleteTokens 单次进化调用的输出上限。
// 对齐 api.SimpleCompleteMaxTokens (65536): 出卷/诊断的 JSON 在大输入下会吃 thinking,
// 上限给足余量; 短输出 end_turn 早停, 不浪费。
const maxCompleteTokens = api.SimpleCompleteMaxTokens

// APILLM 生产实现: 包 api.Client, 经 SendMessage 拿真实 Usage。
type APILLM struct {
	Client *api.Client
}

// Complete 见 LLM。
func (l *APILLM) Complete(ctx context.Context, system, user string) (string, Usage, error) {
	if l == nil || l.Client == nil {
		return "", Usage{}, fmt.Errorf("harnesspro: LLM 客户端未注入")
	}
	msgs := []types.APIMessage{{
		Role: "user",
		Content: json.RawMessage(`[{"type":"text","text":` +
			marshalJSONString(user) + `}]`),
	}}
	resp, err := l.Client.SendMessage(ctx, msgs, []string{system}, nil, maxCompleteTokens)
	if err != nil {
		return "", Usage{}, err
	}
	var sb strings.Builder
	for _, b := range resp.Content {
		if b.Type == types.ContentBlockText {
			sb.WriteString(b.Text)
		}
	}
	var u Usage
	if resp.Usage != nil {
		u.Input = resp.Usage.InputTokens
		u.Output = resp.Usage.OutputTokens
	}
	return sb.String(), u, nil
}

// Model 见 LLM。
func (l *APILLM) Model() string {
	if l == nil || l.Client == nil {
		return ""
	}
	return l.Client.Model
}

// marshalJSONString 把字符串编码为 JSON 字面量 (与 pkg/api 的 mustMarshalString 同源;
// 本包不 import 私有助手, 自行持有这一个 6 行函数)。
func marshalJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// extractJSON 从模型产出里抠出第一个完整 JSON 对象/数组。
// 模型爱用 ```json 围栏或前后散文 —— 出卷/诊断/计划全部要求 JSON, 这里统一宽容解析,
// 解析失败由调用方决定重试或降级 (不让一层 ``` 围栏毁掉一轮进化)。
func extractJSON(s string) (string, bool) {
	s = strings.TrimSpace(s)
	// 去代码围栏。
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = strings.TrimSpace(s[idx+1:])
		}
		if strings.HasSuffix(s, "```") {
			s = strings.TrimSpace(s[:len(s)-3])
		}
	}
	// 找第一个 { 或 [ 与配对的收尾 (括号计数, 字符串感知的简化版: 进化 JSON 不含裸括号)。
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '{' || s[i] == '[' {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	open := s[start]
	close := byte('}')
	if open == '[' {
		close = ']'
	}
	depth := 0
	inStr := false
	var esc bool
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}
