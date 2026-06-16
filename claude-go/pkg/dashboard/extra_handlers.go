package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
)

// =====================================================================
// /api/workflows  -> 返回所有内置 workflow 名字列表
// /api/workflows/:name  -> 返回具体 workflow 的 DAG 结构 (stages + depends)
// =====================================================================

type workflowStageDTO struct {
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Prompt    string   `json:"prompt,omitempty"` // 供编辑预填 (列表里多数为空/简短)
	DependsOn []string `json:"dependsOn,omitempty"`
	Parallel  bool     `json:"parallel,omitempty"`
}

type workflowDefDTO struct {
	Name         string             `json:"name"`
	Description  string             `json:"description,omitempty"`
	Mode         string             `json:"mode,omitempty"`
	Rounds       int                `json:"rounds,omitempty"`
	Stages       []workflowStageDTO `json:"stages"`
	Custom       bool               `json:"custom,omitempty"`       // 运行时定义的动态工作流
	ProducesCode bool               `json:"producesCode,omitempty"` // 跑编译/测试门禁
	QualityGate  string             `json:"qualityGate,omitempty"`  // 内容质量门禁策略
}

// handleWorkflows: GET 列出所有工作流(内置+动态); POST 注册一个动态工作流。
func (s *Server) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleWorkflowCreate(w, r)
		return
	}
	list := agent.ListWorkflows()
	out := make([]workflowDefDTO, 0, len(list))
	for i := range list {
		wf := list[i]
		out = append(out, toWorkflowDTO(&wf))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleWorkflowCreate POST /api/workflows: 校验并注册一个动态工作流, 同时落盘 (重启可恢复)。
// 必须在 :18080 (飞书挂载、与 teamMgr 同进程的 dashboard) 调用, 注册才能即时被团队执行使用。
func (s *Server) handleWorkflowCreate(w http.ResponseWriter, r *http.Request) {
	var def agent.WorkflowDef
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("JSON 解析失败: %w", err))
		return
	}
	// 用内置角色注册表校验 (registerBuiltins 同步加载, 与 cwd 无关)
	roles := agent.NewRoleRegistry(s.cfg.StateDir)
	if err := agent.RegisterWorkflow(&def, roles); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// 落盘持久化 (失败不阻断: 已在进程内注册, 仅重启后丢失)
	if err := agent.SaveWorkflowToDir(filepath.Join(s.cfg.StateDir, "workflows"), &def); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"workflow": toWorkflowDTO(&def), "persisted": false, "warn": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"workflow": toWorkflowDTO(&def), "persisted": true})
}

// dashLLMAdapter 把 dashboard Config.LLMComplete 回调适配成 agent.LLMClient。
type dashLLMAdapter struct {
	fn func(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

func (a dashLLMAdapter) SimpleComplete(ctx context.Context, sys, user string) (string, error) {
	return a.fn(ctx, sys, user)
}

// roleInfoDTO 角色富信息 (供工作流详细 DAG 的节点详情面板)。
type roleInfoDTO struct {
	Name         string   `json:"name"`
	Resolved     string   `json:"resolved,omitempty"`
	Description  string   `json:"description,omitempty"`  // 功能设计
	SystemPrompt string   `json:"systemPrompt,omitempty"` // 角色基础提示词
	Skills       []string `json:"skills,omitempty"`       // 真实 per-role skills (builtin+file+recommended)
	Tags         []string `json:"tags,omitempty"`
	Found        bool     `json:"found"`
}

// simpleCompleteFn 把 cfg.LLMComplete 适配为 agent.LLMClient (供意图识别器用)。
type simpleCompleteFn func(ctx context.Context, systemPrompt, userPrompt string) (string, error)

func (f simpleCompleteFn) SimpleComplete(ctx context.Context, sys, user string) (string, error) {
	return f(ctx, sys, user)
}

// handleIntent POST /api/intent {text} — 识别一条会话消息是否为"需编排多agent完成的目标"。
// 供 webapp 会话做自动编排路由: orchestrate=true 时建议用 workflow 跑团队(默认 swarm 自动拆解),
// 否则走普通单 agent 对话。复用 IntentRecognizer (关键词 + LLM 参数提取)。
func (s *Server) handleIntent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Text) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("text 必填"))
		return
	}
	// 优先 LLM 语义分类 (理解意图, 而非正则关键词匹配)。
	if s.cfg.LLMComplete != nil {
		if resp, ok := s.llmClassifyIntent(r.Context(), in.Text); ok {
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}
	// 回退: 关键词识别器 (仅在 LLM 不可用时)。
	rec := agent.NewIntentRecognizer(nil)
	intent := rec.Recognize(r.Context(), in.Text)
	resp := map[string]any{"orchestrate": false, "via": "keyword-fallback"}
	if intent != nil && intent.Action == "create_and_run" && intent.Confidence >= 0.6 {
		obj := intent.Objective
		if strings.TrimSpace(obj) == "" {
			obj = in.Text
		}
		wf := intent.Workflow
		if strings.TrimSpace(wf) == "" {
			wf = "swarm"
		}
		resp["orchestrate"] = true
		resp["objective"] = obj
		resp["workflow"] = wf
		resp["confidence"] = intent.Confidence
	}
	writeJSON(w, http.StatusOK, resp)
}

