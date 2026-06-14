package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/skills"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// capability_handlers.go: skills / tools / MCP 的查看与管理 + 引用关系 + 自定义 skill 创建。

var (
	toolRegOnce   sync.Once
	cachedToolReg *tool.Registry
)

func toolRegistry() *tool.Registry {
	toolRegOnce.Do(func() {
		cachedToolReg = tool.NewRegistry()
		builtin.RegisterBaseTools(cachedToolReg, nil)
	})
	return cachedToolReg
}

func (s *Server) projectCwd() string { return filepath.Dir(s.cfg.StateDir) }

func (s *Server) freshSkillRegistry() *skills.Registry {
	sr := skills.NewRegistry()
	sr.LoadDefaults(s.projectCwd())
	return sr
}

// ---- Skills ----

// handleSkills GET /api/skills (列表) | POST /api/skills (创建)
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleSkillCreate(w, r)
		return
	}
	sr := s.freshSkillRegistry()
	list := sr.All()
	type skillSummary struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		WhenToUse   string   `json:"when_to_use,omitempty"`
		LoadedFrom  string   `json:"loaded_from"`
		Model       string   `json:"model,omitempty"`
		Tools       []string `json:"allowed_tools,omitempty"`
		Custom      bool     `json:"custom"`
	}
	out := make([]skillSummary, 0, len(list))
	for _, sk := range list {
		out = append(out, skillSummary{
			Name: sk.Name, Description: sk.Description, WhenToUse: sk.WhenToUse,
			LoadedFrom: sk.LoadedFrom, Model: sk.Model, Tools: sk.AllowedTools,
			Custom: sk.LoadedFrom != "builtin",
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSkillDetail GET/DELETE /api/skills/{name}
func (s *Server) handleSkillDetail(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/skills/"), "/")
	if name == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("skill name required"))
		return
	}
	sr := s.freshSkillRegistry()
	sk, ok := sr.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("skill %q not found", name))
		return
	}
	if r.Method == http.MethodDelete {
		if sk.LoadedFrom == "builtin" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("内置 skill 不可删除"))
			return
		}
		dir := sk.SkillDir
		if dir == "" && sk.SourcePath != "" {
			dir = filepath.Dir(sk.SourcePath)
		}
		if dir != "" && strings.Contains(dir, "skills") {
			_ = os.RemoveAll(dir)
		}
		if s.cfg.ReloadSkills != nil {
			s.cfg.ReloadSkills()
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "deleted": name})
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

// skillCreateInput 自定义 skill 输入 (遵循 Anthropic Agent Skills 最佳实践)。
type skillCreateInput struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	WhenToUse    string   `json:"when_to_use"`
	Body         string   `json:"body"`
	Model        string   `json:"model,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

func validSkillName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

func (s *Server) handleSkillCreate(w http.ResponseWriter, r *http.Request) {
	var in skillCreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !validSkillName(in.Name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name 必须为小写字母/数字/连字符 (best practice)"))
		return
	}
	if strings.TrimSpace(in.Description) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("description 不能为空 (应第三人称写明'做什么+何时用')"))
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("body 不能为空"))
		return
	}
	md := buildSkillMarkdown(in)
	dir := filepath.Join(s.cfg.StateDir, "skills", in.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.cfg.ReloadSkills != nil {
		s.cfg.ReloadSkills()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": in.Name})
}

func buildSkillMarkdown(in skillCreateInput) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + in.Name + "\n")
	b.WriteString("description: " + oneLine(in.Description) + "\n")
	if strings.TrimSpace(in.WhenToUse) != "" {
		b.WriteString("when_to_use: " + oneLine(in.WhenToUse) + "\n")
	}
	if in.Model != "" {
		b.WriteString("model: " + in.Model + "\n")
	}
	if len(in.AllowedTools) > 0 {
		b.WriteString("allowed_tools:\n")
		for _, t := range in.AllowedTools {
			b.WriteString("  - " + t + "\n")
		}
	}
	b.WriteString("version: \"1.0\"\n")
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(in.Body))
	b.WriteString("\n")
	return b.String()
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
}

// handleSkillGenerate POST /api/skills/generate {objective} → LLM 按最佳实践生成 SKILL.md 草稿(不保存)。
func (s *Server) handleSkillGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	if s.cfg.LLMComplete == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("LLM 不可用"))
		return
	}
	var body struct {
		Objective string `json:"objective"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Objective) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("objective 不能为空"))
		return
	}
	sys := "你是 Anthropic Agent Skills 专家。按官方最佳实践设计一个 Skill。只输出 JSON, 不要解释。"
	user := fmt.Sprintf(`需求: %s

按 Anthropic Agent Skills 最佳实践输出一个 skill 的 JSON:
{"name":"小写字母-连字符命名(动名词更佳, 如 reviewing-go-code)","description":"第三人称, 同时写清楚【做什么】和【何时使用】(用于技能发现, 务必含触发条件)","when_to_use":"触发条件一句话","body":"Markdown 正文: 精简、可操作的指令; 遵循渐进式披露; 适当给示例"}

要求: name 只含小写字母/数字/连字符; description 第三人称且含'何时用'; body 不要太长。`, oneLine(body.Objective))
	out, err := s.cfg.LLMComplete(context.Background(), sys, user)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	js := extractJSONObj(out)
	if js == "" {
		writeError(w, http.StatusBadGateway, fmt.Errorf("LLM 未返回可解析 JSON"))
		return
	}
	var draft skillCreateInput
	if err := json.Unmarshal([]byte(js), &draft); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	warn := ""
	if !validSkillName(strings.TrimSpace(draft.Name)) {
		warn = "name 不符合规范(应小写字母/数字/连字符), 请修正"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"skill": draft, "warn": warn})
}

