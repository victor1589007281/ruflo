// Package dashboard 提供 claude-go 运行数据的只读 Web 可视化。
//
// 设计原则:
//   - 只读: 仅扫描本地文件系统, 不写入任何数据, 不提供 mutating API。
//   - 零依赖: 前端资源 embed 进二进制, 离线可用。
//   - 低耦合: 仅依赖 basedir 定位数据目录, 不 import 其他 agent/metrics 包。
//     (避免 agent/teams 等包的初始化副作用如 cron goroutine)
//
// 具体设计见 docs/claude-go-dashboard-design.md。
package dashboard

import "time"

// TeamSummary 团队列表卡片使用的轻量摘要。
type TeamSummary struct {
	Name        string    `json:"name"`
	Workflow    string    `json:"workflow"`
	Status      string    `json:"status"`
	Objective   string    `json:"objective"`
	ChatID      string    `json:"chatId,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	FinishedAt  time.Time `json:"finishedAt,omitempty"`
	DurationSec float64   `json:"durationSec"`
	StagesTotal int       `json:"stagesTotal"`
	StagesDone  int       `json:"stagesDone"`
	StagesFail  int       `json:"stagesFail"`
	AgentsTotal int       `json:"agentsTotal"`
	Error       string    `json:"error,omitempty"`
}

// StageDTO 团队阶段展示数据。
type StageDTO struct {
	Name        string    `json:"name"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	Input       string    `json:"input,omitempty"`
	Output      string    `json:"output,omitempty"`
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	Duration    string    `json:"duration,omitempty"`
	DurationSec float64   `json:"durationSec"`
}

// AgentDTO 团队成员展示数据。
type AgentDTO struct {
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	Status   string    `json:"status"`
	Result   string    `json:"result,omitempty"`
	Error    string    `json:"error,omitempty"`
	Phase    string    `json:"phase,omitempty"`    // 实时: 当前阶段 (运行中心跳回填)
	LastBeat time.Time `json:"lastBeat,omitempty"` // 实时: 上次心跳时间
}

// ProgressDTO 团队实时进展 (运行中由 Coordinator 心跳回填, 只读展示)。
// 兼作 team.json 中 progress 字段的解析目标 (json tag 对齐)。
type ProgressDTO struct {
	Phase        string    `json:"phase,omitempty"`
	Iteration    int       `json:"iteration,omitempty"`
	BytesWritten int64     `json:"bytesWritten,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt,omitempty"`
	TaskID       string    `json:"taskId,omitempty"`
}

// TeamDetail 团队详情。
type TeamDetail struct {
	TeamSummary
	Cwd              string              `json:"cwd,omitempty"`
	Progress         *ProgressDTO        `json:"progress,omitempty"` // 运行中实时进展
	Agents           []AgentDTO          `json:"agents"`
	Stages           []StageDTO          `json:"stages"`
	Report           string              `json:"report,omitempty"`
	TaskIDs          map[string]string   `json:"taskIds,omitempty"`
	AdversaryRounds  []AdversaryRoundDTO `json:"adversaryRounds,omitempty"`
	RunMetrics       []RunMetricSeries   `json:"runMetrics,omitempty"`
}

// AdversaryRoundDTO 对抗循环某一轮的多维评分 (从 blackboard 的 eval-*-score 解析得到)。
// 分数区间 0-10, 与 agent.EvalScore 一致。
// Phase 区分来源: "global" = 经典对抗循环, "task-{name}" = Orchestrator 任务级对抗。
type AdversaryRoundDTO struct {
	Round           int     `json:"round"`
	Phase           string  `json:"phase,omitempty"`
	Correctness     float64 `json:"correctness"`
	Completeness    float64 `json:"completeness"`
	Security        float64 `json:"security"`
	CodeQuality     float64 `json:"codeQuality"`
	DesignAlignment float64 `json:"designAlignment"`
	AvgScore        float64 `json:"avgScore"`
	Passed          bool    `json:"passed"`
	Raw             string  `json:"raw,omitempty"`
}

// RunMetricSeries 单个指标在某 run 的时序点。
type RunMetricSeries struct {
	Name   string             `json:"name"`
	Points []MetricPoint      `json:"points"`
	Labels map[string]string  `json:"labels,omitempty"`
}

