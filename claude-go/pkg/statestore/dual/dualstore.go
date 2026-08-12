// Package dual 状态存储双写后端: 文件系统 + agentDB 同时落盘, 读源可配置切换。
//
// 背景 (2026-08-13 需求): agentDB 是自研组件, 稳定性尚在观察期——
// 因此文件系统**永远写** (权威副本), agentDB 作为第二落点 (失败仅记日志, 不阻断
// 主流程 = fail-open); 读取经 CLAUDE_GO_STATESTORE_READ 配置:
//
//	file     只读文件 (默认, 现状行为, 零风险)
//	agentdb  只读 agentDB (严格: 未命中/错误即返回, 用于验证 agentDB 数据完整性)
//	fallback agentDB 优先, 未命中/错误回退文件 (agentDB 信任度提升后的推荐档)
//
// 写放大代价: 每次写多一次 agentDB 往返 (嵌入式为本地库调用, 远程为 HTTP)。
// 关停面: CLAUDE_GO_STATESTORE_BACKEND=file 完全退回单文件 (零 agentDB 依赖)。
package dual

import (
	"fmt"
	"log"

	"github.com/anthropic/claude-go/pkg/agentdbclient"
	"github.com/anthropic/claude-go/pkg/statestore"
	"github.com/anthropic/claude-go/pkg/statestore/agentdbstore"
)

// ReadSource 读源配置。
type ReadSource string

const (
	ReadFile     ReadSource = "file"     // 只读文件 (默认)
	ReadAgentDB             = "agentdb"  // 只读 agentDB (严格)
	ReadFallback            = "fallback" // agentDB 优先, 回退文件
)

// Store 双写 StateStore。写: 文件先行 (必须成功) + agentDB best-effort;
// 读: 按 ReadSource 路由。
type Store struct {
	file  statestore.StateStore
	agent statestore.StateStore
	read  ReadSource
}

var _ statestore.StateStore = (*Store)(nil)

// NewDual 构造双写存储。agent 为 nil 时退化为纯文件 (所有读取走文件)。
func NewDual(file, agent statestore.StateStore, read ReadSource) *Store {
	if read == "" {
		read = ReadFile
	}
	if agent == nil {
		read = ReadFile
	}
	return &Store{file: file, agent: agent, read: read}
}

// OpenForStateDir 按 env 装配状态存储 (main/feishu 共用收敛点):
//
//	CLAUDE_GO_STATESTORE_BACKEND: file (纯文件) | dual (默认, 文件+agentDB 双写)
//	CLAUDE_GO_STATESTORE_READ:    file (默认) | agentdb | fallback
//	CLAUDE_GO_AGENTDB_URL:        非空 → 远程 agentDB (agentdbclient); 空 → 嵌入式 (<stateDir>/agentdb)
//
// agentDB 打开失败不致命: 记日志后退化为纯文件 (fail-open, 与"agentDB 观察期"一致)。
func OpenForStateDir(stateDir string, getenv func(string) string) statestore.StateStore {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	file := statestore.NewFileStore(stateDir)
	if getenv("CLAUDE_GO_STATESTORE_BACKEND") == "file" {
		return file
	}
	var agent statestore.StateStore
	if url := getenv("CLAUDE_GO_AGENTDB_URL"); url != "" {
		agent = remoteAdapter{c: agentdbclient.New(url)}
	} else {
		st, err := agentdbstore.Open(stateDir + "-agentdb")
		if err != nil {
			log.Printf("[statestore] agentDB 嵌入后端打开失败 (%v), 退化为纯文件", err)
			return file
		}
		agent = st
	}
	read := ReadSource(getenv("CLAUDE_GO_STATESTORE_READ"))
	switch read {
	case ReadFile, ReadAgentDB, ReadFallback:
	default:
		read = ReadFile
	}
	log.Printf("[statestore] 双写已启用: read=%s (file=%s)", read, stateDir)
	return NewDual(file, agent, read)
}

// KV 双写 KV。
func (s *Store) KV(bucket string) statestore.KVStore {
	if s.agent == nil {
		return s.file.KV(bucket)
	}
	return &dualKV{f: s.file.KV(bucket), a: s.agent.KV(bucket), read: s.read}
}

// Log 双写追加日志。
func (s *Store) Log(bucket string) statestore.AppendLog {
	if s.agent == nil {
		return s.file.Log(bucket)
	}
	return &dualLog{f: s.file.Log(bucket), a: s.agent.Log(bucket), read: s.read}
}

