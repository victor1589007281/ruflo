package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/anthropic/claude-go/pkg/llmgw"
	"github.com/anthropic/claude-go/pkg/types"
)

// ============================================================================
// 假网关：按脚本依次返回文本/错误，并记录每次调用
// ============================================================================

// scriptedGateway 是 llmgw.LLMGateway 的脚本化假实现。
//
// 必须实现全部 5 个方法（Stream/Complete/Simple/Raw/Diag）——只实现用到的那一个
// 连编译都过不了。未用的四个如实返回 "not implemented"，而不是 nil：
// 若被测代码真去调了它们，测试应该炸出来，而不是拿到一个零值继续跑。
type scriptedGateway struct {
	mu sync.Mutex

	// outs 是每次 Complete 返回的文本；不足时重复最后一个。
	outs []string
	// errs 是每次 Complete 返回的错误；不足时补 nil。
	errs []error
	// usages 是每次 Complete 附带的 usage；不足时补 nil。
	usages []*types.Usage

	calls int
	// models 记录每次调用时网关自身的模型标识（ModelRouter 用）。
	model string
	// routed 非空时 Complete 记录 enqueued 的模型到 here。
	onComplete func(ctx context.Context) error
}

func (g *scriptedGateway) Complete(ctx context.Context, req llmgw.ChatRequest) (*types.APIResponse, error) {
	g.mu.Lock()
	idx := g.calls
	g.calls++
	text := ""
	if len(g.outs) > 0 {
		text = g.outs[minInt(idx, len(g.outs)-1)]
	}
	var callErr error
	if idx < len(g.errs) {
		callErr = g.errs[idx]
	}
	var u *types.Usage
	if idx < len(g.usages) {
		u = g.usages[idx]
	}
	hook := g.onComplete
	g.mu.Unlock()

	if hook != nil {
		if err := hook(ctx); err != nil {
			return nil, err
		}
	}
	if callErr != nil {
		return nil, callErr
	}
	return &types.APIResponse{
		Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: text}},
		Usage:   u,
	}, nil
}

func (g *scriptedGateway) Stream(context.Context, llmgw.ChatRequest) (<-chan types.StreamDelta, <-chan error) {
	return nil, nil
}

func (g *scriptedGateway) Simple(context.Context, string, string) (string, error) {
	return "", errors.New("scriptedGateway: Simple 未实现")
}

func (g *scriptedGateway) Raw(context.Context, json.RawMessage, int) (string, error) {
	return "", errors.New("scriptedGateway: Raw 未实现")
}

func (g *scriptedGateway) Diag(context.Context, string, string) (string, string, error) {
	return "", "", errors.New("scriptedGateway: Diag 未实现")
}

func (g *scriptedGateway) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// modelRecordingGateway 记录 Complete 时被路由到的模型。
//
// seen 用指针共享：WithModel 返回的是**另一份**网关，若各自持有一个 slice，
// 断言读到的会是原对象上那个永远为空的切片——测试于是恒过，而路由其实没生效。
type modelRecordingGateway struct {
	scriptedGateway
	seen *[]string
}

func (g *modelRecordingGateway) Complete(ctx context.Context, req llmgw.ChatRequest) (*types.APIResponse, error) {
	*g.seen = append(*g.seen, g.model)
	return g.scriptedGateway.Complete(ctx, req)
}

func (g *modelRecordingGateway) WithModel(model string) llmgw.LLMGateway {
	cp := &modelRecordingGateway{seen: g.seen}
	cp.model = model
	cp.outs = g.outs
	cp.errs = g.errs
	cp.usages = g.usages
	return cp
}

// recordingObserver 收集全部尝试记录。
type recordingObserver struct {
	mu       sync.Mutex
	attempts []JSONAttempt
}

func (o *recordingObserver) OnJSONAttempt(a JSONAttempt) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.attempts = append(o.attempts, a)
}

func (o *recordingObserver) all() []JSONAttempt {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]JSONAttempt(nil), o.attempts...)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ============================================================================
// 测试用 schema 与目标结构
// ============================================================================

const personSchema = `{
  "type": "object",
  "required": ["name", "tags", "kind"],
  "properties": {
    "name": {"type": "string"},
    "age": {"type": "integer"},
    "tags": {"type": "array", "items": {"type": "string"}},
    "kind": {"type": "string", "enum": ["friend", "family"]}
  }
}`

