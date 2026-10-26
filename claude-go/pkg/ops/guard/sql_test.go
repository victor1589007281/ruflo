package guard

import "testing"

func TestValidateReadOnly_AllowsReadQueries(t *testing.T) {
	ok := []string{
		"SELECT 1",
		"select * from t where id = 1",
		"SHOW GLOBAL STATUS",
		"SHOW CREATE TABLE `t`",
		"DESC t",
		"EXPLAIN SELECT * FROM t",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"SELECT 'DROP TABLE t' AS s", // 字面量里的禁词不算语法
		"SELECT 1 -- UPDATE t SET a=1\n",
		"/* note */ SELECT 1",
	}
	for _, q := range ok {
		if err := ValidateReadOnly(q); err != nil {
			t.Errorf("expected allow, got %v for %q", err, q)
		}
	}
}

func TestValidateReadOnly_RejectsWritesAndSideEffects(t *testing.T) {
	bad := []string{
		"UPDATE t SET a=1",
		"DELETE FROM t",
		"INSERT INTO t VALUES (1)",
		"DROP TABLE t",
		"SET GLOBAL max_connections=1000",
		"SELECT 1; DROP TABLE t",
		"SELECT * FROM t INTO OUTFILE '/tmp/x'",
		"SELECT * FROM t FOR UPDATE",
		"SELECT * FROM t LOCK IN SHARE MODE",
		"EXPLAIN ANALYZE SELECT * FROM t",
		"SELECT SLEEP(10)",
		"SELECT BENCHMARK(1000000, MD5('x'))",
		"SELECT GET_LOCK('x', 10)",
		"SELECT LOAD_FILE('/etc/passwd')",
		"/*!50000 DROP TABLE t */ SELECT 1",
		"SELECT 'unterminated",
		"SELECT 1 /* unterminated",
		"",
	}
	for _, q := range bad {
		if err := ValidateReadOnly(q); err == nil {
			t.Errorf("expected reject, got allow for %q", q)
		}
	}
}

func TestValidateReadOnly_RejectsMultiStatementHiddenInComment(t *testing.T) {
	// 注释被剥掉后只剩一条 SELECT，必须允许；分号在注释里不构成多语句。
	if err := ValidateReadOnly("SELECT 1 -- ; DROP TABLE t"); err != nil {
		t.Fatalf("comment-only semicolon should be allowed: %v", err)
	}
	// 但真正的第二条语句必须被拦。
	if err := ValidateReadOnly("SELECT 1; SELECT 2"); err == nil {
		t.Fatal("multi statement must be rejected")
	}
}