// llmClassifyIntent 用 LLM 语义判断: 该消息是"需编排多 agent 完成的任务目标"还是普通对话。
// 返回 (resp, true) 表示成功; (nil,false) 表示 LLM 失败, 调用方回退关键词。
func (s *Server) llmClassifyIntent(ctx context.Context, text string) (map[string]any, bool) {
	var wfs strings.Builder
	for _, wf := range agent.ListWorkflows() {
		desc := wf.Description
		if len(desc) > 60 {
			desc = desc[:60]
		}
		wfs.WriteString(fmt.Sprintf("- %s: %s\n", wf.Name, desc))
	}
	sys := "你是任务路由分类器。判断用户消息是'需要多步/动手才能完成的任务目标'(应编排团队多agent执行) 还是'普通对话/提问/查询/闲聊'(单agent回答即可)。只输出 JSON, 不要解释。"
	user := fmt.Sprintf(`可用工作流 (选最匹配的; 都不匹配则用 swarm 自动拆解):
%s
用户消息: %q

只输出: {"orchestrate": true/false, "workflow": "工作流名或swarm", "objective": "把消息整理成清晰、命令式的执行目标", "confidence": 0到1, "reason": "一句话判断依据"}
判定 orchestrate=true 仅当用户想"做出某产物/完成某复杂任务"(如: 开发/实现某功能、调研某主题并成文、制作图文音视频、多步分析报告)。
判定 false: 简单提问、概念解释、闲聊、打招呼、查询状态、单轮即可答完的请求。`, wfs.String(), text)

	out, err := s.cfg.LLMComplete(ctx, sys, user)
	if err != nil {
		return nil, false
	}
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	var parsed struct {
		Orchestrate bool    `json:"orchestrate"`
		Workflow    string  `json:"workflow"`
		Objective   string  `json:"objective"`
		Confidence  float64 `json:"confidence"`
		Reason      string  `json:"reason"`
	}
	if json.Unmarshal([]byte(out[start:end+1]), &parsed) != nil {
		return nil, false
	}
	resp := map[string]any{
		"orchestrate": parsed.Orchestrate, "confidence": parsed.Confidence,
		"reason": parsed.Reason, "via": "llm",
	}
	if parsed.Orchestrate {
		obj := strings.TrimSpace(parsed.Objective)
		if obj == "" {
			obj = text
		}
		wf := strings.TrimSpace(parsed.Workflow)
		if wf == "" || agent.GetWorkflow(wf) == nil && wf != "swarm" {
			wf = "swarm"
		}
		resp["objective"] = obj
		resp["workflow"] = wf
	}
	return resp, true
}

