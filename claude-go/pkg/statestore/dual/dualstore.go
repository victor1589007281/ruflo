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

// Get 按读源路由读取: agentdb 严格读, fallback 是 agentDB 未命中/出错时回退文件, 默认只读文件。
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

// Put 先写文件 (权威副本, 失败即返回错误), 再 best-effort 写 agentDB (失败只记日志)。
//
// 算法解读：
//
//	原理：双写的一致性策略是「文件权威 + agentDB 观察」—— 文件侧成功才算写成功,
//	      agentDB 只是第二落点, 失败只记日志 (fail-open), 不阻断主流程。
//	关键不变量：写入顺序不可颠倒, 且 agentDB 的错误不得冒泡。顺序决定了
//	      「文件有、agentDB 可能没有」这种偏斜方向; 反过来先写 agentDB 再写文件,
//	      一旦文件失败, 读源是 agentdb 时就会读到文件侧不存在的状态 ——
//	      此时文件已不再是权威副本, 整个「以文件为准」的前提被破坏。
//	代价与边界：两侧不共享事务, 中间崩溃会留下单侧写入; 读侧靠 ReadSource 决定
//	      容忍哪种偏斜 (file 档只看文件, fallback 档以 agentDB 优先、缺了才回退文件)。
//	设计权衡：接受「可能不一致」换取「agentDB 挂掉不影响主流程」——
//	      在 agentDB 的观察期内, 可用性优先于双份数据的严格一致。
func (d dualKV) Put(key string, v any) error {
	if err := d.f.Put(key, v); err != nil {
		return err // 文件是权威副本, 必须成功
	}
	if err := d.a.Put(key, v); err != nil {
		warn("KV.Put", err)
	}
	return nil
}

// Delete 两侧都删, 但只把文件侧的错误返回给调用方 —— agentDB 删失败仅记日志 (fail-open)。
func (d dualKV) Delete(key string) error {
	err := d.f.Delete(key)
	if aerr := d.a.Delete(key); aerr != nil {
		warn("KV.Delete", aerr)
	}
	return err
}

// Keys 按读源路由列出键。
//
// 注意：fallback 档与 Get 的语义不一致 —— 这里只要 agentDB 不报错就采信 (哪怕返回空集),
// 不像 Get 那样要求「命中」; 因此 agentDB 侧数据尚未补齐时, fallback 读会看到空列表而不是回退到文件。
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

// Append 先追加到文件日志 (失败即返回), 再 best-effort 追加到 agentDB。
//
// 注意：两侧是分别追加而非原子双写, 中途失败会让 agentDB 侧缺行 ——
// 所以 agentDB 的日志只适合观测, 不能当作与文件等价的副本。
func (d dualLog) Append(v any) error {
	if err := d.f.Append(v); err != nil {
		return err
	}
	if err := d.a.Append(v); err != nil {
		warn("Log.Append", err)
	}
	return nil
}

// ReadAll 按读源路由遍历全部日志行。
//
// 注意：fallback 档只有 agentDB 报错时才回退文件 —— 与 Get 不同, 它无法感知「读到了空日志」,
// 因此 agentDB 侧日志缺失时 fallback 会读到空集而不是文件里的全量历史。
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

// Put 先写文件侧 blob 并取回内容哈希, 再 best-effort 写 agentDB; 返回文件侧哈希作为权威地址。
//
// 注意：agentDB 返回的哈希若与文件侧不同, 只记一条 warn 日志 —— 内容寻址存储里
// 哈希不一致意味着两侧存了不同内容, 但调用方拿不到这个信号 (仍返回成功)。
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

// Get 按读源路由取回 blob 内容。
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

// Has 判断 blob 是否存在。
//
// 注意：fallback 档用「或」而不是「回退」—— agentDB 说没有、文件说有, 也算存在,
// 因为双写是 fail-open 的, 只要有一侧落盘成功内容就取得到 (取用走 Get 的同一套路由)。
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

// KV 把远程 client 的 KV 返回类型提升为 statestore.KVStore。
func (r remoteAdapter) KV(bucket string) statestore.KVStore    { return r.c.KV(bucket) }

// Log 把远程 client 的 Log 返回类型提升为 statestore.AppendLog。
func (r remoteAdapter) Log(bucket string) statestore.AppendLog { return r.c.Log(bucket) }

// Blob 把远程 client 的 Blob 返回类型提升为 statestore.BlobStore。
func (r remoteAdapter) Blob() statestore.BlobStore             { return r.c.Blob() }
