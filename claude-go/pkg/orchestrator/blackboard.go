package orchestrator

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WriteMeta 写入操作的元数据。
type WriteMeta struct {
	Author   string // 写入者标识 (如执行器名称)
	Category string // 数据类别 (如 "output", "eval", "metric")
}

// ReadMeta 读取操作返回的元数据。
type ReadMeta struct {
	Author    string
	Category  string
	Version   int64
	UpdatedAt time.Time
}

// Entry 黑板中的单条记录。
type Entry struct {
	Key       string    `json:"key"`
	Value     any       `json:"value"`
	Author    string    `json:"author"`
	Category  string    `json:"category"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ChangeEvent 黑板变更事件, 通过 Watch 通道推送。
type ChangeEvent struct {
	Key      string
	OldValue any
	NewValue any
	Version  int64
}

// QueryFilter 定义黑板查询的过滤条件。
type QueryFilter struct {
	Category string // 按类别过滤
	Author   string // 按作者过滤
	MinVer   int64  // 最小版本号
}

// ReadOnlyBlackboard 是 TaskRunner 可见的黑板只读视图。
// 通过此接口隔离读写权限, Runner 只能读不能写。
type ReadOnlyBlackboard interface {
	Read(key string) (any, ReadMeta, error)
	Query(prefix string, filter QueryFilter) []Entry
	Snapshot() map[string]Entry
}

// Blackboard 是引擎的共享状态存储, 带版本控制和变更通知。
//
// 设计特点:
//   - 版本控制: 每次写入递增版本号, 可用于冲突检测 (乐观锁)
//   - 事件驱动: Watch 机制替代轮询, 任务完成时通过事件通知下游
//   - 结构化 Key: 推荐使用 "{taskID}/output" 格式, 便于查询和隔离
//   - 容量控制: 可配置单值最大字节数, 防止 LLM 输出过大撑爆内存
//   - 持久化: 可选的定时落盘 (2 秒 debounce), 配合检查点实现崩溃恢复
type Blackboard struct {
	mu       sync.RWMutex
	entries  map[string]Entry
	watchers map[string][]chan ChangeEvent
	maxSize  int    // 单值最大字节数; 0 = 不限
	persist  string // 持久化文件路径; 空 = 纯内存
	dirty    bool   // 脏标记, 用于 debounce 持久化
	closeCh  chan struct{}
}

// BlackboardOption 配置选项函数。
type BlackboardOption func(*Blackboard)

// WithMaxValueSize 设置单个值的最大字节数限制。
func WithMaxValueSize(n int) BlackboardOption {
	return func(b *Blackboard) { b.maxSize = n }
}

// WithPersistence 开启文件持久化, 2 秒 debounce 写盘。
func WithPersistence(path string) BlackboardOption {
	return func(b *Blackboard) { b.persist = path }
}

// NewBlackboard 创建黑板实例。
func NewBlackboard(opts ...BlackboardOption) *Blackboard {
	b := &Blackboard{
		entries:  make(map[string]Entry),
		watchers: make(map[string][]chan ChangeEvent),
		closeCh:  make(chan struct{}),
	}
	for _, o := range opts {
		o(b)
	}
	if b.persist != "" {
		_ = b.loadFromDisk()
		go b.persistLoop()
	}
	return b
}

// Write 写入一个值, 返回新的版本号。
//
// 写入流程:
//  1. 校验值大小是否超限
//  2. 递增版本号 (基于该 key 的上一个版本)
//  3. 存入 entries map
//  4. 标记 dirty (触发异步落盘)
//  5. 遍历 watchers, 通知匹配前缀的监听者
func (b *Blackboard) Write(key string, value any, meta WriteMeta) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.maxSize > 0 {
		data, _ := json.Marshal(value)
		if len(data) > b.maxSize {
			return 0, fmt.Errorf("值大小 %d 超过限制 %d (key: %s)", len(data), b.maxSize, key)
		}
	}

	old := b.entries[key]
	ver := old.Version + 1
	entry := Entry{
		Key:       key,
		Value:     value,
		Author:    meta.Author,
		Category:  meta.Category,
		Version:   ver,
		UpdatedAt: time.Now(),
	}
	b.entries[key] = entry
	b.dirty = true

	// 通知匹配前缀的 watcher (非阻塞发送, 避免慢消费者阻塞写入)
	evt := ChangeEvent{Key: key, OldValue: old.Value, NewValue: value, Version: ver}
	for prefix, chs := range b.watchers {
		if strings.HasPrefix(key, prefix) {
			for _, ch := range chs {
				select {
				case ch <- evt:
				default:
					// 消费者过慢导致通道满, 丢弃事件但记录警告便于排查 "为什么没收到事件"
					log.Printf("[blackboard] Watcher 通道已满, 丢弃事件: key=%s, prefix=%s", key, prefix)
				}
			}
		}
	}
	return ver, nil
}

// Read 读取指定 key 的最新值。
func (b *Blackboard) Read(key string) (any, ReadMeta, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	e, ok := b.entries[key]
	if !ok {
		return nil, ReadMeta{}, fmt.Errorf("key 不存在: %s", key)
	}
	return e.Value, ReadMeta{
		Author:    e.Author,
		Category:  e.Category,
		Version:   e.Version,
		UpdatedAt: e.UpdatedAt,
	}, nil
}

// Query 按前缀和过滤条件查询条目。
func (b *Blackboard) Query(prefix string, filter QueryFilter) []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var result []Entry
	for _, e := range b.entries {
		if !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		if filter.Category != "" && e.Category != filter.Category {
			continue
		}
		if filter.Author != "" && e.Author != filter.Author {
			continue
		}
		if filter.MinVer > 0 && e.Version < filter.MinVer {
			continue
		}
		result = append(result, e)
	}
	return result
}

// Snapshot 返回所有条目的深拷贝。
func (b *Blackboard) Snapshot() map[string]Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	snap := make(map[string]Entry, len(b.entries))
	for k, v := range b.entries {
		snap[k] = v
	}
	return snap
}

// Watch 返回一个事件通道, 接收匹配前缀的变更事件。
// 通道缓冲区为 64, 如果消费者过慢会丢弃事件 (不阻塞写入)。
func (b *Blackboard) Watch(prefix string) <-chan ChangeEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan ChangeEvent, 64)
	b.watchers[prefix] = append(b.watchers[prefix], ch)
	return ch
}

// Close 停止持久化循环并清理资源。
func (b *Blackboard) Close() {
	close(b.closeCh)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, chs := range b.watchers {
		for _, ch := range chs {
			close(ch)
		}
	}
	b.watchers = make(map[string][]chan ChangeEvent)
	if b.persist != "" && b.dirty {
		_ = b.saveToDisk()
	}
}

// persistLoop 2 秒 debounce 持久化循环。
// 仅在 dirty 标记为 true 时写盘, 避免无变更时的 I/O 开销。
func (b *Blackboard) persistLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			b.mu.Lock()
			if b.dirty {
				_ = b.saveToDisk()
				b.dirty = false
			}
			b.mu.Unlock()
		case <-b.closeCh:
			return
		}
	}
}

// saveToDisk 原子写盘 (先写临时文件再 rename 以防写半截)。
func (b *Blackboard) saveToDisk() error {
	dir := filepath.Dir(b.persist)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(b.entries, "", "  ")
	if err != nil {
		return err
	}

	// 原子写: 先写临时文件, 再 rename
	tmp := b.persist + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, b.persist)
}

// loadFromDisk 从持久化文件加载初始数据。
func (b *Blackboard) loadFromDisk() error {
	data, err := os.ReadFile(b.persist)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &b.entries)
}
