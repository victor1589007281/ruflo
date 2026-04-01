// Package plugins 提供基于 JSON 注册表的插件元数据管理（非动态加载 .so）。
//
// 设计思路：
//   - 注册表文件默认位于 {baseDir}/.claude-flow/plugins/registry.json，与 Claude Flow 生态约定路径一致。
//   - PluginManager 在每次 List/Install 等操作前后用互斥锁 + load/save 保证并发安全；缺失文件视为空注册表。
//   - Install 按名称 upsert；Uninstall 过滤删除；Enable/Disable 切换 Enabled 标志。
package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// PluginInfo 描述已安装或登记的一条插件元数据（序列化至 registry.json）。
type PluginInfo struct {
	Name        string `json:"name"`        // 插件唯一名称/ID
	Version     string `json:"version"`     // 语义化版本字符串
	Description string `json:"description"` // 人类可读说明
	Enabled     bool   `json:"enabled"`     // 是否启用（CLI/业务可据此过滤）
}

type registryFile struct {
	Plugins []PluginInfo `json:"plugins"`
}

// PluginManager 对磁盘上的插件注册表执行增删改查。
type PluginManager struct {
	path string       // registry.json 绝对路径
	mu   sync.Mutex // 序列化 load/save 与列表遍历
}

// NewPluginManager 在 baseDir/.claude-flow/plugins/registry.json 创建管理器；baseDir 空则使用当前工作目录。
func NewPluginManager(baseDir string) *PluginManager {
	if baseDir == "" {
		baseDir, _ = os.Getwd()
	}
	p := filepath.Join(baseDir, ".claude-flow", "plugins", "registry.json")
	return &PluginManager{path: p}
}

// NewPluginManagerWithPath 使用显式注册表文件路径（便于测试或自定义布局）。
func NewPluginManagerWithPath(registryPath string) *PluginManager {
	return &PluginManager{path: registryPath}
}

// load 读取并反序列化注册表；文件不存在或空文件返回零值结构，不视为错误。
func (m *PluginManager) load() (registryFile, error) {
	var r registryFile
	b, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, nil
		}
		return r, err
	}
	if len(b) == 0 {
		return r, nil
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, err
	}
	return r, nil
}

// save 将注册表以缩进 JSON 写回 path，必要时创建父目录。
func (m *PluginManager) save(r registryFile) error {
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, b, 0o644)
}

// indexByName 构建 插件名 → Plugins 切片下标，供 Install/Enable 等 O(1) 定位。
func (m *PluginManager) indexByName(r registryFile) map[string]int {
	out := make(map[string]int)
	for i, p := range r.Plugins {
		out[p.Name] = i
	}
	return out
}

// List 返回注册表中全部插件副本，按 Name 字典序排序。
func (m *PluginManager) List() ([]PluginInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.load()
	if err != nil {
		return nil, err
	}
	sort.Slice(r.Plugins, func(i, j int) bool { return r.Plugins[i].Name < r.Plugins[j].Name })
	out := make([]PluginInfo, len(r.Plugins))
	copy(out, r.Plugins)
	return out, nil
}

// Install 按名称新增或更新插件记录，默认 Enabled=true。
func (m *PluginManager) Install(name, version, description string) error {
	if name == "" {
		return fmt.Errorf("plugins: name required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.load()
	if err != nil {
		return err
	}
	idx := m.indexByName(r)
	pi := PluginInfo{Name: name, Version: version, Description: description, Enabled: true}
	if i, ok := idx[name]; ok {
		r.Plugins[i] = pi
	} else {
		r.Plugins = append(r.Plugins, pi)
	}
	return m.save(r)
}

// Uninstall 按名称从注册表移除插件（不存在则静默无操作于该条）。
func (m *PluginManager) Uninstall(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.load()
	if err != nil {
		return err
	}
	next := r.Plugins[:0]
	for _, p := range r.Plugins {
		if p.Name != name {
			next = append(next, p)
		}
	}
	r.Plugins = next
	return m.save(r)
}

// Enable 将指定插件标记为启用。
func (m *PluginManager) Enable(name string) error {
	return m.setEnabled(name, true)
}

// Disable 将指定插件标记为禁用。
func (m *PluginManager) Disable(name string) error {
	return m.setEnabled(name, false)
}

// setEnabled 在持锁下查找名称并写入 Enabled 标志。
func (m *PluginManager) setEnabled(name string, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.load()
	if err != nil {
		return err
	}
	idx := m.indexByName(r)
	i, ok := idx[name]
	if !ok {
		return fmt.Errorf("plugins: %q not found", name)
	}
	r.Plugins[i].Enabled = on
	return m.save(r)
}

// DefaultPath 返回相对于项目根目录的约定注册表路径（.claude-flow/plugins/registry.json）。
func DefaultPath() string {
	return filepath.Join(".claude-flow", "plugins", "registry.json")
}
