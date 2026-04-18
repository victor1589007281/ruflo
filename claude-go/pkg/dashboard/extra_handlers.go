package dashboard

import (
	"bufio"
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
	DependsOn []string `json:"dependsOn,omitempty"`
	Parallel  bool     `json:"parallel,omitempty"`
}

type workflowDefDTO struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Mode        string             `json:"mode,omitempty"`
	Rounds      int                `json:"rounds,omitempty"`
	Stages      []workflowStageDTO `json:"stages"`
}

func (s *Server) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	list := agent.ListWorkflows()
	out := make([]workflowDefDTO, 0, len(list))
	for i := range list {
		wf := list[i]
		out = append(out, toWorkflowDTO(&wf))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleWorkflow(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/workflows/")
	name = strings.Trim(name, "/")
	if name == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("workflow name required"))
		return
	}
	wf := agent.GetWorkflow(name)
	if wf == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("workflow %q not found", name))
		return
	}
	writeJSON(w, http.StatusOK, toWorkflowDTO(wf))
}

func toWorkflowDTO(wf *agent.WorkflowDef) workflowDefDTO {
	dto := workflowDefDTO{
		Name: wf.Name, Description: wf.Description, Mode: wf.Mode, Rounds: wf.Rounds,
	}
	for _, st := range wf.Stages {
		dto.Stages = append(dto.Stages, workflowStageDTO{
			Name: st.Name, Role: st.Role,
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
	path := s.resolveLogPath(r)
	if path == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"path": "", "lines": []string{}})
		return
	}
	lines, err := tailLines(path, n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":  path,
		"lines": lines,
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

	path := s.resolveLogPath(r)
	if path == "" {
		fmt.Fprintf(w, "event: error\ndata: no log file found\n\n")
		flusher.Flush()
		return
	}
	fmt.Fprintf(w, "event: info\ndata: tailing %s\n\n", path)
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
	events, err := s.provider.ModuleMetricEvents(module, limit, since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
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
	OK        bool   `json:"ok"`
	Queued    bool   `json:"queued"`
	Message   string `json:"message,omitempty"`
	ActionID  string `json:"actionId,omitempty"`
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
	switch kind + "." + action {
	case "cron.enable", "cron.disable", "cron.pause", "cron.resume":
		if err := s.toggleCron(target, action); err == nil {
			immediate = fmt.Sprintf("cron %s=%s 已写入磁盘", target, action)
		}
	}
	writeJSON(w, http.StatusOK, actionResp{
		OK:       true,
		Queued:   true,
		Message:  firstNonEmpty(immediate, "动作已排队, 等待 claude-go 主进程消费 (.claude-go/.dashboard/actions/)"),
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
