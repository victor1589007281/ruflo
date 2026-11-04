// jsonout.go — 结构化输出助手（Moqi R-H8）。
//
// 背景：业务层（记忆抽取、事实裁决、意图分类）需要模型稳定吐出**可落库**的 JSON。
// 直接调 LLMGateway.Simple 拿到一段文本再自己 json.Unmarshal 有三个已经踩过的坑：
//
//  1. 模型把 JSON 包在 Markdown 围栏里，或前面加一句"结果如下："——解析失败；
//  2. 模型漏字段/类型错/枚举越界，本地解析却成功，于是**半条记录**被写进库；
//  3. 一次失败就放弃，或无限重试把 429 打得更死。
//
// 本文件把"校验 → 有限修复重试 → 只在通过后交付"收敛成一个入口。这里刻意用
// LLMGateway.Complete 而不是 Simple：只有 Complete 回传 *APIResponse.Usage，
// 而 R-H8 要求把**全部尝试**的 usage 相加后交给业务层记账——用 Simple 就只能
// 拿到文本，失败的那两次花费在账上凭空消失。
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/llmgw"
	"github.com/anthropic/claude-go/pkg/types"
)

// defaultJSONMaxAttempts 是未显式设置 MaxAttempts 时的**总调用次数**（含第一次）。
//
// 刻意定义成"总次数"而不是"重试次数"：Moqi R-H8 的验收原话是"第一次返回非法
// JSON、第二次合法 -> 成功；两次都非法 -> typed error"，也就是默认路径最多
// 两次模型调用。"重试次数=2"会被读成三次调用，同一份契约在实现与验收之间差
// 一次请求——这种差一在本仓是典型事故（重试单层化）。
const defaultJSONMaxAttempts = 2

// maxRepairEchoBytes 是重试提示里回显上一次非法输出时的字节上限。
//
// 把整段（可能几千 token 的）非法输出原样塞回去既贵又会把上下文推向窗口边缘，
// 而修复真正需要的是两样东西：校验错误 + 出错位置附近的一小段原文。
const maxRepairEchoBytes = 2000

// JSONAttempt 是一次结构化输出尝试的观测记录。
//
// 为什么要逐次记录而不是只记最终结果：模型第一次吐的是截断的 JSON、第二次才
// 合法，与"两次都合法但第二次被覆盖"在最终结果上完全一样。只有逐次的 Raw +
// ValidationErr 才能回答"这条记录到底是哪次生成的、修过什么"。
type JSONAttempt struct {
	// Attempt 从 1 开始，等于这是第几次模型调用。
	Attempt int
	// Raw 是模型**原始**文本，不含本地修复——审计要看到模型真正吐了什么。
	Raw string
	// Valid 表示本次尝试（经过本地修复后）是否通过 schema 校验。
	Valid bool
	// ValidationErr 是未通过时的可读原因（带 $ 路径）。Valid 时为空。
	ValidationErr string
	// Repaired 表示本地修复动作（去 Markdown 围栏、截取首尾括号之间的片段）
	// **改动过** Raw。它回答的是"这次通过，是模型对了还是我们替它修了"。
	Repaired bool
}

// JSONAttemptObserver 接收每一次尝试。
//
// 用接口而不是回调字段，与 internal_hook.HookObserver 同一套理由：装配方可以
// 同时挂 trace 与 metrics 两个观察者，nil 观察者退化为零开销。
type JSONAttemptObserver interface {
	OnJSONAttempt(a JSONAttempt)
}

