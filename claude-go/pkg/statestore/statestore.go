// Package statestore 实现 design/02 §3.4.1 的状态存储抽象 (R0 接口抽取)。
//
// StateStore 是"一切外置的地基": bucket 化 KV + 追加日志 + 内容寻址 Blob。
// 本包提供两个后端:
//   - FileStore: 文件后端 (默认), 磁盘布局 <root>/kv|log|blob, 写入原子安全;
//   - MemStore:  纯内存后端 (测试/嵌入用), 语义与 FileStore 一致。
//
// 后续演进 (sqlite / redis+pg / nats-jetstream) 只需实现同一组接口。
package statestore

// StateStore 状态存储抽象: bucket 化 KV + 追加日志 + 内容寻址 Blob。
type StateStore interface {
	// KV 返回指定 bucket 的键值存储 (team 投影 / cron jobs / registry / settings 快照)。
	KV(bucket string) KVStore
	// Log 返回指定 bucket 的追加日志 (journal / llm.jsonl / trace / reward, append-only)。
	Log(bucket string) AppendLog
	// Blob 返回内容寻址 Blob 存储 (REPORT.md / 媒体产物 / skill 正文)。
	Blob() BlobStore
}

// KVStore bucket 内的键值存储。值以 JSON 编码持久化。
type KVStore interface {
	// Get 读取 key 并 JSON 反序列化到 out; key 不存在时返回 (false, nil)。
	Get(key string, out any) (bool, error)
	// Put 将 v JSON 序列化后写入 key。
	Put(key string, v any) error
	// Delete 删除 key; key 不存在时不报错 (幂等)。
	Delete(key string) error
	// Keys 返回 bucket 内全部 key (按字典序排序, 便于确定性遍历)。
	Keys() ([]string, error)
}

// AppendLog 追加日志: 每条记录一行 JSON (JSONL)。
type AppendLog interface {
	// Append 追加一行 JSON。
	Append(v any) error
	// ReadAll 逐行回调; line 为该行 JSON 字节 (回调方可安全持有, 已拷贝)。
	// 容忍尾部截断行 (崩溃安全): 文件末尾的半行 JSON 被静默跳过;
	// 但中部出现坏行说明数据损坏, 返回错误。
	// 回调返回非 nil 错误时立即中止并透传该错误。
	ReadAll(fn func(line []byte) error) error
}

// BlobStore 内容寻址 Blob 存储 (sha256)。
type BlobStore interface {
	// Put 写入数据, 返回 sha256 十六进制哈希; 已存在时直接返回 (去重, 幂等)。
	Put(data []byte) (hash string, err error)
	// Get 按哈希读取数据; 不存在时返回错误。
	Get(hash string) ([]byte, error)
	// Has 判断哈希对应的数据是否存在。
	Has(hash string) bool
}