// handleVerifyGoal POST /api/verify-goal {objective, result} — LLM 验收: 结果是否达成目标。
// 供会话编排的"目标循环": 未达成则带 gap 反馈重跑。
func (s *Server) handleVerifyGoal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var in struct {
		Objective string `json:"objective"`
		Result    string `json:"result"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Objective) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("objective 必填"))
		return
	}
	if s.cfg.LLMComplete == nil {
		// LLM 不可用: 保守认为已达成 (避免无限重跑)
		writeJSON(w, http.StatusOK, map[string]any{"met": true, "score": 6, "gap": "", "via": "no-llm"})
		return
	}
	result := in.Result
	if len(result) > 7000 {
		result = result[:7000]
	}
	sys := "你是严格的验收员。判断'结果'是否真正达成了'目标'。只输出 JSON, 不要解释。"
	user := fmt.Sprintf(`目标:
%s

结果:
%s

只输出: {"met": true/false, "score": 0到10, "gap": "若未达成, 给出具体差距与下一轮必须改进的点(命令式); 达成则空串"}`, in.Objective, result)
	out, err := s.cfg.LLMComplete(r.Context(), sys, user)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"met": true, "score": 6, "gap": "", "via": "llm-error"})
		return
	}
	st, en := strings.Index(out, "{"), strings.LastIndex(out, "}")
	resp := map[string]any{"met": true, "score": 6, "gap": ""}
	if st >= 0 && en > st {
		var parsed struct {
			Met   bool    `json:"met"`
			Score float64 `json:"score"`
			Gap   string  `json:"gap"`
		}
		if json.Unmarshal([]byte(out[st:en+1]), &parsed) == nil {
			resp = map[string]any{"met": parsed.Met, "score": parsed.Score, "gap": parsed.Gap}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRoles GET /api/roles?names=a,b,c — 批量返回角色富信息 (description/systemPrompt/真实skills/tags)。
// 注: tools/MCP 是全局的(角色无 per-role 工具映射), 不在此返回, 由前端如实标注。
func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	reg := agent.NewRoleRegistry(s.cfg.StateDir)
	out := []roleInfoDTO{}
	seen := map[string]bool{}
	describe := func(n string) {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		dto := roleInfoDTO{Name: n}
		if info := reg.DescribeRole(n); info != nil {
			dto.Found = true
			dto.Resolved = info.Resolved
			dto.Description = info.Description
			dto.Tags = info.Tags
			merged := append(append(append([]string{}, info.BuiltinSkills...), info.FileSkills...), info.RecommendedSkills...)
			dto.Skills = dedupStrings(merged)
		}
		if rd := reg.Get(n); rd != nil {
			dto.SystemPrompt = rd.SystemPrompt
		}
		out = append(out, dto)
	}
	names := strings.TrimSpace(r.URL.Query().Get("names"))
	if names == "" {
		// 无 names: 返回全部 agent (供 webapp Agents 展示页)
		all := reg.Names()
		sort.Strings(all)
		for _, n := range all {
			describe(n)
		}
	} else {
		for _, n := range strings.Split(names, ",") {
			describe(n)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// handleWorkflowGenerate POST /api/workflows/generate {objective}
// 用 LLM 生成一个工作流编排, Validate 后返回(不注册), 供前端审核/编辑后再注册。
// 这与蜂群 decompose 是同源操作(LLM 从目标生成编排), 区别是产出可复用、可审核的 WorkflowDef。
func (s *Server) handleWorkflowGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	if s.cfg.LLMComplete == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("LLM 不可用: 请通过 :18080 (注入了 LLM 的 dashboard) 调用"))
		return
	}
	var body struct {
		Objective   string             `json:"objective"`
		Current     *agent.WorkflowDef `json:"current,omitempty"`     // 迭代: 当前编排
		Instruction string             `json:"instruction,omitempty"` // 迭代: 调整指令
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	roles := agent.NewRoleRegistry(s.cfg.StateDir)
	def, err := agent.GenerateWorkflowDef(r.Context(), dashLLMAdapter{s.cfg.LLMComplete}, body.Objective, roles.Names(), body.Current, body.Instruction)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	valErr := ""
	if e := def.Validate(roles); e != nil {
		valErr = e.Error() // 非致命: 生成可能不完美, 用户在审核时修正
	}
	// 返回完整 def (含每阶段 prompt, 供前端预填编辑器)
	writeJSON(w, http.StatusOK, map[string]interface{}{"workflow": def, "validationError": valErr})
}

// handleTeamSwarmPlan GET /api/teams/{name}/swarm-plan
// 返回蜂群运行时 LLM 动态生成的编排 (SubTask DAG): stages(供 MiniStageDiagram 画图) + tasks(含描述)。
// 让"黑盒的"蜂群编排变得可视化。
func (s *Server) handleTeamSwarmPlan(w http.ResponseWriter, r *http.Request, name string) {
	bb, err := s.provider.Blackboard(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	resp := map[string]interface{}{"mode": "swarm", "stages": []workflowStageDTO{}, "available": false}
	planJSON := ""
	if bb != nil {
		planJSON = bb.Map["swarm-plan"]
	}
	if strings.TrimSpace(planJSON) != "" {
		var plan agent.DecompositionPlan
		if err := json.Unmarshal([]byte(planJSON), &plan); err == nil {
			stages := make([]workflowStageDTO, 0, len(plan.SubTasks))
			for _, st := range plan.SubTasks {
				// Name 用 ID 以保证 DependsOn 的边能对上; 描述放 tasks 里
				stages = append(stages, workflowStageDTO{
					Name: st.ID, Role: st.Role, DependsOn: append([]string(nil), st.DependsOn...),
				})
			}
			resp["stages"] = stages
			resp["tasks"] = plan.SubTasks
			resp["strategy"] = plan.Strategy
			resp["rationale"] = plan.Rationale
			resp["available"] = len(plan.SubTasks) > 0
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleWorkflow(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/workflows/")
	name = strings.Trim(name, "/")
	if name == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("workflow name required"))
		return
	}
	if r.Method == http.MethodDelete {
		s.handleWorkflowDelete(w, r, name)
		return
	}
	wf := agent.GetWorkflow(name)
	if wf == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("workflow %q not found", name))
		return
	}
	writeJSON(w, http.StatusOK, toWorkflowDTO(wf))
}

// handleWorkflowDelete DELETE /api/workflows/{name}: 删除一个动态工作流 (内置不可删) + 删除落盘文件。
func (s *Server) handleWorkflowDelete(w http.ResponseWriter, r *http.Request, name string) {
	if err := agent.UnregisterWorkflow(name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	_ = os.Remove(filepath.Join(s.cfg.StateDir, "workflows", sanitizeWFFile(name)+".json"))
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "deleted": name})
}

// sanitizeWFFile 与 agent.SaveWorkflowToDir 的文件名规则一致。
func sanitizeWFFile(name string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", "..", "_", " ", "-")
	return r.Replace(name)
}

func toWorkflowDTO(wf *agent.WorkflowDef) workflowDefDTO {
	dto := workflowDefDTO{
		Name: wf.Name, Description: wf.Description, Mode: wf.Mode, Rounds: wf.Rounds,
		Custom: wf.Custom, ProducesCode: wf.ProducesCode, QualityGate: wf.QualityGate,
	}
	for _, st := range wf.Stages {
		dto.Stages = append(dto.Stages, workflowStageDTO{
			Name: st.Name, Role: st.Role, Prompt: st.Prompt,
			DependsOn: append([]string(nil), st.DependsOn...),
			Parallel:  st.Parallel,
		})
	}
	return dto
}

// =====================================================================
// /api/search?q=...&type=team|task|cron|all
// 支持: 名称精确 / 前缀匹配 / objective & description 分词命中
// =====================================================================

type searchHitDTO struct {
	Kind      string   `json:"kind"`   // team | task | cron | stage
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Snippet   string   `json:"snippet,omitempty"`
	Score     float64  `json:"score"`
	Link      string   `json:"link"`
	Tags      []string `json:"tags,omitempty"`
	Timestamp string   `json:"ts,omitempty"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []searchHitDTO{})
		return
	}
	typ := strings.ToLower(r.URL.Query().Get("type"))
	tokens := tokenize(q)
	limit := parseIntQuery(r, "limit", 30)

	hits := []searchHitDTO{}

	if typ == "" || typ == "all" || typ == "team" {
		list, _ := s.provider.ListTeams()
		for _, t := range list {
			sc := scoreTokens(t.Name, t.Objective, t.Workflow, tokens)
			if sc <= 0 {
				continue
			}
			hits = append(hits, searchHitDTO{
				Kind: "team", ID: t.Name, Title: t.Name,
				Snippet:   truncateString(t.Objective, 160),
				Score:     sc,
				Link:      "#/teams/" + t.Name,
				Tags:      []string{t.Workflow, t.Status},
				Timestamp: t.CreatedAt.Format(time.RFC3339),
			})
		}
	}
	if typ == "" || typ == "all" || typ == "cron" {
		list, _ := s.provider.ListCronJobs()
		for _, c := range list {
			sc := scoreTokens(c.Name, c.Payload, c.Workflow, tokens)
			if sc <= 0 {
				continue
			}
			status := "paused"
			if c.Enabled {
				status = "active"
			}
			hits = append(hits, searchHitDTO{
				Kind: "cron", ID: c.ID, Title: c.Name,
				Snippet: truncateString(c.Payload, 160),
				Score:   sc,
				Link:    "#/cron",
				Tags:    []string{status, c.Schedule, c.Workflow},
			})
		}
	}
	if typ == "" || typ == "all" || typ == "task" {
		list, _ := s.provider.ListTasks()
		for _, t := range list {
			sc := scoreTokens(t.Subject, t.Description, t.ID, tokens)
			if sc <= 0 {
				continue
			}
			hits = append(hits, searchHitDTO{
				Kind: "task", ID: t.ID, Title: t.Subject,
				Snippet:   truncateString(t.Description, 160),
				Score:     sc,
				Link:      "#/tasks",
				Tags:      []string{t.Status, t.Owner},
				Timestamp: t.CreatedAt.Format(time.RFC3339),
			})
		}
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	writeJSON(w, http.StatusOK, hits)
}

