// Package linux 采集主机侧只读证据：负载、内存/swap、PSI、磁盘与 inode、
// 进程状态、failed units、内核日志。
//
// 这里不执行任意 shell：所有外部命令都是固定 argv（ps/df/systemctl/journalctl/ss），
// 由代码决定参数；模型只能选择采样次数与窗口。
package linux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/ops/evidence"
)

const (
	defaultSamples        = 3
	defaultSampleInterval = 1 * time.Second
	defaultCmdTimeout     = 8 * time.Second
	defaultWindow         = 30 * time.Minute
	maxCommandBytes       = 256 << 10
)

// 阈值集中在这里，便于按环境调整而不改分析逻辑。
const (
	loadPerCPUWarn  = 1.5
	memAvailWarn    = 0.10
	swapUsedWarn    = 0.50
	psiCPUWarn      = 20.0
	psiIOWarn       = 20.0
	psiMemWarn      = 10.0
	diskUsedWarn    = 0.90
	inodeUsedWarn   = 0.90
	journalLookback = 30 * time.Minute
)

// Config 是 Linux 采集参数。Target 只用于报告与档案命名，不参与命令拼接。
type Config struct {
	Target         string
	Window         time.Duration
	Samples        int
	SampleInterval time.Duration
	QueryTimeout   time.Duration
	Now            func() time.Time
}

// Collector 采集单台主机。Samples 连续采样，且不隐去“只覆盖几秒”的事实。
type Collector struct {
	cfg  Config
	now  func() time.Time
	host string
}