type personOut struct {
	Name string   `json:"name"`
	Age  int      `json:"age"`
	Tags []string `json:"tags"`
	Kind string   `json:"kind"`
}

const validPerson = `{"name":"小明","tags":["同桌"],"kind":"friend"}`

func validUsage(in, out int) *types.Usage {
	return &types.Usage{InputTokens: in, OutputTokens: out}
}

// ============================================================================
// CompleteJSON
// ============================================================================

// TestCompleteJSON_首次即合法 覆盖 happy path：一次调用、写入 out、usage 原样透传。
func TestCompleteJSON_首次即合法(t *testing.T) {
	gw := &scriptedGateway{
		outs:   []string{validPerson},
		usages: []*types.Usage{validUsage(11, 7)},
	}
	var out personOut
	obs := &recordingObserver{}

	usage, err := CompleteJSON(context.Background(), gw, "sys", "user", json.RawMessage(personSchema), &out,
		JSONOptions{Observer: obs})
	if err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if gw.callCount() != 1 {
		t.Errorf("调用次数 = %d, 期望 1", gw.callCount())
	}
	if out.Name != "小明" || out.Kind != "friend" || len(out.Tags) != 1 {
		t.Errorf("out 未正确写入: %+v", out)
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, 期望 11/7", usage)
	}
	atts := obs.all()
	if len(atts) != 1 || !atts[0].Valid || atts[0].Attempt != 1 || atts[0].Repaired {
		t.Errorf("尝试记录不符: %+v", atts)
	}
}

// TestCompleteJSON_Markdown围栏被修复 覆盖最常见的“JSON 裹在 ``` 里”。
// 修复属于本地确定性动作，必须记 Repaired=true——审计要能区分“模型对了”和
// “我们替它修了”。
func TestCompleteJSON_Markdown围栏被修复(t *testing.T) {
	gw := &scriptedGateway{outs: []string{"```json\n" + validPerson + "\n```"}}
	var out personOut
	obs := &recordingObserver{}

	if _, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), &out,
		JSONOptions{Observer: obs}); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if out.Name != "小明" {
		t.Errorf("围栏未修复, out=%+v", out)
	}
	atts := obs.all()
	if len(atts) != 1 || !atts[0].Repaired || !atts[0].Valid {
		t.Errorf("应记录一次 Repaired=true 的通过尝试: %+v", atts)
	}
	if !strings.HasPrefix(atts[0].Raw, "```") {
		t.Errorf("Raw 必须是模型原文(含围栏), 实际 %q", atts[0].Raw)
	}
}

// TestCompleteJSON_第一次非法第二次合法重试成功 是 R-H8 的验收原话。
// 同时验证 usage 是两次之和——失败那次也花了钱。
func TestCompleteJSON_第一次非法第二次合法重试成功(t *testing.T) {
	gw := &scriptedGateway{
		outs: []string{
			`{"name":"小明","tags":"不是数组","kind":"friend"}`, // tags 类型错
			validPerson,
		},
		usages: []*types.Usage{validUsage(10, 5), validUsage(12, 6)},
	}
	var out personOut
	obs := &recordingObserver{}

	usage, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), &out,
		JSONOptions{Observer: obs})
	if err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if gw.callCount() != 2 {
		t.Errorf("调用次数 = %d, 期望 2", gw.callCount())
	}
	if usage.InputTokens != 22 || usage.OutputTokens != 11 {
		t.Errorf("usage 未跨尝试求和: %+v", usage)
	}
	atts := obs.all()
	if len(atts) != 2 {
		t.Fatalf("尝试记录 = %d, 期望 2", len(atts))
	}
	if atts[0].Valid || atts[0].ValidationErr == "" {
		t.Errorf("第一次应记录失败原因: %+v", atts[0])
	}
	if !strings.Contains(atts[0].ValidationErr, "$.tags") {
		t.Errorf("校验错误应带路径 $, 实际 %q", atts[0].ValidationErr)
	}
	if !atts[1].Valid {
		t.Errorf("第二次应通过: %+v", atts[1])
	}
	if out.Tags == nil || len(out.Tags) != 1 {
		t.Errorf("out 未写入第二次结果: %+v", out)
	}
}