// JSONOptions 控制 CompleteJSON 的行为。零值即安全默认。
type JSONOptions struct {
	// MaxAttempts 是**模型调用总次数**（含第一次），<=0 取 defaultJSONMaxAttempts(2)。
	// 注意语义：2 表示"第一次 + 一次修复重试"，不是"两次重试"。
	MaxAttempts int
	// Model 指定本次调用用的模型。空 = 用网关装配期的模型。
	//
	// 非空时网关必须能按请求换模型（实现 ModelRouter，或就是默认的 *llmgw.Local）。
	// 做不到时**报错而不是忽略**：静默忽略会把"这次抽取用的是哪个模型"变成一句
	// 无法复核的谎话，而角色路由的收益恰好建立在"路由结果可观测"上。
	Model string
	// TurnID 只用于错误归类（harness.Error.TurnID），让业务层把失败对回某一轮。
	// 这里显式放在 options 里而不是从 ctx 里捞：ctx 键是各自包的私有约定，
	// harness 去猜一个键名等于制造第二套真相源。
	TurnID string
	// MaxTokens 是单次调用输出上限，<=0 取 api.SimpleCompleteMaxTokens。
	//
	// 复用那个常量而不是写一个小值：历史上 4096/16384 都让抽取出的 JSON 被截断在
	// 半途（见 pkg/api.SimpleComplete 的演进记录），两处各写一份上限，漂移的表现
	// 就是"同一段 prompt 在 Simple 路径完整、在结构化路径被截断"。
	MaxTokens int
	// Observer 逐次接收尝试记录。可为 nil。
	Observer JSONAttemptObserver
}

// ModelRouter 是网关的**可选**能力：返回一个绑定到指定模型的网关视图。
//
// 为什么需要它：llmgw.ChatRequest 没有 Model 字段（模型属于网关装配期配置），
// 而 R-H8 要求调用方能按角色指定模型。把这个能力做成可选接口，存量网关一行不改
// 也能继续工作；想支持按请求选模型的实现只需加一个方法。
type ModelRouter interface {
	WithModel(model string) llmgw.LLMGateway
}

// CompleteJSON 让模型产出一段 JSON 并校验，校验通过后才写入 out。
//
// 契约：
//   - 返回的 types.Usage 是**全部尝试**用量的加和（失败调用同样花钱）；
//   - out 只在某次尝试通过 schema 校验后才被写入，失败时保持调用前的值不变；
//   - 重试遇到 ctx 取消时立刻返回取消错误，而不是把"取消"包装成校验失败；
//   - 网关本身的传输错误不重试（重试归网关，见 pkg/llmgw/remote.go 的"重试单层化"）。
func CompleteJSON(ctx context.Context, gw llmgw.LLMGateway, system, user string, schema json.RawMessage, out any, opts JSONOptions) (types.Usage, error) {
	var usage types.Usage
	if gw == nil {
		return usage, NewError(ErrKindInternal, opts.TurnID, fmt.Errorf("CompleteJSON: 网关为 nil"))
	}
	if err := joCheckOutPointer(out); err != nil {
		return usage, NewError(ErrKindInternal, opts.TurnID, err)
	}
	// schema 的问题必须在**发出任何请求之前**暴露：拿一个校验器不认识的 schema
	// 去跑模型，结果只能是"每次都失败"或更糟的"每次都通过"，两者都白烧钱。
	if err := joCheckSchemaSupported(schema); err != nil {
		return usage, NewError(ErrKindInternal, opts.TurnID, fmt.Errorf("CompleteJSON: %w", err))
	}
	routed, err := joResolveModel(gw, opts.Model)
	if err != nil {
		return usage, NewError(ErrKindModel, opts.TurnID, err)
	}
	gw = routed

	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultJSONMaxAttempts
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = api.SimpleCompleteMaxTokens
	}

	prompt := user
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// 取消优先于一切：上一轮进入重试前已经取消，就不该再发一次模型调用。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return usage, NewError(ErrKindCanceled, opts.TurnID, ctxErr)
		}

		resp, callErr := gw.Complete(ctx, llmgw.ChatRequest{
			Messages:  []types.APIMessage{joUserMessage(prompt)},
			System:    []string{system},
			MaxTokens: maxTokens,
		})
		// usage 先记账再判成败：失败的调用也真的消耗了 token。
		if resp != nil && resp.Usage != nil {
			usage.InputTokens += resp.Usage.InputTokens
			usage.OutputTokens += resp.Usage.OutputTokens
			usage.CacheReadInputTokens += resp.Usage.CacheReadInputTokens
			usage.CacheCreationInputTokens += resp.Usage.CacheCreationInputTokens
		}
		if callErr != nil {
			// "调用失败"与"调用被取消"必须分开：前者是模型侧故障，后者是调用方
			// 主动收手；把取消报成模型错误会让上层以为可以重试。
			if ctxErr := ctx.Err(); ctxErr != nil {
				return usage, NewError(ErrKindCanceled, opts.TurnID, ctxErr)
			}
			return usage, NewError(ErrKindModel, opts.TurnID,
				fmt.Errorf("CompleteJSON: 第 %d 次模型调用失败: %w", attempt, callErr))
		}

		raw := joResponseText(resp)
		repaired, didRepair := joRepairJSON(raw)
		verr := joValidateDocument([]byte(repaired), schema)

		att := JSONAttempt{
			Attempt:  attempt,
			Raw:      raw,
			Valid:    verr == nil,
			Repaired: didRepair,
		}
		if verr != nil {
			att.ValidationErr = verr.Error()
			lastErr = verr
		}
		if opts.Observer != nil {
			opts.Observer.OnJSONAttempt(att)
		}

		if att.Valid {
			// 只有走到这里才碰 out。失败路径上一行都不能写：业务层拿到一个"字段
			// 填了一半、其余是零值"的结构，再写进库就是一条看起来合法的脏记录。
			if err := joAssignJSON(out, []byte(repaired)); err != nil {
				return usage, NewError(ErrKindInternal, opts.TurnID,
					fmt.Errorf("CompleteJSON: schema 已通过但无法写入 out (%T): %w", out, err))
			}
			return usage, nil
		}

		if attempt < maxAttempts {
			prompt = joRepairPrompt(user, raw, att.ValidationErr)
		}
	}

	// 最后一轮之后仍可能刚被取消：此时返回取消而不是校验错误，保证"取消"这个
	// 事实不被最后一轮的 schema 失败掩盖。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return usage, NewError(ErrKindCanceled, opts.TurnID, ctxErr)
	}
	return usage, NewError(ErrKindModel, opts.TurnID,
		fmt.Errorf("CompleteJSON: %d 次尝试均未通过 JSON Schema 校验: %v", maxAttempts, lastErr))
}

