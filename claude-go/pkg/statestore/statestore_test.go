package statestore

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// forEachStore 对 FileStore 与 MemStore 各跑一遍同一组语义测试。
func forEachStore(t *testing.T, fn func(t *testing.T, s StateStore)) {
	t.Helper()
	t.Run("file", func(t *testing.T) {
		fn(t, NewFileStore(t.TempDir()))
	})
	t.Run("mem", func(t *testing.T) {
		fn(t, NewMemStore())
	})
}

type demoVal struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestKVRoundtrip(t *testing.T) {
	forEachStore(t, func(t *testing.T, s StateStore) {
		kv := s.KV("teams")

		// 不存在 → (false, nil)
		var got demoVal
		ok, err := kv.Get("a", &got)
		if err != nil || ok {
			t.Fatalf("Get 未写入的 key: ok=%v err=%v, 期望 (false, nil)", ok, err)
		}

		want := demoVal{Name: "alpha", Count: 42}
		if err := kv.Put("a", want); err != nil {
			t.Fatalf("Put 失败: %v", err)
		}
		ok, err = kv.Get("a", &got)
		if err != nil || !ok {
			t.Fatalf("Get: ok=%v err=%v", ok, err)
		}
		if got != want {
			t.Fatalf("roundtrip 不一致: got=%+v want=%+v", got, want)
		}

		// 覆盖写
		want2 := demoVal{Name: "beta", Count: 7}
		if err := kv.Put("a", want2); err != nil {
			t.Fatalf("覆盖 Put 失败: %v", err)
		}
		if _, err := kv.Get("a", &got); err != nil || got != want2 {
			t.Fatalf("覆盖后 Get: got=%+v err=%v", got, err)
		}
	})
}

func TestKVDeleteAndKeys(t *testing.T) {
	forEachStore(t, func(t *testing.T, s StateStore) {
		kv := s.KV("jobs")
		for _, k := range []string{"z", "a", "m"} {
			if err := kv.Put(k, demoVal{Name: k}); err != nil {
				t.Fatalf("Put %s 失败: %v", k, err)
			}
		}
		keys, err := kv.Keys()
		if err != nil {
			t.Fatalf("Keys 失败: %v", err)
		}
		if len(keys) != 3 || keys[0] != "a" || keys[1] != "m" || keys[2] != "z" {
			t.Fatalf("Keys 应为排序后的 [a m z], got=%v", keys)
		}

		if err := kv.Delete("m"); err != nil {
			t.Fatalf("Delete 失败: %v", err)
		}
		var got demoVal
		ok, err := kv.Get("m", &got)
		if err != nil || ok {
			t.Fatalf("Delete 后 Get: ok=%v err=%v", ok, err)
		}
		// 幂等: 再删不存在的 key 不报错
		if err := kv.Delete("m"); err != nil {
			t.Fatalf("重复 Delete 应幂等: %v", err)
		}
		keys, _ = kv.Keys()
		if len(keys) != 2 {
			t.Fatalf("Delete 后应剩 2 个 key, got=%v", keys)
		}
	})
}

func TestKVConcurrent(t *testing.T) {
	forEachStore(t, func(t *testing.T, s StateStore) {
		kv := s.KV("concurrent")
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				key := fmt.Sprintf("k%d", n)
				for j := 0; j < 20; j++ {
					if err := kv.Put(key, demoVal{Name: key, Count: j}); err != nil {
						t.Errorf("并发 Put 失败: %v", err)
						return
					}
					var got demoVal
					if _, err := kv.Get(key, &got); err != nil {
						t.Errorf("并发 Get 失败: %v", err)
						return
					}
					if _, err := kv.Keys(); err != nil {
						t.Errorf("并发 Keys 失败: %v", err)
						return
					}
				}
			}(i)
		}
		wg.Wait()
		keys, err := kv.Keys()
		if err != nil || len(keys) != 8 {
			t.Fatalf("并发写后应有 8 个 key, got=%v err=%v", keys, err)
		}
	})
}

func TestBadBucketName(t *testing.T) {
	forEachStore(t, func(t *testing.T, s StateStore) {
		for _, bad := range []string{"", "a/b", "../x", "a b", "中文", "a\x00b"} {
			kv := s.KV(bad)
			if err := kv.Put("k", 1); err == nil {
				t.Errorf("KV bucket %q 应报错", bad)
			}
			if _, err := kv.Get("k", new(int)); err == nil {
				t.Errorf("KV Get bucket %q 应报错", bad)
			}
			if _, err := kv.Keys(); err == nil {
				t.Errorf("KV Keys bucket %q 应报错", bad)
			}
			if err := kv.Delete("k"); err == nil {
				t.Errorf("KV Delete bucket %q 应报错", bad)
			}
			lg := s.Log(bad)
			if err := lg.Append(1); err == nil {
				t.Errorf("Log bucket %q 应报错", bad)
			}
			if err := lg.ReadAll(func([]byte) error { return nil }); err == nil {
				t.Errorf("Log ReadAll bucket %q 应报错", bad)
			}
		}
		// 合法名: 点/下划线/连字符
		if err := s.KV("a.b_c-d").Put("k", 1); err != nil {
			t.Errorf("合法 bucket 名不应报错: %v", err)
		}
	})
}

