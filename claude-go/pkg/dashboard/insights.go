package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BuildInsights 基于当前本地数据, 生成可操作的只读诊断建议。
// 不依赖 LLM, 规则驱动, 覆盖 roadmap v2.0 "Ask Claude 面板" 的诊断部分能力。
func (p *Provider) BuildInsights() (*InsightsResp, error) {
	now := time.Now()
	resp := &InsightsResp{GeneratedAt: now}
	var insights []InsightDTO

	// 1) 指标模块告警 & 趋势
	sums, _ := p.AllMetricSummaries()
	for _, s := range sums {
		for _, al := range s.TrendAlerts {
			insights = append(insights, InsightDTO{
				ID:         fmt.Sprintf("trend-%s-%d", s.Module, len(insights)),
				Title:      al,
				Severity:   severityFromAlert(al),
				Kind:       "trend",
				Module:     s.Module,
				CreatedAt:  now,
				Suggestion: suggestionForTrend(s.Module, al),
			})
		}
		for name, st := range s.Metrics {
			// 饱和/极值异常
			if st != nil && st.Count >= 5 {
				if st.Max > 0 && st.Min == st.Max {
					insights = append(insights, InsightDTO{
						ID:       fmt.Sprintf("flat-%s-%s", s.Module, name),
						Title:    fmt.Sprintf("[%s] 指标 %s 长期持平 (value=%.2f)", s.Module, name, st.Max),
						Severity: "info",
						Kind:     "saturation",
						Module:   s.Module,
						Target:   name,
						Evidence: map[string]string{"min": fmt.Sprintf("%.3f", st.Min), "max": fmt.Sprintf("%.3f", st.Max), "count": fmt.Sprintf("%d", st.Count)},
						CreatedAt: now,
						Suggestion: "可检查该指标是否仍在产生新事件, 或调整采样频率。",
					})
				}
				// 失败率极高
				if (strings.Contains(name, "fail") || strings.Contains(name, "error")) && st.Avg > 0.5 {
					insights = append(insights, InsightDTO{
						ID:        fmt.Sprintf("high-fail-%s-%s", s.Module, name),
						Title:     fmt.Sprintf("[%s] %s 平均值偏高 (x̄=%.2f)", s.Module, name, st.Avg),
						Severity:  "warn",
						Kind:      "anomaly",
						Module:    s.Module,
						Target:    name,
						CreatedAt: now,
						Suggestion: "检查对应 team/cron 的最近失败堆栈, 或下调并发。",
					})
				}
				// pass rate 过低
				if strings.Contains(name, "pass_rate") && st.Avg < 0.3 && st.Count >= 3 {
					insights = append(insights, InsightDTO{
						ID:        fmt.Sprintf("low-pass-%s-%s", s.Module, name),
						Title:     fmt.Sprintf("[%s] 通过率偏低 (x̄=%.0f%%)", s.Module, st.Avg*100),
						Severity:  "critical",
						Kind:      "anomaly",
						Module:    s.Module,
						Target:    name,
						CreatedAt: now,
						Suggestion: "检查对抗循环配置、评估阈值或目标复杂度是否过高。",
					})
				}
			}
		}
	}

	// 2) Teams 层面: 连续失败 / 长时间运行
	teams, _ := p.ListTeams()
	recentFailed := 0
	cutoff := now.Add(-24 * time.Hour)
	for _, t := range teams {
		if t.Status == "failed" && t.CreatedAt.After(cutoff) {
			recentFailed++
		}
		if t.Status == "running" && !t.StartedAt.IsZero() && now.Sub(t.StartedAt) > 30*time.Minute {
			insights = append(insights, InsightDTO{
				ID:        "stuck-" + t.Name,
				Title:     fmt.Sprintf("团队 %s 已运行 %s", t.Name, fmtDuration(now.Sub(t.StartedAt))),
				Severity:  "warn",
				Kind:      "anomaly",
				Target:    t.Name,
				CreatedAt: now,
				Suggestion: "检查是否有 stage 卡住, 可使用 claude-go team status 查看, 必要时 stop 后重启。",
			})
		}
	}
	if recentFailed >= 3 {
		insights = append(insights, InsightDTO{
			ID:        "many-fail-24h",
			Title:     fmt.Sprintf("近 24h 有 %d 次团队失败", recentFailed),
			Severity:  "critical",
			Kind:      "anomaly",
			CreatedAt: now,
			Suggestion: "优先查看告警模块与最近失败团队的 REPORT.md, 聚焦重复出现的错误。",
		})
	}

	// 3) Dreaming 长时间未触发
	if dr, err := p.LoadDreaming(); err == nil {
		if dr.Enabled && !dr.LastDreamAt.IsZero() {
			age := now.Sub(dr.LastDreamAt)
			if age > 24*time.Hour {
				insights = append(insights, InsightDTO{
					ID:         "dreaming-idle",
					Title:      fmt.Sprintf("距离上次 Dreaming 已 %s", fmtDuration(age)),
					Severity:   "info",
					Kind:       "idle",
					Module:     "dreaming",
					CreatedAt:  now,
					Suggestion: "确认 min-hours / min-sessions 阈值是否合理, 或手工触发一次整理以避免记忆堆积。",
				})
			}
		}
	}

	// 4) Evolution: 经验利用率为 0 的占比
	if evo, err := p.LoadEvolution(); err == nil && evo.TotalExperiences > 5 {
		if evo.TotalUsageCount == 0 {
			insights = append(insights, InsightDTO{
				ID:         "evolution-unused",
				Title:      fmt.Sprintf("经验库共 %d 条, 累计使用次数 0", evo.TotalExperiences),
				Severity:   "warn",
				Kind:       "idle",
				Module:     "evolution",
				CreatedAt:  now,
				Suggestion: "检查进化引擎是否启用经验注入 (prompt 注入链路可能被跳过)。",
			})
		}
	}

	// 5) 成功亮点 (Success)
	if evo, err := p.LoadEvolution(); err == nil && evo.AvgQuality >= 0.8 && evo.TotalUsageCount >= 10 {
		insights = append(insights, InsightDTO{
			ID:         "evolution-healthy",
			Title:      fmt.Sprintf("经验质量良好 (x̄=%.2f, 累计使用 %d 次)", evo.AvgQuality, evo.TotalUsageCount),
			Severity:   "info",
			Kind:       "success",
			Module:     "evolution",
			CreatedAt:  now,
			Suggestion: "可将 Top 经验导出为稳定 prompt, 沉淀为标准技能。",
		})
	}

	sort.SliceStable(insights, func(i, j int) bool {
		return severityScore(insights[i].Severity) > severityScore(insights[j].Severity)
	})
	resp.Insights = insights
	resp.Total = len(insights)
	return resp, nil
}