// MetricPoint 单个时序点。
type MetricPoint struct {
	Timestamp time.Time `json:"ts"`
	Value     float64   `json:"value"`
}

// MetricEventDTO 单条原始指标点。
type MetricEventDTO struct {
	Timestamp time.Time         `json:"ts"`
	Module    string            `json:"module"`
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Labels    map[string]string `json:"labels,omitempty"`
	RunID     string            `json:"run_id,omitempty"`
}

// MetricStatDTO 单指标聚合。
type MetricStatDTO struct {
	Name   string  `json:"name"`
	Count  int     `json:"count"`
	Last   float64 `json:"last"`
	Avg    float64 `json:"avg"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	StdDev float64 `json:"stdDev"`
	Trend  string  `json:"trend"`
}

// ModuleSummaryDTO 模块指标摘要。
type ModuleSummaryDTO struct {
	Module       string                    `json:"module"`
	SnapshotTime time.Time                 `json:"snapshotTime"`
	Metrics      map[string]*MetricStatDTO `json:"metrics"`
	TrendAlerts  []string                  `json:"trendAlerts,omitempty"`
	EventCount   int                       `json:"eventCount"`
}

// DailyBucket 每日聚合点 (用于 7 天柱状图)。
type DailyBucket struct {
	Date        string  `json:"date"`
	Runs        int     `json:"runs"`
	Successes   int     `json:"successes"`
	Failures    int     `json:"failures"`
	AvgDuration float64 `json:"avgDuration"`
}

// OverviewResp 首页数据。
type OverviewResp struct {
	StateDir     string        `json:"stateDir"`
	Ready        bool          `json:"ready"`
	TotalTeams   int           `json:"totalTeams"`
	RunningTeams int           `json:"runningTeams"`
	FailedTeams  int           `json:"failedTeams"`
	TotalExps    int           `json:"totalExperiences"`
	AvgQuality   float64       `json:"avgQuality"`
	ActiveCrons  int           `json:"activeCrons"`
	RecentRuns   []TeamSummary `json:"recentRuns"`
	DailyRuns    []DailyBucket `json:"dailyRuns"`
	Alerts       []string      `json:"alerts"`
	DreamEnabled bool          `json:"dreamEnabled"`
	LastDreamAt  time.Time     `json:"lastDreamAt,omitempty"`
	DreamCount   int           `json:"dreamCount"`
}

// CronJobDTO 定时任务展示数据。
type CronJobDTO struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Schedule   string    `json:"schedule"`
	JobType    string    `json:"jobType"`
	Workflow   string    `json:"workflow,omitempty"`
	Payload    string    `json:"payload"`
	ChatID     string    `json:"chatId,omitempty"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"createdAt"`
	LastRunAt  time.Time `json:"lastRunAt,omitempty"`
	LastResult string    `json:"lastResult,omitempty"`
	RunCount   int       `json:"runCount"`
	FailCount  int       `json:"failCount"`
}

// DreamingResp Dreaming 概览与历史。
type DreamingResp struct {
	Enabled       bool          `json:"enabled"`
	MemoryDir     string        `json:"memoryDir"`
	LastDreamAt   time.Time     `json:"lastDreamAt,omitempty"`
	DreamCount    int           `json:"dreamCount"`
	Logs          []DreamLogDTO `json:"logs"`
	MetricSummary *ModuleSummaryDTO `json:"metricSummary,omitempty"`
	MemoryFiles   []MemoryFileDTO   `json:"memoryFiles"`
	IndexMarkdown string            `json:"indexMarkdown,omitempty"`
}

// DreamLogDTO 单次 dream 记录 (从 memory/dreaming/dream-*.md 衍生)。
type DreamLogDTO struct {
	Filename string    `json:"filename"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"modTime"`
	Preview  string    `json:"preview,omitempty"`
}

// MemoryFileDTO 记忆文件卡片。
type MemoryFileDTO struct {
	Filename string    `json:"filename"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"modTime"`
	Topics   []string  `json:"topics,omitempty"`
}

// EvolutionResp 进化机制总览。
type EvolutionResp struct {
	TotalExperiences  int                `json:"totalExperiences"`
	TotalTrajectories int                `json:"totalTrajectories"`
	AvgQuality        float64            `json:"avgQuality"`
	TotalUsageCount   int                `json:"totalUsageCount"`
	SuccessRate       float64            `json:"successRate"`
	CategoryCounts    map[string]int     `json:"categoryCounts"`
	RoleCounts        map[string]int     `json:"roleCounts"`
	QualityHistogram  []HistBucket       `json:"qualityHistogram"`
	TopExperiences    []ExperienceDTO    `json:"topExperiences"`
	RecentTrajectories []TrajectoryDTO   `json:"recentTrajectories"`
	MetricSummary     *ModuleSummaryDTO  `json:"metricSummary,omitempty"`
	Heatmap            []HeatmapCell     `json:"heatmap,omitempty"`
}

// ExperienceDTO 经验条目。
type ExperienceDTO struct {
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
	SuccessRate  float64   `json:"successRate"`
}

// TrajectoryDTO 轨迹条目。
type TrajectoryDTO struct {
	ID        string    `json:"id"`
	TeamName  string    `json:"teamName"`
	StageName string    `json:"stageName"`
	Role      string    `json:"role"`
	Objective string    `json:"objective"`
	Success   bool      `json:"success"`
	Duration  string    `json:"duration"`
	Timestamp time.Time `json:"timestamp"`
	Error     string    `json:"error,omitempty"`
}

// HistBucket 直方图桶。
type HistBucket struct {
	From  float64 `json:"from"`
	To    float64 `json:"to"`
	Count int     `json:"count"`
}

// TaskDTO V2 任务。
type TaskDTO struct {
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

// APIError 标准错误响应。
type APIError struct {
	Error string `json:"error"`
}

// InsightDTO 自动诊断的单条洞察。
//   - Severity: info / warn / critical
//   - Kind:     trend / saturation / idle / anomaly / success
//   - Evidence: 构成该洞察的证据 (如指标名 / 值 / 对比值)
type InsightDTO struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Severity  string            `json:"severity"`
	Kind      string            `json:"kind"`
	Module    string            `json:"module,omitempty"`
	Target    string            `json:"target,omitempty"`
	Evidence  map[string]string `json:"evidence,omitempty"`
	Suggestion string           `json:"suggestion,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
}

