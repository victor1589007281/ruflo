// Package logging 提供基于 log/slog 的结构化日志，支持按模块文件、全局合并文件与可选控制台输出。
//
// Init 使用 sync.Once 保证仅初始化一次；后续 Init 返回首次结果（成功或错误）。
// 传入非 nil 的 LogConfig 时，Dir/Level/Format 中空字符串表示使用默认值；Console 为 bool 零值时表示 false。
// 若只覆盖部分字段且仍需控制台输出，请显式设置 Console: true。
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// LogConfig 日志初始化配置（可从应用配置映射而来）。
type LogConfig struct {
	Dir     string // 日志目录，默认 ".claude-go/logs"
	Level   string // debug/info/warn/error，默认 "info"
	Format  string // "text" 或 "json"，默认 "text"
	Console bool   // 是否同时输出到控制台，默认 true（仅当 cfg==nil 时；见包注释）
}

var (
	initOnce sync.Once
	initErr  error

	stateMu sync.RWMutex
	state   *initState
)

type initState struct {
	cfg *LogConfig

	globalFile *os.File
	globalLW   *lockedWriter

	loggers map[string]*slog.Logger
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// Write 加锁后写底层 writer: 模块文件与全局文件会被多个 slog.Logger 共享, 无锁并发写会撕裂行。
func (lw *lockedWriter) Write(p []byte) (n int, err error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

// Init 幂等初始化日志目录、全局合并文件与内部状态。多次调用仅第一次生效，均返回第一次的错误（若有）。
func Init(cfg *LogConfig) error {
	initOnce.Do(func() {
		initErr = initInternal(cfg)
	})
	return initErr
}

// initInternal 真正做初始化: 合并配置、建目录、打开全局合并文件、装载包级状态。
//
// 全局文件用 O_APPEND 打开 (多进程追加同一文件时靠内核保证不互相覆盖);
// state 在写锁下整体替换, 因此 For() 里读到的要么是旧状态要么是新状态, 不会是半成品。
func initInternal(cfg *LogConfig) error {
	merged := mergeConfig(cfg)

	if err := os.MkdirAll(merged.Dir, 0o755); err != nil {
		return fmt.Errorf("logging: create log dir: %w", err)
	}

	globalPath := filepath.Join(merged.Dir, "claude-go.log")
	gf, err := os.OpenFile(globalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("logging: open global log: %w", err)
	}

	stateMu.Lock()
	state = &initState{
		cfg:        merged,
		globalFile: gf,
		globalLW:   &lockedWriter{w: gf},
		loggers:    make(map[string]*slog.Logger),
	}
	stateMu.Unlock()

	return nil
}

// mergeConfig 把用户配置叠加到默认值上: Dir/Level/Format 为空则用默认, Console 直接取值。
//
// 注意：Console 没有"未设置"的表示 (bool 零值就是 false), 所以 cfg 非 nil 且
// 未显式写 Console: true 时, 控制台输出会被**关掉** —— 与包注释里"默认 true"的
// 描述只在 cfg == nil 时一致。
func mergeConfig(cfg *LogConfig) *LogConfig {
	out := &LogConfig{
		Dir:     ".claude-go/logs",
		Level:   "info",
		Format:  "text",
		Console: true,
	}
	if cfg == nil {
		return out
	}
	if cfg.Dir != "" {
		out.Dir = cfg.Dir
	}
	if cfg.Level != "" {
		out.Level = cfg.Level
	}
	if cfg.Format != "" {
		out.Format = cfg.Format
	}
	out.Console = cfg.Console
	return out
}

// parseLevel 把配置里的级别字符串解析成 slog.Level; 认不出的一律回落 info (含 "warning" 变体)。
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// newHandler 按 format 建 slog handler: "json" 走 JSONHandler, 其余 (含空串) 走 TextHandler。
func newHandler(w io.Writer, level slog.Level, format string) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		return slog.NewJSONHandler(w, opts)
	default:
		return slog.NewTextHandler(w, opts)
	}
}

// Default 返回默认模块（module 名为 default）的 Logger。Init 前等价于 slog.Default()。
func Default() *slog.Logger {
	return For("default")
}

// For 返回带 module 属性的模块 Logger；写入 {Dir}/{module}.log、全局 claude-go.log，并按配置写入控制台。
// Init 前返回 slog.Default()。module 为空时按 default 处理。文件名会对路径分隔符等进行安全化。
func For(module string) *slog.Logger {
	stateMu.RLock()
	ready := state != nil
	stateMu.RUnlock()
	if !ready {
		return slog.Default()
	}

	key := module
	if key == "" {
		key = "default"
	}

	stateMu.Lock()
	defer stateMu.Unlock()

	if state == nil {
		return slog.Default()
	}
	if lg, ok := state.loggers[key]; ok {
		return lg
	}

	fileBase := safeFileBase(key)
	modPath := filepath.Join(state.cfg.Dir, fileBase+".log")
	mf, err := os.OpenFile(modPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// 不因单个模块文件失败阻塞：退回全局 + 控制台（或仅控制台）
		return fallbackLogger(state, key, err)
	}

	modLW := &lockedWriter{w: mf}

	var writers []io.Writer
	writers = append(writers, modLW, state.globalLW)
	if state.cfg.Console {
		writers = append(writers, os.Stderr)
	}

	mw := io.MultiWriter(writers...)
	h := newHandler(mw, parseLevel(state.cfg.Level), state.cfg.Format)
	lg := slog.New(h).With("module", key)
	state.loggers[key] = lg
	return lg
}

// fallbackLogger 在模块文件打不开时退回"全局文件 + 可选控制台", 并把失败原因作为属性写进日志。
//
// 降级而不是报错: 一个模块的日志文件建不出来, 不该让整个进程起不来。
func fallbackLogger(st *initState, module string, openErr error) *slog.Logger {
	var writers []io.Writer
	writers = append(writers, st.globalLW)
	if st.cfg.Console {
		writers = append(writers, os.Stderr)
	}
	mw := io.MultiWriter(writers...)
	h := newHandler(mw, parseLevel(st.cfg.Level), st.cfg.Format)
	lg := slog.New(h).With(
		"module", module,
		"logging_file_error", openErr.Error(),
	)
	st.loggers[module] = lg
	return lg
}

// safeFileBase 把模块名净化成安全的文件名: ".."、路径分隔符都替换成 "_", 再去掉首尾的点与空格。
//
// 空结果回落 "default" —— 模块名可能来自外部输入, 不净化的后果是日志写到目录之外。
func safeFileBase(name string) string {
	if name == "" {
		return "default"
	}
	s := strings.ReplaceAll(name, "..", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.Trim(s, ". ")
	if s == "" {
		return "default"
	}
	return s
}
