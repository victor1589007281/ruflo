package memory

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ruflo/ruflo-go/api"
	_ "modernc.org/sqlite"
)

// SQLiteBackend persists memory entries and embeddings.
type SQLiteBackend struct {
	db *sql.DB
}

// OpenSQLite opens or creates a SQLite database at path.
func OpenSQLite(path string) (*SQLiteBackend, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("memory sqlite: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	b := &SQLiteBackend{db: db}
	if err := b.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return b, nil
}

func (b *SQLiteBackend) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memory_entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key TEXT NOT NULL,
			value TEXT NOT NULL,
			namespace TEXT NOT NULL DEFAULT '',
			tags TEXT NOT NULL DEFAULT '[]',
			embedding BLOB,
			metadata TEXT NOT NULL DEFAULT '{}',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			ttl_seconds INTEGER,
			UNIQUE(key, namespace)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_namespace ON memory_entries(namespace);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_updated ON memory_entries(updated_at);`,
	}
	for _, s := range stmts {
		if _, err := b.db.Exec(s); err != nil {
			return fmt.Errorf("memory sqlite: migrate: %w", err)
		}
	}
	return nil
}

// Close closes the database.
func (b *SQLiteBackend) Close() error {
	if b == nil || b.db == nil {
		return nil
	}
	return b.db.Close()
}

// Ping verifies the database connection is alive.
func (b *SQLiteBackend) Ping() error {
	if b == nil || b.db == nil {
		return errors.New("memory sqlite: nil db")
	}
	return b.db.Ping()
}

func float32SliceToBlob(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func blobToFloat32Slice(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, errors.New("memory sqlite: invalid embedding blob")
	}
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		u := binary.LittleEndian.Uint32(b[i*4:])
		out[i] = math.Float32frombits(u)
	}
	return out, nil
}

func tagsToJSON(tags []string) (string, error) {
	if tags == nil {
		tags = []string{}
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func tagsFromJSON(s string) ([]string, error) {
	var tags []string
	if s == "" {
		return tags, nil
	}
	if err := json.Unmarshal([]byte(s), &tags); err != nil {
		return nil, err
	}
	return tags, nil
}

func metaToJSON(m map[string]string) (string, error) {
	if m == nil {
		m = map[string]string{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func metaFromJSON(s string) (map[string]string, error) {
	m := map[string]string{}
	if s == "" || s == "{}" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// UpsertEntry inserts or updates a row by (key, namespace).
func (b *SQLiteBackend) UpsertEntry(in MemoryEntryInput, embedding []float32) (*api.MemoryEntry, error) {
	now := time.Now().Unix()
	tagsJSON, err := tagsToJSON(in.Tags)
	if err != nil {
		return nil, err
	}
	metaJSON, err := metaToJSON(in.Metadata)
	if err != nil {
		return nil, err
	}
	var ttl any
	if in.TTLSeconds != nil {
		ttl = *in.TTLSeconds
	}
	blob := float32SliceToBlob(embedding)

	tx, err := b.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	var createdAt int64
	row := tx.QueryRow(`SELECT id, created_at FROM memory_entries WHERE key=? AND namespace=?`, in.Key, in.Namespace)
	scanErr := row.Scan(&id, &createdAt)
	if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
		return nil, scanErr
	}
	if errors.Is(scanErr, sql.ErrNoRows) {
		res, err := tx.Exec(`INSERT INTO memory_entries(key, value, namespace, tags, embedding, metadata, created_at, updated_at, ttl_seconds)
			VALUES(?,?,?,?,?,?,?,?,?)`,
			in.Key, in.Value, in.Namespace, tagsJSON, blob, metaJSON, now, now, ttl)
		if err != nil {
			return nil, err
		}
		rid, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		id = rid
		createdAt = now
	} else {
		_, err := tx.Exec(`UPDATE memory_entries SET value=?, tags=?, embedding=?, metadata=?, updated_at=?, ttl_seconds=? WHERE id=?`,
			in.Value, tagsJSON, blob, metaJSON, now, ttl, id)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return b.GetByID(id)
}

// UpdateEntryByID sets value, embedding blob, and updated_at for a row.
func (b *SQLiteBackend) UpdateEntryByID(id int64, value string, embedding []float32) error {
	if b == nil || b.db == nil {
		return errors.New("memory sqlite: nil db")
	}
	blob := float32SliceToBlob(embedding)
	_, err := b.db.Exec(`UPDATE memory_entries SET value=?, embedding=?, updated_at=? WHERE id=?`,
		value, blob, time.Now().Unix(), id)
	return err
}

// ListIDKeysInNamespace returns id and key for all rows in a namespace.
func (b *SQLiteBackend) ListIDKeysInNamespace(namespace string) ([]struct {
	ID  int64
	Key string
}, error) {
	if b == nil || b.db == nil {
		return nil, errors.New("memory sqlite: nil db")
	}
	rows, err := b.db.Query(`SELECT id, key FROM memory_entries WHERE namespace=?`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pair struct {
		ID  int64
		Key string
	}
	var out []struct {
		ID  int64
		Key string
	}
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.ID, &p.Key); err != nil {
			return nil, err
		}
		out = append(out, struct {
			ID  int64
			Key string
		}{ID: p.ID, Key: p.Key})
	}
	return out, rows.Err()
}

// ListIDKeysForKeys returns id and key for rows matching namespace and keys.
func (b *SQLiteBackend) ListIDKeysForKeys(namespace string, keys []string) ([]struct {
	ID  int64
	Key string
}, error) {
	if b == nil || b.db == nil {
		return nil, errors.New("memory sqlite: nil db")
	}
	if len(keys) == 0 {
		return nil, nil
	}
	ph := make([]string, len(keys))
	args := make([]any, 0, 1+len(keys))
	args = append(args, namespace)
	for i := range keys {
		ph[i] = "?"
		args = append(args, keys[i])
	}
	q := `SELECT id, key FROM memory_entries WHERE namespace=? AND key IN (` + strings.Join(ph, ",") + `)`
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		ID  int64
		Key string
	}
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		out = append(out, struct {
			ID  int64
			Key string
		}{ID: id, Key: key})
	}
	return out, rows.Err()
}

// BulkDeleteKeys removes rows for keys in namespace; returns rows deleted.
func (b *SQLiteBackend) BulkDeleteKeys(namespace string, keys []string) (int64, error) {
	if b == nil || b.db == nil {
		return 0, errors.New("memory sqlite: nil db")
	}
	if len(keys) == 0 {
		return 0, nil
	}
	ph := make([]string, len(keys))
	args := make([]any, 0, 1+len(keys))
	args = append(args, namespace)
	for i := range keys {
		ph[i] = "?"
		args = append(args, keys[i])
	}
	q := `DELETE FROM memory_entries WHERE namespace=? AND key IN (` + strings.Join(ph, ",") + `)`
	res, err := b.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAllInNamespace removes every row in a namespace; returns rows deleted.
func (b *SQLiteBackend) DeleteAllInNamespace(namespace string) (int64, error) {
	if b == nil || b.db == nil {
		return 0, errors.New("memory sqlite: nil db")
	}
	res, err := b.db.Exec(`DELETE FROM memory_entries WHERE namespace=?`, namespace)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetByID loads one row.
func (b *SQLiteBackend) GetByID(id int64) (*api.MemoryEntry, error) {
	row := b.db.QueryRow(`SELECT id, key, value, namespace, tags, embedding, metadata, created_at, updated_at, ttl_seconds FROM memory_entries WHERE id=?`, id)
	return scanEntry(row)
}

// GetByKey loads by composite key.
func (b *SQLiteBackend) GetByKey(key, namespace string) (*api.MemoryEntry, error) {
	row := b.db.QueryRow(`SELECT id, key, value, namespace, tags, embedding, metadata, created_at, updated_at, ttl_seconds FROM memory_entries WHERE key=? AND namespace=?`, key, namespace)
	return scanEntry(row)
}

// DeleteKey removes a row.
func (b *SQLiteBackend) DeleteKey(key, namespace string) error {
	_, err := b.db.Exec(`DELETE FROM memory_entries WHERE key=? AND namespace=?`, key, namespace)
	return err
}

// DeleteID removes by id.
func (b *SQLiteBackend) DeleteID(id int64) error {
	_, err := b.db.Exec(`DELETE FROM memory_entries WHERE id=?`, id)
	return err
}

// List returns paginated rows for a namespace.
func (b *SQLiteBackend) List(namespace string, limit, offset int) ([]api.MemoryEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	q := `SELECT id, key, value, namespace, tags, embedding, metadata, created_at, updated_at, ttl_seconds FROM memory_entries`
	var args []any
	if namespace != "" {
		q += ` WHERE namespace=?`
		args = append(args, namespace)
	}
	q += ` ORDER BY updated_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.MemoryEntry
	for rows.Next() {
		e, err := scanEntryRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ListDistinctNamespaces returns sorted unique namespaces, optionally limited to those starting with prefix.
func (b *SQLiteBackend) ListDistinctNamespaces(prefix string) ([]string, error) {
	if b == nil || b.db == nil {
		return nil, errors.New("memory sqlite: nil db")
	}
	var q string
	var args []any
	if prefix == "" {
		q = `SELECT DISTINCT namespace FROM memory_entries ORDER BY namespace`
	} else {
		q = `SELECT DISTINCT namespace FROM memory_entries WHERE namespace LIKE ? ORDER BY namespace`
		args = append(args, prefix+"%")
	}
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// Count returns total rows (optionally by namespace).
func (b *SQLiteBackend) Count(namespace string) (int64, error) {
	q := `SELECT COUNT(1) FROM memory_entries`
	var args []any
	if namespace != "" {
		q += ` WHERE namespace=?`
		args = append(args, namespace)
	}
	var n int64
	if err := b.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ListEmbeddings returns all rows that have embeddings (for index rebuild).
func (b *SQLiteBackend) ListEmbeddings() ([]struct {
	ID        int64
	Embedding []float32
}, error) {
	rows, err := b.db.Query(`SELECT id, embedding FROM memory_entries WHERE embedding IS NOT NULL AND length(embedding) > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		ID        int64
		Embedding []float32
	}
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		vec, err := blobToFloat32Slice(blob)
		if err != nil {
			return nil, err
		}
		out = append(out, struct {
			ID        int64
			Embedding []float32
		}{ID: id, Embedding: vec})
	}
	return out, rows.Err()
}

// FilterByTags searches by tag overlap using JSON substring match.
func (b *SQLiteBackend) FilterByTags(namespace string, tags []string, limit int) ([]api.MemoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, key, value, namespace, tags, embedding, metadata, created_at, updated_at, ttl_seconds FROM memory_entries WHERE 1=1`
	var args []any
	if namespace != "" {
		q += ` AND namespace=?`
		args = append(args, namespace)
	}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		q += ` AND tags LIKE ?`
		args = append(args, "%\""+t+"\"%")
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.MemoryEntry
	for rows.Next() {
		e, err := scanEntryRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func scanEntry(row *sql.Row) (*api.MemoryEntry, error) {
	var id int64
	var key, val, ns, tagsJSON, metaJSON string
	var blob []byte
	var created, updated int64
	var ttl sql.NullInt64
	if err := row.Scan(&id, &key, &val, &ns, &tagsJSON, &blob, &metaJSON, &created, &updated, &ttl); err != nil {
		return nil, err
	}
	return buildEntry(id, key, val, ns, tagsJSON, blob, metaJSON, created, updated, ttl)
}

func scanEntryRows(rows *sql.Rows) (*api.MemoryEntry, error) {
	var id int64
	var key, val, ns, tagsJSON, metaJSON string
	var blob []byte
	var created, updated int64
	var ttl sql.NullInt64
	if err := rows.Scan(&id, &key, &val, &ns, &tagsJSON, &blob, &metaJSON, &created, &updated, &ttl); err != nil {
		return nil, err
	}
	return buildEntry(id, key, val, ns, tagsJSON, blob, metaJSON, created, updated, ttl)
}

func buildEntry(id int64, key, val, ns, tagsJSON string, blob []byte, metaJSON string, created, updated int64, ttl sql.NullInt64) (*api.MemoryEntry, error) {
	tags, err := tagsFromJSON(tagsJSON)
	if err != nil {
		return nil, err
	}
	meta, err := metaFromJSON(metaJSON)
	if err != nil {
		return nil, err
	}
	var emb []float32
	if len(blob) > 0 {
		emb, err = blobToFloat32Slice(blob)
		if err != nil {
			return nil, err
		}
	}
	var ttlPtr *int64
	if ttl.Valid {
		v := ttl.Int64
		ttlPtr = &v
	}
	return &api.MemoryEntry{
		ID:         id,
		Key:        key,
		Value:      val,
		Namespace:  ns,
		Tags:       tags,
		Embedding:  emb,
		CreatedAt:  time.Unix(created, 0).UTC(),
		UpdatedAt:  time.Unix(updated, 0).UTC(),
		TTLSeconds: ttlPtr,
		Metadata:   meta,
	}, nil
}
