package search

// sources.go —— 数据源的运行期覆盖 (面板可改) 与生效配置快照 (面板可读)。
//
// 两件事合成一个文件族, 都在 StateDir 下:
//
//	sources.json     ← 面板写的**覆盖**: 启用/停用、权重、层级、优先级。
//	providers.json   ← 进程启动时写出的**生效**快照: 每个源最终的 tier/权重/
//	                   出口/是否配了 key。面板靠它展示"现在到底在用什么"。
//
// 为什么不把面板的改动做成热更新: 检索数据面在每个 claude-go 进程里各建一份
// (会话进程、控制面、网关), 热更新只能改到正在跑的那一份。改成"进程启动时读
// 覆盖文件"后, 下一个会话/下一次调度自然生效 —— 语义简单且不会出现"面板关了
// 但某个进程还在打这个源"的分叉。UI 上明确写"下个进程生效"。
//
// 覆盖只开放**安全旋钮** (开关/权重/层级/顺序)。API key、出口代理地址不进这个
// 文件: 一个是凭据, 一个是网络拓扑, 都不该由网页改。

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SourceOverride 是单个源的覆盖项; 指针字段 = "没提到就别动"。
type SourceOverride struct {
	Enabled *bool    `json:"enabled,omitempty"`
	Weight  *float64 `json:"weight,omitempty"`
	Tier    *string  `json:"tier,omitempty"`
	Egress  *string  `json:"egress,omitempty"`
}

// SourcesFile 是面板写入的源覆盖文件。
type SourcesFile struct {
	UpdatedAt        time.Time                 `json:"updatedAt,omitempty"`
	UpdatedBy        string                    `json:"updatedBy,omitempty"`
	Providers        map[string]SourceOverride `json:"providers,omitempty"`
	ProviderPriority []string                  `json:"providerPriority,omitempty"`
	IntentPriority   map[string][]string       `json:"intentPriority,omitempty"`
}

// SourcesPath 返回覆盖文件路径 (与使用记录同目录)。
func SourcesPath() string {
	dir := StateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "sources.json")
}

// EffectiveSourcesPath 返回生效配置快照路径。
func EffectiveSourcesPath() string {
	dir := StateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "providers.json")
}

// LoadSourcesFile 读覆盖文件。文件不存在返回 (nil, nil)。
//
// 解析失败必须**响亮**: 静默忽略会让面板上的开关看起来生效了、实际没生效。
func LoadSourcesFile() (*SourcesFile, error) {
	p := SourcesPath()
	if p == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sf SourcesFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		log.Printf("[search] 数据源覆盖文件 %s 解析失败 (本次按默认源配置运行): %v", p, err)
		return nil, err
	}
	return &sf, nil
}