// Blob 双写内容寻址存储。
func (s *Store) Blob() statestore.BlobStore {
	if s.agent == nil {
		return s.file.Blob()
	}
	return &dualBlob{f: s.file.Blob(), a: s.agent.Blob(), read: s.read}
}

// warn agentDB 侧失败观测 (fail-open: 只记日志不阻断)。
func warn(op string, err error) {
	log.Printf("[statestore-dual] agentDB %s 失败 (fail-open, 文件侧已落盘): %v", op, err)
}

// ---- KV ----

type dualKV struct {
	f, a statestore.KVStore
	read ReadSource
}

func (d dualKV) Get(key string, out any) (bool, error) {
	switch d.read {
	case ReadAgentDB:
		return d.a.Get(key, out)
	case ReadFallback:
		ok, err := d.a.Get(key, out)
		if err == nil && ok {
			return true, nil
		}
		if err != nil {
			warn("KV.Get", err)
		}
		return d.f.Get(key, out)
	default:
		return d.f.Get(key, out)
	}
}

func (d dualKV) Put(key string, v any) error {
	if err := d.f.Put(key, v); err != nil {
		return err // 文件是权威副本, 必须成功
	}
	if err := d.a.Put(key, v); err != nil {
		warn("KV.Put", err)
	}
	return nil
}

func (d dualKV) Delete(key string) error {
	err := d.f.Delete(key)
	if aerr := d.a.Delete(key); aerr != nil {
		warn("KV.Delete", aerr)
	}
	return err
}

func (d dualKV) Keys() ([]string, error) {
	switch d.read {
	case ReadAgentDB:
		return d.a.Keys()
	case ReadFallback:
		keys, err := d.a.Keys()
		if err == nil {
			return keys, nil
		}
		warn("KV.Keys", err)
		return d.f.Keys()
	default:
		return d.f.Keys()
	}
}

// ---- Log ----

type dualLog struct {
	f, a statestore.AppendLog
	read ReadSource
}

func (d dualLog) Append(v any) error {
	if err := d.f.Append(v); err != nil {
		return err
	}
	if err := d.a.Append(v); err != nil {
		warn("Log.Append", err)
	}
	return nil
}

func (d dualLog) ReadAll(fn func(line []byte) error) error {
	switch d.read {
	case ReadAgentDB:
		return d.a.ReadAll(fn)
	case ReadFallback:
		if err := d.a.ReadAll(fn); err == nil {
			return nil
		} else {
			warn("Log.ReadAll", err)
		}
		return d.f.ReadAll(fn)
	default:
		return d.f.ReadAll(fn)
	}
}

// ---- Blob ----

type dualBlob struct {
	f, a statestore.BlobStore
	read ReadSource
}

func (d dualBlob) Put(data []byte) (string, error) {
	h, err := d.f.Put(data)
	if err != nil {
		return "", err
	}
	if ah, aerr := d.a.Put(data); aerr != nil {
		warn("Blob.Put", aerr)
	} else if ah != h {
		warn("Blob.Put", fmt.Errorf("哈希不一致 file=%s agentdb=%s", h, ah))
	}
	return h, nil
}

func (d dualBlob) Get(hash string) ([]byte, error) {
	switch d.read {
	case ReadAgentDB:
		return d.a.Get(hash)
	case ReadFallback:
		data, err := d.a.Get(hash)
		if err == nil {
			return data, nil
		}
		warn("Blob.Get", err)
		return d.f.Get(hash)
	default:
		return d.f.Get(hash)
	}
}

func (d dualBlob) Has(hash string) bool {
	switch d.read {
	case ReadAgentDB:
		return d.a.Has(hash)
	case ReadFallback:
		return d.a.Has(hash) || d.f.Has(hash)
	default:
		return d.f.Has(hash)
	}
}

// remoteAdapter agentdbclient.Client → statestore.StateStore 适配。
// Client 的 KV/Log/Blob 返回未导出具体类型 (方法集吻合但签名不实现接口),
// 这层薄适配只做返回类型提升, 不改任何行为。
type remoteAdapter struct {
	c *agentdbclient.Client
}

func (r remoteAdapter) KV(bucket string) statestore.KVStore    { return r.c.KV(bucket) }
func (r remoteAdapter) Log(bucket string) statestore.AppendLog { return r.c.Log(bucket) }
func (r remoteAdapter) Blob() statestore.BlobStore             { return r.c.Blob() }
