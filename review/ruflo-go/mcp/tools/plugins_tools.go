package tools

// 本文件将 pkg/plugins 的 PluginManager 暴露为 MCP 工具（plugins_*），数据落在 resolveDataDir()/plugins/registry.json。
//
// 设计思路：
//   - 列表/安装/卸载/启用/禁用均委托给现有插件管理器，保持与 CLI 行为一致。
//   - Install 的 version、description 有默认值与校验逻辑在 manager 内。

import (
	"context"
	"path/filepath"

	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/plugins"
)

// pluginsManager 构造指向数据目录下 registry.json 的 PluginManager 实例。
func pluginsManager() *plugins.PluginManager {
	return plugins.NewPluginManagerWithPath(filepath.Join(resolveDataDir(), "plugins", "registry.json"))
}

// pluginsTools 返回 plugins_list/install/uninstall/enable/disable 工具定义。
func pluginsTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{Name: "plugins_list", Description: "List plugins", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handlePluginsList)},
		{Name: "plugins_install", Description: "Install a plugin", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "version": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}}, "required": []string{"name"}}, Handler: toolHandler(handlePluginsInstall)},
		{Name: "plugins_uninstall", Description: "Uninstall a plugin", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}, Handler: toolHandler(handlePluginsUninstall)},
		{Name: "plugins_enable", Description: "Enable a plugin", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}, Handler: toolHandler(handlePluginsEnable)},
		{Name: "plugins_disable", Description: "Disable a plugin", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}, Handler: toolHandler(handlePluginsDisable)},
	}
}

// RegisterPluginsTools 向注册表登记插件管理相关 MCP 工具。
func RegisterPluginsTools(reg *mcp.ToolRegistry) error {
	for _, t := range pluginsTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// handlePluginsList 处理 plugins_list：返回已注册插件列表。
func handlePluginsList(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	list, err := pluginsManager().List()
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"plugins": list}}
}

// handlePluginsInstall 处理 plugins_install：name 必填；version 默认 0.0.0；description 可选。
func handlePluginsInstall(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if name == "" {
		return mcp.MCPToolResult{OK: false, Error: "name required"}
	}
	ver := strArg(m, "version")
	if ver == "" {
		ver = "0.0.0"
	}
	desc := strArg(m, "description")
	if err := pluginsManager().Install(name, ver, desc); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"name": name, "version": ver}}
}

// handlePluginsUninstall 处理 plugins_uninstall：按 name 卸载。
func handlePluginsUninstall(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if err := pluginsManager().Uninstall(name); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"uninstalled": name}}
}

// handlePluginsEnable 处理 plugins_enable：按 name 启用插件。
func handlePluginsEnable(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if err := pluginsManager().Enable(name); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"enabled": name}}
}

// handlePluginsDisable 处理 plugins_disable：按 name 禁用插件。
func handlePluginsDisable(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if err := pluginsManager().Disable(name); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"disabled": name}}
}
