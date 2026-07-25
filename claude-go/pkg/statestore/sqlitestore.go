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

func (s *SQLiteStore) KV(bucket string) KVStore    { return &sqliteKV{s: s, bucket: bucket} }
func (s *SQLiteStore) Log(bucket string) AppendLog { return &sqliteLog{s: s, bucket: bucket} }
func (s *SQLiteStore) Blob() BlobStore             { return &sqliteBlob{s: s} }

// --- KV ---

type sqliteKV struct {
	s      *SQLiteStore
	bucket string
}

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
	_, err = k.s.db.Exec(
		`INSERT INTO kv(bucket,key,value) VALUES(?,?,?) ON CONFLICT(bucket,key) DO UPDATE SET value=excluded.value`,
		k.bucket, key, raw)
	return err
}

func (k *sqliteKV) Delete(key string) error {
	if err := validateBucket(k.bucket); err != nil {
		return err
	}
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	_, err := k.s.db.Exec(`DELETE FROM kv WHERE bucket=? AND key=?`, k.bucket, key)
	return err
}

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

func (b *sqliteBlob) Get(hash string) ([]byte, error) {
	var data []byte
	err := b.s.db.QueryRow(`SELECT data FROM blob WHERE hash=?`, hash).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("statestore/sqlite: blob %s 不存在", hash)
	}
	return data, err
}

func (b *sqliteBlob) Has(hash string) bool {
	var one int
	err := b.s.db.QueryRow(`SELECT 1 FROM blob WHERE hash=?`, hash).Scan(&one)
	return err == nil
}
