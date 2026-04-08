// Package dynmcp 实现动态 MCP 服务器管理。
// 对应 TS: services/mcp/useManageMCPConnections.ts
//
// 核心能力:
//   - 运行时动态注册/卸载 MCP 服务器 (不需要重启)
//   - 进程级别共享连接 (所有 Session 共享同一个 Manager)
//   - 工具注册表自动刷新 (通过版本号 + refreshTools 模式)
//   - 线程安全 (RWMutex 保护共享状态)
//
// 对应 TS 中的关键模式:
//   - connectToServer (memoized): 对应 AddServer
//   - updateServer + flushPendingUpdates: 对应 version 递增触发 refresh
//   - excludeStalePluginClients: 对应 RemoveServer
//   - refreshTools callback: 对应 RefreshToolsForRegistry
package dynmcp

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/anthropic/claude-go/pkg/mcp"
	"github.com/anthropic/claude-go/pkg/tool"
)

// Manager 动态 MCP 管理器。
// 进程级别单例，所有 Session 共享同一个 Manager 实例。
//
// 设计思路 (对应 TS: AppState.mcp):
//   - connections: 当前所有活跃的 MCP 连接 (对应 mcp.clients)
//   - version: 每次 add/remove 递增，Session 通过比较 version 判断是否需要 refresh
//   - subscribers: 注册的回调函数，在连接变更时被通知
type Manager struct {
	mu          sync.RWMutex
	connections map[string]*mcp.Connection // name → connection
	version     atomic.Int64               // 每次变更递增，用于 refreshTools 判断
	mcpClient   *mcp.Client
	onChange    []func()                   // 变更通知回调
}

// NewManager 创建动态 MCP 管理器
func NewManager() *Manager {
	m := &Manager{
		connections: make(map[string]*mcp.Connection),
		mcpClient:   mcp.NewClient(),
	}
	return m
}

// AddServer 动态添加一个 MCP 服务器。
// 对应 TS: connectToServer (memoized) → updateServer
//
// 流程:
//  1. 如果同名服务器已存在，先关闭旧连接
//  2. 建立新连接 (initialize + tools/list)
//  3. 更新 connections map
//  4. 递增 version (触发 Session 刷新工具)
//  5. 通知所有 subscriber
func (m *Manager) AddServer(ctx context.Context, config mcp.ServerConfig) error {
	// 先关闭同名旧连接
	m.mu.Lock()
	if old, exists := m.connections[config.Name]; exists {
		old.Close()
		delete(m.connections, config.Name)
	}
	m.mu.Unlock()

	conn, err := m.mcpClient.Connect(ctx, config)
	if err != nil {
		return fmt.Errorf("连接 MCP 服务器 %q 失败: %w", config.Name, err)
	}

	m.mu.Lock()
	m.connections[config.Name] = conn
	m.version.Add(1)
	callbacks := make([]func(), len(m.onChange))
	copy(callbacks, m.onChange)
	m.mu.Unlock()

	log.Printf("[DynMCP] 已添加: %s (%d 个工具)", config.Name, len(conn.Tools))

	for _, cb := range callbacks {
		cb()
	}
	return nil
}

// RemoveServer 动态卸载一个 MCP 服务器。
// 对应 TS: excludeStalePluginClients → clearServerCache
func (m *Manager) RemoveServer(name string) error {
	m.mu.Lock()
	conn, exists := m.connections[name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("MCP 服务器 %q 不存在", name)
	}
	delete(m.connections, name)
	m.version.Add(1)
	callbacks := make([]func(), len(m.onChange))
	copy(callbacks, m.onChange)
	m.mu.Unlock()

	if err := conn.Close(); err != nil {
		log.Printf("[DynMCP] 关闭 %s 失败: %v", name, err)
	}

	log.Printf("[DynMCP] 已移除: %s", name)

	for _, cb := range callbacks {
		cb()
	}
	return nil
}

// ListServers 列出所有已连接的 MCP 服务器
func (m *Manager) ListServers() []ServerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []ServerInfo
	for name, conn := range m.connections {
		var toolNames []string
		for _, t := range conn.Tools {
			toolNames = append(toolNames, t.Name)
		}
		result = append(result, ServerInfo{
			Name:      name,
			Status:    conn.Status,
			Transport: conn.Config.Transport,
			ToolCount: len(conn.Tools),
			Tools:     toolNames,
		})
	}
	return result
}

// ServerInfo 服务器信息摘要
type ServerInfo struct {
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Transport string   `json:"transport"`
	ToolCount int      `json:"toolCount"`
	Tools     []string `json:"tools"`
}

// GetConnections 获取所有活跃连接 (用于工具注册)
func (m *Manager) GetConnections() []*mcp.Connection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*mcp.Connection, 0, len(m.connections))
	for _, conn := range m.connections {
		result = append(result, conn)
	}
	return result
}

// Version 返回当前版本号。
// Session 在每轮 queryLoop 开始时比较 version 决定是否需要 refreshTools。
// 对应 TS: refreshTools callback 在 query.ts 的 turns 之间调用。
func (m *Manager) Version() int64 {
	return m.version.Load()
}

// OnChange 注册变更回调
func (m *Manager) OnChange(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = append(m.onChange, fn)
}

// RefreshToolsForRegistry 将当前 MCP 工具同步到 Registry。
// 对应 TS: assembleToolPool 中合并 mcp.tools
//
// 算法:
//  1. 移除 Registry 中所有 "mcp_" 前缀的工具
//  2. 重新注册当前 Manager 中所有连接的工具
func (m *Manager) RefreshToolsForRegistry(reg *tool.Registry) {
	// 移除所有旧 MCP 工具
	for _, name := range reg.Names() {
		if len(name) > 4 && name[:4] == "mcp_" {
			reg.Remove(name)
		}
	}
	// 注册当前 MCP 工具
	mcp.RegisterMCPTools(reg, m.GetConnections())
}

// Shutdown 关闭所有连接
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, conn := range m.connections {
		if err := conn.Close(); err != nil {
			log.Printf("[DynMCP] 关闭 %s 失败: %v", name, err)
		}
	}
	m.connections = make(map[string]*mcp.Connection)
}

// InitFromConfigs 从配置列表批量初始化连接
func (m *Manager) InitFromConfigs(ctx context.Context, configs []mcp.ServerConfig) {
	for _, cfg := range configs {
		if err := m.AddServer(ctx, cfg); err != nil {
			log.Printf("[DynMCP] 初始化 %s 失败: %v", cfg.Name, err)
		}
	}
}
