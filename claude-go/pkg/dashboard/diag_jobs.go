// Package dashboard 的 v1.5 异步 LLM 诊断作业:
//
// 动机: Team / Dreaming / Evolution 的 LLM 诊断一次往往要调用 Anthropic 30~60s,
// 直接走同步 HTTP 会:
//   1. 阻塞前端 UI, 用户以为卡死
//   2. 在慢网络下超时, 白白浪费 token
//   3. 用户关闭页面就丢弃结果, 无从回看
//
// 解决方案 (受 LLM 批量推理 API 启发):
//   POST /api/diag/jobs {kind, target?}  ->  返回 jobID, 立刻 202
//   GET  /api/diag/jobs/{id}             ->  pending / running / done / failed
//   GET  /api/diag/jobs                  ->  最近 100 条作业, 用作用户历史
//
// 作业数据都放内存 (重启丢失), 这是有意的: 诊断是一次性 advisory, 重启后再诊断即可。
package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DiagJobStatus 作业生命周期状态。
type DiagJobStatus string

const (
	DiagPending DiagJobStatus = "pending"
	DiagRunning DiagJobStatus = "running"
	DiagDone    DiagJobStatus = "done"
	DiagFailed  DiagJobStatus = "failed"
)

// DiagJob 表示一次异步诊断作业。
// Result / LLM 在 status=done 时有效。
type DiagJob struct {
	ID         string        `json:"id"`
	Kind       string        `json:"kind"`   // team / dreaming / evolution
	Target     string        `json:"target"` // team name or ""
	Status     DiagJobStatus `json:"status"`
	CreatedAt  time.Time     `json:"createdAt"`
	StartedAt  time.Time     `json:"startedAt,omitempty"`
	FinishedAt time.Time     `json:"finishedAt,omitempty"`
	DurationMS int64         `json:"durationMs,omitempty"`
	// 输出
	Summary string     `json:"summary,omitempty"`
	LLM     LLMProfile `json:"llm,omitempty"`
	Error   string     `json:"error,omitempty"`
	// 用于 UI 展示: 说明这次诊断分析了哪些数据维度
	Scope []string `json:"scope,omitempty"`
}

// diagJobStore 提供并发安全的作业登记 + LRU 保留最近 N 条。
type diagJobStore struct {
	mu    sync.RWMutex
	order []string          // 插入顺序
	jobs  map[string]*DiagJob
	max   int
}

func newDiagJobStore(max int) *diagJobStore {
	if max <= 0 {
		max = 100
	}
	return &diagJobStore{jobs: map[string]*DiagJob{}, max: max}
}

func (s *diagJobStore) add(j *DiagJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
	s.order = append(s.order, j.ID)
	// LRU 清理
	for len(s.order) > s.max {
		oldID := s.order[0]
		s.order = s.order[1:]
		delete(s.jobs, oldID)
	}
}

func (s *diagJobStore) get(id string) (*DiagJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
}

func (s *diagJobStore) update(id string, mut func(*DiagJob)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok {
		mut(j)
	}
}

func (s *diagJobStore) list() []*DiagJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*DiagJob, 0, len(s.order))
	// 最新的在前面
	for i := len(s.order) - 1; i >= 0; i-- {
		if j, ok := s.jobs[s.order[i]]; ok {
			out = append(out, j)
		}
	}
	return out
}

// ----------------------------------------------------------------------
// HTTP 入口
// ----------------------------------------------------------------------

type diagJobCreateReq struct {
	Kind   string `json:"kind"`             // team / dreaming / evolution
	Target string `json:"target,omitempty"` // team name
}

type diagJobCreateResp struct {
	ID     string        `json:"id"`
	Status DiagJobStatus `json:"status"`
	Kind   string        `json:"kind"`
	Target string        `json:"target,omitempty"`
}

func (s *Server) handleDiagJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jobs := s.jobs.list()
		writeJSON(w, http.StatusOK, jobs)
	case http.MethodPost:
		s.handleDiagJobCreate(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("GET/POST only"))
	}
}

