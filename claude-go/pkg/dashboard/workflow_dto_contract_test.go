package dashboard

// workflow_dto_contract_test.go —— 工作流 DTO 与 claude-go 定义的**字段契约**。
//
// 为什么需要反射而不是逐字段手写断言: 这份 DTO 的失效形态是"agent.StageDef 新增了
// 一个字段, 设置页 GET 不到它, 用户看不到 → 保存时被当成未声明抹掉"。手工断言只能
// 保证"今天写了的没漏", 保证不了"明天新增的会红"。反射遍历 tag 才能让 CI 在
// 加字段而忘补 DTO 的那一刻失败。
//
// 允许的例外只有一处: StageDef.Agent 的 JSON 键是 agent, 但 DTO 的历史键是 role
// (前端一直按 role 读)。写方向不经过 DTO —— CREATE/UPDATE 直接解码
// agent.WorkflowDef, 所以 role 只影响读方向。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/graph"
)

// jsonKeyNames 收集类型的 JSON 键 → Go 字段名 (忽略 `json:"-"` 与无 tag 字段)。
func jsonKeyNames(t reflect.Type) map[string]string {
	out := make(map[string]string, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" {
			continue
		}
		key := strings.Split(tag, ",")[0]
		if key == "" || key == "-" {
			continue
		}
		out[key] = f.Name
	}
	return out
}

// assertDTOHasAllJSONKeys 断言 src 的每个 JSON 键都出现在 dto 中。
// missed 用于显式豁免 (键 → 理由), 冲突时打印理由, 避免"下次又有人绕过断言"。
func assertDTOHasAllJSONKeys(t *testing.T, src, dto reflect.Type, exempt map[string]string) {
	t.Helper()
	got := jsonKeyNames(dto)
	for i := 0; i < src.NumField(); i++ {
		f := src.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" {
			continue
		}
		key := strings.Split(tag, ",")[0]
		if key == "" || key == "-" {
			continue
		}
		if _, ok := got[key]; ok {
			continue
		}
		if reason, ok := exempt[key]; ok {
			t.Logf("豁免: %s.%s (json %q) 不在 %s 中: %s", src.Name(), f.Name, key, dto.Name(), reason)
			continue
		}
		t.Errorf("%s.%s 的 JSON 键 %q 未出现在 %s —— claude-go 新增字段后必须同步 DTO, 否则设置页读不到它, 保存时会把用户已有的声明当成未声明抹掉",
			src.Name(), f.Name, key, dto.Name())
	}
}

func TestWorkflowStageDTOContract(t *testing.T) {
	assertDTOHasAllJSONKeys(t, reflect.TypeOf(agent.StageDef{}), reflect.TypeOf(workflowStageDTO{}),
		map[string]string{
			"agent": "StageDef.Agent 在 DTO 里叫 Role (JSON 键 role); 写方向由 Create/Update 直接解码 agent.WorkflowDef, 不经过 DTO。",
		})

	// 例外必须真的有一条可读的替代键, 否则上面的豁免会变成"字段永久消失"的许可证。
	if _, ok := jsonKeyNames(reflect.TypeOf(workflowStageDTO{}))["role"]; !ok {
		t.Fatalf("workflowStageDTO 缺少 json:%q —— StageDef.Agent 的读方向依赖这个历史键", "role")
	}
}

func TestWorkflowDefDTOContract(t *testing.T) {
	assertDTOHasAllJSONKeys(t, reflect.TypeOf(agent.WorkflowDef{}), reflect.TypeOf(workflowDefDTO{}), nil)
}