func tokenize(q string) []string {
	q = strings.ToLower(q)
	rep := strings.NewReplacer(",", " ", ";", " ", "，", " ", "。", " ", "、", " ",
		"(", " ", ")", " ", "[", " ", "]", " ", "【", " ", "】", " ",
		":", " ", "：", " ", "?", " ", "!", " ")
	q = rep.Replace(q)
	raw := strings.Fields(q)
	// 去重
	seen := map[string]bool{}
	out := []string{}
	for _, t := range raw {
		if len(t) == 0 {
			continue
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// scoreTokens 在候选文本中搜多个 token. 返回 0 表示无匹配。
func scoreTokens(fields ...interface{}) float64 {
	if len(fields) < 2 {
		return 0
	}
	tokens, _ := fields[len(fields)-1].([]string)
	if len(tokens) == 0 {
		return 0
	}
	texts := fields[:len(fields)-1]
	joined := ""
	for _, t := range texts {
		if str, ok := t.(string); ok {
			joined += " " + strings.ToLower(str)
		}
	}
	if joined == "" {
		return 0
	}
	var score float64
	matched := 0
	for _, tok := range tokens {
		if strings.Contains(joined, tok) {
			matched++
			// 名称命中 (第一个 field) 加权
			if first, ok := texts[0].(string); ok {
				flow := strings.ToLower(first)
				if flow == tok {
					score += 3
				} else if strings.HasPrefix(flow, tok) {
					score += 2
				} else if strings.Contains(flow, tok) {
					score += 1.2
				}
			}
			score += 1
		}
	}
	if matched == 0 {
		return 0
	}
	score *= float64(matched) / float64(len(tokens))
	return score
}

// =====================================================================
// /api/logs/stream  -> SSE tail 实时日志
// /api/logs/tail?n=500  -> 最近 N 行
// 监听 .claude-go/.dashboard/logs/ 和 stdout 捕获的文件 (本轮 dashboard 启动日志 fallback)
// =====================================================================

func (s *Server) handleLogsTail(w http.ResponseWriter, r *http.Request) {
	n := parseIntQuery(r, "n", 500)
	path := s.resolveLogBySource(r)
	if path == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"path":   "",
			"source": r.URL.Query().Get("source"),
			"lines":  []string{},
			"note":   "请求的日志源不存在, 请确认已启动对应组件 (dashboard / feishu bot / client / mcp)",
		})
		return
	}
	lines, err := tailLines(path, n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":   path,
		"source": r.URL.Query().Get("source"),
		"lines":  lines,
	})
}

func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	path := s.resolveLogBySource(r)
	if path == "" {
		src := r.URL.Query().Get("source")
		fmt.Fprintf(w, "event: error\ndata: log source %q not available\n\n", src)
		flusher.Flush()
		return
	}
	fmt.Fprintf(w, "event: info\ndata: tailing %s (source=%s)\n\n", path, r.URL.Query().Get("source"))
	flusher.Flush()

	// 初始化发送最后 200 行
	initial, _ := tailLines(path, 200)
	for _, ln := range initial {
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", strings.ReplaceAll(ln, "\n", " "))
	}
	flusher.Flush()

	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return
	}
	reader := bufio.NewReader(f)
	ticker := time.NewTicker(800 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			for {
				line, err := reader.ReadString('\n')
				if line != "" {
					line = strings.TrimRight(line, "\r\n")
					fmt.Fprintf(w, "event: log\ndata: %s\n\n", line)
				}
				if err != nil {
					break
				}
			}
			flusher.Flush()
		}
	}
}