func (s *Server) handleDiagJobCreate(w http.ResponseWriter, r *http.Request) {
	var req diagJobCreateReq
	if r.Body != nil {
		defer r.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(r.Body, 16*1024))
		if len(body) > 0 {
			_ = json.Unmarshal(body, &req)
		}
	}
	req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	if req.Kind == "" {
		writeError(w, http.StatusBadRequest, errors.New("kind required (team|dreaming|evolution)"))
		return
	}
	if req.Kind == "team" && req.Target == "" {
		writeError(w, http.StatusBadRequest, errors.New("target (team name) required for kind=team"))
		return
	}

	jobID := fmt.Sprintf("diag-%s-%d", req.Kind, time.Now().UnixNano())
	j := &DiagJob{
		ID:        jobID,
		Kind:      req.Kind,
		Target:    req.Target,
		Status:    DiagPending,
		CreatedAt: time.Now(),
	}
	s.jobs.add(j)

	// 后台 goroutine 执行, 不依赖 r.Context() (HTTP 会早就关闭)
	go s.runDiagJob(jobID)

	writeJSON(w, http.StatusAccepted, diagJobCreateResp{
		ID: jobID, Status: DiagPending, Kind: req.Kind, Target: req.Target,
	})
}

func (s *Server) handleDiagJobDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/diag/jobs/")
	id = strings.Trim(id, "/")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("job id required"))
		return
	}
	j, ok := s.jobs.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("job %s not found", id))
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// runDiagJob 真正执行一次诊断 (后台)。
//
// 超时: 每个 job 最多跑 120s. 超过则作业标记为 failed。
// 理由: Anthropic Streaming 在网络良好时 < 30s, 120s 已经非常宽裕;
// 再长基本是网络异常或模型被限流, 继续等没意义。
func (s *Server) runDiagJob(jobID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	s.jobs.update(jobID, func(j *DiagJob) {
		j.Status = DiagRunning
		j.StartedAt = time.Now()
	})
	j, ok := s.jobs.get(jobID)
	if !ok {
		return
	}

	var (
		summary string
		profile LLMProfile
		scope   []string
		err     error
	)
	switch j.Kind {
	case "team":
		summary, profile, scope, err = s.diagnoseTeam(ctx, j.Target)
	case "dreaming":
		summary, profile, scope, err = s.diagnoseDreaming(ctx)
	case "evolution":
		summary, profile, scope, err = s.diagnoseEvolution(ctx)
	default:
		err = fmt.Errorf("unknown diag kind: %s", j.Kind)
	}

	s.jobs.update(jobID, func(j *DiagJob) {
		j.FinishedAt = time.Now()
		j.DurationMS = j.FinishedAt.Sub(j.StartedAt).Milliseconds()
		j.LLM = profile
		j.Scope = scope
		if err != nil {
			j.Status = DiagFailed
			j.Error = err.Error()
		} else {
			j.Status = DiagDone
			j.Summary = summary
		}
	})
}

// ----------------------------------------------------------------------
// 诊断任务具体实现
// ----------------------------------------------------------------------

