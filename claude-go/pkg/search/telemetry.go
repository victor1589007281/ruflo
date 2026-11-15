package search

// telemetry.go —— 检索的**使用记录** (每条查询一行 JSONL)。
//
// 为什么要单独落一份文件, 而不是只靠 Metrics:
//   - Metrics 是进程内计数, 会话一结束 (CLI 进程退出) 就没了, 而且只有一个
//     总数, 看不出"哪条查询没过质量门""哪个源在拖慢 p95";
//   - 调优需要**逐条现场**: 查询词、意图、命中源、被降级的源、门的原因、耗时、
//     结果数、缓存命中。这些只在调度完成的那一刻完整存在。
//
// 落盘位置由 SEARCH_STATE_DIR 决定 (默认 $HOME/.claude-go/search), 与
// sources.json 同目录 —— 会话进程、控制面、网关写同一份, 面板从主机直接读。
// 追加写 (O_APPEND) 对小行是原子的, 多进程并发不需要文件锁。
//
// 纪律: 记录失败绝不影响检索。写盘出任何问题只记一次日志, 然后闭嘴。

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// telemetryOff 报告本进程是否应当**跳过落盘** (使用记录 + 生效快照)。
//
// 两个场景必须跳过:
//  1. 显式关闭: SEARCH_TELEMETRY_DISABLED=1/true —— 一次性脚本、压测、演示;
//  2. go test 二进制 (可执行名以 .test 结尾) 且没显式给 SEARCH_STATE_DIR ——
//     否则跑一次单测就会把假查询、假 providers.json 写进真实状态目录,
//     污染面板上的"真实使用记录"。显式指定了目录的测试照常落盘。
func telemetryOff() bool {
	if v := strings.TrimSpace(os.Getenv("SEARCH_TELEMETRY_DISABLED")); v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	if strings.TrimSpace(os.Getenv("SEARCH_STATE_DIR")) != "" {
		return false
	}
	return strings.HasSuffix(filepath.Base(os.Args[0]), ".test")
}