// ---- Tools ----

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	reg := toolRegistry()
	type toolDTO struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	}
	all := reg.All()
	out := make([]toolDTO, 0, len(all))
	for _, t := range all {
		out = append(out, toolDTO{Name: t.Name(), Description: t.Description(), InputSchema: t.InputSchema()})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- MCP ----

func (s *Server) handleMCPServers(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MCPServers == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.MCPServers())
}

// ---- References (谁引用了) ----

// handleReferences GET /api/references?skill=X | ?role=Y
func (s *Server) handleReferences(w http.ResponseWriter, r *http.Request) {
	rr := agent.NewRoleRegistry(s.projectCwd())
	skill := strings.TrimSpace(r.URL.Query().Get("skill"))
	role := strings.TrimSpace(r.URL.Query().Get("role"))

	type wfRef struct {
		Workflow string `json:"workflow"`
		Stage    string `json:"stage"`
		Role     string `json:"role"`
	}

	if skill != "" {
		roles := []string{}
		for _, rn := range rr.Names() {
			info := rr.DescribeRole(rn)
			if info == nil {
				continue
			}
			if containsStr(info.BuiltinSkills, skill) || containsStr(info.FileSkills, skill) || containsStr(info.RecommendedSkills, skill) {
				roles = append(roles, rn)
			}
		}
		refs := []wfRef{}
		for _, wf := range agent.ListWorkflows() {
			for _, st := range wf.Stages {
				if containsStr(roles, st.Role) {
					refs = append(refs, wfRef{wf.Name, st.Name, st.Role})
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"skill": skill, "roles": roles, "workflows": refs})
		return
	}
	if role != "" {
		info := rr.DescribeRole(role)
		skillsUsed := []string{}
		if info != nil {
			skillsUsed = dedupStrings(append(append(append([]string{}, info.BuiltinSkills...), info.FileSkills...), info.RecommendedSkills...))
		}
		refs := []wfRef{}
		for _, wf := range agent.ListWorkflows() {
			for _, st := range wf.Stages {
				if st.Role == role {
					refs = append(refs, wfRef{wf.Name, st.Name, st.Role})
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"role": role, "skills": skillsUsed, "workflows": refs})
		return
	}
	writeError(w, http.StatusBadRequest, fmt.Errorf("需要 ?skill= 或 ?role= 参数"))
}

func containsStr(arr []string, s string) bool {
	for _, a := range arr {
		if a == s {
			return true
		}
	}
	return false
}

// extractJSONObj 从可能含散文/代码块的文本里抽取首个平衡 JSON 对象。
func extractJSONObj(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
