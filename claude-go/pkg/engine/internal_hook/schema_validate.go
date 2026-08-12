// schema_validate.go — tool_use 入参 JSON Schema 校验 + 错误回注重试环 (L2)。
//
// 手册 13.3.3 L2：弱模型 (本地 2-12B) 的 tool_use 入参失败大头是 schema 违背
// (缺 required、类型错、enum 越界)。此前的兜底是 JSONRepair 的**沉默修复**——
// 模型不知道自己在裸奔，同样的错误会反复犯。本 hook 把"沉默修复"升级为
// "校验 + 错误回注、原地重填"：执行前对 input 做确定性 schema 校验，
// 失败则不执行、把具体错误作为 is_error tool_result 回注，让模型原地修正；
// 同一签名调用重试超限 (默认 2 次) 后注入终止错误，强制模型换策略。
//
// 业界依据: SWE-agent ACI (arXiv:2405.15793) 的护栏原则——坏调用直接拒绝并把
// 错误作为反馈返回；BFCL 失败分类显示格式/参数错是小模型失败大头。
//
// 位置: PhasePreToolUse, 优先级 75 (JSONRepair(70) 之后, LoopDetectorInput(80) 之前)。
// 仅在 weakmodel.Profile.SchemaRetry 开启时注册 (见 engine.registerInternalHooks)。
package internal_hook

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// mini JSON Schema 校验器 (stdlib-only 子集)
//
// 支持: type (string/number/integer/boolean/object/array/null 及联合类型数组)、
// required、properties (递归)、items (单 schema)、enum。
// 其余关键字 (additionalProperties/pattern/minLength 等) 一律放行——
// 本校验器的目标是拦"硬伤", 不是全量 JSON Schema 合规。
// schema 本身不可解析时放行 (容错, 不错杀)。
// ============================================================================

// ValidateAgainstSchema 校验 input 是否满足 schema 的硬性约束。
// 返回 nil 表示通过 (或无法判定, 放行); 非 nil error 是首个人类可读错误 (带路径)。
func ValidateAgainstSchema(input, schema json.RawMessage) error {
	if len(schema) == 0 {
		return nil
	}
	var sch map[string]interface{}
	if err := json.Unmarshal(schema, &sch); err != nil {
		return nil // schema 不可解析: 放行 (工具作者的 schema 问题不该拦模型)
	}
	var val interface{}
	if err := json.Unmarshal(input, &val); err != nil {
		return fmt.Errorf("input 不是合法 JSON: %v", err)
	}
	return validateValue(val, sch, "$")
}

