package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ============== Cron ==============

type rawCronJob struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Schedule   string    `json:"schedule"`
	JobType    string    `json:"jobType"`
	Payload    string    `json:"payload"`
	ChatID     string    `json:"chatId"`
	Workflow   string    `json:"workflow,omitempty"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"createdAt"`
	LastRunAt  time.Time `json:"lastRunAt,omitempty"`
	LastResult string    `json:"lastResult,omitempty"`
	RunCount   int       `json:"runCount"`
	FailCount  int       `json:"failCount"`
}

// ListCronJobs 读取 cron/cron_jobs.json。
func (p *Provider) ListCronJobs() ([]CronJobDTO, error) {
	path := p.pathIn("cron", "cron_jobs.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []CronJobDTO{}, nil
		}
		return nil, err
	}
	var raw []rawCronJob
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]CronJobDTO, 0, len(raw))
	for _, j := range raw {
		out = append(out, CronJobDTO{
			ID: j.ID, Name: j.Name, Schedule: j.Schedule,
			JobType: j.JobType, Workflow: j.Workflow, Payload: j.Payload,
			ChatID: j.ChatID, Enabled: j.Enabled,
			CreatedAt: j.CreatedAt, LastRunAt: j.LastRunAt, LastResult: j.LastResult,
			RunCount: j.RunCount, FailCount: j.FailCount,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// ============== Dreaming ==============

// LoadDreaming 读取梦境日志、memory 目录内容, 汇总出 DreamingResp。
func (p *Provider) LoadDreaming() (*DreamingResp, error) {
	resp := &DreamingResp{Enabled: true}

	// dream 日志在 memory/dreaming/ (按 dreamer 实现), 退化情况下尝试 memory/ 本身
	candidates := []string{
		p.pathIn("memory", "dreaming"),
		p.pathIn("memory"),
	}
	seen := map[string]bool{}
	for _, dir := range candidates {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if !strings.HasPrefix(e.Name(), "dream-") || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			full := filepath.Join(dir, e.Name())
			if seen[full] {
				continue
			}
			seen[full] = true
			info, err := e.Info()
			if err != nil {
				continue
			}
			preview := ""
			if b, err := os.ReadFile(full); err == nil {
				preview = string(b)
				if len(preview) > 400 {
					preview = preview[:400] + "…"
				}
			}
			resp.Logs = append(resp.Logs, DreamLogDTO{
				Filename: e.Name(), Path: full,
				Size: info.Size(), ModTime: info.ModTime(), Preview: preview,
			})
		}
	}
	sort.Slice(resp.Logs, func(i, j int) bool {
		return resp.Logs[i].ModTime.After(resp.Logs[j].ModTime)
	})
	resp.DreamCount = len(resp.Logs)
	if len(resp.Logs) > 0 {
		resp.LastDreamAt = resp.Logs[0].ModTime
	}

	// memory 目录下的其它 .md 文件
	resp.MemoryDir = p.pathIn("memory")
	if entries, err := os.ReadDir(resp.MemoryDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if strings.HasPrefix(e.Name(), "dream-") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			resp.MemoryFiles = append(resp.MemoryFiles, MemoryFileDTO{
				Filename: e.Name(), Size: info.Size(), ModTime: info.ModTime(),
			})
		}
		sort.Slice(resp.MemoryFiles, func(i, j int) bool {
			return resp.MemoryFiles[i].ModTime.After(resp.MemoryFiles[j].ModTime)
		})
	}

	if data, err := os.ReadFile(filepath.Join(resp.MemoryDir, "index.md")); err == nil {
		resp.IndexMarkdown = string(data)
	}

	// Dream 指标
	if sum, err := p.ModuleMetricSummary("dreaming"); err == nil {
		resp.MetricSummary = sum
	}
	return resp, nil
}

// ============== Evolution ==============

type rawExperience struct {
	ID           string    `json:"id"`
	Category     string    `json:"category"`
	Role         string    `json:"role,omitempty"`
	Content      string    `json:"content"`
	Quality      float64   `json:"quality"`
	UsageCount   int       `json:"usageCount"`
	SuccessCount int       `json:"successCount"`
	Tags         []string  `json:"tags,omitempty"`
	Source       string    `json:"source"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type rawTrajectory struct {
	ID        string    `json:"id"`
	TeamName  string    `json:"teamName"`
	StageName string    `json:"stageName"`
	Role      string    `json:"role"`
	Objective string    `json:"objective"`
	Input     string    `json:"input,omitempty"`
	Output    string    `json:"output,omitempty"`
	Error     string    `json:"error,omitempty"`
	Success   bool      `json:"success"`
	Duration  string    `json:"duration"`
	Timestamp time.Time `json:"timestamp"`
}

// LoadEvolution 读取进化引擎数据。
func (p *Provider) LoadEvolution() (*EvolutionResp, error) {
	resp := &EvolutionResp{
		CategoryCounts: map[string]int{},
		RoleCounts:     map[string]int{},
	}
	expPath := p.pathIn("evolution", "experiences.json")
	trajPath := p.pathIn("evolution", "trajectories.json")

	var exps []rawExperience
	if data, err := os.ReadFile(expPath); err == nil {
		_ = json.Unmarshal(data, &exps)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var trajs []rawTrajectory
	if data, err := os.ReadFile(trajPath); err == nil {
		_ = json.Unmarshal(data, &trajs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	resp.TotalExperiences = len(exps)
	resp.TotalTrajectories = len(trajs)
	if len(exps) > 0 {
		qSum := 0.0
		for _, e := range exps {
			qSum += e.Quality
			resp.TotalUsageCount += e.UsageCount
			resp.CategoryCounts[defaultStr(e.Category, "unknown")]++
			if e.Role != "" {
				resp.RoleCounts[e.Role]++
			}
		}
		resp.AvgQuality = qSum / float64(len(exps))
	}

	// 质量分桶 (10 个桶, 0-1)
	hist := make([]HistBucket, 10)
	for i := range hist {
		hist[i].From = float64(i) / 10
		hist[i].To = float64(i+1) / 10
	}
	for _, e := range exps {
		q := e.Quality
		if q < 0 {
			q = 0
		}
		if q >= 1 {
			q = 0.9999
		}
		idx := int(q * 10)
		if idx < 0 {
			idx = 0
		}
		if idx > 9 {
			idx = 9
		}
		hist[idx].Count++
	}
	resp.QualityHistogram = hist

	// Top experiences: 按 usage*0.6 + quality*0.4
	sorted := make([]rawExperience, len(exps))
	copy(sorted, exps)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		score := func(e rawExperience) float64 {
			return float64(e.UsageCount)*0.6 + e.Quality*0.4
		}
		return score(a) > score(b)
	})
	topN := 10
	if len(sorted) < topN {
		topN = len(sorted)
	}
	for _, e := range sorted[:topN] {
		dto := ExperienceDTO{
			ID: e.ID, Category: e.Category, Role: e.Role,
			Content: e.Content, Quality: e.Quality,
			UsageCount: e.UsageCount, SuccessCount: e.SuccessCount,
			Tags: e.Tags, Source: e.Source,
			CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
		}
		if e.UsageCount > 0 {
			dto.SuccessRate = float64(e.SuccessCount) / float64(e.UsageCount)
		}
		resp.TopExperiences = append(resp.TopExperiences, dto)
	}

	// 成功率 (基于 trajectories)
	if len(trajs) > 0 {
		succ := 0
		for _, t := range trajs {
			if t.Success {
				succ++
			}
		}
		resp.SuccessRate = float64(succ) / float64(len(trajs))
	}

	// 最近 30 条轨迹
	sort.Slice(trajs, func(i, j int) bool {
		return trajs[i].Timestamp.After(trajs[j].Timestamp)
	})
	limit := 30
	if len(trajs) < limit {
		limit = len(trajs)
	}
	for _, t := range trajs[:limit] {
		resp.RecentTrajectories = append(resp.RecentTrajectories, TrajectoryDTO{
			ID: t.ID, TeamName: t.TeamName, StageName: t.StageName,
			Role: t.Role, Objective: t.Objective,
			Success: t.Success, Duration: t.Duration,
			Timestamp: t.Timestamp, Error: t.Error,
		})
	}

	// 指标摘要
	if sum, err := p.ModuleMetricSummary("evolution"); err == nil {
		resp.MetricSummary = sum
	}
	// 热力图 (category × role) 从全部经验构造, 而不是 top10
	var allForHeat []ExperienceDTO
	for _, e := range exps {
		allForHeat = append(allForHeat, ExperienceDTO{
			Category: e.Category, Role: e.Role,
		})
	}
	resp.Heatmap = p.BuildHeatmap(allForHeat)
	return resp, nil
}

// ============== Tasks ==============

type rawTask struct {
	ID          string    `json:"id"`
	Subject     string    `json:"subject"`
	Description string    `json:"description,omitempty"`
	Status      string    `json:"status"`
	Owner       string    `json:"owner,omitempty"`
	Priority    int       `json:"priority,omitempty"`
	DependsOn   []string  `json:"dependsOn,omitempty"`
	CreatedAt   time.Time `json:"createdAt,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
}

// ListTasks 读取 V2 tasks。
func (p *Provider) ListTasks() ([]TaskDTO, error) {
	path := p.pathIn("tasks", "tasks.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []TaskDTO{}, nil
		}
		return nil, err
	}
	// tasks.json 可能是数组或 { tasks: [...] } 结构
	var arr []rawTask
	if err := json.Unmarshal(data, &arr); err != nil {
		var wrap struct {
			Tasks []rawTask `json:"tasks"`
		}
		if err2 := json.Unmarshal(data, &wrap); err2 != nil {
			return nil, err
		}
		arr = wrap.Tasks
	}
	out := make([]TaskDTO, 0, len(arr))
	for _, t := range arr {
		out = append(out, TaskDTO{
			ID: t.ID, Subject: t.Subject, Description: t.Description,
			Status: t.Status, Owner: t.Owner, Priority: t.Priority,
			DependsOn: t.DependsOn, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		})
	}
	return out, nil
}