// TestCompleteJSON_全部失败返回typedError且不写out 是“不允许半条记录”的核心。
func TestCompleteJSON_全部失败返回typedError且不写out(t *testing.T) {
	gw := &scriptedGateway{
		outs:   []string{`{"name":"小明"}`, `not json at all`},
		usages: []*types.Usage{validUsage(3, 2), validUsage(4, 1)},
	}
	// 预置哨兵值：失败后必须逐字段保持原样。
	out := personOut{Name: "sentinel", Age: 42, Tags: []string{"keep"}, Kind: "family"}

	usage, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), &out,
		JSONOptions{})
	if err == nil {
		t.Fatal("期望报错, 实际成功")
	}
	var herr *Error
	if !errors.As(err, &herr) {
		t.Fatalf("错误不是 *harness.Error: %T", err)
	}
	if herr.Kind != ErrKindModel {
		t.Errorf("Kind = %q, 期望 %q", herr.Kind, ErrKindModel)
	}
	if out.Name != "sentinel" || out.Age != 42 || out.Kind != "family" || len(out.Tags) != 1 || out.Tags[0] != "keep" {
		t.Errorf("失败路径改动了 out（半条记录）: %+v", out)
	}
	if usage.InputTokens != 7 || usage.OutputTokens != 3 {
		t.Errorf("失败尝试的 usage 也要记账: %+v", usage)
	}
}

// TestCompleteJSON_默认两次调用 锁死 MaxAttempts 的语义：默认值 2 = 总调用次数，
// 不是“两次重试”。这个差一必须在测试里钉住，否则实现与验收会各读一套。
func TestCompleteJSON_默认两次调用(t *testing.T) {
	gw := &scriptedGateway{outs: []string{`{}`}}
	_, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), new(personOut),
		JSONOptions{})
	if err == nil {
		t.Fatal("期望报错")
	}
	if gw.callCount() != defaultJSONMaxAttempts {
		t.Errorf("默认调用次数 = %d, 期望 %d", gw.callCount(), defaultJSONMaxAttempts)
	}
}

// TestCompleteJSON_MaxAttempts倍率 验证显式值同样是总次数。
func TestCompleteJSON_MaxAttempts是总次数(t *testing.T) {
	gw := &scriptedGateway{outs: []string{`{}`}}
	_, _ = CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), new(personOut),
		JSONOptions{MaxAttempts: 4})
	if gw.callCount() != 4 {
		t.Errorf("调用次数 = %d, 期望 4", gw.callCount())
	}
}

// TestCompleteJSON_重试前ctx取消立刻返回取消 而不是校验错误。
// 取消是调用方的主动决定，不能伪装成“模型输出不合法”。
func TestCompleteJSON_重试前ctx取消立刻返回取消(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gw := &scriptedGateway{outs: []string{`{}`}}
	var out personOut
	obs := &cancelingObserver{cancel: cancel}

	_, err := CompleteJSON(ctx, gw, "s", "u", json.RawMessage(personSchema), &out,
		JSONOptions{MaxAttempts: 5, Observer: obs})
	if err == nil {
		t.Fatal("期望返回取消错误")
	}
	var herr *Error
	if !errors.As(err, &herr) || herr.Kind != ErrKindCanceled {
		t.Fatalf("Kind 应为 canceled, 实际 %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("应可 errors.Is(context.Canceled), 实际 %v", err)
	}
	// 第一次尝试已经跑完；取消发生在它之后，因此只应有 1 次调用。
	if gw.callCount() != 1 {
		t.Errorf("取消后仍发起了调用: 次数 = %d", gw.callCount())
	}
	if out.Name != "" {
		t.Errorf("取消路径不应写 out: %+v", out)
	}
}

// cancelingObserver 在第一次尝试后取消 ctx。
type cancelingObserver struct{ cancel context.CancelFunc }

func (o *cancelingObserver) OnJSONAttempt(JSONAttempt) { o.cancel() }

// TestCompleteJSON_调用前已取消 不发起任何请求。
func TestCompleteJSON_调用前已取消(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gw := &scriptedGateway{outs: []string{validPerson}}
	_, err := CompleteJSON(ctx, gw, "s", "u", json.RawMessage(personSchema), new(personOut), JSONOptions{})
	var herr *Error
	if !errors.As(err, &herr) || herr.Kind != ErrKindCanceled {
		t.Fatalf("期望 canceled, 实际 %v", err)
	}
	if gw.callCount() != 0 {
		t.Errorf("已取消仍发起 %d 次调用", gw.callCount())
	}
}

// TestCompleteJSON_网关传输错误不重试 验证“重试单层化”：模型侧故障直接上报，
// 由网关自己做重试/熔断，harness 不再叠一层。
func TestCompleteJSON_网关传输错误不重试(t *testing.T) {
	gw := &scriptedGateway{
		outs: []string{validPerson, validPerson},
		errs: []error{errors.New("upstream 500")},
	}
	_, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), new(personOut),
		JSONOptions{MaxAttempts: 3})
	if err == nil {
		t.Fatal("期望报错")
	}
	var herr *Error
	if !errors.As(err, &herr) || herr.Kind != ErrKindModel {
		t.Fatalf("Kind 应为 model, 实际 %v", err)
	}
	if gw.callCount() != 1 {
		t.Errorf("传输错误后发起了 %d 次调用, 期望 1", gw.callCount())
	}
}