// Record 是一条检索/抓取/研究的使用记录。
//
// 字段刻意扁平: 面板按这个结构直接聚合, 不做二次解析。省略空值以控制行大小
// (一条查询通常 300–600 字节)。
type Record struct {
	Time      time.Time `json:"ts"`
	Kind      string    `json:"kind"` // search | fetch | research
	Query     string    `json:"query,omitempty"`
	Intent    string    `json:"intent,omitempty"`
	Lang      string    `json:"lang,omitempty"`
	Freshness string    `json:"freshness,omitempty"`

	Status     string `json:"status,omitempty"` // ok | partial | empty | error
	CacheHit   bool   `json:"cacheHit,omitempty"`
	GatePassed bool   `json:"gatePassed"`
	GateReason string `json:"gateReason,omitempty"`

	Providers  []string         `json:"providers,omitempty"`  // 本次真正返回结果的源
	Failed     []string         `json:"failed,omitempty"`     // 降级/失败摘要 (含退避中的源)
	ProviderMS map[string]int64 `json:"providerMs,omitempty"` // 源 → 该源耗时

	LatencyMS int64   `json:"latencyMs"`
	Results   int     `json:"results"`
	CostUSD   float64 `json:"costUsd,omitempty"`

	TopDomains []string `json:"topDomains,omitempty"`
	Domains    int      `json:"domains,omitempty"`

	// fetch / research
	URL        string `json:"url,omitempty"`
	Bytes      int    `json:"bytes,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
	Rounds     int    `json:"rounds,omitempty"`
	Evidence   int    `json:"evidence,omitempty"`

	Error string `json:"error,omitempty"`
	// Process 标识记录来自哪类进程 (cli / serve / mcp), 便于区分"会话里的检索"
	// 与"调度器里的检索"。由 SetProcess 设置。
	Process string `json:"process,omitempty"`
}

// Recorder 把记录追加到按天切分的 JSONL 文件。
type Recorder struct {
	dir string

	mu   sync.Mutex
	day  string
	file *os.File
}

// NewRecorder 在 dir/telemetry 下准备记录器 (目录不存在则创建)。
func NewRecorder(dir string) (*Recorder, error) {
	if dir == "" {
		return nil, nil
	}
	tdir := filepath.Join(dir, "telemetry")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		return nil, err
	}
	return &Recorder{dir: tdir}, nil
}

// Dir 返回记录文件所在目录。
func (r *Recorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

// Record 追加一条记录。并发安全; 跨天自动换文件并清理 30 天前的旧文件。
func (r *Recorder) Record(rec Record) {
	if r == nil {
		return
	}
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	day := rec.Time.Local().Format("2006-01-02")

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil || r.day != day {
		if r.file != nil {
			r.file.Close()
			r.file = nil
		}
		name := filepath.Join(r.dir, "search-"+day+".jsonl")
		f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			// 只报一次: 目录被删/权限变了时不能每条查询都刷日志。
			if r.day != "error" {
				log.Printf("[search] 打开使用记录文件失败 %s: %v (后续记录丢弃)", name, err)
			}
			r.day = "error"
			return
		}
		r.file = f
		r.day = day
		r.pruneLocked(rec.Time)
	}
	line = append(line, '\n')
	if _, err := r.file.Write(line); err != nil {
		log.Printf("[search] 写使用记录失败: %v", err)
	}
}

// pruneLocked 删除 30 天前的记录文件; 每天换文件时最多跑一次。
func (r *Recorder) pruneLocked(now time.Time) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -30).Local().Format("2006-01-02")
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) < len("search-2026-01-01.jsonl") || name[:7] != "search-" {
			continue
		}
		day := name[len("search-") : len("search-")+10]
		if day < cutoff {
			_ = os.Remove(filepath.Join(r.dir, name))
		}
	}
}

// Close 关闭当前文件。
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		err := r.file.Close()
		r.file = nil
		return err
	}
	return nil
}

// 进程级单例。检索数据面本身就是进程级单例 (builtin.DefaultDataStack 用
// sync.Once 装配), 记录器跟着它走; 这样 Manager / LegacySearcher / MCP 三处
// 调用点都不用各自携带一个 recorder 参数 (少一个"有的入口不记录"的分叉点)。
var (
	recorderMu sync.RWMutex
	recorder   *Recorder

	processMu   sync.RWMutex
	processName string
)

// SetRecorder 安装进程级记录器; nil 表示关闭记录。
func SetRecorder(r *Recorder) {
	recorderMu.Lock()
	old := recorder
	recorder = r
	recorderMu.Unlock()
	if old != nil && old != r {
		_ = old.Close()
	}
}

// SetProcess 标注记录来源 (cli / serve / mcp), 写进每条记录。
func SetProcess(name string) {
	processMu.Lock()
	processName = name
	processMu.Unlock()
}

func currentProcess() string {
	processMu.RLock()
	defer processMu.RUnlock()
	return processName
}

// StateDir 返回检索的共享状态目录, 并说明来源:
//
//  1. SEARCH_STATE_DIR 环境变量 (控制面/网关/会话进程统一指到同一份);
//  2. $HOME/.claude-go/search。
//
// 面板 (webapp) 读的也是这个目录, 所以两种来源都必须在面板的扫描列表里。
func StateDir() string {
	if v := os.Getenv("SEARCH_STATE_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude-go", "search")
}

// telemetryMu 保护 StateDir 上的懒初始化 (LoadConfig → InitTelemetry 只跑一次)。
var telemetryOnce sync.Once

// InitTelemetry 按 StateDir 建记录器。重复调用无副作用; 目录不可用时关闭记录
// (检索照常工作)。
func InitTelemetry() *Recorder {
	telemetryOnce.Do(func() {
		if telemetryOff() {
			return
		}
		// 进程类型默认取可执行文件名 (claude-go ~ cli, 控制面/网关由部署显式
		// 传 CLAUDE_GO_PROCESS), 面板按它区分"会话里的检索"与"调度器里的检索"。
		if currentProcess() == "" {
			if v := os.Getenv("CLAUDE_GO_PROCESS"); v != "" {
				SetProcess(v)
			} else if len(os.Args) > 0 {
				SetProcess(filepath.Base(os.Args[0]))
			}
		}
		r, err := NewRecorder(StateDir())
		if err != nil {
			log.Printf("[search] 使用记录目录不可用 %s: %v (不记录)", StateDir(), err)
			return
		}
		SetRecorder(r)
	})
	recorderMu.RLock()
	defer recorderMu.RUnlock()
	return recorder
}

// recordUsage 写一条记录 (nil 记录器时静默返回)。
func recordUsage(rec Record) {
	recorderMu.RLock()
	r := recorder
	recorderMu.RUnlock()
	if r == nil {
		return
	}
	if rec.Process == "" {
		rec.Process = currentProcess()
	}
	r.Record(rec)
}

// countDomains 数结果里有多少个不同域名 (域名多样性门与面板的分布图要看它)。
func countDomains(results []Result) int {
	seen := map[string]struct{}{}
	for _, r := range results {
		if host := canonicalHost(r.URL); host != "" {
			seen[host] = struct{}{}
		}
	}
	return len(seen)
}

// topDomains 取结果里的主要域名 (按出现次数取前 n), 面板用它看"检索到的都是些什么站"。
func topDomains(results []Result, n int) []string {
	counts := map[string]int{}
	for _, r := range results {
		host := canonicalHost(r.URL)
		if host == "" {
			continue
		}
		counts[host]++
	}
	type kv struct {
		host string
		n    int
	}
	list := make([]kv, 0, len(counts))
	for h, c := range counts {
		list = append(list, kv{h, c})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].host < list[j].host
	})
	if len(list) > n {
		list = list[:n]
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.host)
	}
	return out
}
