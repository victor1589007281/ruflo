package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func cronFile(dir string) string { return filepath.Join(dir, "cron_jobs.json") }

// 正常路径: 落盘能被原样读回来, 且留下 .bak。
func TestCronPersistThenLoad(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronScheduler(dir, nil)
	if err := cs.AddJob(&CronJob{Name: "IMA", Schedule: "0 6 * * *", JobType: "sync", Payload: "ima"}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	data, err := os.ReadFile(cronFile(dir))
	if err != nil {
		t.Fatalf("落盘文件不存在: %v", err)
	}
	var onDisk map[string]*CronJob
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("落盘内容不是合法任务表: %v", err)
	}
	if len(onDisk) != 1 {
		t.Fatalf("落盘任务数 = %d, 期望 1", len(onDisk))
	}

	// 第二次落盘 (模拟任务跑完更新统计) 应把上一版留成 .bak
	if err := cs.PauseJob("cron-1"); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	if _, err := os.Stat(cronFile(dir) + ".bak"); err != nil {
		t.Fatalf("没有留下 .bak: %v", err)
	}

	// 新进程读回
	cs2 := NewCronScheduler(dir, nil)
	if got := len(cs2.ListJobs()); got != 1 {
		t.Fatalf("重启后任务数 = %d, 期望 1", got)
	}
}

// 0 字节文件 (上次落盘被截断) 必须从 .bak 恢复, 而不是静默变成空调度器。
func TestCronLoadRecoversFromBakWhenFileTruncated(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronScheduler(dir, nil)
	if err := cs.AddJob(&CronJob{Name: "微信读书", Schedule: "30 6 * * *", JobType: "sync", Payload: "weread"}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := cs.PauseJob("cron-1"); err != nil { // 触发第二次落盘 → 生成 .bak
		t.Fatalf("PauseJob: %v", err)
	}
	if err := os.WriteFile(cronFile(dir), nil, 0644); err != nil { // 模拟被截断
		t.Fatalf("truncate: %v", err)
	}

	cs2 := NewCronScheduler(dir, nil)
	jobs := cs2.ListJobs()
	if len(jobs) != 1 || jobs[0].Payload != "weread" {
		t.Fatalf("恢复失败, 任务表 = %+v", jobs)
	}

	// 坏文件要留证据, 盘上要回到完好状态 (下次启动不再走恢复路径)
	bad, _ := filepath.Glob(cronFile(dir) + ".corrupt-*")
	if len(bad) != 1 {
		t.Fatalf("没有留下 .corrupt 证据: %v", bad)
	}
	data, err := os.ReadFile(cronFile(dir))
	if err != nil || len(data) == 0 {
		t.Fatalf("恢复后没有回写好任务表: err=%v len=%d", err, len(data))
	}
	var onDisk map[string]*CronJob
	if err := json.Unmarshal(data, &onDisk); err != nil || len(onDisk) != 1 {
		t.Fatalf("回写内容不可用: err=%v jobs=%d", err, len(onDisk))
	}

	cs3 := NewCronScheduler(dir, nil)
	if got := len(cs3.ListJobs()); got != 1 {
		t.Fatalf("第三次启动任务数 = %d, 期望 1", got)
	}
}

// 没有 .bak 时不能假装恢复成功, 但必须照常启动 (调度器空转好过起不来)。
func TestCronLoadEmptyFileWithoutBak(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(cronFile(dir), nil, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cs := NewCronScheduler(dir, nil)
	if got := len(cs.ListJobs()); got != 0 {
		t.Fatalf("任务数 = %d, 期望 0", got)
	}
	// 之后的写入仍然要能正常工作
	if err := cs.AddJob(&CronJob{Name: "新任务", Schedule: "*/5 * * * *", JobType: "query"}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	cs2 := NewCronScheduler(dir, nil)
	if got := len(cs2.ListJobs()); got != 1 {
		t.Fatalf("新任务没落盘, 任务数 = %d", got)
	}
}

// 落盘不能留临时文件残渣。
func TestCronPersistLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	cs := NewCronScheduler(dir, nil)
	for i := 0; i < 3; i++ {
		if err := cs.AddJob(&CronJob{Name: "t", Schedule: "0 6 * * *", JobType: "sync", Payload: "ima"}); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".cron_jobs-*"))
	if len(left) != 0 {
		t.Fatalf("留下了临时文件: %v", left)
	}
}