// TestCompleteJSON_不支持的schema关键字在发请求前报错。
// 校验器静默忽略关键字 = 把非法输出判成合法，因此必须 fail-loud。
func TestCompleteJSON_不支持的schema关键字在发请求前报错(t *testing.T) {
	schema := json.RawMessage(`{
      "type": "object",
      "properties": {"name": {"type": "string", "minLength": 3}}
    }`)
	gw := &scriptedGateway{outs: []string{validPerson}}
	_, err := CompleteJSON(context.Background(), gw, "s", "u", schema, new(personOut), JSONOptions{})
	if err == nil {
		t.Fatal("期望因 minLength 报错")
	}
	if !strings.Contains(err.Error(), "minLength") {
		t.Errorf("错误应点名不支持的关键字, 实际 %v", err)
	}
	if gw.callCount() != 0 {
		t.Errorf("schema 不合规仍发起了 %d 次模型调用", gw.callCount())
	}
}

// TestCompleteJSON_注释型关键字可忽略 反证上一条不是“所有未知都拒绝”。
func TestCompleteJSON_注释型关键字可忽略(t *testing.T) {
	schema := json.RawMessage(`{
      "type": "object",
      "title": "person",
      "properties": {
        "name": {"type": "string", "description": "名字"},
        "tags": {"type": "array", "items": {"type": "string"}, "default": []},
        "kind": {"type": "string", "enum": ["friend", "family"], "examples": ["friend"]}
      },
      "required": ["name", "tags", "kind"]
    }`)
	gw := &scriptedGateway{outs: []string{validPerson}}
	if _, err := CompleteJSON(context.Background(), gw, "s", "u", schema, new(personOut), JSONOptions{}); err != nil {
		t.Fatalf("注释型关键字不应报错: %v", err)
	}
}

// TestCompleteJSON_类型枚举required数组逐项生效 覆盖校验子集的每个关键字。
func TestCompleteJSON_类型枚举required数组逐项生效(t *testing.T) {
	cases := []struct {
		name   string
		doc    string
		substr string
	}{
		{"缺必填", `{"name":"a","tags":[]}`, "缺少必填字段"},
		{"类型错", `{"name":1,"tags":[],"kind":"friend"}`, "$.name"},
		{"枚举越界", `{"name":"a","tags":[],"kind":"enemy"}`, "enum"},
		{"数组元素类型错", `{"name":"a","tags":[1],"kind":"friend"}`, "$.tags[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &scriptedGateway{outs: []string{tc.doc}}
			obs := &recordingObserver{}
			_, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), new(personOut),
				JSONOptions{MaxAttempts: 1, Observer: obs})
			if err == nil {
				t.Fatalf("期望校验失败")
			}
			atts := obs.all()
			if len(atts) != 1 || atts[0].Valid {
				t.Fatalf("应记录一次失败尝试: %+v", atts)
			}
			if !strings.Contains(atts[0].ValidationErr, tc.substr) {
				t.Errorf("校验错误应含 %q, 实际 %q", tc.substr, atts[0].ValidationErr)
			}
		})
	}
}

// TestCompleteJSON_整数类型不接受小数。
func TestCompleteJSON_整数类型不接受小数(t *testing.T) {
	gw := &scriptedGateway{outs: []string{`{"name":"a","age":1.5,"tags":[],"kind":"friend"}`}}
	obs := &recordingObserver{}
	_, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), new(personOut),
		JSONOptions{MaxAttempts: 1, Observer: obs})
	if err == nil {
		t.Fatal("1.5 不应满足 integer")
	}
	if got := obs.all()[0].ValidationErr; !strings.Contains(got, "$.age") {
		t.Errorf("错误应指向 $.age, 实际 %q", got)
	}
}