// TestWorkflowDTOCopiesMutableGraphFields 锁住深拷贝契约。
//
// 只做浅拷贝时, 这里对 DTO 的每一次就地修改都会改到原定义; 而 toWorkflowDTO 的入参
// 常常是注册表里的那份指针。事故形态是"GET 一次详情就悄悄改坏工作流", 且没有任何
// 报错 —— 所以用测试把每个引用类型的隔离都钉住。
func TestWorkflowDTOCopiesMutableGraphFields(t *testing.T) {
	def := &agent.WorkflowDef{
		Name: "dto-deep-copy",
		Mode: "graph",
		Stages: []agent.StageDef{{
			Name:           "a",
			Agent:          "coder",
			DependsOn:      []string{"up"},
			Hooks:          []string{"pre:shell:echo"},
			Skills:         []string{"sk"},
			EdgeConditions: map[string]string{"up": "ok"},
			Retry:          &graph.RetryPolicy{MaxRetries: 2, BackoffSec: 3},
			Loop: &graph.LoopPolicy{
				MaxIterations: 3,
				Terminator: &graph.TerminatorSpec{
					Name:   "adaptive",
					Params: map[string]string{"k": "v"},
				},
			},
			Placement: &graph.PlacementSpec{Require: []string{"bash"}, Affinity: "team", AffinityKey: "k"},
		}},
		Graph: &agent.WorkflowGraphPolicy{
			DefaultRetry: &graph.RetryPolicy{MaxRetries: 1},
			Groups: []agent.StageGroupOverride{{
				ID:            "g",
				Members:       []string{"a"},
				MaxIterations: 2,
				Terminator: &graph.TerminatorSpec{
					Name:   "adaptive",
					Params: map[string]string{"p": "q"},
				},
			}},
		},
	}

	dto := toWorkflowDTO(def)
	if len(dto.Stages) != 1 || dto.Graph == nil || len(dto.Graph.Groups) != 1 {
		t.Fatalf("DTO 未搬全阶段/图字段: stages=%d graph=%v", len(dto.Stages), dto.Graph)
	}

	// 就地修改 DTO 的每一个引用类型。
	dto.Stages[0].DependsOn[0] = "changed"
	dto.Stages[0].Hooks[0] = "changed"
	dto.Stages[0].Skills[0] = "changed"
	dto.Stages[0].EdgeConditions["up"] = "changed"
	dto.Stages[0].Retry.MaxRetries = 99
	dto.Stages[0].Loop.Terminator.Params["k"] = "changed"
	dto.Stages[0].Placement.Require[0] = "changed"
	dto.Graph.DefaultRetry.MaxRetries = 99
	dto.Graph.Groups[0].Members[0] = "changed"
	dto.Graph.Groups[0].Terminator.Params["p"] = "changed"

	st := def.Stages[0]
	if st.DependsOn[0] != "up" || st.Hooks[0] != "pre:shell:echo" || st.Skills[0] != "sk" {
		t.Errorf("阶段字符串切片被 DTO 修改污染: dependsOn=%v hooks=%v skills=%v", st.DependsOn, st.Hooks, st.Skills)
	}
	if st.EdgeConditions["up"] != "ok" {
		t.Errorf("EdgeConditions 与注册表共享 map: %v", st.EdgeConditions)
	}
	if st.Retry.MaxRetries != 2 {
		t.Errorf("Retry 与注册表共享: %+v", st.Retry)
	}
	if st.Loop.Terminator.Params["k"] != "v" {
		t.Errorf("Loop.Terminator.Params 与注册表共享: %+v", st.Loop.Terminator.Params)
	}
	if st.Placement.Require[0] != "bash" {
		t.Errorf("Placement.Require 与注册表共享: %v", st.Placement.Require)
	}
	if def.Graph.DefaultRetry.MaxRetries != 1 {
		t.Errorf("Graph.DefaultRetry 与注册表共享: %+v", def.Graph.DefaultRetry)
	}
	if def.Graph.Groups[0].Members[0] != "a" {
		t.Errorf("Graph.Groups[].Members 与注册表共享: %v", def.Graph.Groups[0].Members)
	}
	if def.Graph.Groups[0].Terminator.Params["p"] != "q" {
		t.Errorf("Graph.Groups[].Terminator.Params 与注册表共享: %+v", def.Graph.Groups[0].Terminator.Params)
	}
}

// TestWorkflowRuntimeCapabilitiesRoute 验证静态路径优先于 /api/workflows/{name},
// 并确认响应里的判据来自 agent 包 (与执行分发口同源)。
func TestWorkflowRuntimeCapabilitiesRoute(t *testing.T) {
	t.Setenv("CLAUDE_GO_GRAPH_ENGINE", "1")

	s := &Server{cfg: Config{StateDir: t.TempDir()}, provider: NewProvider(t.TempDir(), 0)}
	mux := http.NewServeMux()
	// 故意按 server.go 的顺序注册: 精确路径在通配之前。
	mux.HandleFunc("/api/workflows/runtime-capabilities", s.handleWorkflowRuntimeCapabilities)
	mux.HandleFunc("/api/workflows/", s.handleWorkflow)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/workflows/runtime-capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("静态路径应优先于 /api/workflows/{name}, got %d", resp.StatusCode)
	}
	var got workflowRuntimeCapabilitiesDTO
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.GraphEngineEnabled {
		t.Errorf("CLAUDE_GO_GRAPH_ENGINE=1 时 graphEngineEnabled 应为 true")
	}
	pl, ok := got.Modes["pipeline"]
	if !ok {
		t.Fatalf("modes 应含 pipeline, got %v", got.Modes)
	}
	if !pl.HasGraphTemplate {
		t.Errorf("pipeline 应有已验证等价的图模板")
	}
	if strings.TrimSpace(pl.Equivalence) == "" {
		t.Errorf("pipeline 应带等价性说明")
	}
	if _, ok := got.Modes["graph"]; !ok {
		t.Errorf("modes 应显式包含 graph (它不经过模板表, 需单独回答)")
	}
}
