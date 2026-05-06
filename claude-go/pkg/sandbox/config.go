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

func CurrentConfig() runtimeConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return currentConfig
}

func ResetConfigForTest() {
	configMu.Lock()
	currentConfig = defaultRuntimeConfig()
	configMu.Unlock()
	resetActiveMetrics()
	resetDefaultManager()
}

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
