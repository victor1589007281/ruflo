// Package codeintel 提供对 GitNexus 和 Graphify CLI 的薄包装。
//
// 设计原则：Go 仅做编排，所有智能工作交给外部工具。
//   - GitNexus (Node.js): 结构查询、影响分析、符号导航
//   - Graphify (Python): 语义查询、社区检测、路径分析
package codeintel

import (
	"encoding/json"
	"fmt"
	"time"
)

// QueryResult 统一查询结果。
type QueryResult struct {
	QueryType string      `json:"query_type"`
	Shard     string      `json:"shard,omitempty"`
	Results   interface{} `json:"results"`
	Tokens    int         `json:"estimated_tokens"`
	LatencyMs int64       `json:"latency_ms"`
}

// BuildProgress 构建进度。
type BuildProgress struct {
	Phase   string `json:"phase"`
	Shard   string `json:"shard,omitempty"`
	Current int    `json:"current"`
	Total   int    `json:"total"`
	Message string `json:"message"`
}

// ============================================================================
// 查询参数类型
// ============================================================================

// NavigateQuery 符号导航查询。
type NavigateQuery struct {
	Symbol string `json:"symbol"`
	Depth  int    `json:"depth,omitempty"`
	Shard  string `json:"shard,omitempty"`
}

// ImpactQuery 影响分析查询。
type ImpactQuery struct {
	FilePath string `json:"file_path"`
	Depth    int    `json:"depth,omitempty"`
}

// CommunityQuery 社区查询。
type CommunityQuery struct {
	Shard string `json:"shard"`
	TopN  int    `json:"top_n,omitempty"`
}

// CrossShardQuery 跨片查询。
type CrossShardQuery struct {
	Symbol      string `json:"symbol"`
	TargetShard string `json:"target_shard,omitempty"`
}

// GrepOptions Grep 查询选项。
type GrepOptions struct {
	Dir        string
	MaxResults int
}

// ReadOptions 文件读取选项。
type ReadOptions struct {
	Offset int
	Limit  int
}

// ============================================================================
// 统一查询参数
// ============================================================================

// UnifiedQuery 统一查询参数（用于 Router 分发）。
type UnifiedQuery struct {
	QueryType    string `json:"query_type"`
	Shard        string `json:"shard,omitempty"`
	Symbol       string `json:"symbol,omitempty"`
	TargetSymbol string `json:"target_symbol,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	Depth        int    `json:"depth,omitempty"`
	TopN         int    `json:"top_n,omitempty"`
	TargetShard  string `json:"target_shard,omitempty"`
}

// QueryType 枚举。
const (
	QueryNavigate    = "navigate"
	QueryImpact      = "impact"
	QueryFindRefs    = "find_refs"
	QueryCommunities = "communities"
	QueryGodNodes    = "god_nodes"
	QueryPath        = "path"
	QuerySurprises   = "surprises"
	QueryCrossShard  = "cross_shard"
	QueryStatus      = "status"
	QueryNativeGrep  = "native_grep"
	QueryNativeRead  = "native_read"
)

// ============================================================================
// 辅助函数
// ============================================================================

// tryParseJSON 尝试将字节解析为 JSON，失败则包装为文本对象。
func tryParseJSON(data []byte) interface{} {
	var v interface{}
	if err := json.Unmarshal(data, &v); err == nil {
		return v
	}
	return map[string]interface{}{
		"output": string(data),
	}
}

// wrapResult 将原始输出包装为 QueryResult。
func wrapResult(queryType string, raw []byte, start time.Time) *QueryResult {
	results := tryParseJSON(raw)
	content, _ := json.MarshalIndent(results, "", "  ")
	return &QueryResult{
		QueryType: queryType,
		Results:   results,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}
}

// wrapError 将错误包装为包含原始输出的结果。
func wrapError(queryType string, raw []byte, err error, start time.Time) *QueryResult {
	results := map[string]interface{}{
		"error":   err.Error(),
		"output":  string(raw),
		"warning": "external tool reported an error; output included above",
	}
	content, _ := json.MarshalIndent(results, "", "  ")
	return &QueryResult{
		QueryType: queryType,
		Results:   results,
		Tokens:    len(content) / 4,
		LatencyMs: time.Since(start).Milliseconds(),
	}
}

// filterJSONOutput 从混合日志的输出中提取 JSON 部分。
// 跳过常见的日志 JSON（如 {"level":40,...}），优先返回结果 JSON。
func filterJSONOutput(data []byte) []byte {
	for i := 0; i < len(data); i++ {
		if data[i] == '{' || data[i] == '[' {
			end := findJSONEnd(data, i)
			if end <= i {
				continue
			}
			candidate := data[i : end+1]
			var v map[string]interface{}
			if err := json.Unmarshal(candidate, &v); err == nil {
				// 如果解析出的对象只有日志字段，跳过它继续找下一个
				if isLogJSON(v) {
					i = end
					continue
				}
				return candidate
			}
			// 如果不是 map 但可能是 array，也尝试
			var arr []interface{}
			if err := json.Unmarshal(candidate, &arr); err == nil {
				return candidate
			}
		}
	}
	return data
}

// isLogJSON 判断一个 JSON 对象是否为日志行。
func isLogJSON(v map[string]interface{}) bool {
	hasLevel := false
	hasTime := false
	hasName := false
	for k := range v {
		switch k {
		case "level", "time", "name", "msg", "pid", "hostname":
			if k == "level" {
				hasLevel = true
			}
			if k == "time" {
				hasTime = true
			}
			if k == "name" {
				hasName = true
			}
		default:
			return false
		}
	}
	return hasLevel && hasTime && hasName
}

// findJSONEnd 找到从 start 开始的 JSON 对象的结束位置（粗略估计）。
func findJSONEnd(data []byte, start int) int {
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(data); i++ {
		b := data[i]
		if inString {
			if escape {
				escape = false
				continue
			}
			if b == '\\' {
				escape = true
				continue
			}
			if b == '"' {
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			continue
		}
		if b == '{' || b == '[' {
			depth++
			continue
		}
		if b == '}' || b == ']' {
			depth--
			if depth == 0 {
				return i
			}
			continue
		}
	}
	return start
}

// execError 包装 exec 错误信息。
func execError(tool string, err error, stderr []byte) error {
	return fmt.Errorf("%s exec error: %w (stderr: %s)", tool, err, string(stderr))
}