// ============================================================================
// 网关调用细节
// ============================================================================

// joResolveModel 按 opts.Model 换用模型视图；不需要换模型时原样返回。
func joResolveModel(gw llmgw.LLMGateway, model string) (llmgw.LLMGateway, error) {
	if model == "" {
		return gw, nil
	}
	if r, ok := gw.(ModelRouter); ok {
		routed := r.WithModel(model)
		if routed == nil {
			return nil, fmt.Errorf("CompleteJSON: 网关 %T 的 WithModel(%q) 返回 nil", gw, model)
		}
		return routed, nil
	}
	// 默认实现 *llmgw.Local 的模型挂在 api.Client 上。这里补一个桥接而不是
	// 去改 llmgw（那是别人的文件，且 Local 之外的实现也未必有同样的字段）：
	// 不补的话，"指定模型"这个选项在默认部署形态下永远不可用。
	if local, ok := gw.(*llmgw.Local); ok {
		if local == nil || local.Client == nil {
			return nil, fmt.Errorf("CompleteJSON: *llmgw.Local 的 Client 为 nil，无法按请求选模型 %q", model)
		}
		return llmgw.NewLocal(local.Client.WithModel(model)), nil
	}
	return nil, fmt.Errorf("CompleteJSON: 网关 %T 不支持按请求选模型 (opts.Model=%q)；请让它实现 harness.ModelRouter，或在装配期把模型配在网关自身上", gw, model)
}

// joUserMessage 构造单条 user 文本消息（content 用 Anthropic 的块数组形态）。
func joUserMessage(text string) types.APIMessage {
	content, _ := json.Marshal([]types.ContentBlock{{Type: types.ContentBlockText, Text: text}})
	return types.APIMessage{Role: "user", Content: content}
}