// New 构造采集器并做最小自检。
func New(cfg Config) (*Collector, error) {
	if cfg.Samples <= 0 {
		cfg.Samples = defaultSamples
	}
	if cfg.SampleInterval <= 0 {
		cfg.SampleInterval = defaultSampleInterval
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = defaultCmdTimeout
	}
	if cfg.Window <= 0 {
		cfg.Window = defaultWindow
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if strings.TrimSpace(cfg.Target) == "" {
		h, err := os.Hostname()
		if err != nil {
			return nil, err
		}
		cfg.Target = h
	}
	host, err := os.Hostname()
	if err != nil {
		host = cfg.Target
	}
	return &Collector{cfg: cfg, now: cfg.Now, host: host}, nil
}

type statusSample struct {
	at           time.Time
	loadValid    bool
	memValid     bool
	procValid    bool
	load1        float64
	load5        float64
	load15       float64
	memTotal     uint64
	memAvailable uint64
	swapTotal    uint64
	swapFree     uint64
	psiCPU       float64
	psiIO        float64
	psiMem       float64
	psiValid     bool
	dState       int
	zombie       int
	dTop         []string
}

type fsRow struct {
	Device     string
	Mount      string
	Size       uint64
	Used       uint64
	Avail      uint64
	UsedRatio  float64
	Inodes     uint64
	InodesUsed uint64
	InodesFree uint64
	InodeRatio float64
	InodeValid bool
}

type kernelEvent struct {
	At      time.Time
	Line    string
	Kind    string // oom|segfault|io|hung|fault|other
	Process string
	PID     int
}

type failedUnit struct {
	Unit        string
	Load        string
	Active      string
	Sub         string
	Description string
}

type snapshot struct {
	hostname      string
	kernel        string
	osRelease     string
	cpuCount      int
	uptimeSeconds float64

	samples []statusSample
	fs      []fsRow
	fsValid bool
	fsNote  string

	fileNrAllocated uint64
	fileMax         uint64
	fdValid         bool
	fdNote          string

	failedUnits []failedUnit
	failedValid bool
	failedNote  string

	kernelEvents []kernelEvent
	kernelValid  bool
	kernelNote   string

	sockets     string
	socketValid bool
	socketNote  string
}

// Collect 采集一次主机快照并生成证据包。
func (c *Collector) Collect(ctx context.Context) (*evidence.Bundle, error) {
	s := &snapshot{hostname: c.host, cpuCount: runtime.NumCPU()}
	s.kernel = firstLine(readFile("/proc/sys/kernel/osrelease"))
	s.osRelease = osPrettyName(readFile("/etc/os-release"))
	s.uptimeSeconds = parseUptime(readFile("/proc/uptime"))

	for i := 0; i < c.cfg.Samples; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sm := statusSample{at: c.now()}
		if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
			sm.load1, sm.load5, sm.load15 = parseLoad(string(raw))
			sm.loadValid = true
		}
		if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
			sm.memTotal, sm.memAvailable, sm.swapTotal, sm.swapFree = parseMeminfo(string(raw))
			sm.memValid = true
		}
		if cpu, io, mem, ok := readPSI(); ok {
			sm.psiCPU, sm.psiIO, sm.psiMem, sm.psiValid = cpu, io, mem, true
		}
		if out, _, err := c.run(ctx, "ps", "-eo", "stat=,comm=", "--no-headers"); err == nil {
			sm.dState, sm.zombie, sm.dTop = parseProcessStates(out)
			sm.procValid = true
		}
		s.samples = append(s.samples, sm)
		if i < c.cfg.Samples-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.cfg.SampleInterval):
			}
		}
	}

	if out, truncated, err := c.run(ctx, "df", "-P", "-B1", "-x", "tmpfs", "-x", "devtmpfs", "-x", "squashfs"); err != nil {
		s.fsNote = "df 失败: " + err.Error()
	} else {
		s.fs, s.fsValid = parseDf(out)
		if truncated {
			s.fsNote = "df 输出被截断，磁盘清单可能不完整"
		}
	}
	if inodes, _, err := c.run(ctx, "df", "-P", "-i", "-x", "tmpfs", "-x", "devtmpfs", "-x", "squashfs"); err == nil {
		mergeInodes(s.fs, parseDfInodes(inodes))
	}

	if raw, err := os.ReadFile("/proc/sys/fs/file-nr"); err == nil {
		s.fileNrAllocated = parseFirstUint(string(raw))
		s.fdValid = true
	}
	if raw, err := os.ReadFile("/proc/sys/fs/file-max"); err == nil {
		s.fileMax = parseFirstUint(string(raw))
	} else {
		s.fdNote = "读取 /proc/sys/fs/file-max 失败"
	}

	if out, _, err := c.run(ctx, "systemctl", "--failed", "--no-pager", "--plain", "--no-legend"); err != nil {
		s.failedNote = "systemctl --failed 不可用: " + err.Error()
	} else {
		s.failedUnits = parseFailedUnits(out)
		s.failedValid = true
	}

	since := fmt.Sprintf("-%d min", int(c.cfg.Window.Minutes()))
	if since == "-0 min" {
		since = "-30 min"
	}
	if out, truncated, err := c.run(ctx, "journalctl", "-k", "-p", "warning", "--since", since, "--no-pager", "-o", "short-iso"); err != nil {
		s.kernelNote = "journalctl -k 不可用: " + err.Error()
	} else {
		s.kernelEvents = parseKernelLog(out, c.now())
		s.kernelValid = true
		if truncated {
			s.kernelNote = "内核日志输出被截断（只保留了前 256KiB）"
		}
	}

	if out, _, err := c.run(ctx, "ss", "-s"); err != nil {
		s.socketNote = "ss -s 不可用: " + err.Error()
	} else {
		s.sockets = strings.TrimSpace(out)
		s.socketValid = s.sockets != ""
	}

	b := c.buildBundle(s)
	analyze(b, s)
	if err := b.Validate(); err != nil {
		return b, err
	}
	return b, nil
}

// run 执行固定 argv 命令；不经过 shell，也不接受模型提供的参数。
func (c *Collector) run(ctx context.Context, name string, args ...string) (out string, truncated bool, err error) {
	runCtx, cancel := context.WithTimeout(ctx, c.cfg.QueryTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	stdout := &capBuffer{limit: maxCommandBytes}
	stderr := &capBuffer{limit: 4096}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	out = stdout.String()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return out, stdout.truncated, fmt.Errorf("%v: %s", err, msg)
		}
		return out, stdout.truncated, err
	}
	return out, stdout.truncated, nil
}

type capBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	remain := b.limit - b.buf.Len()
	if remain <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remain {
		b.buf.Write(p[:remain])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *capBuffer) String() string { return b.buf.String() }

// ---- 解析 ------------------------------------------------------------------

func readFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func osPrettyName(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"'`)
		}
	}
	return ""
}

func parseUptime(raw string) float64 {
	f := strings.Fields(raw)
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func parseLoad(raw string) (l1, l5, l15 float64) {
	f := strings.Fields(raw)
	if len(f) >= 3 {
		l1, _ = strconv.ParseFloat(f[0], 64)
		l5, _ = strconv.ParseFloat(f[1], 64)
		l15, _ = strconv.ParseFloat(f[2], 64)
	}
	return
}

func parseMeminfo(raw string) (total, available, swapTotal, swapFree uint64) {
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch strings.TrimSuffix(f[0], ":") {
		case "MemTotal":
			total = v * 1024
		case "MemAvailable":
			available = v * 1024
		case "SwapTotal":
			swapTotal = v * 1024
		case "SwapFree":
			swapFree = v * 1024
		}
	}
	return
}

func readPSI() (cpu, io, mem float64, ok bool) {
	read := func(name string) (float64, bool) {
		raw, err := os.ReadFile(filepath.Join("/proc/pressure", name))
		if err != nil {
			return 0, false
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(line, "some ") {
				continue
			}
			for _, kv := range strings.Fields(line)[1:] {
				if strings.HasPrefix(kv, "avg300=") {
					v, err := strconv.ParseFloat(strings.TrimPrefix(kv, "avg300="), 64)
					return v, err == nil
				}
			}
		}
		return 0, false
	}
	var okCPU, okIO, okMem bool
	cpu, okCPU = read("cpu")
	io, okIO = read("io")
	mem, okMem = read("memory")
	return cpu, io, mem, okCPU && okIO && okMem
}

// parseProcessStates 只统计状态，不采集命令行，避免把业务参数写进证据档案。
func parseProcessStates(raw string) (dState, zombie int, dTop []string) {
	counts := map[string]int{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		state := f[0]
		comm := strings.Join(f[1:], " ")
		if strings.Contains(state, "D") {
			dState++
			counts[comm]++
		}
		if strings.Contains(state, "Z") {
			zombie++
		}
	}
	type kv struct {
		k string
		n int
	}
	var list []kv
	for k, n := range counts {
		list = append(list, kv{k, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].k < list[j].k
	})
	for i, x := range list {
		if i >= 5 {
			break
		}
		dTop = append(dTop, fmt.Sprintf("%s×%d", x.k, x.n))
	}
	return
}

func parseDf(raw string) ([]fsRow, bool) {
	var rows []fsRow
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[0] == "Filesystem" {
			continue
		}
		size, _ := strconv.ParseUint(f[1], 10, 64)
		used, _ := strconv.ParseUint(f[2], 10, 64)
		avail, _ := strconv.ParseUint(f[3], 10, 64)
		pct := strings.TrimSuffix(f[len(f)-2], "%")
		p, _ := strconv.ParseFloat(pct, 64)
		rows = append(rows, fsRow{
			Device: f[0], Mount: f[len(f)-1], Size: size, Used: used, Avail: avail,
			UsedRatio: p / 100,
		})
	}
	return rows, len(rows) > 0
}

func parseDfInodes(raw string) map[string]fsRow {
	out := map[string]fsRow{}
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[0] == "Filesystem" {
			continue
		}
		mount := f[len(f)-1]
		total, _ := strconv.ParseUint(f[1], 10, 64)
		used, _ := strconv.ParseUint(f[2], 10, 64)
		free, _ := strconv.ParseUint(f[3], 10, 64)
		pct := strings.TrimSuffix(f[len(f)-2], "%")
		p, _ := strconv.ParseFloat(pct, 64)
		out[mount] = fsRow{Inodes: total, InodesUsed: used, InodesFree: free, InodeRatio: p / 100, InodeValid: true}
	}
	return out
}

