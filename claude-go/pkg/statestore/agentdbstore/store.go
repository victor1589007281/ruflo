// Package agentdbstore implements statestore.StateStore backed by the embedded
// AgentDB data plane (规划 14.1.4.4 层1).
//
// AgentDB 的 agentstore 原语与 statestore 三原语逐项对齐:
//
//	bucket KV   → agentstore.BucketKV (LSM 前缀分区 bucket\x00key, Scan 即 Keys)
//	AppendLog   → agentstore.JSONLog  (O_APPEND 单行, torn-tail 容忍, 中部损坏报错)
//	sha256 Blob → agentstore.BlobStore (内容寻址去重)
//	CAS / 租约   → agentstore.LeaseManager + BucketKV.CAS (first-committer-wins)
//
// 本包只依赖 agentDB 的 pkg/kv + pkg/agentstore (轻量), 不拉整个 serve/gRPC 面。
// 单写者模型: agentstore.Store.mu 提供进程内串行; 分布式场景改用 agentdbclient
// (pkg/agentdbclient) 经 serve 远程访问同一数据面。
package agentdbstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/anthropic/claude-go/pkg/statestore"
	"gitee.com/lorydb/agentDB/pkg/agentstore"
	"gitee.com/lorydb/agentDB/pkg/kv"
)

// Store 是嵌入 agentDB 的 statestore.StateStore 实现。
type Store struct {
	kv *kv.DB
	st *agentstore.Store
}

var _ statestore.StateStore = (*Store)(nil)

// Open 在 dir 打开嵌入的 agentDB 存储 (惰性建目录; kvDB WAL 每记录 fsync)。
func Open(dir string) (*Store, error) {
	k, err := kv.Open(dir, kv.Options{})
	if err != nil {
		return nil, fmt.Errorf("agentdbstore: open kv: %w", err)
	}
	st, err := agentstore.Open(k, filepath.Join(dir, "agentstore"))
	if err != nil {
		_ = k.Close()
		return nil, fmt.Errorf("agentdbstore: open agentstore: %w", err)
	}
	return &Store{kv: k, st: st}, nil
}

// Close 关闭 kvDB (flush WAL 并落盘)。
func (s *Store) Close() error { return s.kv.Close() }

// KV 返回 bucket 的键值存储; 非法 bucket 名返回所有方法报错的占位实例。
func (s *Store) KV(bucket string) statestore.KVStore {
	b, err := s.st.KV(bucket)
	if err != nil {
		return badKV{err: err}
	}
	return &kvStore{b: b}
}

// Log 返回 bucket 的 JSONL 追加日志; 非法 bucket 名返回占位实例。
func (s *Store) Log(bucket string) statestore.AppendLog {
	lg, err := s.st.Log(bucket)
	if err != nil {
		return badLog{err: err}
	}
	return &logStore{lg: lg}
}

// Blob 返回 sha256 内容寻址 Blob 存储 (直接透传, 接口逐项一致)。
func (s *Store) Blob() statestore.BlobStore { return s.st.Blob() }

// Leases 返回租约管理器 (分布式横切: 任务认领 / worker 心跳, 规划 14.1.5)。
func (s *Store) Leases() (*agentstore.LeaseManager, error) {
	return agentstore.NewLeaseManager(s.st)
}

// CAS 对 bucket/key 执行原子比较并交换 (旧值为 JSON 字节; old 为 nil 表示键必须不存在)。
func (s *Store) CAS(ctx context.Context, bucket, key string, oldVal, newVal any) error {
	b, err := s.st.KV(bucket)
	if err != nil {
		return err
	}
	var oldRaw []byte
	if oldVal != nil {
		oldRaw, err = json.Marshal(oldVal)
		if err != nil {
			return fmt.Errorf("agentdbstore: marshal old value: %w", err)
		}
	}
	newRaw, err := json.Marshal(newVal)
	if err != nil {
		return fmt.Errorf("agentdbstore: marshal new value: %w", err)
	}
	return b.CAS(ctx, key, oldRaw, newRaw)
}

// ---------- 非法 bucket 名的占位实现 ----------

type badKV struct{ err error }

// Get 占位实现: 恒报"未找到 + 建桶时的错误", 让非法 bucket 名在首次使用时立刻现形。
func (b badKV) Get(string, any) (bool, error) { return false, b.err }
// Put 占位实现: 恒返回建桶错误。
func (b badKV) Put(string, any) error         { return b.err }
// Delete 占位实现: 恒返回建桶错误。
func (b badKV) Delete(string) error           { return b.err }
// Keys 占位实现: 恒返回 (nil, 建桶错误)。
func (b badKV) Keys() ([]string, error)       { return nil, b.err }

type badLog struct{ err error }

// Append 占位实现: 恒返回建桶错误。
func (b badLog) Append(any) error                      { return b.err }
// ReadAll 占位实现: 恒返回建桶错误, 且不调用回调。
func (b badLog) ReadAll(func(line []byte) error) error { return b.err }

// ---------- KV (JSON 编解码在适配层收口) ----------

type kvStore struct {
	b *agentstore.BucketKV
}

// Get 读一个键并反序列化到 out; 键不存在返回 (false, nil), 其余错误原样上抛。
//
// 算法解读：
//
//	原理：把 agentstore.BucketKV 的字节语义适配成 statestore.KVStore 的"任意值 +
//	      存在性"语义, JSON 编解码在这一层收口。
//	关键不变量：**kv.ErrNotFound 必须与真错误分开** —— 前者转成 (false, nil), 后者
//	      原样返回。把 not-found 当错误会让所有"首次读取即建默认值"的调用方在空库上
//	      直接失败; 反过来把真错误吞成 not-found 会让损坏的库静默变空库。
//	代价与边界：每次 Get 一次 json.Unmarshal; out 必须是可写指针, 反序列化失败时
//	      out 可能已被部分写入 (标准库语义)。
//	设计权衡：不做缓存 —— 适配层保持无状态, 缓存交给上层 (单写者模型由 agentstore
//	      的 Store.mu 串行, 见包注释)。
func (k *kvStore) Get(key string, out any) (bool, error) {
	raw, err := k.b.Get(context.Background(), key)
	if errors.Is(err, kv.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("agentdbstore: 反序列化 key %q 失败: %w", key, err)
	}
	return true, nil
}

// Put 序列化 v 并写入键 (序列化失败时**不落盘**, 避免写入半截值)。
func (k *kvStore) Put(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("agentdbstore: 序列化 key %q 失败: %w", key, err)
	}
	return k.b.Put(context.Background(), key, raw)
}

// Delete 删除键; 键不存在不算错误 (底层语义)。
func (k *kvStore) Delete(key string) error { return k.b.Delete(context.Background(), key) }

// Keys 列出本 bucket 全部键 (底层前缀扫描)。
func (k *kvStore) Keys() ([]string, error) { return k.b.Keys(context.Background()) }

// ---------- Log (JSON 编解码在适配层收口) ----------

type logStore struct {
	lg *agentstore.JSONLog
}

// Append 追加一条记录 (JSON 单行), 由底层保证 O_APPEND 原子追加。
func (l *logStore) Append(v any) error {
	line, err := json.Marshal(v) // json.Marshal 不产生换行, 恒为单行
	if err != nil {
		return fmt.Errorf("agentdbstore: 序列化日志记录失败: %w", err)
	}
	return l.lg.Append(line)
}

// ReadAll 逐行回放全部记录; 回调返回错误即中止 (torn-tail 容忍、中部损坏报错由底层负责)。
func (l *logStore) ReadAll(fn func(line []byte) error) error {
	return l.lg.ReadAll(fn)
}
