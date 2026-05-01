package observability

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// PromptObservatory 提示词观测台: 版本管理、A/B 测试、效果追踪。
// 基于 SQLite 存储, 支持按 template + version 查询历史表现。
type PromptObservatory struct {
	db *sql.DB
	mu sync.RWMutex
}

// PromptRecord 单次提示词渲染记录。
type PromptRecord struct {
	ID           int64
	PromptID     string
	Version      string
	Hash         string
	TemplateName string
	TemplateBody string
	VariablesJSON string
	Rendered     string
	RenderedLen  int
	RenderTimeMs float64
	CreatedAt    time.Time
}

// PromptVersionStats 单个 prompt 版本的聚合统计。
type PromptVersionStats struct {
	PromptID      string
	Version       string
	Hash          string
	UseCount      int
	AvgLatencyMs  float64
	AvgTokensIn   float64
	AvgTokensOut  float64
	SuccessRate   float64
	LastUsedAt    time.Time
}

// NewPromptObservatory 创建提示词观测台, 在指定路径初始化 SQLite。
func NewPromptObservatory(dbPath string) (*PromptObservatory, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开 sqlite %s: %w", dbPath, err)
	}
	if err := initPromptSchema(db); err != nil {
		return nil, fmt.Errorf("初始化 schema: %w", err)
	}
	return &PromptObservatory{db: db}, nil
}

// Close 关闭数据库连接。
func (p *PromptObservatory) Close() error {
	return p.db.Close()
}

// RecordPrompt 记录一次提示词渲染。
func (p *PromptObservatory) RecordPrompt(rec PromptRecord) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if rec.Hash == "" && rec.Rendered != "" {
		rec.Hash = HashPrompt(rec.Rendered)
	}

	res, err := p.db.Exec(`
		INSERT INTO prompt_versions (prompt_id, version, hash, template_name, template_body, variables_json, rendered, rendered_len, render_time_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, rec.PromptID, rec.Version, rec.Hash, rec.TemplateName, rec.TemplateBody, rec.VariablesJSON, rec.Rendered, rec.RenderedLen, rec.RenderTimeMs, time.Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// LookupVersion 按 prompt_id + version 查询最新记录。
func (p *PromptObservatory) LookupVersion(promptID, version string) (*PromptRecord, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	row := p.db.QueryRow(`
		SELECT id, prompt_id, version, hash, template_name, template_body, variables_json, rendered, rendered_len, render_time_ms, created_at
		FROM prompt_versions
		WHERE prompt_id = ? AND version = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, promptID, version)

	return scanPromptRecord(row)
}