func mergeInodes(rows []fsRow, inodes map[string]fsRow) {
	for i := range rows {
		if in, ok := inodes[rows[i].Mount]; ok {
			rows[i].Inodes = in.Inodes
			rows[i].InodesUsed = in.InodesUsed
			rows[i].InodesFree = in.InodesFree
			rows[i].InodeRatio = in.InodeRatio
			rows[i].InodeValid = in.InodeValid
		}
	}
}

func parseFirstUint(raw string) uint64 {
	f := strings.Fields(raw)
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseUint(f[0], 10, 64)
	return v
}

func parseFailedUnits(raw string) []failedUnit {
	var out []failedUnit
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		out = append(out, failedUnit{
			Unit: f[0], Load: f[1], Active: f[2], Sub: f[3],
			Description: strings.Join(f[4:], " "),
		})
	}
	return out
}

var (
	reKilledProcess = regexp.MustCompile(`(?i)Killed process (\d+) \(([^)]+)\)`)
	reOOMTask       = regexp.MustCompile(`(?i)oom-kill:.*?task=([^,]+), pid=(\d+)`)
	reSegfault      = regexp.MustCompile(`(?i)([A-Za-z0-9_.\-/]+)\[(\d+)\]: segfault at`)
)

// parseKernelLog 只保留可分类的内核告警；UFW BLOCK 这类常规丢包不计入异常。
func parseKernelLog(raw string, now time.Time) []kernelEvent {
	var out []kernelEvent
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "ufw block") || strings.Contains(lower, "ufw audit") {
			continue
		}
		ev := kernelEvent{Line: line, At: now}
		if f := strings.Fields(line); len(f) > 0 {
			if ts, ok := parseJournalTS(f[0]); ok {
				ev.At = ts
			}
		}
		switch {
		case strings.Contains(lower, "out of memory") || strings.Contains(lower, "oom-kill") || strings.Contains(lower, "killed process"):
			ev.Kind = "oom"
			if m := reKilledProcess.FindStringSubmatch(line); len(m) == 3 {
				ev.PID, _ = strconv.Atoi(m[1])
				ev.Process = m[2]
			} else if m := reOOMTask.FindStringSubmatch(line); len(m) == 3 {
				ev.Process = strings.TrimSpace(m[1])
				ev.PID, _ = strconv.Atoi(m[2])
			}
		case strings.Contains(lower, "segfault at"):
			ev.Kind = "segfault"
			if m := reSegfault.FindStringSubmatch(line); len(m) == 3 {
				ev.Process = m[1]
				ev.PID, _ = strconv.Atoi(m[2])
			}
		case strings.Contains(lower, "i/o error") || strings.Contains(lower, "blk_update_request") ||
			strings.Contains(lower, "ext4-fs error") || strings.Contains(lower, "xfs") && strings.Contains(lower, "error") ||
			strings.Contains(lower, "nvme") && strings.Contains(lower, "timeout"):
			ev.Kind = "io"
		case strings.Contains(lower, "blocked for more than") || strings.Contains(lower, "hung_task") ||
			strings.Contains(lower, "soft lockup") || strings.Contains(lower, "hard lockup") ||
			strings.Contains(lower, "rcu stall"):
			ev.Kind = "hung"
		case strings.Contains(lower, "bug:") || strings.Contains(lower, "general protection fault") ||
			strings.Contains(lower, "kernel panic") || strings.Contains(lower, "oops:"):
			ev.Kind = "fault"
		default:
			ev.Kind = "other"
		}
		out = append(out, ev)
	}
	return out
}

func parseJournalTS(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339, "2006-01-02T15:04:05.000000-0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
