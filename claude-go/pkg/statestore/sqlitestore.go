// sqlitestore.go — StateStore 的 SQLite 后端 (design/02 R2 状态外置)。
//
// 用纯 Go 驱动 modernc.org/sqlite (无 CGO, 契合 CGO_ENABLED=0 静态部署), WAL 模式。
// 相比 FileStore: 单文件、事务性、便于备份/迁移, 是走向 T2/T3 多进程一致的过渡后端。
// 语义与 FileStore/MemStore 完全一致 (同一组 statestore_test 断言通过)。
package statestore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// SQLiteStore StateStore 的 sqlite 实现。
type SQLiteStore struct {
	db *sql.DB
	mu sync.Mutex // sqlite 单写者: 进程内串行化写, 避免 SQLITE_BUSY
}

// NewSQLiteStore 打开/创建 sqlite 状态库 (WAL 模式)。
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	// WAL 让读写互不阻塞 (读走快照); busy_timeout 兜住偶发争用, 避免直接抛 SQLITE_BUSY。
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("statestore/sqlite: 打开失败: %w", err)
	}
	db.SetMaxOpenConns(1) // 纯 Go sqlite 单连接最稳
	schema := []string{
		`CREATE TABLE IF NOT EXISTS kv (bucket TEXT NOT NULL, key TEXT NOT NULL, value BLOB, PRIMARY KEY(bucket,key))`,
		`CREATE TABLE IF NOT EXISTS log (bucket TEXT NOT NULL, seq INTEGER PRIMARY KEY AUTOINCREMENT, line BLOB)`,
		`CREATE INDEX IF NOT EXISTS idx_log_bucket ON log(bucket, seq)`,
		`CREATE TABLE IF NOT EXISTS blob (hash TEXT PRIMARY KEY, data BLOB)`,
	}
	for _, s := range schema {
		if _, err := db.Exec(s); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("statestore/sqlite: 建表失败: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

// Close 关闭数据库。
func (s *SQLiteStore) Close() error { return s.db.Close() }

// KV 返回某个 bucket 的键值视图。
func (s *SQLiteStore) KV(bucket string) KVStore    { return &sqliteKV{s: s, bucket: bucket} }
// Log 返回某个 bucket 的追加日志视图。
func (s *SQLiteStore) Log(bucket string) AppendLog { return &sqliteLog{s: s, bucket: bucket} }
// Blob 返回全局内容寻址 blob 存储。
func (s *SQLiteStore) Blob() BlobStore             { return &sqliteBlob{s: s} }

// --- KV ---

type sqliteKV struct {
	s      *SQLiteStore
	bucket string
}

// Get 读取 key 并反序列化到 out; 键不存在返回 (false, nil) 而不是错误。
func (k *sqliteKV) Get(key string, out any) (bool, error) {
	if err := validateBucket(k.bucket); err != nil {
		return false, err
	}
	var raw []byte
	err := k.s.db.QueryRow(`SELECT value FROM kv WHERE bucket=? AND key=?`, k.bucket, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Put 序列化并 upsert 一个键值对。
func (k *sqliteKV) Put(key string, v any) error {
	if err := validateBucket(k.bucket); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	// upsert 一条语句原子完成: 避免"先查后写"两步之间的读-改-写竞态。
	_, err = k.s.db.Exec(
		`INSERT INTO kv(bucket,key,value) VALUES(?,?,?) ON CONFLICT(bucket,key) DO UPDATE SET value=excluded.value`,
		k.bucket, key, raw)
	return err
}

// Delete 删除一个键; 键不存在不算错误。
func (k *sqliteKV) Delete(key string) error {
	if err := validateBucket(k.bucket); err != nil {
		return err
	}
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	_, err := k.s.db.Exec(`DELETE FROM kv WHERE bucket=? AND key=?`, k.bucket, key)
	return err
}

// Keys 按字典序返回 bucket 内全部键。
func (k *sqliteKV) Keys() ([]string, error) {
	if err := validateBucket(k.bucket); err != nil {
		return nil, err
	}
	rows, err := k.s.db.Query(`SELECT key FROM kv WHERE bucket=? ORDER BY key`, k.bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// --- Log ---

type sqliteLog struct {
	s      *SQLiteStore
	bucket string
}

// Append 序列化后追加一行日志 (seq 由 AUTOINCREMENT 分配, 保证回放顺序)。
func (l *sqliteLog) Append(v any) error {
	if err := validateBucket(l.bucket); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	_, err = l.s.db.Exec(`INSERT INTO log(bucket,line) VALUES(?,?)`, l.bucket, raw)
	return err
}

// ReadAll 按 seq 顺序回放 bucket 内全部日志行, 逐行回调。
func (l *sqliteLog) ReadAll(fn func(line []byte) error) error {
	if err := validateBucket(l.bucket); err != nil {
		return err
	}
	rows, err := l.s.db.Query(`SELECT line FROM log WHERE bucket=? ORDER BY seq`, l.bucket)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var line []byte
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if !json.Valid(line) {
			continue // 容忍坏行 (与 FileStore 一致)
		}
		// 拷一份再交给回调: rows.Scan 会复用底层缓冲, 直接透出会让回调在下一轮读到被覆盖的数据。
		cp := make([]byte, len(line))
		copy(cp, line)
		if err := fn(cp); err != nil {
			return err
		}
	}
	return rows.Err()
}

// --- Blob ---

type sqliteBlob struct{ s *SQLiteStore }

// Put 按内容哈希去重写入 blob, 返回 sha256 十六进制哈希; 已存在则忽略 (幂等)。
//
// 算法解读：
//
//	原理：内容寻址 —— 键由内容本身算出, 于是"同内容只存一份"天然成立, 不需要额外的引用
//	      计数或去重表。
//	关键不变量：写必须幂等 (ON CONFLICT DO NOTHING)。若用普通 INSERT, 重复写入同一内容会因
//	      主键冲突报错 —— 而"同一份产出被存两次"在 trace / 记忆路径上完全正常, 报错会把
//	      正常流程变成失败。
//	代价与边界：每次写都要全量算一遍 sha256 (O(内容长度)), 无法增量。哈希是唯一定位方式,
//	      因此**内容一旦落库就不可变**。
//	设计权衡：不做引用计数 —— 于是删除是"谁都不敢删", blob 只增不减; 换来的是写入路径
//	      无锁无协调, 多进程下也天然安全。
func (b *sqliteBlob) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	// 幂等: 已存在则忽略 (内容寻址去重)
	_, err := b.s.db.Exec(`INSERT INTO blob(hash,data) VALUES(?,?) ON CONFLICT(hash) DO NOTHING`, hash, data)
	if err != nil {
		return "", err
	}
	return hash, nil
}

// Get 按哈希取 blob 内容; 不存在时返回错误而不是空内容。
func (b *sqliteBlob) Get(hash string) ([]byte, error) {
	var data []byte
	err := b.s.db.QueryRow(`SELECT data FROM blob WHERE hash=?`, hash).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("statestore/sqlite: blob %s 不存在", hash)
	}
	return data, err
}

// Has 判断哈希对应的 blob 是否存在。
func (b *sqliteBlob) Has(hash string) bool {
	var one int
	err := b.s.db.QueryRow(`SELECT 1 FROM blob WHERE hash=?`, hash).Scan(&one)
	return err == nil
}
