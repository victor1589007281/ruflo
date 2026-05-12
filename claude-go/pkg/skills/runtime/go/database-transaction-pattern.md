# Skill: Database Transaction Pattern

## 场景 (When to use)
当需要执行多步骤数据库操作，要求要么全部成功要么全部回滚，同时支持上下文取消和超时。

## 代码模板 (Template)
```go
package db

import (
	"context"
	"database/sql"
	"fmt"
)

type TxFn func(ctx context.Context, tx *sql.Tx) error

func WithTransaction(ctx context.Context, db *sql.DB, fn TxFn) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(ctx, tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("rollback failed: %v (original: %w)", rbErr, err)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}
	return nil
}

func CreateOrder(ctx context.Context, db *sql.DB, userID string, items []string) error {
	return WithTransaction(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		// TODO: insert order record
		_, err := tx.ExecContext(ctx, "INSERT INTO orders (user_id) VALUES (?)", userID)
		if err != nil {
			return fmt.Errorf("insert order: %w", err)
		}

		for _, item := range items {
			// TODO: insert order items
			_, err := tx.ExecContext(ctx, "INSERT INTO order_items (item) VALUES (?)", item)
			if err != nil {
				return fmt.Errorf("insert item: %w", err)
			}
		}
		return nil
	})
}
```

## 反模式警告 (Anti-patterns)
- 不要在事务函数内部调用 `tx.Commit()` 或 `tx.Rollback()`，由 `WithTransaction` 统一管理
- 不要在事务中执行不相关的 I/O 操作，避免长时间持有连接
- 不要忘记在 `defer` 中处理 `panic`，否则可能导致连接泄漏

## 契约要求 (Contract Requirements)
- `TxFn` 必须接受 `context.Context` 和 `*sql.Tx`
- 数据库操作必须使用 `ExecContext`/`QueryContext` 系列方法

## 测试模板 (Test Template)
```go
func TestWithTransaction(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE test (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}

	err = WithTransaction(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO test (id) VALUES (1)")
		return err
	})
	if err != nil {
		t.Fatalf("transaction failed: %v", err)
	}
}
```
