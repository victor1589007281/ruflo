package sandbox

import (
	"sync"

	"github.com/anthropic/claude-go/pkg/metrics"
)

type BoolPtr = *bool

type Config struct {
	Enabled             BoolPtr       `json:"enabled,omitempty"`
	Mode                string        `json:"mode,omitempty"` // auto|native-cgroupv2|docker|k8s|process
	RequiredForTeam     BoolPtr       `json:"requiredForTeam,omitempty"`
	AllowUnsafeFallback bool          `json:"allowUnsafeFallback,omitempty"`
	DefaultImage        string        `json:"defaultImage,omitempty"`
	NetworkDisabled     BoolPtr       `json:"networkDisabled,omitempty"`
	OutputMaxBytes      int64         `json:"outputMaxBytes,omitempty"`
	PreviewMaxBytes     int           `json:"previewMaxBytes,omitempty"`
	LogMaxBytes         int64         `json:"logMaxBytes,omitempty"`
	PidsMax             int           `json:"pidsMax,omitempty"`
	MetricsEnabled      BoolPtr       `json:"metricsEnabled,omitempty"`
	StateDir            string        `json:"-"`
	Docker              DockerConfig  `json:"docker,omitempty"`
	Native              NativeConfig  `json:"native,omitempty"`
	K8S                 K8SConfig     `json:"k8s,omitempty"`
	Resolved            runtimeConfig `json:"-"`
}

type DockerConfig struct {
	Image                  string `json:"image,omitempty"`
	DisableAutoPull        bool   `json:"disableAutoPull,omitempty"`
	KeepSandboxOnFailure   bool   `json:"keepSandboxOnFailure,omitempty"`
	TempSize               string `json:"tempSize,omitempty"`
	UseCurrentDockerDaemon bool   `json:"useCurrentDockerDaemon,omitempty"`
}

type NativeConfig struct {
	BasePath string `json:"basePath,omitempty"`
}

type K8SConfig struct {
	Enabled                 BoolPtr `json:"enabled,omitempty"`
	Namespace               string  `json:"namespace,omitempty"`
	ServiceAccount          string  `json:"serviceAccount,omitempty"`
	Image                   string  `json:"image,omitempty"`
	WorkspacePVC            string  `json:"workspacePVC,omitempty"`
	WorkspaceSubPath        string  `json:"workspaceSubPath,omitempty"`
	WorkspaceMountPath      string  `json:"workspaceMountPath,omitempty"`
	TTLSecondsAfterFinished int32   `json:"ttlSecondsAfterFinished,omitempty"`
}

type runtimeConfig struct {
	Enabled             bool
	Mode                string
	RequiredForTeam     bool
	AllowUnsafeFallback bool
	DefaultImage        string
	NetworkDisabled     bool
	OutputMaxBytes      int64
	PreviewMaxBytes     int
	LogMaxBytes         int64
	PidsMax             int
	MetricsEnabled      bool
	StateDir            string
	Docker              DockerConfig
	Native              NativeConfig
	K8S                 K8SConfig
}

var (
	configMu      sync.RWMutex
	currentConfig = defaultRuntimeConfig()
)

// defaultRuntimeConfig 返回沙箱的内置默认配置 (启用 / auto 模式 / 团队必需 / 禁网 /
// 输出 4MiB、预览 128KiB、日志 16MiB / pids 256 / 开指标)。
// Configure 每次都从这里起步再叠加, 所以这里的值就是"未配置项"的实际取值。
func defaultRuntimeConfig() runtimeConfig {
	return runtimeConfig{
		Enabled:             true,
		Mode:                "auto",
		RequiredForTeam:     true,
		AllowUnsafeFallback: false,
		NetworkDisabled:     true,
		OutputMaxBytes:      4 * 1024 * 1024,
		PreviewMaxBytes:     128 * 1024,
		LogMaxBytes:         16 * 1024 * 1024,
		PidsMax:             256,
		MetricsEnabled:      true,
		Docker: DockerConfig{
			Image:    "golang:1.24",
			TempSize: "512m",
		},
		K8S: K8SConfig{
			Namespace:               "claude-go-sandbox",
			WorkspaceMountPath:      "/workspace",
			TTLSecondsAfterFinished: 3600,
		},
	}
}