// TestCompleteJSON_out必须是非nil指针 在发请求前拦住。
func TestCompleteJSON_out必须是非nil指针(t *testing.T) {
	gw := &scriptedGateway{outs: []string{validPerson}}
	if _, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), personOut{}, JSONOptions{}); err == nil {
		t.Error("非指针 out 应报错")
	}
	var nilPtr *personOut
	if _, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), nilPtr, JSONOptions{}); err == nil {
		t.Error("nil 指针 out 应报错")
	}
	if gw.callCount() != 0 {
		t.Errorf("out 非法仍发起了调用: %d", gw.callCount())
	}
}

// TestCompleteJSON_网关nil报错。
func TestCompleteJSON_网关nil报错(t *testing.T) {
	if _, err := CompleteJSON(context.Background(), nil, "s", "u", json.RawMessage(personSchema), new(personOut), JSONOptions{}); err == nil {
		t.Fatal("nil 网关应报错")
	}
}

// TestCompleteJSON_按请求选模型 验证 ModelRouter 路径真的换了模型。
func TestCompleteJSON_按请求选模型(t *testing.T) {
	var seen []string
	gw := &modelRecordingGateway{
		scriptedGateway: scriptedGateway{outs: []string{validPerson}},
		seen:            &seen,
	}
	_, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), new(personOut),
		JSONOptions{Model: "extract-model"})
	if err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if len(seen) != 1 || seen[0] != "extract-model" {
		t.Errorf("模型路由未生效: %v", seen)
	}
}

// TestCompleteJSON_不支持选模型的网关报错而不是忽略。
func TestCompleteJSON_不支持选模型的网关报错而不是忽略(t *testing.T) {
	gw := &scriptedGateway{outs: []string{validPerson}}
	_, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), new(personOut),
		JSONOptions{Model: "extract-model"})
	if err == nil {
		t.Fatal("网关不支持按请求选模型时应报错")
	}
	if !strings.Contains(err.Error(), "ModelRouter") {
		t.Errorf("错误应指出缺失能力, 实际 %v", err)
	}
	if gw.callCount() != 0 {
		t.Errorf("应无调用, 实际 %d", gw.callCount())
	}
}

// TestCompleteJSON_TurnID进错误 让业务层能把失败对回某一轮。
func TestCompleteJSON_TurnID进错误(t *testing.T) {
	gw := &scriptedGateway{outs: []string{`{}`}}
	_, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), new(personOut),
		JSONOptions{MaxAttempts: 1, TurnID: "turn-77"})
	var herr *Error
	if !errors.As(err, &herr) || herr.TurnID != "turn-77" {
		t.Fatalf("TurnID 未透传: %v", err)
	}
}

// TestCompleteJSON_修复不回显时语义不变：前后缀包裹的合法 JSON 能被取出。
func TestCompleteJSON_前后缀包裹的JSON被取出(t *testing.T) {
	gw := &scriptedGateway{outs: []string{"好的，结果如下：\n" + validPerson + "\n希望有帮助。"}}
	var out personOut
	obs := &recordingObserver{}
	if _, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), &out,
		JSONOptions{Observer: obs}); err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if out.Name != "小明" {
		t.Errorf("未取出 JSON: %+v", out)
	}
	if !obs.all()[0].Repaired {
		t.Error("截取动作应记 Repaired=true")
	}
}

// TestCompleteJSON_截断的JSON不被“补全” 反证 joRepairJSON 只做确定性动作。
// 模型输出被 max_tokens 截断在半途时，本地补括号等于发明内容 —— 必须失败。
func TestCompleteJSON_截断的JSON不被补全(t *testing.T) {
	truncated := `{"name":"小明","tags":["同桌"],"kind":"frie`
	gw := &scriptedGateway{outs: []string{truncated}}
	obs := &recordingObserver{}
	_, err := CompleteJSON(context.Background(), gw, "s", "u", json.RawMessage(personSchema), new(personOut),
		JSONOptions{MaxAttempts: 1, Observer: obs})
	if err == nil {
		t.Fatal("截断的 JSON 不应通过")
	}
	if atts := obs.all(); atts[0].Repaired {
		t.Error("不该把截断认作已修复")
	}
}