func TestLogAppendReadAll(t *testing.T) {
	forEachStore(t, func(t *testing.T, s StateStore) {
		lg := s.Log("journal")
		for i := 0; i < 5; i++ {
			if err := lg.Append(map[string]int{"seq": i}); err != nil {
				t.Fatalf("Append 失败: %v", err)
			}
		}
		var lines [][]byte
		err := lg.ReadAll(func(line []byte) error {
			cp := make([]byte, len(line))
			copy(cp, line)
			lines = append(lines, cp)
			return nil
		})
		if err != nil {
			t.Fatalf("ReadAll 失败: %v", err)
		}
		if len(lines) != 5 {
			t.Fatalf("应读到 5 行, got=%d", len(lines))
		}
		if string(lines[3]) != `{"seq":3}` {
			t.Fatalf("第 4 行内容异常: %s", lines[3])
		}

		// 回调错误透传并中止
		calls := 0
		err = lg.ReadAll(func([]byte) error {
			calls++
			return fmt.Errorf("stop")
		})
		if err == nil || calls != 1 {
			t.Fatalf("回调错误应中止并透传: err=%v calls=%d", err, calls)
		}
	})
}

// TestLogCrashSafety 崩溃安全: 手工写半行模拟进程崩溃, ReadAll 跳过尾部坏行不报错。
func TestLogCrashSafety(t *testing.T) {
	root := t.TempDir()
	s := NewFileStore(root)
	lg := s.Log("crash")
	for i := 0; i < 3; i++ {
		if err := lg.Append(map[string]int{"seq": i}); err != nil {
			t.Fatalf("Append 失败: %v", err)
		}
	}
	// 手工追加半行 JSON (无换行), 模拟写到一半断电
	path := filepath.Join(root, "log", "crash.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("打开日志文件失败: %v", err)
	}
	if _, err := f.WriteString(`{"seq":3,"tru`); err != nil {
		t.Fatalf("写半行失败: %v", err)
	}
	f.Close()

	count := 0
	if err := lg.ReadAll(func([]byte) error { count++; return nil }); err != nil {
		t.Fatalf("尾部截断行应被容忍, got err=%v", err)
	}
	if count != 3 {
		t.Fatalf("应读到 3 条完整记录, got=%d", count)
	}

	// 半行后继续 Append: O_APPEND 直接接在半行后, 合并成一个尾部坏行,
	// 仍按尾部截断容忍 (不丢已完整落盘的 3 条)。
	if err := lg.Append(map[string]int{"seq": 4}); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}
	count = 0
	if err := lg.ReadAll(func([]byte) error { count++; return nil }); err != nil {
		t.Fatalf("合并尾部坏行应被容忍, got err=%v", err)
	}
	if count != 3 {
		t.Fatalf("应仍读到 3 条完整记录, got=%d", count)
	}

	// 中部完整坏行 (带换行, 后面还有合法行) → 属数据损坏, ReadAll 报错 (语义钉死)
	lg2 := s.Log("corrupt")
	if err := lg2.Append(map[string]int{"seq": 0}); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}
	path2 := filepath.Join(root, "log", "corrupt.jsonl")
	f2, err := os.OpenFile(path2, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("打开日志文件失败: %v", err)
	}
	if _, err := f2.WriteString("{\"bad\n"); err != nil {
		t.Fatalf("写坏行失败: %v", err)
	}
	f2.Close()
	if err := lg2.Append(map[string]int{"seq": 1}); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}
	if err := lg2.ReadAll(func([]byte) error { return nil }); err == nil {
		t.Fatalf("中部坏行应报错")
	}
}

func TestBlobDedupeAndGet(t *testing.T) {
	forEachStore(t, func(t *testing.T, s StateStore) {
		blob := s.Blob()
		data := []byte("hello content-addressed world")
		h1, err := blob.Put(data)
		if err != nil {
			t.Fatalf("Put 失败: %v", err)
		}
		h2, err := blob.Put(data)
		if err != nil {
			t.Fatalf("幂等 Put 失败: %v", err)
		}
		if h1 != h2 {
			t.Fatalf("同内容应同 hash: %s != %s", h1, h2)
		}
		if len(h1) != 64 {
			t.Fatalf("hash 应为 sha256 十六进制 64 位, got=%d", len(h1))
		}
		if !blob.Has(h1) {
			t.Fatalf("Has(%s) 应为 true", h1)
		}
		got, err := blob.Get(h1)
		if err != nil || string(got) != string(data) {
			t.Fatalf("Get 内容不一致: err=%v got=%q", err, got)
		}
		// 不存在 → 报错
		missing := "0000000000000000000000000000000000000000000000000000000000000000"
		if _, err := blob.Get(missing); err == nil {
			t.Fatalf("Get 不存在的 blob 应报错")
		}
		if blob.Has(missing) {
			t.Fatalf("Has 不存在的 blob 应为 false")
		}
	})
}

// TestBlobSingleCopy 文件后端: 同内容只存一份物理文件。
func TestBlobSingleCopy(t *testing.T) {
	root := t.TempDir()
	s := NewFileStore(root)
	data := []byte("dedupe me")
	hash, err := s.Blob().Put(data)
	if err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	if _, err := s.Blob().Put(data); err != nil {
		t.Fatalf("重复 Put 失败: %v", err)
	}
	var files int
	err = filepath.Walk(filepath.Join(root, "blob"), func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 blob 目录失败: %v", err)
	}
	if files != 1 {
		t.Fatalf("同内容应只存一份, got=%d 个文件", files)
	}
	// 布局: <root>/blob/<hash前2字符>/<hash>
	if _, err := os.Stat(filepath.Join(root, "blob", hash[:2], hash)); err != nil {
		t.Fatalf("blob 文件布局异常: %v", err)
	}
}
