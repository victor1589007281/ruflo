// Package sync 实现外部数据源（IMA、微信读书）到知识库的增量同步。
package sync

import (
	"time"
)

// JobStatus 表示同步任务状态。
type JobStatus string

const (
	JobPending   JobStatus = "pending"
	JobRunning   JobStatus = "running"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
)

// Job 表示一次同步任务。
type Job struct {
	ID         string                 `json:"jobId"`
	Source     string                 `json:"source"`
	Status     JobStatus              `json:"status"`
	Message    string                 `json:"message,omitempty"`
	Stats      map[string]interface{} `json:"stats,omitempty"`
	StartedAt  time.Time              `json:"startedAt"`
	FinishedAt *time.Time             `json:"finishedAt,omitempty"`
}

// SourceIndex 是某个数据源的索引文件结构。
type SourceIndex struct {
	Version  int                  `json:"version"`
	Source   string               `json:"source"`
	SyncedAt time.Time            `json:"syncedAt"`
	Items    map[string]IndexItem `json:"items"`
}

// IndexItem 记录单个外部条目在知识库中的位置与版本信息。
type IndexItem struct {
	ExternalID string    `json:"externalId"`
	Path       string    `json:"path"`
	Title      string    `json:"title"`
	UpdatedAt  time.Time `json:"updatedAt"`
	SyncedAt   time.Time `json:"syncedAt"`
	Hash       string    `json:"hash"`
	URL        string    `json:"url"`
}

// ExternalItem 是适配器返回的统一条目结构。
type ExternalItem struct {
	ExternalID string
	Type       string
	Title      string
	URL        string
	UpdatedAt  time.Time
	Tags       []string
	Body       string
}

// Config 是 sync 模块配置。
type Config struct {
	KnowledgeRepo string
	IMA           IMAConfig
	WeRead        WeReadConfig
}

// IMAConfig 是 IMA 同步配置。
type IMAConfig struct {
	Enabled  bool   `json:"enabled"`
	Cron     string `json:"cron"`
	ClientID string `json:"clientId"`
	APIKey   string `json:"apiKey"`
}

// WeReadConfig 是微信读书同步配置。
type WeReadConfig struct {
	Enabled bool   `json:"enabled"`
	Cron    string `json:"cron"`
	APIKey  string `json:"apiKey"`
}

// Adapter 列出并拉取外部数据源的条目。
type Adapter interface {
	// Source 返回数据源名称（如 ima、weread）。
	Source() string
	// List 返回所有需要同步的条目概要（不含正文）。
	List() ([]ExternalItem, error)
	// Fetch 拉取单个条目的完整正文。
	Fetch(item ExternalItem) (ExternalItem, error)
}
