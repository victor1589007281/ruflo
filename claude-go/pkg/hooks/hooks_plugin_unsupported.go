//go:build !(linux || freebsd || darwin)
// +build !linux,!freebsd,!darwin

package hooks

import (
	"fmt"

	"github.com/anthropic/claude-go/pkg/types"
)

// executePluginHook 非 unix 平台 (windows 等) 的桩实现: Go plugin 不受支持, 恒报错。
// 注意：这是**编译期按平台二选一**的实现 (unix 版本在 hooks_plugin.go), 不是死代码;
// 报错而非静默跳过, 是为了让"配了 plugin hook 却没生效"这件事在运行期可见。
func (r *Runner) executePluginHook(config types.HookConfig, input types.HookInput) (*types.HookOutput, error) {
	return nil, fmt.Errorf("plugin hook: Go plugin is not supported on this platform")
}
