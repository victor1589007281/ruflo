//go:build linux || freebsd || darwin
// +build linux freebsd darwin

package hooks

import (
	"encoding/json"
	"fmt"
	"plugin"
	"strings"

	"github.com/anthropic/claude-go/pkg/types"
)

// executePluginHook 通过 Go plugin 加载 .so 文件并调用导出函数。
// 期望的插件导出符号签名: func([]byte) ([]byte, error)
// 输入输出均为 JSON 序列化的 bytes。
//
// 配置: PluginPath = .so 文件路径（Command 作为回退）,
//       PluginSymbol = 导出符号名（默认 "Hook"）。
func (r *Runner) executePluginHook(config types.HookConfig, input types.HookInput) (*types.HookOutput, error) {
	soPath := strings.TrimSpace(config.PluginPath)
	if soPath == "" {
		soPath = strings.TrimSpace(config.Command)
	}
	if soPath == "" {
		return nil, fmt.Errorf("plugin hook: empty plugin path")
	}

	symbolName := strings.TrimSpace(config.PluginSymbol)
	if symbolName == "" {
		symbolName = "Hook"
	}

	p, err := plugin.Open(soPath)
	if err != nil {
		return nil, fmt.Errorf("plugin hook: open %s: %w", soPath, err)
	}

	sym, err := p.Lookup(symbolName)
	if err != nil {
		return nil, fmt.Errorf("plugin hook: lookup symbol %s: %w", symbolName, err)
	}

	hookFn, ok := sym.(func([]byte) ([]byte, error))
	if !ok {
		return nil, fmt.Errorf("plugin hook: symbol %s has incompatible type (expected func([]byte) ([]byte, error))", symbolName)
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("plugin hook: marshal input: %w", err)
	}

	outputJSON, err := hookFn(inputJSON)
	if err != nil {
		return nil, fmt.Errorf("plugin hook: execution failed: %w", err)
	}

	var hookOut types.HookOutput
	if err := json.Unmarshal(outputJSON, &hookOut); err != nil {
		return nil, fmt.Errorf("plugin hook: unmarshal output: %w", err)
	}

	return &hookOut, nil
}