func validateValue(v interface{}, sch map[string]interface{}, path string) error {
	// enum: 值必须命中枚举之一
	if enumRaw, ok := sch["enum"]; ok {
		if enum, ok := enumRaw.([]interface{}); ok && len(enum) > 0 {
			hit := false
			for _, e := range enum {
				if jsonDeepEqual(e, v) {
					hit = true
					break
				}
			}
			if !hit {
				return fmt.Errorf("%s 的值 %v 不在 enum 允许集合内", path, truncateForErr(v))
			}
		}
	}

	// type: 支持单值或联合类型数组
	if typeRaw, ok := sch["type"]; ok {
		if !checkJSONType(v, typeRaw) {
			return fmt.Errorf("%s 类型错误: 期望 %s, 实际 %s", path, typeName(typeRaw), jsonTypeOf(v))
		}
	}

	switch tv := v.(type) {
	case map[string]interface{}:
		// required
		if reqRaw, ok := sch["required"]; ok {
			if req, ok := reqRaw.([]interface{}); ok {
				for _, r := range req {
					if name, ok := r.(string); ok {
						if _, present := tv[name]; !present {
							return fmt.Errorf("%s 缺少必填字段 %q", path, name)
						}
					}
				}
			}
		}
		// properties: 仅校验出现的字段 (缺失由 required 管)
		if propsRaw, ok := sch["properties"]; ok {
			if props, ok := propsRaw.(map[string]interface{}); ok {
				keys := make([]string, 0, len(props))
				for k := range props {
					keys = append(keys, k)
				}
				sort.Strings(keys) // 确定性错误顺序
				for _, k := range keys {
					sub, ok := props[k].(map[string]interface{})
					if !ok {
						continue
					}
					if fv, present := tv[k]; present {
						if err := validateValue(fv, sub, path+"."+k); err != nil {
							return err
						}
					}
				}
			}
		}
	case []interface{}:
		if itemsRaw, ok := sch["items"]; ok {
			if items, ok := itemsRaw.(map[string]interface{}); ok {
				for i, el := range tv {
					if err := validateValue(el, items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// checkJSONType 判定值是否满足 type 关键字 (单值字符串或联合类型数组, 任一命中即过)。
func checkJSONType(v interface{}, typeRaw interface{}) bool {
	switch t := typeRaw.(type) {
	case string:
		return matchJSONType(v, t)
	case []interface{}:
		for _, item := range t {
			if s, ok := item.(string); ok && matchJSONType(v, s) {
				return true
			}
		}
	}
	return true // type 写法不认识: 放行
}

func matchJSONType(v interface{}, t string) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]interface{})
		return ok
	case "array":
		_, ok := v.([]interface{})
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "null":
		return v == nil
	}
	return true // 未知类型名: 放行
}

// typeName 把 type 关键字 (字符串或数组) 渲染为人类可读形式。
func typeName(typeRaw interface{}) string {
	switch t := typeRaw.(type) {
	case string:
		return t
	case []interface{}:
		var parts []string
		for _, item := range t {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "|")
		}
	}
	return "any"
}

func jsonTypeOf(v interface{}) string {
	switch v.(type) {
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case nil:
		return "null"
	}
	return "unknown"
}

// jsonDeepEqual JSON 语义的值相等 (数字统一 float64 后比较, 与 encoding/json 一致)。
func jsonDeepEqual(a, b interface{}) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func truncateForErr(v interface{}) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

// schemaHint 从 schema 提取一行式参数提示 (required 标记 *), 供回注错误时告诉
// 模型正确形状——比贴完整 JSON Schema 省 token 且对弱模型更直白。
func schemaHint(schema json.RawMessage) string {
	var sch map[string]interface{}
	if err := json.Unmarshal(schema, &sch); err != nil {
		return ""
	}
	props, _ := sch["properties"].(map[string]interface{})
	if len(props) == 0 {
		return ""
	}
	reqSet := map[string]bool{}
	if req, ok := sch["required"].([]interface{}); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				reqSet[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	var parts []string
	for _, n := range names {
		mark := ""
		if reqSet[n] {
			mark = "*"
		}
		t := "any"
		if sub, ok := props[n].(map[string]interface{}); ok {
			t = typeName(sub["type"])
		}
		parts = append(parts, fmt.Sprintf("%s%s:%s", n, mark, t))
	}
	hint := strings.Join(parts, ", ")
	if len(hint) > 400 {
		hint = hint[:400] + "…"
	}
	return hint + "  (*=必填)"
}

// ============================================================================
// SchemaValidateHook — PhasePreToolUse, 优先级 75
// ============================================================================

// SchemaValidateHook 执行前校验 tool_use 入参, 失败回注错误让模型原地重填。
//
// 重试计数按 "工具名 + 归一化入参哈希" 签名: 模型重填后签名变化即重新计数,
// 同一烂调用反复重发才会触顶——触顶后注入**终止错误** (模型须换策略),
// 避免"校验-重填"自身成为新的死循环 (loop_detector 管不了的语义级循环)。
type SchemaValidateHook struct {
	schemas    map[string]json.RawMessage // 工具名 → input_schema
	metrics    *EngineMetrics
	maxRetries int

	mu       sync.Mutex
	attempts map[string]int // 签名 → 已重试次数
}

// NewSchemaValidateHook 创建校验 hook。schemas 为空或 maxRetries<1 时返回 nil。
func NewSchemaValidateHook(schemas map[string]json.RawMessage, metrics *EngineMetrics, maxRetries int) *SchemaValidateHook {
	if len(schemas) == 0 || maxRetries < 1 {
		return nil
	}
	return &SchemaValidateHook{
		schemas:    schemas,
		metrics:    metrics,
		maxRetries: maxRetries,
		attempts:   make(map[string]int),
	}
}

func (h *SchemaValidateHook) Name() string { return "schema_validate" }
func (h *SchemaValidateHook) Priority() int {
	return 75 // JSONRepair(70) 之后, LoopDetectorInput(80) 之前
}
func (h *SchemaValidateHook) Phases() []InternalHookPhase {
	return []InternalHookPhase{PhasePreToolUse}
}

// Execute 校验所有 tool_use 块; 失败块替换为错误 tool_result 回注。
func (h *SchemaValidateHook) Execute(ctx *HookContext) (*HookResult, error) {
	if h == nil || len(ctx.ToolUseBlocks) == 0 {
		return nil, nil
	}

	var allowed []types.ContentBlock
	var appendMsgs []types.Message
	blocked := 0

	for _, blk := range ctx.ToolUseBlocks {
		schema, ok := h.schemas[blk.Name]
		if !ok || len(schema) == 0 {
			allowed = append(allowed, blk) // 无 schema 的工具不拦
			continue
		}
		verr := ValidateAgainstSchema(blk.Input, schema)
		if verr == nil {
			allowed = append(allowed, blk)
			continue
		}

		blocked++
		sig := blk.Name + "|" + NormalizeHash([]byte(blk.Input))
		h.mu.Lock()
		h.attempts[sig]++
		n := h.attempts[sig]
		h.mu.Unlock()

		var content string
		if n > h.maxRetries {
			// 终止错误: 同一签名屡教不改, 强制换策略 (防语义级死循环)
			content = fmt.Sprintf(
				"[schema_validate] 工具 %s 的相同调用已连续 %d 次未通过参数校验, 本调用被终止。最后一次错误: %v。请改用其它方法推进任务, 不要再次以相同参数调用本工具。",
				blk.Name, n-1, verr)
			if h.metrics != nil {
				h.metrics.SchemaValidationBlocked.Add(1)
			}
		} else {
			content = fmt.Sprintf(
				"[schema_validate] 工具 %s 的参数未通过校验, 未执行 (%d/%d)。错误: %v。正确参数形状: %s。请修正后重新调用。",
				blk.Name, n, h.maxRetries, verr, schemaHint(schema))
			if h.metrics != nil {
				h.metrics.SchemaValidationRetried.Add(1)
			}
		}
		appendMsgs = append(appendMsgs, types.Message{
			Type: types.MessageTypeUser,
			UUID: GenerateUUID(),
			Content: []types.ContentBlock{{
				Type:      types.ContentBlockToolResult,
				ToolUseID: blk.ID,
				Content:   content,
				IsError:   true,
			}},
			CreatedAt: time.Now(),
		})
	}

	if blocked == 0 {
		return nil, nil
	}
	logging.For("engine").Warn("schema_validate blocked tool calls",
		"blocked", blocked, "allowed", len(allowed))

	result := &HookResult{ToolUseBlocks: allowed, AppendMsgs: appendMsgs}
	if len(allowed) == 0 {
		result.InjectContinue = true
	}
	return result, nil
}