// resolveLogPath 选择本实例的日志文件。优先顺序:
//  1) 查询参数 path 指定 (限制在 state 根目录内)
//  2) .claude-go/.dashboard/dashboard.log
//  3) $HOME/.claude-go/history.jsonl
func (s *Server) resolveLogPath(r *http.Request) string {
	root := s.cfg.StateDir
	if qp := r.URL.Query().Get("path"); qp != "" {
		abs := qp
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, qp)
		}
		if strings.HasPrefix(abs, root) {
			if info, err := os.Stat(abs); err == nil && !info.IsDir() {
				return abs
			}
		}
	}
	candidates := []string{
		filepath.Join(root, ".dashboard", "dashboard.log"),
		filepath.Join(root, "daemon.log"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".claude-go", "history.jsonl"),
			filepath.Join(home, ".claude-go", "debug.log"),
		)
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func tailLines(path string, n int) ([]string, error) {
	if n <= 0 {
		n = 100
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const bufSize = 64 * 1024
	size := fi.Size()
	var chunk []byte
	offset := size
	read := int64(0)
	for offset > 0 && strings.Count(string(chunk), "\n") <= n {
		step := int64(bufSize)
		if offset < step {
			step = offset
		}
		offset -= step
		buf := make([]byte, step)
		_, err := f.ReadAt(buf, offset)
		if err != nil && err != io.EOF {
			return nil, err
		}
		chunk = append(buf, chunk...)
		read += step
		// 安全上限: 读取上限 8MB
		if read > 8*1024*1024 {
			break
		}
	}
	lines := strings.Split(strings.TrimRight(string(chunk), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// =====================================================================
// /api/hivemind -> 群体智能 (swarm_intel) 概览 + 历史
// =====================================================================

type hiveMindResp struct {
	Enabled       bool                     `json:"enabled"`
	DataDir       string                   `json:"dataDir"`
	Predictions   []hivePredictionDTO      `json:"predictions,omitempty"`
	Pheromones    []hivePheromoneDTO       `json:"pheromones,omitempty"`
	MetricsSeries []hiveMetricPointDTO     `json:"metricsSeries,omitempty"`
	Summary       map[string]interface{}   `json:"summary"`
	Notes         []string                 `json:"notes,omitempty"`
}
type hivePredictionDTO struct {
	ID         string    `json:"id"`
	ChatID     string    `json:"chatId,omitempty"`
	Objective  string    `json:"objective"`
	Confidence float64   `json:"confidence"`
	Consensus  float64   `json:"consensus"`
	Agents     int       `json:"agents"`
	Timestamp  time.Time `json:"timestamp"`
	Outcome    string    `json:"outcome,omitempty"`
}
type hivePheromoneDTO struct {
	Domain   string  `json:"domain"`
	Strength float64 `json:"strength"`
	Updated  string  `json:"updated,omitempty"`
}
type hiveMetricPointDTO struct {
	Timestamp time.Time `json:"timestamp"`
	Metric    string    `json:"metric"`
	Value     float64   `json:"value"`
}

func (s *Server) handleHiveMind(w http.ResponseWriter, r *http.Request) {
	resp := hiveMindResp{
		Summary: map[string]interface{}{},
		Notes:   []string{},
	}
	dataDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		dataDir = filepath.Join(home, ".claude-go", "swarm_intel")
	}
	resp.DataDir = dataDir
	if info, err := os.Stat(dataDir); err == nil && info.IsDir() {
		resp.Enabled = true
	} else {
		resp.Notes = append(resp.Notes, "swarm_intel 目录不存在或为空: "+dataDir)
	}

	// predictions
	if entries, err := os.ReadDir(filepath.Join(dataDir, "predictions")); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			p := filepath.Join(dataDir, "predictions", e.Name())
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
			for sc.Scan() {
				var rec hivePredictionDTO
				if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
					continue
				}
				resp.Predictions = append(resp.Predictions, rec)
			}
			f.Close()
		}
	}

	// pheromones
	if entries, err := os.ReadDir(filepath.Join(dataDir, "pheromones")); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dataDir, "pheromones", e.Name()))
			if err != nil {
				continue
			}
			var arr []hivePheromoneDTO
			_ = json.Unmarshal(b, &arr)
			resp.Pheromones = append(resp.Pheromones, arr...)
		}
	}

	// metrics
	mfile := filepath.Join(dataDir, "metrics", "swarm_metrics.jsonl")
	if f, err := os.Open(mfile); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var rec hiveMetricPointDTO
			if err := json.Unmarshal(sc.Bytes(), &rec); err == nil {
				resp.MetricsSeries = append(resp.MetricsSeries, rec)
			}
		}
		f.Close()
	}

	// summary
	resp.Summary["predictions"] = len(resp.Predictions)
	resp.Summary["pheromones"] = len(resp.Pheromones)
	resp.Summary["metricsPoints"] = len(resp.MetricsSeries)
	if len(resp.Predictions) == 0 && len(resp.Pheromones) == 0 {
		resp.Notes = append(resp.Notes,
			"当前没有群体智能预测记录。swarm_intel 引擎仅在 Feishu Bot 或 `claude-go team` swarm workflow 调用时启用。")
	}
	writeJSON(w, http.StatusOK, resp)
}

// =====================================================================
// /api/timeseries/:module/:metric  -> 按时间窗口聚合的点序列 (用于 Dreaming/Evolution 进化/退化判定)
// =====================================================================