// diagnoseTeam 复用现有 payload 构造, 只把同步 LLM 调用换成 90s 窗口。
func (s *Server) diagnoseTeam(ctx context.Context, name string) (string, LLMProfile, []string, error) {
	detail, err := s.provider.GetTeam(name)
	if err != nil {
		return "", LLMProfile{}, nil, fmt.Errorf("team %q not found: %w", name, err)
	}
	bb, _ := s.provider.Blackboard(name)
	bbMap := map[string]string{}
	if bb != nil {
		bbMap = bb.Map
	}

	payload := map[string]interface{}{
		"name":        detail.Name,
		"workflow":    detail.Workflow,
		"status":      detail.Status,
		"objective":   truncateString(detail.Objective, 1500),
		"error":       detail.Error,
		"durationSec": detail.DurationSec,
		"stagesTotal": detail.StagesTotal,
		"stagesDone":  detail.StagesDone,
		"stagesFail":  detail.StagesFail,
	}
	stages := []map[string]interface{}{}
	for _, st := range detail.Stages {
		stages = append(stages, map[string]interface{}{
			"name":     st.Name,
			"role":     st.Role,
			"status":   st.Status,
			"duration": st.DurationSec,
			"error":    st.Error,
			"output":   truncateString(st.Output, 600),
		})
	}
	payload["stages"] = stages
	payload["rounds"] = detail.AdversaryRounds
	if len(bbMap) > 0 {
		truncBB := map[string]string{}
		for k, v := range bbMap {
			truncBB[k] = truncateString(v, 400)
		}
		payload["blackboard"] = truncBB
	}
	if len(detail.RunMetrics) > 0 {
		summary := map[string]interface{}{}
		for _, m := range detail.RunMetrics {
			n := len(m.Points)
			if n == 0 {
				continue
			}
			summary[m.Name] = m.Points[n-1].Value
		}
		payload["latestMetrics"] = summary
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	sys := `你是一位资深的多智能体团队评审专家。根据给定的团队运行快照 (JSON), 输出中文诊断:
1. 运行过程: 阶段顺序 / 并发度 / 重试情况是否符合该 workflow 预期
2. 输出质量: 各阶段产出是否与 objective 匹配, 有无缺失/重复/偏题
3. 运行状态: 是否存在卡死 (长时间 running) / 错误堆积 / 黑板异常
4. 优化建议: 给出 3 条可执行建议 (按优先级), 必要时指出调整 workflow 阶段/角色分工
请用简洁 Markdown, 不超过 5 段, 避免复读 JSON。`
	user := "团队运行快照:\n```json\n" + string(b) + "\n```"
	summary, profile, err := LLMComplete(ctx, sys, user, 90*time.Second)
	return summary, profile, []string{
		"teams/" + name + "/team.json",
		"teams/" + name + "/blackboard.json",
		"teams/" + name + "/checkpoints.json",
		"metrics/teams.jsonl",
	}, err
}

// diagnoseDreaming 采集 memory/dreaming 目录 + 元数据, 让 LLM 点评 "做梦机制" 的运行情况。
func (s *Server) diagnoseDreaming(ctx context.Context) (string, LLMProfile, []string, error) {
	memDir := s.provider.pathIn("memory")
	dreamDir := filepath.Join(memDir, "dreaming")

	dreamFiles := []map[string]interface{}{}
	if entries, err := os.ReadDir(dreamDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, _ := e.Info()
			size := int64(0)
			if info != nil {
				size = info.Size()
			}
			preview := ""
			if strings.HasSuffix(e.Name(), ".md") || strings.HasSuffix(e.Name(), ".txt") {
				if b, err := os.ReadFile(filepath.Join(dreamDir, e.Name())); err == nil {
					preview = truncateString(string(b), 400)
				}
			}
			ts := ""
			if info != nil {
				ts = info.ModTime().Format(time.RFC3339)
			}
			dreamFiles = append(dreamFiles, map[string]interface{}{
				"name":    e.Name(),
				"size":    size,
				"modTime": ts,
				"preview": preview,
			})
		}
	}
	payload := map[string]interface{}{
		"memoryDirExists": dirExists(memDir),
		"dreamDir":        dreamDir,
		"dreamFiles":      dreamFiles,
		"dreamFileCount":  len(dreamFiles),
	}
	// 最近 7 天 dreaming_* 指标
	if events, err := s.provider.ModuleMetricEvents("dreaming", 1000, time.Now().Add(-7*24*time.Hour)); err == nil {
		counts := map[string]int{}
		for _, e := range events {
			counts[e.Name]++
		}
		payload["recentMetrics7d"] = counts
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	sys := `你是资深的智能记忆/做梦机制 (Dreaming) 评审专家。根据给定快照, 输出中文诊断:
1. 整体质量: dream 文件数量 / 大小 / 模式是否健康 (稀疏? 过密? 模板化?)
2. 运行状态: 最近 7 天是否有产出 (看 modTime / recentMetrics7d), 是否存在长期不触发
3. 被使用情况: dreaming 产出是否被下游消费 (有没有 retrieval 相关指标)
4. 优化建议: 3 条可执行建议, 涵盖触发频率 / 质量过滤 / 索引加速
请用 Markdown, 不超过 4 段。若数据极少则以 "尚未充分运行" 为第一判断。`
	user := "Dreaming 快照:\n```json\n" + string(b) + "\n```"
	summary, profile, err := LLMComplete(ctx, sys, user, 90*time.Second)
	return summary, profile, []string{
		"memory/dreaming/*.md",
		"metrics/dreaming.jsonl",
	}, err
}

// diagnoseEvolution 读取 evolution 目录下的元数据, 让 LLM 判断 "进化机制" 是否良性。
func (s *Server) diagnoseEvolution(ctx context.Context) (string, LLMProfile, []string, error) {
	evoDir := s.provider.pathIn("evolution")
	files := []map[string]interface{}{}
	if entries, err := os.ReadDir(evoDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, _ := e.Info()
			size := int64(0)
			ts := ""
			if info != nil {
				size = info.Size()
				ts = info.ModTime().Format(time.RFC3339)
			}
			preview := ""
			if strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".md") {
				if b, err := os.ReadFile(filepath.Join(evoDir, e.Name())); err == nil {
					preview = truncateString(string(b), 400)
				}
			}
			files = append(files, map[string]interface{}{
				"name": e.Name(), "size": size, "modTime": ts, "preview": preview,
			})
		}
	}
	payload := map[string]interface{}{
		"evoDirExists": dirExists(evoDir),
		"files":        files,
		"fileCount":    len(files),
	}
	if events, err := s.provider.ModuleMetricEvents("evolution", 1000, time.Now().Add(-30*24*time.Hour)); err == nil {
		counts := map[string]int{}
		for _, e := range events {
			counts[e.Name]++
		}
		payload["recentMetrics30d"] = counts
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	sys := `你是智能体进化 (Evolution) 机制评审专家。给定快照, 输出中文诊断:
1. 数据质量: 进化记录是否连续, 是否有退化迹象 (分数/指标走低)
2. 运行状态: 最近 30 天有无运行, 迭代频率是否合理
3. 被使用情况: 进化结果是否被上层消费 (例如 prompt 更新 / agent 重建)
4. 优化建议: 3 条, 涵盖评测覆盖 / 回滚策略 / 触发阈值
请用 Markdown, 不超过 4 段。`
	user := "Evolution 快照:\n```json\n" + string(b) + "\n```"
	summary, profile, err := LLMComplete(ctx, sys, user, 90*time.Second)
	return summary, profile, []string{
		"evolution/*.json",
		"metrics/evolution.jsonl",
	}, err
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// ----------------------------------------------------------------------
// /api/dreaming/trigger  -  主动触发一次 dreaming
//
// 目前 Dreamer 实例在主进程 (feishu bot / cli). Dashboard 作为观测层并不持有
// Dreamer 对象, 因此这里采用 "写触发文件" 的方式, 与 actions 目录一致:
// 主进程的 watcher 会消费该信号。同时返回一个 hint 告诉用户:
// 若主进程未在运行, 触发只是排队, 不会立刻产生 dream 文件。
// ----------------------------------------------------------------------

type dreamingTriggerResp struct {
	Queued  bool   `json:"queued"`
	File    string `json:"file"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (s *Server) handleDreamingTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("POST only"))
		return
	}
	triggerDir := filepath.Join(s.cfg.StateDir, ".dashboard", "triggers")
	if err := os.MkdirAll(triggerDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	file := filepath.Join(triggerDir, fmt.Sprintf("dream-%d.json", time.Now().UnixNano()))
	body := map[string]interface{}{
		"type":      "dreaming.force",
		"requested": time.Now().Format(time.RFC3339),
		"source":    "dashboard",
	}
	b, _ := json.MarshalIndent(body, "", "  ")
	if err := os.WriteFile(file, b, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, dreamingTriggerResp{
		Queued:  true,
		File:    file,
		Message: "已排队 Dreaming 触发信号",
		Hint:    "主进程 (feishu bot / daemon) 会在下一次 tick 消费该文件; 若主进程未运行, 仅落盘不生效。",
	})
}
