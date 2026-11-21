package harnesspro

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVersionRegistrySeparatesLineageAndContent(t *testing.T) {
	registry, err := NewVersionRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := versionedTestHarness(t, "run-a")
	b := versionedTestHarness(t, "run-b")
	if a.VersionID == b.VersionID {
		t.Fatal("相同 candidate_id 跨 run 不应复用 version_id")
	}
	if a.ContentID != b.ContentID {
		t.Fatal("相同指令跨 run 应共享 content_id")
	}
	ts := IdentifyTaskSet("demo", "v1", []Task{{ID: "a", Prompt: "a"}, {ID: "b", Prompt: "b"}})
	for _, h := range []Harness{a, b} {
		if err := registry.Register(h, "model-a", ts, ""); err != nil {
			t.Fatal(err)
		}
	}
	// resume 重入同版本不重复登记; 同阶段观测也幂等替换。
	if err := registry.Register(a, "model-a", ts, ""); err != nil {
		t.Fatal(err)
	}
	otherTS := IdentifyTaskSet("demo", "v2", []Task{{ID: "a", Prompt: "changed"}, {ID: "b", Prompt: "b"}})
	if err := registry.Register(a, "model-a", otherTS, ""); err == nil {
		t.Fatal("同一版本不能混入不同任务集的观测")
	}
	if err := registry.Register(a, "model-b", ts, ""); err == nil {
		t.Fatal("同一版本不能在重入时静默改模型")
	}
	for i := 0; i < 2; i++ {
		if err := registry.ObserveEval("run-a", 1, "eval", CandidateEval{
			VersionID: a.VersionID, PassRate: 0.5, PassFrac: 0.75, CostTokens: 100, NEval: 2,
		}, true, false, []string{"medium"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := registry.Get(a.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Observations) != 1 || got.Observations[0].PassFrac != 0.75 {
		t.Fatalf("观测被重复追加或丢失: %+v", got.Observations)
	}
	sameContent, err := registry.List("", a.ContentID, nil)
	if err != nil || len(sameContent) != 2 {
		t.Fatalf("按 content_id 跨 run 比较应得 2 版, got %d, %v", len(sameContent), err)
	}
	onlyA, err := registry.List("run-a", "", nil)
	if err != nil || len(onlyA) != 1 || onlyA[0].VersionID != a.VersionID {
		t.Fatalf("run 过滤错误: %+v, %v", onlyA, err)
	}
	if _, err := registry.Get("../escape"); err == nil {
		t.Fatal("非法版本路径必须拒绝")
	}
	bad := a
	bad.ContentID = "hc-forged"
	if _, err := IdentifyHarness(bad, "run-a"); err == nil {
		t.Fatal("伪造 content_id 不能被静默接受")
	}
}

func TestTaskSetDirectoryRegistrationAndImmutability(t *testing.T) {
	source := t.TempDir()
	manifest := `{"name":"support-json","version":"v1","description":"two real prompts","tasks_file":"tasks.json"}`
	tasks := `[
		{"id":"json-a","category":"json","difficulty":0.2,"prompt":"输出 {\\\"kind\\\":\\\"a\\\"}"},
		{"id":"json-b","category":"json","difficulty":0.4,"prompt":"输出 {\\\"kind\\\":\\\"b\\\"}"}
	]`
	if err := os.WriteFile(filepath.Join(source, "taskset.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "tasks.json"), []byte(tasks), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := LoadTaskSet(source)
	if err != nil {
		t.Fatal(err)
	}
	if set.TaskCount != 2 || set.Hash == "" || set.TasksFile != "tasks.json" {
		t.Fatalf("任务集加载结果不完整: %+v", set)
	}
	store := NewTaskSetStore(filepath.Join(t.TempDir(), "registry"))
	if err := store.Register(set); err != nil {
		t.Fatal(err)
	}
	latest, err := store.Get("support-json")
	if err != nil || latest.Hash != set.Hash || len(latest.Tasks) != 2 || latest.TasksFile != "" {
		t.Fatalf("注册后的任务集应自包含: %+v, %v", latest, err)
	}
	list, err := store.List()
	if err != nil || len(list) != 1 || list[0].TaskCount != 2 || list[0].Tasks != nil {
		t.Fatalf("列表摘要错误: %+v, %v", list, err)
	}
	changed := set
	changed.Tasks = append([]Task(nil), set.Tasks...)
	changed.Tasks[0].Prompt = "换掉考场"
	if err := store.Register(changed); err == nil {
		t.Fatal("同 name@version 不同 hash 必须拒绝覆盖")
	}
	changed.Version = "v2"
	if err := store.Register(changed); err != nil {
		t.Fatalf("提升版本后应允许注册: %v", err)
	}
	if latest, err := store.Get("support-json"); err != nil || latest.Version != "v2" {
		t.Fatalf("省略版本应取最近导入: %+v, %v", latest, err)
	}
}

func TestTaskSetRejectsPathEscapeAndDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	body := `{"name":"escape","version":"v1","tasks_file":"../tasks.json"}`
	if err := os.WriteFile(filepath.Join(dir, "taskset.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTaskSet(dir); err == nil {
		t.Fatal("tasks_file 路径逃逸必须拒绝")
	}
	if err := ValidateTaskPool([]Task{{ID: "same", Prompt: "a"}, {ID: "same", Prompt: "b"}}); err == nil {
		t.Fatal("重复任务 ID 必须拒绝")
	}
}