type timePointDTO struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}
type timeSeriesResp struct {
	Module  string         `json:"module"`
	Metric  string         `json:"metric"`
	Points  []timePointDTO `json:"points"`
	Trend   string         `json:"trend"`   // improving | degrading | stable | insufficient
	Slope   float64        `json:"slope"`
	First   *float64       `json:"first,omitempty"`
	Last    *float64       `json:"last,omitempty"`
	ChangePct *float64     `json:"changePct,omitempty"`
}

func (s *Server) handleTimeSeries(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/timeseries/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("path must be /api/timeseries/:module/:metric"))
		return
	}
	module, metric := parts[0], parts[1]
	limit := parseIntQuery(r, "limit", 1000)
	sinceStr := r.URL.Query().Get("since")
	var since time.Time
	if sinceStr != "" {
		if d, err := time.ParseDuration(sinceStr); err == nil {
			since = time.Now().Add(-d)
		}
	}

	// 优先从 Prometheus 查询 range query
	var events []MetricEventDTO
	promURL := os.Getenv("CLAUDE_GO_PROMETHEUS_URL")
	if promURL != "" {
		end := time.Now()
		start := end.Add(-24 * time.Hour)
		if !since.IsZero() {
			start = since
		}
		step := time.Duration(float64(end.Sub(start).Seconds())/500) * time.Second
		if step < 15*time.Second {
			step = 15 * time.Second
		}
		query := fmt.Sprintf(`{__name__=~"claude_go_.*", module="%s"}`, module)
		resp, err := queryPromRange(promURL, query, start, end, step)
		if err == nil && resp != nil && resp.Status == "success" && len(resp.Data.Result) > 0 {
			for _, r := range resp.Data.Result {
				if r.Metric["__name__"] != "claude_go_"+metric {
					continue
				}
				for _, v := range r.Values {
					if len(v) < 2 {
						continue
					}
					tsFloat, _ := v[0].(float64)
					valFloat, _ := v[1].(string)
					val, _ := strconv.ParseFloat(valFloat, 64)
					events = append(events, MetricEventDTO{
						Timestamp: time.Unix(int64(tsFloat), 0),
						Module:    module,
						Name:      metric,
						Value:     val,
						Labels:    r.Metric,
					})
				}
			}
		}
	}

	// 回退: Provider (自带 Prometheus->JSONL dual-read)
	if len(events) == 0 {
		evts, err := s.provider.ModuleMetricEvents(module, limit, since)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for _, e := range evts {
			if e.Name != metric {
				continue
			}
			events = append(events, e)
		}
	}
	resp := timeSeriesResp{Module: module, Metric: metric, Points: []timePointDTO{}}
	for _, e := range events {
		if e.Name != metric {
			continue
		}
		resp.Points = append(resp.Points, timePointDTO{Timestamp: e.Timestamp, Value: e.Value})
	}
	sort.Slice(resp.Points, func(i, j int) bool { return resp.Points[i].Timestamp.Before(resp.Points[j].Timestamp) })

	resp.Trend, resp.Slope = computeTrend(resp.Points)
	if len(resp.Points) > 0 {
		f := resp.Points[0].Value
		l := resp.Points[len(resp.Points)-1].Value
		resp.First, resp.Last = &f, &l
		if f != 0 {
			cp := (l - f) / f * 100
			resp.ChangePct = &cp
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// computeTrend 简单 OLS slope; 指标/度量不同方向语义不同, 这里返回通用判定 (up/down)。
func computeTrend(points []timePointDTO) (trend string, slope float64) {
	n := len(points)
	if n < 3 {
		return "insufficient", 0
	}
	var sumX, sumY, sumXY, sumX2 float64
	t0 := points[0].Timestamp.Unix()
	for _, p := range points {
		x := float64(p.Timestamp.Unix() - t0)
		sumX += x
		sumY += p.Value
		sumXY += x * p.Value
		sumX2 += x * x
	}
	fn := float64(n)
	denom := fn*sumX2 - sumX*sumX
	if denom == 0 {
		return "stable", 0
	}
	slope = (fn*sumXY - sumX*sumY) / denom
	meanY := sumY / fn
	if meanY == 0 {
		if slope > 0 {
			trend = "improving"
		} else if slope < 0 {
			trend = "degrading"
		} else {
			trend = "stable"
		}
		return
	}
	// 相对斜率: 每秒变化 / 均值 * 3600 (每小时变化率)
	norm := slope / meanY * 3600
	if norm > 0.01 {
		trend = "improving"
	} else if norm < -0.01 {
		trend = "degrading"
	} else {
		trend = "stable"
	}
	return
}

// =====================================================================
// /api/actions/{kind}/{target}  动作接口 (POST)
//    kind: cron | team
//    cron actions: enable | disable | trigger | remove
//    team actions: stop | restart | delete
// 由于 dashboard 只读原则: 动作只对磁盘 JSON 做标记, 真正调度由 claude-go 主进程消费 action queue
// =====================================================================

type actionResp struct {
	OK       bool   `json:"ok"`
	Queued   bool   `json:"queued"`
	Message  string `json:"message,omitempty"`
	Hint     string `json:"hint,omitempty"`
	ActionID string `json:"actionId,omitempty"`
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/actions/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 3 {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("path must be /api/actions/{kind}/{action}/{target}"))
		return
	}
	kind, action, target := parts[0], parts[1], parts[2]
	if kind == "" || action == "" || target == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("empty kind/action/target"))
		return
	}

	// 支持可选 JSON payload: {workflow, objective, lang, analysts, rounds, ...}
	var payload map[string]interface{}
	if r.Body != nil {
		defer r.Body.Close()
		if body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024)); err == nil && len(body) > 0 {
			_ = json.Unmarshal(body, &payload)
		}
	}

	actionID := fmt.Sprintf("%s-%s-%s-%d", kind, action, target, time.Now().UnixMilli())
	rec := map[string]interface{}{
		"id":        actionID,
		"kind":      kind,
		"action":    action,
		"target":    target,
		"status":    "pending",
		"requested": time.Now().Format(time.RFC3339),
		"source":    "dashboard",
	}
	if payload != nil {
		rec["payload"] = payload
	}
	queueDir := filepath.Join(s.cfg.StateDir, ".dashboard", "actions")
	_ = os.MkdirAll(queueDir, 0o755)
	fpath := filepath.Join(queueDir, actionID+".json")
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(fpath, b, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("queue action: %w", err))
		return
	}

	// 对某些 action 我们直接执行 (无副作用的开关切换): cron enable/disable/pause/resume
	immediate := ""
	hint := ""
	switch kind + "." + action {
	case "cron.enable", "cron.disable", "cron.pause", "cron.resume":
		if err := s.toggleCron(target, action); err == nil {
			immediate = fmt.Sprintf("cron %s=%s 已写入磁盘", target, action)
		}
	case "cron.create":
		if payload == nil {
			payload = map[string]interface{}{}
		}
		if _, ok := payload["id"]; !ok && target != "" && target != "new" {
			payload["id"] = target
		}
		if newID, err := s.cronCreate(payload); err == nil {
			immediate = fmt.Sprintf("cron %s 已创建", newID)
			rec["payload"] = payload
			hint = "提示: 若主进程 (feishu bot / daemon) 未在运行, 新建任务只落盘、不调度。"
		} else {
			hint = "创建失败: " + err.Error()
		}
	case "cron.update", "cron.edit":
		if err := s.cronUpdate(target, payload); err == nil {
			immediate = fmt.Sprintf("cron %s 已更新", target)
		} else {
			hint = "更新失败: " + err.Error()
		}
	case "cron.delete", "cron.remove":
		if err := s.cronDelete(target); err == nil {
			immediate = fmt.Sprintf("cron %s 已删除", target)
		} else {
			hint = "删除失败: " + err.Error()
		}
	case "cron.trigger":
		hint = "已排队立即触发, 需要主进程消费 (feishu bot / daemon) 才能真正运行。"
	case "team.create":
		if s.cfg.TeamAction != nil {
			if err := s.cfg.TeamAction(action, target, payload); err != nil {
				hint = fmt.Sprintf("操作失败: %v", err)
			} else {
				immediate = fmt.Sprintf("团队 %s 已创建", target)
			}
		} else {
			// 为 team.create 给出可在 terminal 直接粘贴的 CLI 命令, 便于主进程 (chat/feishu) 消费或用户手动执行。
			if payload != nil {
				wf, _ := payload["workflow"].(string)
				obj, _ := payload["objective"].(string)
				lang, _ := payload["lang"].(string)
				if wf != "" {
					cmd := fmt.Sprintf("claude-go team create %s %s %q", target, wf, obj)
					if lang != "" {
						cmd += " --lang " + lang
					}
					hint = "若无主进程消费, 可手动运行: " + cmd
				}
			}
		}
	case "team.run":
		if s.cfg.TeamAction != nil {
			if err := s.cfg.TeamAction(action, target, payload); err != nil {
				hint = fmt.Sprintf("操作失败: %v", err)
			} else {
				immediate = fmt.Sprintf("团队 %s 已开始运行", target)
			}
		} else {
			hint = "无主进程消费, 可手动运行: claude-go run \"/team run " + target + " <目标描述>\""
		}
	case "swarm.create", "swarm.predict":
		if payload != nil {
			obj, _ := payload["objective"].(string)
			if obj != "" {
				hint = fmt.Sprintf("若无主进程消费, 可手动运行: claude-go swarm predict %q", obj)
			}
		}
	case "swarm.simulate":
		if payload == nil {
			payload = map[string]interface{}{}
		}
		mode, _ := payload["mode"].(string)
		obj, _ := payload["objective"].(string)
		if obj == "" {
			if sc, ok := payload["scenario"].(string); ok {
				obj = sc
				payload["objective"] = sc
			}
		}
		if mode == "" {
			mode = "social"
			payload["mode"] = mode
		}
		hint = fmt.Sprintf("已排队 swarm.simulate: mode=%s objective=%q. 主进程可调用 swarm_intel.Engine.Simulate(ctx, cfg) 消费。",
			mode, obj)
	case "team.stop", "team.restart", "team.delete", "team.resume", "team.refine", "team.fork":
		if s.cfg.TeamAction != nil {
			if err := s.cfg.TeamAction(action, target, payload); err != nil {
				hint = fmt.Sprintf("操作失败: %v", err)
			} else {
				immediate = fmt.Sprintf("团队 %s 已执行 %s", target, action)
			}
		} else if s.cfg.BotAPIURL != "" {
			// 转发到 feishu bot 的 wiki API (已挂载带 TeamAction 的 dashboard)
			forwardURL := strings.TrimRight(s.cfg.BotAPIURL, "/") + r.URL.Path
			req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, forwardURL, r.Body)
			if err != nil {
				hint = fmt.Sprintf("转发失败: %v", err)
				break
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				hint = fmt.Sprintf("转发到 bot API 失败: %v", err)
				break
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var fwd actionResp
				if json.NewDecoder(resp.Body).Decode(&fwd) == nil {
					immediate = fwd.Message
					hint = fwd.Hint
				} else {
					immediate = fmt.Sprintf("团队 %s 已执行 %s", target, action)
				}
			} else {
				hint = fmt.Sprintf("Bot API 返回 %s", resp.Status)
			}
		} else {
			hint = "无主进程消费, 请通过飞书发送 /team stop " + target
		}
	}
	writeJSON(w, http.StatusOK, actionResp{
		OK:       true,
		Queued:   immediate == "",
		Message:  firstNonEmpty(immediate, "动作已排队, 等待 claude-go 主进程消费 (.claude-go/.dashboard/actions/)"),
		Hint:     hint,
		ActionID: actionID,
	})
}

