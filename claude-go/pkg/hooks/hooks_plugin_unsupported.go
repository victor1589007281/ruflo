//go:build !(linux || freebsd || darwin)
// +build !linux,!freebsd,!darwin

package hooks

import (
	"fmt"

	"github.com/anthropic/claude-go/pkg/types"
)

func (r *Runner) executePluginHook(config types.HookConfig, input types.HookInput) (*types.HookOutput, error) {
	return nil, fmt.Errorf("plugin hook: Go plugin is not supported on this platform")
}