// joResponseText 拼接响应里的文本块。工具块/思考块不参与结构化输出。
func joResponseText(resp *types.APIResponse) string {
	if resp == nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range resp.Content {
		if b.Type == types.ContentBlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// joRepairPrompt 构造修复重试的 user 提示：原任务 + 校验错误 + 有界的原文回显。
func joRepairPrompt(user, prevRaw, validationErr string) string {
	excerpt, _ := TruncateResult(prevRaw, maxRepairEchoBytes)
	return user + "\n\n---\nYour previous output did not pass JSON Schema validation.\n" +
		"Validation error: " + validationErr + "\n" +
		"Previous output (possibly truncated):\n" + excerpt + "\n" +
		"Return ONLY the corrected JSON value itself: no Markdown code fence, no explanation."
}

// ============================================================================
// out 写入：通过校验才落值
// ============================================================================

// joCheckOutPointer 在发请求前确认 out 是可写指针。
func joCheckOutPointer(out any) error {
	if out == nil {
		return fmt.Errorf("CompleteJSON: out 为 nil")
	}
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Ptr {
		return fmt.Errorf("CompleteJSON: out 必须是指针，实际是 %T", out)
	}
	if rv.IsNil() {
		return fmt.Errorf("CompleteJSON: out 是 nil 指针 (%T)", out)
	}
	return nil
}

// joAssignJSON 先把 JSON 反序列化到同类型的临时值，成功后才整体赋给 out。
//
// 为什么不直接 json.Unmarshal(raw, out)：目标结构是多个字段的复合体，
// 解码器在遇到后面的字段出错时，**前面的字段已经被写进 out 了**。这就是
// "半条记录"的来源——out 看起来有值，其实只有一半。用临时值 + 整体 Set
// 把赋值变成要么全有、要么全无。
func joAssignJSON(out any, raw []byte) error {
	rv := reflect.ValueOf(out)
	tmp := reflect.New(rv.Elem().Type())
	if err := json.Unmarshal(raw, tmp.Interface()); err != nil {
		return err
	}
	rv.Elem().Set(tmp.Elem())
	return nil
}

// ============================================================================
// 极简 JSON Schema 校验器（stdlib-only 子集）
//
// 只实现 type / required / properties / items / enum 五个关键字。
// 与 internal_hook.ValidateAgainstSchema 的关键差别是**失败方向**：那边服务于
// 工具调用（schema 解析不了就放行，避免误杀），这边服务于落库前的守门，
// 任何"不认识"都必须报错。
//
// 为什么"不认识的关键字"必须报错：静默忽略一个校验关键字（比如 minLength、
// pattern、additionalProperties:false）会让一份**本应非法**的输出被判成合法，
// 而调用方看到的是"校验通过"，于是脏数据带着合格标记进入下游。校验器的
// 沉默比模型的错误更危险——模型的错还能在 Raw 里看见，校验器的错看不见。
//
// 纯注释类关键字（title/description/default/examples/$schema/$comment/$id）
// 刻意放行：它们不施加任何约束，忽略它们不可能把非法文档判成合法。区分
// "断言型"与"注释型"是这里唯一需要人判断的地方。
// ============================================================================

// supportedSchemaKeywords 是本校验器真正实现的**断言型**关键字。
var supportedSchemaKeywords = map[string]struct{}{
	"type":       {},
	"required":   {},
	"properties": {},
	"items":      {},
	"enum":       {},
}

// annotationSchemaKeywords 不施加约束、可以安全忽略的注释型关键字。
var annotationSchemaKeywords = map[string]struct{}{
	"$schema":     {},
	"$id":         {},
	"$comment":    {},
	"title":       {},
	"description": {},
	"default":     {},
	"examples":    {},
}

// supportedJSONTypes 是允许出现在 type 关键字里的类型名。
var supportedJSONTypes = map[string]struct{}{
	"string":  {},
	"number":  {},
	"integer": {},
	"boolean": {},
	"object":  {},
	"array":   {},
	"null":    {},
}

// joCheckSchemaSupported 在调用模型前校验 schema 本身：语法、关键字白名单、
// 各关键字的形状。任何一处不合规都报错，绝不带着"半懂"的 schema 去跑模型。
func joCheckSchemaSupported(schema json.RawMessage) error {
	if len(schema) == 0 {
		return fmt.Errorf("schema 为空")
	}
	var root interface{}
	if err := json.Unmarshal(schema, &root); err != nil {
		return fmt.Errorf("schema 不是合法 JSON: %w", err)
	}
	return joCheckSchemaNode(root, "$")
}

// joCheckSchemaNode 递归检查一个 schema 节点。
//
// 关键不变量：即使某个 property 在当前数据里没出现，也要检查它的子 schema。
// 只在"遇到值"时顺带检查的话，藏在可选分支里的非法关键字会一直不被发现，
// 直到某天该字段真的出现——那时错误现场离根因已经隔了一次线上事故。
func joCheckSchemaNode(node interface{}, path string) error {
	obj, ok := node.(map[string]interface{})
	if !ok {
		return fmt.Errorf("schema %s 必须是对象（布尔 schema 与 tuple items 不支持）", path)
	}
	for _, k := range joSortedKeys(obj) {
		if _, ok := supportedSchemaKeywords[k]; ok {
			continue
		}
		if _, ok := annotationSchemaKeywords[k]; ok {
			continue
		}
		return fmt.Errorf("schema %s 使用了不支持的校验关键字 %q；本校验器只实现 type/required/properties/items/enum，多出来的关键字若被静默忽略，非法输出就会以“校验通过”的身份落库", path, k)
	}
	if t, ok := obj["type"]; ok {
		if err := joCheckTypeKeyword(t, path); err != nil {
			return err
		}
	}
	if req, ok := obj["required"]; ok {
		arr, ok := req.([]interface{})
		if !ok {
			return fmt.Errorf("schema %s 的 required 必须是字符串数组", path)
		}
		for _, v := range arr {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("schema %s 的 required 含非字符串项", path)
			}
		}
	}
	if enumRaw, ok := obj["enum"]; ok {
		arr, ok := enumRaw.([]interface{})
		if !ok || len(arr) == 0 {
			return fmt.Errorf("schema %s 的 enum 必须是非空数组", path)
		}
	}
	if propsRaw, ok := obj["properties"]; ok {
		props, ok := propsRaw.(map[string]interface{})
		if !ok {
			return fmt.Errorf("schema %s 的 properties 必须是对象", path)
		}
		for _, k := range joSortedKeys(props) {
			if err := joCheckSchemaNode(props[k], path+".properties."+k); err != nil {
				return err
			}
		}
	}
	if items, ok := obj["items"]; ok {
		if err := joCheckSchemaNode(items, path+".items"); err != nil {
			return err
		}
	}
	return nil
}

// joCheckTypeKeyword 校验 type 关键字的写法与取值。
//
// 拼错的类型名（"strng"）必须在入口报错：joMatchesType 对未知类型只会返回
// false，表现为"模型怎么答都不过"——那是极难回溯的现场。
func joCheckTypeKeyword(t interface{}, path string) error {
	check := func(name interface{}) error {
		s, ok := name.(string)
		if !ok {
			return fmt.Errorf("schema %s 的 type 含非字符串项", path)
		}
		if _, ok := supportedJSONTypes[s]; !ok {
			return fmt.Errorf("schema %s 的 type 取值 %q 不受支持", path, s)
		}
		return nil
	}
	switch v := t.(type) {
	case string:
		return check(v)
	case []interface{}:
		if len(v) == 0 {
			return fmt.Errorf("schema %s 的 type 数组为空", path)
		}
		for _, name := range v {
			if err := check(name); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("schema %s 的 type 必须是字符串或字符串数组", path)
	}
}

// joValidateDocument 解析并校验模型输出；返回 nil 表示通过。
func joValidateDocument(raw []byte, schema json.RawMessage) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return fmt.Errorf("模型输出为空")
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("输出不是合法 JSON: %w", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(schema, &root); err != nil {
		// 入口已检查过，走到这里是调用期间 schema 被改坏，按失败处理。
		return fmt.Errorf("schema 不可解析: %w", err)
	}
	return joValidateNode(v, root, "$")
}

// joValidateNode 递归校验：先 enum（与结构无关），再 type，再按值的实际形态下钻。
//
// properties 只校验**出现**的字段；缺失由 required 负责。顺序上先 enum 后 type，
// 是因为枚举越界比类型错误的提示更具体（"期望 a|b|c" 直接给出可选项）。
// 键排序后再遍历：否则同一份输入每次报出的"第一条错误"不同，重试提示不可复现。
func joValidateNode(v interface{}, sch map[string]interface{}, path string) error {
	if enumRaw, ok := sch["enum"]; ok {
		if enum, ok := enumRaw.([]interface{}); ok {
			hit := false
			for _, e := range enum {
				if reflect.DeepEqual(e, v) {
					hit = true
					break
				}
			}
			if !hit {
				return fmt.Errorf("%s 的值 %s 不在 enum 允许集合内", path, joPreview(v))
			}
		}
	}

	if typeRaw, ok := sch["type"]; ok {
		if !joMatchesTypeKeyword(v, typeRaw) {
			return fmt.Errorf("%s 类型错误: 期望 %s, 实际 %s", path, joTypeName(typeRaw), joJSONTypeOf(v))
		}
	}

	switch tv := v.(type) {
	case map[string]interface{}:
		if reqRaw, ok := sch["required"]; ok {
			if req, ok := reqRaw.([]interface{}); ok {
				for _, r := range req {
					name, _ := r.(string)
					if _, present := tv[name]; !present {
						return fmt.Errorf("%s 缺少必填字段 %q", path, name)
					}
				}
			}
		}
		if propsRaw, ok := sch["properties"]; ok {
			if props, ok := propsRaw.(map[string]interface{}); ok {
				for _, k := range joSortedKeys(props) {
					sub, ok := props[k].(map[string]interface{})
					if !ok {
						continue
					}
					if fv, present := tv[k]; present {
						if err := joValidateNode(fv, sub, path+"."+k); err != nil {
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
					if err := joValidateNode(el, items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// joMatchesTypeKeyword 判定值是否满足 type（单值或联合类型数组，任一命中即过）。
func joMatchesTypeKeyword(v interface{}, typeRaw interface{}) bool {
	switch t := typeRaw.(type) {
	case string:
		return joMatchesType(v, t)
	case []interface{}:
		for _, name := range t {
			if s, ok := name.(string); ok && joMatchesType(v, s) {
				return true
			}
		}
		return false
	}
	return false
}

// joMatchesType 实现单个类型名的匹配。
func joMatchesType(v interface{}, t string) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]interface{})
		return ok
	case "array":
		_, ok := v.([]interface{})
		return ok
	case "null":
		return v == nil
	}
	return false
}

// joTypeName 把 type 关键字渲染成可读形式（进错误信息）。
func joTypeName(typeRaw interface{}) string {
	if s, ok := typeRaw.(string); ok {
		return s
	}
	if arr, ok := typeRaw.([]interface{}); ok {
		parts := make([]string, 0, len(arr))
		for _, v := range arr {
			if s, ok := v.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "|")
	}
	return "?"
}

// joJSONTypeOf 返回值的实际 JSON 类型名（进错误信息）。
func joJSONTypeOf(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	}
	return fmt.Sprintf("%T", v)
}

// joPreview 截断并格式化一个值，避免超长输出把错误信息撑爆。
func joPreview(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s, _ := TruncateResult(string(b), 80)
	return s
}

// joSortedKeys 返回对象的键（已排序），保证遍历与错误顺序确定。
func joSortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ============================================================================
// 本地修复
// ============================================================================

// joRepairJSON 对模型原始输出做**确定性**的本地修复，返回修复后的文本以及是否改动过。
//
// 只做两类不会改变语义的修复：
//  1. 去掉 Markdown 代码围栏（```json ... ```）——最常见的形态；
//  2. 从第一个 { / [ 截到最后一个 } / ]——应对"好的，结果如下：{...}"这类前后缀。
//
// 刻意不做的：补逗号、补引号、截断后再闭合。那些会**发明**模型没说的内容，
// 而结构化输出的价值恰恰在于"落库的是模型说的东西"。宁可这一轮失败、把校验
// 错误回注给模型重来，也不替它猜。
func joRepairJSON(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	repaired := false

	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
		} else {
			s = strings.TrimPrefix(s, "```")
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
		repaired = true
	}

	if json.Valid([]byte(s)) {
		return s, repaired
	}

	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '{' || s[i] == '[' {
			start = i
			break
		}
	}
	end := strings.LastIndexAny(s, "}]")
	if start >= 0 && end > start {
		candidate := strings.TrimSpace(s[start : end+1])
		if json.Valid([]byte(candidate)) {
			return candidate, true
		}
	}
	return s, repaired
}
