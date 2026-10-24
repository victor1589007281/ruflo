// Package toolskill implements the Tool Skill Runtime layer.
package toolskill

import (
	"context"
	"os/exec"
	"regexp"
	"sync"
)

// ToolDetector runtime tool discovery.
// Scans the environment at Agent Harness initialization to build a capability map.
type ToolDetector struct {
	mu    sync.RWMutex
	cache map[string]*ToolCapability // tool name -> capability
}

// ToolCapability describes a tool's capability.
type ToolCapability struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Path      string `json:"path,omitempty"`
	OutputFmt string `json:"output_fmt"` // "json", "text", "checkstyle"
	Priority  int    `json:"priority"`   // 1 = highest
}

// NewToolDetector 构造工具探测器 (缓存为空, 需先 Scan 才有内容)。
//
// NewToolDetector creates a new ToolDetector.
func NewToolDetector() *ToolDetector {
	return &ToolDetector{cache: make(map[string]*ToolCapability)}
}

// Scan 并发探测内置的 Go 工具链清单 (staticcheck / golangci-lint / go / gofmt / goimports / gopls), 结果写入缓存。
//
// Scan scans the environment for all known native tools.
//
// 算法解读：
//
//	原理：每个工具一个 goroutine 做「LookPath 找可执行文件 + 跑一次取版本」,
//	      用 WaitGroup 等全部完成后返回; 结果按工具名写入共享缓存。
//	关键不变量：每个 goroutine 必须持锁写缓存, 且闭包要显式接收 t 作为参数 ——
//	      若直接闭包捕获循环变量, 所有 goroutine 会探测同一个 (最后一个) 工具。
//	      另外写缓存必须发生在 wg.Wait() 返回之前, 否则调用方 Scan 之后立刻 Get 会读不到。
//	代价与边界：每个工具都要起一次子进程, 探测耗时取决于最慢的那个 (并行);
//	      探测失败只是 Available=false, 不返回错误 —— 调用方无法区分「工具不存在」与「探测出错」。
//	设计权衡：用并行探测把启动期延迟压到单个工具的耗时量级, 代价是启动瞬间的进程数等于工具数。
func (d *ToolDetector) Scan(ctx context.Context) {
	tools := []struct {
		name      string
		checkCmd  []string
		verRegexp string
		outputFmt string
		priority  int
	}{
		{"staticcheck", []string{"staticcheck", "-version"}, `staticcheck ([\d.]+)`, "text", 1},
		{"golangci-lint", []string{"golangci-lint", "--version"}, `[\d.]+`, "text", 2},
		{"go", []string{"go", "version"}, `go([\d.]+)`, "text", 0}, // go always available
		{"gofmt", []string{"gofmt", "--help"}, ``, "text", 0},
		{"goimports", []string{"goimports", "--help"}, ``, "text", 0},
		{"gopls", []string{"gopls", "version"}, `[\d.]+`, "text", 0},
	}

	var wg sync.WaitGroup
	for _, t := range tools {
		wg.Add(1)
		// t 显式作参数传入而非闭包捕获: 循环变量语义随 Go 版本变化, 传参不受影响。
		go func(t struct {
			name      string
			checkCmd  []string
			verRegexp string
			outputFmt string
			priority  int
		}) {
			defer wg.Done()
			cap := d.probe(ctx, t.name, t.checkCmd, t.verRegexp, t.outputFmt, t.priority)
			d.mu.Lock()
			d.cache[t.name] = cap
			d.mu.Unlock()
		}(t)
	}
	wg.Wait()
}

// probe 探测单个工具: LookPath 判定可用性, 再按正则从版本输出里抠出版本号。
//
// 注意：verRe 为空时只判可用性、不跑子进程; 探测命令的执行错误被忽略,
// 因此「工具存在但一跑就崩」会被判成可用 (Available=true) 且版本为空。
func (d *ToolDetector) probe(ctx context.Context, name string, cmd []string, verRe, outputFmt string, priority int) *ToolCapability {
	cap := &ToolCapability{Name: name, OutputFmt: outputFmt, Priority: priority}

	path, err := exec.LookPath(cmd[0])
	if err != nil {
		cap.Available = false
		return cap
	}
	cap.Available = true
	cap.Path = path

	// Get version
	if verRe != "" {
		out, _ := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
		if m := regexp.MustCompile(verRe).FindStringSubmatch(string(out)); len(m) > 1 {
			cap.Version = m[1]
		}
	}
	return cap
}

// Get 按工具名查缓存, 返回能力描述与是否命中。
//
// Get queries tool availability.
//
// 注意：第二个返回值表示「缓存里有这个键」而不是「工具可用」——
// 探测过但不可用的工具同样返回 true, 需要再判 cap.Available。
func (d *ToolDetector) Get(name string) (*ToolCapability, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, ok := d.cache[name]
	return c, ok
}

// Resolve 按能力名在候选链里挑第一个可用的工具, 全部不可用或能力名未知时返回 nil。
//
// Resolve resolves the best available tool by capability name.
// For example Resolve("static-check") -> staticcheck/golangci-lint/go vet
//
// 注意：候选链里的 "go-vet" 不在 Scan 的探测清单中, 缓存里不会有这个键,
// 因此 static-check 链实际只会命中 staticcheck 或 golangci-lint 两者之一。
func (d *ToolDetector) Resolve(capability string) *ToolCapability {
	chains := map[string][]string{
		"static-check": {"staticcheck", "golangci-lint", "go-vet"},
		"format":       {"gofmt", "goimports"},
		"build":        {"go"},
		"import-fix":   {"goimports", "gofmt"},
	}
	candidates, ok := chains[capability]
	if !ok {
		return nil
	}
	for _, name := range candidates {
		if c, ok := d.Get(name); ok && c.Available {
			return c
		}
	}
	return nil
}

// All 返回缓存的浅拷贝快照 (map 是新建的, 但 *ToolCapability 仍指向同一批对象)。
//
// All returns all detected tools.
func (d *ToolDetector) All() map[string]*ToolCapability {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]*ToolCapability, len(d.cache))
	for k, v := range d.cache {
		out[k] = v
	}
	return out
}

// ScanLanguages 遍历语言注册表里的全部工具并逐个探测, 结果写入同一个缓存。
//
// ScanLanguages scans all registered languages for tool availability.
//
// 注意：与 Scan 不同, 这里是串行探测且不传版本正则 (只判可用性, 拿不到版本号),
// 并且没有 nil 检查 —— reg 为 nil 会直接 panic。
func (d *ToolDetector) ScanLanguages(reg *LanguageRegistry) {
	for _, profile := range reg.profiles {
		for _, tool := range profile.Tools {
			cap := d.probe(context.Background(), tool.Name, []string{tool.Cmd, "--version"}, ``, tool.OutputFmt, tool.Priority)
			d.mu.Lock()
			d.cache[tool.Name] = cap
			d.mu.Unlock()
		}
	}
}