// ============== Overview ==============

// BuildOverview 汇总首页数据。
func (p *Provider) BuildOverview() (*OverviewResp, error) {
	resp := &OverviewResp{
		StateDir: p.stateDir,
		Ready:    p.Exists(),
	}
	if !resp.Ready {
		return resp, nil
	}

	// Teams
	teams, _ := p.ListTeams()
	resp.TotalTeams = len(teams)
	for _, t := range teams {
		switch t.Status {
		case "running":
			resp.RunningTeams++
		case "failed":
			resp.FailedTeams++
		}
	}
	// Recent: 取前 8
	limit := 8
	if len(teams) < limit {
		limit = len(teams)
	}
	resp.RecentRuns = teams[:limit]

	// 最近 7 天每日聚合
	resp.DailyRuns = buildDailyBuckets(teams, 7)

	// Cron
	crons, _ := p.ListCronJobs()
	for _, c := range crons {
		if c.Enabled {
			resp.ActiveCrons++
		}
	}

	// Evolution
	if evo, err := p.LoadEvolution(); err == nil {
		resp.TotalExps = evo.TotalExperiences
		resp.AvgQuality = evo.AvgQuality
	}

	// Dreaming
	if dr, err := p.LoadDreaming(); err == nil {
		resp.DreamEnabled = dr.Enabled
		resp.DreamCount = dr.DreamCount
		resp.LastDreamAt = dr.LastDreamAt
	}

	// Alerts: 取每个模块的 TrendAlerts
	if sums, err := p.AllMetricSummaries(); err == nil {
		for _, s := range sums {
			for _, a := range s.TrendAlerts {
				resp.Alerts = append(resp.Alerts, fmt.Sprintf("[%s] %s", s.Module, a))
			}
		}
	}

	return resp, nil
}

func buildDailyBuckets(teams []TeamSummary, days int) []DailyBucket {
	if days <= 0 {
		days = 7
	}
	today := time.Now().Truncate(24 * time.Hour)
	buckets := make([]DailyBucket, days)
	durations := make([][]float64, days)
	for i := 0; i < days; i++ {
		d := today.AddDate(0, 0, -(days - 1 - i))
		buckets[i].Date = d.Format("01-02")
	}
	for _, t := range teams {
		ref := t.CreatedAt
		if ref.IsZero() {
			continue
		}
		day := ref.Truncate(24 * time.Hour)
		diff := int(today.Sub(day).Hours() / 24)
		idx := days - 1 - diff
		if idx < 0 || idx >= days {
			continue
		}
		buckets[idx].Runs++
		switch t.Status {
		case "completed":
			buckets[idx].Successes++
		case "failed":
			buckets[idx].Failures++
		}
		if t.DurationSec > 0 {
			durations[idx] = append(durations[idx], t.DurationSec)
		}
	}
	for i, ds := range durations {
		if len(ds) > 0 {
			sum := 0.0
			for _, d := range ds {
				sum += d
			}
			buckets[i].AvgDuration = sum / float64(len(ds))
		}
	}
	return buckets
}