// LookupByHash 按 hash 查询提示词记录。
func (p *PromptObservatory) LookupByHash(hash string) (*PromptRecord, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	row := p.db.QueryRow(`
		SELECT id, prompt_id, version, hash, template_name, template_body, variables_json, rendered, rendered_len, render_time_ms, created_at
		FROM prompt_versions
		WHERE hash = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, hash)

	return scanPromptRecord(row)
}

// VersionStats 返回某个 prompt 所有版本的统计。
func (p *PromptObservatory) VersionStats(promptID string) ([]PromptVersionStats, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	rows, err := p.db.Query(`
		SELECT version, hash, COUNT(*) as use_count,
			AVG(render_time_ms) as avg_latency,
			MAX(created_at) as last_used
		FROM prompt_versions
		WHERE prompt_id = ?
		GROUP BY version, hash
		ORDER BY last_used DESC
	`, promptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PromptVersionStats
	for rows.Next() {
		var s PromptVersionStats
		s.PromptID = promptID
		var lastUsedStr string
		if err := rows.Scan(&s.Version, &s.Hash, &s.UseCount, &s.AvgLatencyMs, &lastUsedStr); err != nil {
			return nil, err
		}
		if lastUsedStr != "" {
			if ts, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", lastUsedStr); err == nil {
				s.LastUsedAt = ts
			} else if ts, err := time.Parse("2006-01-02 15:04:05", lastUsedStr); err == nil {
				s.LastUsedAt = ts
			}
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

// RecordOutcome 记录某次 prompt 使用的结果 (用于计算 success_rate / token 效率)。
func (p *PromptObservatory) RecordOutcome(promptID, version string, success bool, tokensIn, tokensOut int, latencyMs float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	_, err := p.db.Exec(`
		INSERT INTO prompt_outcomes (prompt_id, version, success, tokens_in, tokens_out, latency_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, promptID, version, success, tokensIn, tokensOut, latencyMs, time.Now())
	return err
}

// OutcomeStats 按 prompt_id + version 返回效果统计。
func (p *PromptObservatory) OutcomeStats(promptID, version string) (useCount int, successRate float64, avgTokensIn, avgTokensOut, avgLatency float64, err error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	row := p.db.QueryRow(`
		SELECT COUNT(*),
			COALESCE(AVG(CASE WHEN success THEN 1.0 ELSE 0.0 END), 0),
			COALESCE(AVG(tokens_in), 0),
			COALESCE(AVG(tokens_out), 0),
			COALESCE(AVG(latency_ms), 0)
		FROM prompt_outcomes
		WHERE prompt_id = ? AND version = ?
	`, promptID, version)

	err = row.Scan(&useCount, &successRate, &avgTokensIn, &avgTokensOut, &avgLatency)
	return
}

// StartABTest 启动 A/B 测试, 返回 test_id。
func (p *PromptObservatory) StartABTest(testID, promptID, variantA, variantB string, config map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	configJSON := ""
	if len(config) > 0 {
		parts := make([]string, 0, len(config))
		for k, v := range config {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		configJSON = strings.Join(parts, ";")
	}

	_, err := p.db.Exec(`
		INSERT INTO prompt_ab_tests (test_id, prompt_id, variant_a, variant_b, config_json, start_time, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, testID, promptID, variantA, variantB, configJSON, time.Now(), "running")
	return err
}

// RecordABOutcome 记录 A/B 测试单次结果。
func (p *PromptObservatory) RecordABOutcome(testID, variant string, success bool, tokensIn, tokensOut int, latencyMs float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	_, err := p.db.Exec(`
		INSERT INTO prompt_ab_outcomes (test_id, variant, success, tokens_in, tokens_out, latency_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, testID, variant, success, tokensIn, tokensOut, latencyMs, time.Now())
	return err
}

// ABResult 计算 A/B 测试结果并返回 winner。
func (p *PromptObservatory) ABResult(testID string) (*PromptABResult, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	row := p.db.QueryRow(`
		SELECT prompt_id, variant_a, variant_b FROM prompt_ab_tests WHERE test_id = ?
	`, testID)
	var promptID, variantA, variantB string
	if err := row.Scan(&promptID, &variantA, &variantB); err != nil {
		return nil, err
	}

	countA, srA, err := p.abVariantStats(testID, variantA)
	if err != nil {
		return nil, err
	}
	countB, srB, err := p.abVariantStats(testID, variantB)
	if err != nil {
		return nil, err
	}

	res := &PromptABResult{
		TraceID:    testID,
		TestID:     testID,
		VariantA:   variantA,
		VariantB:   variantB,
		SampleSize: countA + countB,
	}

	// 用 success_rate 作为评判标准
	if countA == 0 && countB == 0 {
		res.Winner = "tie"
		return res, nil
	}
	if countA == 0 {
		res.Winner = "B"
		return res, nil
	}
	if countB == 0 {
		res.Winner = "A"
		return res, nil
	}

	if srA > srB {
		res.Winner = "A"
		res.Improvement = (srA - srB) / srB * 100
		res.MetricName = "success_rate"
	} else if srB > srA {
		res.Winner = "B"
		res.Improvement = (srB - srA) / srA * 100
		res.MetricName = "success_rate"
	} else {
		res.Winner = "tie"
	}
	return res, nil
}

func (p *PromptObservatory) abVariantStats(testID, variant string) (count int, successRate float64, err error) {
	row := p.db.QueryRow(`
		SELECT COUNT(*), COALESCE(AVG(CASE WHEN success THEN 1.0 ELSE 0.0 END), 0)
		FROM prompt_ab_outcomes
		WHERE test_id = ? AND variant = ?
	`, testID, variant)
	err = row.Scan(&count, &successRate)
	return
}

// FinishABTest 结束 A/B 测试。
func (p *PromptObservatory) FinishABTest(testID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	_, err := p.db.Exec(`
		UPDATE prompt_ab_tests SET status = ?, end_time = ? WHERE test_id = ?
	`, "finished", time.Now(), testID)
	return err
}

// HashPrompt 计算提示词的 SHA256 哈希 (作为版本指纹)。
func HashPrompt(prompt string) string {
	h := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(h[:])[:16]
}

// ─── schema ────────────────────────────────────────────────────────────────

func initPromptSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS prompt_versions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			prompt_id TEXT NOT NULL,
			version TEXT NOT NULL,
			hash TEXT NOT NULL,
			template_name TEXT,
			template_body TEXT,
			variables_json TEXT,
			rendered TEXT,
			rendered_len INTEGER,
			render_time_ms REAL,
			created_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pv_prompt_version ON prompt_versions(prompt_id, version)`,
		`CREATE INDEX IF NOT EXISTS idx_pv_hash ON prompt_versions(hash)`,
		`CREATE INDEX IF NOT EXISTS idx_pv_created ON prompt_versions(created_at)`,

		`CREATE TABLE IF NOT EXISTS prompt_outcomes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			prompt_id TEXT NOT NULL,
			version TEXT NOT NULL,
			success INTEGER,
			tokens_in INTEGER,
			tokens_out INTEGER,
			latency_ms REAL,
			created_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_po_prompt_version ON prompt_outcomes(prompt_id, version)`,

		`CREATE TABLE IF NOT EXISTS prompt_ab_tests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			test_id TEXT NOT NULL UNIQUE,
			prompt_id TEXT NOT NULL,
			variant_a TEXT,
			variant_b TEXT,
			config_json TEXT,
			start_time DATETIME,
			end_time DATETIME,
			status TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ab_test_id ON prompt_ab_tests(test_id)`,

		`CREATE TABLE IF NOT EXISTS prompt_ab_outcomes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			test_id TEXT NOT NULL,
			variant TEXT,
			success INTEGER,
			tokens_in INTEGER,
			tokens_out INTEGER,
			latency_ms REAL,
			created_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_abo_test_variant ON prompt_ab_outcomes(test_id, variant)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %s: %w", stmt[:40], err)
		}
	}
	return nil
}