func (s *Server) toggleCron(id, action string) error {
	path := s.provider.pathIn("cron", "cron_jobs.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	changed := false
	for i, j := range arr {
		jobID, _ := j["id"].(string)
		if fmt.Sprintf("%v", j["id"]) == id || jobID == id {
			switch action {
			case "enable", "resume":
				j["status"] = "active"
				j["enabled"] = true
			case "disable", "pause":
				j["status"] = "paused"
				j["enabled"] = false
			}
			arr[i] = j
			changed = true
		}
	}
	if !changed {
		return fmt.Errorf("cron %s not found", id)
	}
	newB, _ := json.MarshalIndent(arr, "", "  ")
	return os.WriteFile(path, newB, 0o644)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// =====================================================================
// /api/dreaming/diagnosis  -> 给出 Dreaming 为什么没跑的诊断
// =====================================================================

type dreamingDiagnosisResp struct {
	EnabledInConfig  bool     `json:"enabledInConfig"`
	DreamerWired     bool     `json:"dreamerWired"` // CLI main.go 是否接入了 Dreamer
	LastDreamAt      string   `json:"lastDreamAt,omitempty"`
	SessionsAccum    int      `json:"sessionsAccum"`
	MemoryDirExists  bool     `json:"memoryDirExists"`
	DreamLogCount    int      `json:"dreamLogCount"`
	Reasons          []string `json:"reasons"`
	Recommendations  []string `json:"recommendations"`
}

func (s *Server) handleDreamingDiagnosis(w http.ResponseWriter, r *http.Request) {
	resp := dreamingDiagnosisResp{}
	// 诊断 1: CLI 是否接入
	if data, err := os.ReadFile(filepath.Join(pickGoRoot(s.cfg.StateDir), "cmd", "claude-go", "main.go")); err == nil {
		resp.DreamerWired = strings.Contains(string(data), "dreaming.NewDreamer(")
	}
	// 诊断 2: memory 目录
	memDir := s.provider.pathIn("memory")
	if info, err := os.Stat(memDir); err == nil && info.IsDir() {
		resp.MemoryDirExists = true
	}
	// dream log 数量
	if entries, err := os.ReadDir(filepath.Join(memDir, "dreaming")); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".md") || strings.HasSuffix(e.Name(), ".json") {
				resp.DreamLogCount++
			}
		}
	}
	// 合成诊断
	if !resp.DreamerWired {
		resp.Reasons = append(resp.Reasons,
			"CLI 主入口 (cmd/claude-go/main.go) 未实例化 dreaming.NewDreamer(), 因此 AfterQuery 钩子永远不会触发。")
		resp.Recommendations = append(resp.Recommendations,
			"在 main.go 中构造 Dreamer 并将其注入 Session/Agent 生命周期 (参考 pkg/feishu/bot.go:533 的用法)。",
			"将 Dreamer.SetAPIClient/SetConsolidateFn 挂到全局, 并在每轮 Claude 查询完成后调用 dreamer.AfterQuery(ctx)。",
		)
	}
	if !resp.MemoryDirExists {
		resp.Reasons = append(resp.Reasons, ".claude-go/memory 目录不存在, Dreaming 无法产出整理文件。")
		resp.Recommendations = append(resp.Recommendations,
			"启动 claude-go CLI 一次让 basedir 初始化, 或手动 mkdir .claude-go/memory/dreaming。")
	}
	if resp.DreamerWired && resp.DreamLogCount == 0 {
		resp.Reasons = append(resp.Reasons,
			"尚未达到 DreamConfig.MinSessions 阈值, 或距上次 dream 不足 MinHours。")
		resp.Recommendations = append(resp.Recommendations,
			"在配置中临时降低 dreaming.minSessions 或使用 ForceDream 手动触发一次验证。")
	}
	writeJSON(w, http.StatusOK, resp)
}

// pickGoRoot 尝试从 stateDir 回溯出 repo 根 (含 cmd/claude-go/main.go)
func pickGoRoot(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	// stateDir 通常是 .../<project>/.claude-go , 往上走到 <project>, 再找 cmd/claude-go
	p := stateDir
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(p, "cmd", "claude-go", "main.go")
		if _, err := os.Stat(candidate); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return ""
}

// ============ small utils ============

func truncateString(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}

// ensure strconv usage (linked here for consistency in case future handlers need it).
var _ = strconv.Itoa
