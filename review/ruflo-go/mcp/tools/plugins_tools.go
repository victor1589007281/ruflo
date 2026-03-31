package tools

import (
	"context"
	"path/filepath"

	"github.com/ruflo/ruflo-go/mcp"
	"github.com/ruflo/ruflo-go/pkg/plugins"
)

func pluginsManager() *plugins.PluginManager {
	return plugins.NewPluginManagerWithPath(filepath.Join(resolveDataDir(), "plugins", "registry.json"))
}

func pluginsTools() []*mcp.MCPTool {
	return []*mcp.MCPTool{
		{Name: "plugins_list", Description: "List plugins", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: toolHandler(handlePluginsList)},
		{Name: "plugins_install", Description: "Install a plugin", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "version": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}}, "required": []string{"name"}}, Handler: toolHandler(handlePluginsInstall)},
		{Name: "plugins_uninstall", Description: "Uninstall a plugin", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}, Handler: toolHandler(handlePluginsUninstall)},
		{Name: "plugins_enable", Description: "Enable a plugin", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}, Handler: toolHandler(handlePluginsEnable)},
		{Name: "plugins_disable", Description: "Disable a plugin", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}, Handler: toolHandler(handlePluginsDisable)},
	}
}

func RegisterPluginsTools(reg *mcp.ToolRegistry) error {
	for _, t := range pluginsTools() {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func handlePluginsList(_ context.Context, _ map[string]any) mcp.MCPToolResult {
	list, err := pluginsManager().List()
	if err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"plugins": list}}
}

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

func handlePluginsUninstall(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if err := pluginsManager().Uninstall(name); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"uninstalled": name}}
}

func handlePluginsEnable(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if err := pluginsManager().Enable(name); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"enabled": name}}
}

func handlePluginsDisable(_ context.Context, m map[string]any) mcp.MCPToolResult {
	name := strArg(m, "name")
	if err := pluginsManager().Disable(name); err != nil {
		return mcp.MCPToolResult{OK: false, Error: err.Error()}
	}
	return mcp.MCPToolResult{OK: true, Data: map[string]any{"disabled": name}}
}