func scanPromptRecord(row *sql.Row) (*PromptRecord, error) {
	var r PromptRecord
	var createdRaw sql.NullTime
	err := row.Scan(
		&r.ID, &r.PromptID, &r.Version, &r.Hash, &r.TemplateName, &r.TemplateBody,
		&r.VariablesJSON, &r.Rendered, &r.RenderedLen, &r.RenderTimeMs, &createdRaw,
	)
	if err != nil {
		return nil, err
	}
	if createdRaw.Valid {
		r.CreatedAt = createdRaw.Time
	}
	return &r, nil
}

// PromptHookAdapter 将 PromptHook 事件写入 PromptObservatory。
type PromptHookAdapter struct {
	obs *PromptObservatory
}

// NewPromptHookAdapter 创建适配器。
func NewPromptHookAdapter(obs *PromptObservatory) *PromptHookAdapter {
	return &PromptHookAdapter{obs: obs}
}

// OnPromptRender 记录渲染。
func (a *PromptHookAdapter) OnPromptRender(ev PromptEvent) {
	rec := PromptRecord{
		PromptID:     ev.PromptID,
		Version:      ev.Version,
		Hash:         ev.PromptHash,
		TemplateName: ev.TemplateName,
		RenderedLen:  ev.RenderedLen,
		RenderTimeMs: ev.RenderTimeMs,
	}
	_, _ = a.obs.RecordPrompt(rec)
}

// OnPromptVersion 记录版本。
func (a *PromptHookAdapter) OnPromptVersion(promptID, version, hash string, metadata map[string]string) {
	rec := PromptRecord{
		PromptID: promptID,
		Version:  version,
		Hash:     hash,
	}
	_, _ = a.obs.RecordPrompt(rec)
}

// OnPromptCompare no-op。
func (a *PromptHookAdapter) OnPromptCompare(v1, v2 string, diffStats map[string]int) {}

// OnPromptABStart 启动 A/B 测试。
func (a *PromptHookAdapter) OnPromptABStart(testID, variantA, variantB string, ev PromptEvent) {
	_ = a.obs.StartABTest(testID, ev.PromptID, variantA, variantB, nil)
}

// OnPromptABResult 记录 A/B 结果。
func (a *PromptHookAdapter) OnPromptABResult(res PromptABResult) {}
