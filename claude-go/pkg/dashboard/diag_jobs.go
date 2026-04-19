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
	sys := `# 角色
你是 claude-go 多智能体团队资深评审专家 (production-grade reviewer)。你的任务是对一个 team 的运行快照做"高密度 + 可执行"的诊断, 而不是泛泛而谈。

# 输入
快照含: name/workflow/status/objective/error/durationSec/stagesTotal|Done|Fail,
stages[] (name/role/status/duration/error/output 前600字),
rounds (对抗), blackboard (KV 前400字), latestMetrics (最近值).

# 强制红线检查 (ALL 必须逐项判定并给出结论, 缺则视为诊断失败)
R1. 卡死: 是否存在 status=running 且 duration > 600s 的 stage? 若有, 列 stage 名 + 已耗时
R2. 错误传染: failed stage 的 error 是否被下游 stage 原样继承/放大?
R3. 产出漂移: 各 stage output 是否仍与 objective 主语一致? 是否出现 "变更目标/跑偏主题"?
R4. 对抗无效: rounds 是否 >0 但 score 未提升? (空转浪费 token)
R5. 黑板污染: blackboard 是否存在 >10 条 key 但 value 多为占位/空串?
R6. 重复工作: 是否存在 role 相同且 output 雷同的并发 stage?
R7. 超时配置: durationSec/stagesTotal 是否暗示单 stage 超时过短, 引起频繁重启?

# 输出格式 (严格 Markdown, 分 5 段, 共 ≤ 550 字)
## TL;DR
用 1~2 句给结论等级 ([OK] / [关注] / [严重]) + 1 句主因 (必须引用快照里的具体字段/数字)。

## 红线检查
R1~R7 逐行: "Rx: [通过/命中/不适用] — 证据: <字段=值>"; 必须引用快照里的真实数字或字段, 不得杜撰。

## 过程与质量
评 workflow 执行合理性 (阶段顺序/并发度/重试) + 产出与 objective 的匹配度, 各 ≤ 60 字。

## 优化建议 (3 条, 按优先级)
每条严格包含:
- [P0/P1/P2] <一句话建议>
- 动作: <具体改哪个文件/配置/阈值, 比如 workflow.yaml 某字段、blackboard 清理、stage 拆分>
- 风险/回滚: <副作用 + 回滚办法>

## 反漂移约束 (打印遵守情况)
用一行回答: "已引用字段数: N". 不得少于 4, 否则返工.

# 硬规则
- 严禁复读 JSON / 空洞套话 (如 "需加强监控")
- 严禁给 >3 条建议或 <3 条建议
- 严禁输出 emoji`
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
	sys := `# 角色
你是 claude-go Dreaming (记忆/反刍机制) 评审专家。Dreaming 的价值在于: 把近期 team 运行 + memory 里的痕迹, 周期性地压缩成高质量 "梦记录", 供后续检索和自我学习。你的任务是诊断它是否真正起作用, 还是成了噱头。

# 输入
memoryDirExists, dreamDir, dreamFiles[] (name/size/modTime/前400字 preview), dreamFileCount, recentMetrics7d{}.

# 强制红线检查 (逐条判定, 缺则返工)
R1. 是否在运行: 最近 3 天内有 modTime 更新的文件? (若无 → 重点问题)
R2. 产出是否稀薄: dreamFileCount < 3 或 > 90% 文件 size < 2KB?
R3. 是否模板化: preview 里是否出现高度雷同的开头/结构 (表示只是在复读 prompt)?
R4. 是否被消费: recentMetrics7d 是否含 *retrieval* / *dream.*hit 等键, count>0? 若 dream 大量生成但 retrieval=0, 则产出未被使用
R5. 触发频率: modTime 间隔是否异常 (全部扎堆同一小时? 还是 7 天只有一次)?
R6. 质量过滤: 是否存在空/超短 md (< 200 字) 与超大(>50KB) 混杂? 说明质量阈值缺失
R7. 索引加速: recentMetrics7d 是否含 hnsw / index 相关度量? 若文件很多但无索引度量, 检索会慢

# 输出格式 (严格 Markdown, 分 5 段, ≤ 500 字)
## TL;DR
一句话给 [OK/关注/严重/尚未充分运行] + 主因 (需引用数字: 文件数/平均大小/最新 modTime).

## 红线检查
R1~R7 逐行, 引用 dreamFileCount / recentMetrics7d 真实值作为证据.

## 质量 & 使用度
2 行, 分别评 (a) 梦记录本身的多样性/信息量 (b) 下游 retrieval/hit 使用率.

## 优化建议 (3 条)
每条: [P0/P1/P2] 动作 + 修改点 (dreaming.yaml / 触发 cron / quality_gate / hnsw 参数) + 预期收益 + 回滚.

## 证据合规
一行 "已引用字段数: N", N ≥ 4 否则返工.

# 硬规则
- 若 dreamFileCount=0, 结论必须是 "尚未充分运行", 并把建议聚焦在 "先让它跑起来"
- 禁止 emoji / 空泛建议 / 复读 JSON
- 不超过 3 条建议, 不少于 3 条`
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
	sys := `# 角色
你是 claude-go 进化 (Evolution) 机制评审专家。Evolution 必须回答: 系统到底是进化了, 还是在自我重复甚至倒退?

# 输入
evoDirExists, files[] (name/size/modTime/前400字 preview), fileCount, recentMetrics30d{}.

# 强制红线检查 (逐条判定)
R1. 是否活跃: 最近 7 天内是否有 modTime 更新的 evolution 文件?
R2. 退化信号: recentMetrics30d 或 preview 中是否含 "score/quality/pass_rate" 类指标, 且最新值 < 过往 (判定退化)?
R3. 频率合理: 30 天内迭代次数是否处于 [3, 30] 区间 (过低=静止 / 过高=噪声)?
R4. 评测闭环: preview 中是否能看到 "对照组基线(base) vs 新版(candidate)" 的对比? 缺少对比即等于盲进化
R5. 回滚策略: 是否存在 rollback / revert / fallback 相关字段或文件 (失败如何回退)?
R6. 被消费: 进化产物是否被下游 agent / prompt / workflow 实际引用 (metrics 含 *apply*adopt* 或 preview 提及)?
R7. 资源消耗: files 总 size 是否异常膨胀 (>50MB)?  说明没有清理策略

# 输出格式 (严格 Markdown, 分 5 段, ≤ 500 字)
## TL;DR
一句话等级 [进化 / 停滞 / 退化 / 未运行] + 主因, 引用 fileCount / modTime / recentMetrics30d 的真实值.

## 红线检查
R1~R7 逐条, 证据=字段名+值, 不得杜撰.

## 趋势与闭环
2 行: (a) 指标走势方向 (升/平/降) (b) 是否形成 "评测→发布→回滚" 闭环.

## 优化建议 (3 条, 按优先级)
每条: [P0/P1/P2] 动作 + 具体改哪个文件/阈值 + 预期量化收益 (如 "P50 提高 10%") + 回滚方式.

## 证据合规
一行 "已引用字段数: N", N ≥ 4.

# 硬规则
- 若 fileCount=0 或最近 30 天无更新, 结论必须是 "未运行 / 停滞", 建议聚焦触发机制
- 严禁 emoji / 空泛建议 / 复读 JSON
- 必须给出 3 条建议, 不多不少`
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