// applySourcesFile 把覆盖合并进 cfg。未提到的源/字段保持默认。
func applySourcesFile(cfg *Config) {
	sf, err := LoadSourcesFile()
	if err != nil || sf == nil {
		return
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	for name, ov := range sf.Providers {
		if name == "" {
			continue
		}
		p, exists := cfg.Providers[name]
		if !exists {
			// 覆盖文件里出现配置里没有的源: 当成"启用一个默认关闭的源"处理,
			// 其余字段由 provider 构造函数兜底。
			p = ProviderConfig{Enabled: true, Egress: ModeAuto}
		}
		if ov.Enabled != nil {
			p.Enabled = *ov.Enabled
			p.Disabled = !*ov.Enabled
		}
		if ov.Weight != nil && *ov.Weight >= 0 {
			p.Weight = *ov.Weight
		}
		if ov.Tier != nil {
			p.Tier = parseTier(*ov.Tier)
		}
		if ov.Egress != nil {
			switch Mode(*ov.Egress) {
			case ModeDirect, ModeProxy, ModeAuto:
				p.Egress = Mode(*ov.Egress)
			}
		}
		cfg.Providers[name] = p
	}
	if len(sf.ProviderPriority) > 0 {
		cfg.ProviderPriority = append([]string(nil), sf.ProviderPriority...)
	}
	if len(sf.IntentPriority) > 0 {
		if cfg.IntentPriority == nil {
			cfg.IntentPriority = map[string][]string{}
		}
		for k, v := range sf.IntentPriority {
			cfg.IntentPriority[k] = append([]string(nil), v...)
		}
	}
}

// EffectiveSource 是某个源在**当前进程**里的最终配置。
//
// 分类/用途来自 catalog.go 的静态目录: 面板靠它把十几个源按问题类型分组,
// 而不是甩给用户一张按字母序排的平表。未登记的名字留空, 面板归入"其他"。
type EffectiveSource struct {
	Name             string  `json:"name"`
	Tier             string  `json:"tier"`
	Egress           string  `json:"egress"`
	Weight           float64 `json:"weight"`
	Enabled          bool    `json:"enabled"`
	APIKeyConfigured bool    `json:"apiKeyConfigured"`
	CostEstimateUSD  float64 `json:"costEstimateUsd,omitempty"`
	Healthy          bool    `json:"healthy"`

	Category     string `json:"category,omitempty"`
	CategoryName string `json:"categoryName,omitempty"`
	Label        string `json:"label,omitempty"`
	Desc         string `json:"desc,omitempty"`
	Auth         string `json:"auth,omitempty"`
	// Sites 是聚合源收录的站点清单 (目前只有 techdocs 有); 面板展开时渲染。
	Sites []SiteMeta `json:"sites,omitempty"`
}

// EffectiveSnapshot 是生效配置快照 (面板读它渲染数据源表)。
type EffectiveSnapshot struct {
	GeneratedAt      time.Time           `json:"generatedAt"`
	Process          string              `json:"process,omitempty"`
	SourcesPath      string              `json:"sourcesPath,omitempty"`
	MaxTier          string              `json:"maxTier"`
	MaxCostUSD       float64             `json:"maxCostUsd"`
	SearchTimeoutMS  int64               `json:"searchTimeoutMs"`
	ProviderPriority []string            `json:"providerPriority,omitempty"`
	IntentPriority   map[string][]string `json:"intentPriority,omitempty"`
	// CategoryOrder 是分类的展示顺序 (面板照抄, 免得前端再排一次)。
	CategoryOrder []string          `json:"categoryOrder,omitempty"`
	Sources       []EffectiveSource `json:"sources"`
}

// WriteEffectiveSources 把当前进程实际生效的源配置原子写进 providers.json。
//
// 只写"是什么", 不写密钥: APIKeyConfigured 是布尔。
func WriteEffectiveSources(cfg Config, providers []Provider) {
	if telemetryOff() {
		return
	}
	p := EffectiveSourcesPath()
	if p == "" {
		return
	}
	healthy := map[string]bool{}
	for _, pr := range providers {
		if pr == nil {
			continue
		}
		healthy[pr.Name()] = pr.Healthy()
	}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	snap := EffectiveSnapshot{
		GeneratedAt:      time.Now(),
		Process:          currentProcess(),
		SourcesPath:      SourcesPath(),
		MaxTier:          cfg.MaxTier.String(),
		MaxCostUSD:       cfg.MaxCostUSD,
		SearchTimeoutMS:  cfg.SearchTimeout.Milliseconds(),
		ProviderPriority: cfg.ProviderPriority,
		IntentPriority:   cfg.IntentPriority,
		CategoryOrder:    append([]string(nil), CategoryOrder...),
		Sources:          make([]EffectiveSource, 0, len(names)),
	}
	for _, name := range names {
		pc := cfg.Providers[name]
		// 只有**登记在册** (构造器真的建出来了) 的源才算健康可查;
		// 配置里启用但构造失败的源在快照里显式 enabled=false, 避免面板
		// 显示"开着"却永远没结果。
		constructed, ok := healthy[name]
		meta := ProviderMetaOf(name)
		snap.Sources = append(snap.Sources, EffectiveSource{
			Name:             name,
			Tier:             pc.Tier.String(),
			Egress:           string(pc.Egress),
			Weight:           pc.Weight,
			Enabled:          pc.Enabled && ok,
			APIKeyConfigured: pc.APIKey != "",
			CostEstimateUSD:  pc.CostEstimateUSD,
			Healthy:          constructed,
			Category:         meta.Category,
			CategoryName:     meta.CategoryName,
			Label:            meta.Label,
			Desc:             meta.Desc,
			Auth:             meta.Auth,
			Sites:            meta.Sites,
		})
	}
	writeJSONAtomic(p, snap)
}

// writeJSONAtomic 写临时文件 + rename, 避免面板读到半截 JSON。
func writeJSONAtomic(path string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".sources-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	_ = os.Chmod(name, 0o644)
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
	}
}