// Configure 用传入的 Config 覆盖当前沙箱配置, 并让 manager 重新解析。
//
// 注意：语义是**整体替换而非增量合并** —— 每次从 defaultRuntimeConfig() 起步, 只把
// 本次显式给出的项盖上去, 没给的项一律回到默认值 (含 next.StateDir = cfg.StateDir
// 这种无条件赋值)。因此两次 Configure 之间不会保留上一次的调整。
//
// 这也是"零值即未设置"这一约定的来源: 字符串空、数值 <=0 都当作没配; 指针字段
// (BoolPtr) 用 nil 区分"没配"与"显式配成 false", 其余裸 bool 字段只能表达"开",
// 想显式关掉一个默认开的开关时它们做不到 (默认值本身就是关的, 所以当前不构成问题)。
func Configure(cfg Config) {
	configMu.Lock()
	defer configMu.Unlock()

	next := defaultRuntimeConfig()
	if cfg.Enabled != nil {
		next.Enabled = *cfg.Enabled
	}
	if cfg.Mode != "" {
		next.Mode = cfg.Mode
	}
	if cfg.RequiredForTeam != nil {
		next.RequiredForTeam = *cfg.RequiredForTeam
	}
	if cfg.AllowUnsafeFallback {
		next.AllowUnsafeFallback = true
	}
	if cfg.DefaultImage != "" {
		next.DefaultImage = cfg.DefaultImage
		next.Docker.Image = cfg.DefaultImage
	}
	if cfg.NetworkDisabled != nil {
		next.NetworkDisabled = *cfg.NetworkDisabled
	}
	if cfg.OutputMaxBytes > 0 {
		next.OutputMaxBytes = cfg.OutputMaxBytes
	}
	if cfg.PreviewMaxBytes > 0 {
		next.PreviewMaxBytes = cfg.PreviewMaxBytes
	}
	if cfg.LogMaxBytes > 0 {
		next.LogMaxBytes = cfg.LogMaxBytes
	}
	if cfg.PidsMax > 0 {
		next.PidsMax = cfg.PidsMax
	}
	if cfg.MetricsEnabled != nil {
		next.MetricsEnabled = *cfg.MetricsEnabled
	}
	next.StateDir = cfg.StateDir
	if cfg.Docker.Image != "" {
		next.Docker.Image = cfg.Docker.Image
	}
	if cfg.Docker.TempSize != "" {
		next.Docker.TempSize = cfg.Docker.TempSize
	}
	next.Docker.DisableAutoPull = cfg.Docker.DisableAutoPull
	next.Docker.KeepSandboxOnFailure = cfg.Docker.KeepSandboxOnFailure
	next.Docker.UseCurrentDockerDaemon = cfg.Docker.UseCurrentDockerDaemon
	if cfg.Native.BasePath != "" {
		next.Native.BasePath = cfg.Native.BasePath
	}
	if cfg.K8S.Enabled != nil {
		next.K8S.Enabled = cfg.K8S.Enabled
	}
	if cfg.K8S.Namespace != "" {
		next.K8S.Namespace = cfg.K8S.Namespace
	}
	if cfg.K8S.ServiceAccount != "" {
		next.K8S.ServiceAccount = cfg.K8S.ServiceAccount
	}
	if cfg.K8S.Image != "" {
		next.K8S.Image = cfg.K8S.Image
	}
	if cfg.K8S.WorkspacePVC != "" {
		next.K8S.WorkspacePVC = cfg.K8S.WorkspacePVC
	}
	if cfg.K8S.WorkspaceSubPath != "" {
		next.K8S.WorkspaceSubPath = cfg.K8S.WorkspaceSubPath
	}
	if cfg.K8S.WorkspaceMountPath != "" {
		next.K8S.WorkspaceMountPath = cfg.K8S.WorkspaceMountPath
	}
	if cfg.K8S.TTLSecondsAfterFinished > 0 {
		next.K8S.TTLSecondsAfterFinished = cfg.K8S.TTLSecondsAfterFinished
	}
	currentConfig = next
	resetDefaultManager()
}

// CurrentConfig 返回当前生效的运行时配置快照 (读锁下拷贝值, 调用方拿到的是副本)。
func CurrentConfig() runtimeConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return currentConfig
}

// ResetConfigForTest 把配置与 manager 一起复位到默认值, 供测试用例之间清理全局状态。
func ResetConfigForTest() {
	configMu.Lock()
	currentConfig = defaultRuntimeConfig()
	configMu.Unlock()
	resetActiveMetrics()
	resetDefaultManager()
}

// MetricsCollector 按当前配置返回指标收集器: 关闭指标时返回 nil; 配了 StateDir 就
// 落到该目录的独立 collector, 否则用进程级全局 collector。
func MetricsCollector() *metrics.Collector {
	cfg := CurrentConfig()
	if !cfg.MetricsEnabled {
		return nil
	}
	if cfg.StateDir != "" {
		return metrics.NewCollector(cfg.StateDir)
	}
	return metrics.GlobalLLMCollector()
}