// InsightsResp 本地洞察聚合响应。
type InsightsResp struct {
	GeneratedAt time.Time    `json:"generatedAt"`
	Total       int          `json:"total"`
	Insights    []InsightDTO `json:"insights"`
	// LLM 相关 (仅在 ?llm=1 时填充)
	LLMEnabled  bool   `json:"llmEnabled,omitempty"`
	LLMSummary  string `json:"llmSummary,omitempty"`
	LLMError    string `json:"llmError,omitempty"`
}

// ProjectDTO 多项目聚合列表中的一项。
type ProjectDTO struct {
	Path         string    `json:"path"`
	Name         string    `json:"name"`
	Current      bool      `json:"current"`
	TeamsCount   int       `json:"teamsCount"`
	LastTeamAt   time.Time `json:"lastTeamAt,omitempty"`
	HasMetrics   bool      `json:"hasMetrics"`
	HasDreaming  bool      `json:"hasDreaming"`
	HasEvolution bool      `json:"hasEvolution"`
}

// ProjectsResp 多项目响应。
type ProjectsResp struct {
	Current  string       `json:"current"`
	Projects []ProjectDTO `json:"projects"`
}

// HeatmapCell Evolution 页面 category × role 单元格。
type HeatmapCell struct {
	Category string `json:"category"`
	Role     string `json:"role"`
	Count    int    `json:"count"`
}

// DaemonStatus dashboard 后台进程状态。
type DaemonStatus struct {
	Running   bool      `json:"running"`
	PID       int       `json:"pid,omitempty"`
	Port      int       `json:"port,omitempty"`
	Addr      string    `json:"addr,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	StateDir  string    `json:"stateDir,omitempty"`
	LogFile   string    `json:"logFile,omitempty"`
	URL       string    `json:"url,omitempty"`
	Uptime    string    `json:"uptime,omitempty"`
}