func severityFromAlert(s string) string {
	ls := strings.ToLower(s)
	if strings.Contains(ls, "显著下降") || strings.Contains(ls, "error") {
		return "critical"
	}
	return "warn"
}

func suggestionForTrend(module, alert string) string {
	if strings.Contains(alert, "build_pass_rate") {
		return "连续编译未通过, 优先检查 Go 错误堆栈与依赖版本。"
	}
	if strings.Contains(alert, "duration") {
		return "耗时显著变化, 可能是模型/网络波动; 对比最近一次 team.json 的 stage 耗时。"
	}
	if strings.Contains(alert, "compression") {
		return "记忆压缩率异常, 检查 dreaming 阈值和重要性打分。"
	}
	return "关注该指标的下一次采样, 如持续异常请查看对应模块日志。"
}

func severityScore(s string) int {
	switch s {
	case "critical":
		return 3
	case "warn":
		return 2
	case "info":
		return 1
	}
	return 0
}

func fmtDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%.1fd", d.Hours()/24)
}

// BuildHeatmap 返回 Evolution 的 category × role 热力数据。
func (p *Provider) BuildHeatmap(exps []ExperienceDTO) []HeatmapCell {
	cells := map[string]*HeatmapCell{}
	for _, e := range exps {
		cat := e.Category
		if cat == "" {
			cat = "unknown"
		}
		role := e.Role
		if role == "" {
			role = "-"
		}
		k := cat + "|" + role
		if cells[k] == nil {
			cells[k] = &HeatmapCell{Category: cat, Role: role}
		}
		cells[k].Count++
	}
	out := make([]HeatmapCell, 0, len(cells))
	for _, c := range cells {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Role < out[j].Role
	})
	return out
}

// ListProjects 扫描多项目: 默认从 ~/.claude-go/projects/* 和当前 stateDir 的父级探测。
// 若未开启多项目根, 仅返回当前项目。
func (p *Provider) ListProjects(root string) (*ProjectsResp, error) {
	resp := &ProjectsResp{Current: p.stateDir}
	seen := map[string]bool{}

	addProject := func(path string) {
		if path == "" || seen[path] {
			return
		}
		dto, ok := tryProject(path)
		if !ok {
			return
		}
		dto.Current = (filepath.Clean(path) == filepath.Clean(p.stateDir))
		resp.Projects = append(resp.Projects, dto)
		seen[path] = true
	}

	// 1) 当前
	addProject(p.stateDir)

	// 2) 指定 projects root (用户传入)
	if root != "" {
		if entries, err := os.ReadDir(root); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				candidate := filepath.Join(root, e.Name(), ".claude-go")
				addProject(candidate)
			}
		}
	}

	// 3) 当前 stateDir 父目录同级
	parent := filepath.Dir(filepath.Dir(p.stateDir))
	if entries, err := os.ReadDir(parent); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			addProject(filepath.Join(parent, e.Name(), ".claude-go"))
		}
	}

	sort.Slice(resp.Projects, func(i, j int) bool {
		if resp.Projects[i].Current != resp.Projects[j].Current {
			return resp.Projects[i].Current
		}
		return resp.Projects[i].LastTeamAt.After(resp.Projects[j].LastTeamAt)
	})
	if len(resp.Projects) > 40 {
		resp.Projects = resp.Projects[:40]
	}
	return resp, nil
}

func tryProject(path string) (ProjectDTO, bool) {
	dto := ProjectDTO{Path: path}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return dto, false
	}
	dto.Name = filepath.Base(filepath.Dir(path))
	if entries, err := os.ReadDir(filepath.Join(path, "teams")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dto.TeamsCount++
			if st, err := e.Info(); err == nil {
				if st.ModTime().After(dto.LastTeamAt) {
					dto.LastTeamAt = st.ModTime()
				}
			}
		}
	}
	if info, err := os.Stat(filepath.Join(path, "metrics")); err == nil && info.IsDir() {
		dto.HasMetrics = true
	}
	if info, err := os.Stat(filepath.Join(path, "memory")); err == nil && info.IsDir() {
		dto.HasDreaming = true
	}
	if info, err := os.Stat(filepath.Join(path, "evolution")); err == nil && info.IsDir() {
		dto.HasEvolution = true
	}
	return dto, true
}
