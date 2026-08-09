// Package e2eagentdb — 部署型端到端功能测试 (规划 14.1 全部能力覆盖)。
//
// 前置: 编译并部署真实 agentDB serve 二进制, 设置环境变量:
//
//	cd /home/victor/base/git/gitee/agentDB
//	go build -o /tmp/agentdb-e2e/agentdb ./cmd/agentdb
//	export AGENTDB_E2E_BIN=/tmp/agentdb-e2e/agentdb
//	cd /home/victor/base/git/temp/ruflo/claude-go
//	go test ./pkg/e2eagentdb/ -v -count=1
//
// 测试自身拉起 serve (临时端口 + 临时数据目录, minilm 真实语义嵌入), 经 HTTP
// 驱动 claude-go 的数据面 (agentdbclient) / 语义面 (agentdbsemantic) / 分布式
// (租约+CAS 认领) / 引擎注入 (AgentDBSemanticInjectHook), 并做 SIGTERM 重启
// 持久化验证。未设置 AGENTDB_E2E_BIN 时跳过。
package e2eagentdb_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/agentdbclient"
	"github.com/anthropic/claude-go/pkg/agentdbsemantic"
	"github.com/anthropic/claude-go/pkg/engine/internal_hook"
	"github.com/anthropic/claude-go/pkg/types"
)

const e2eBinEnv = "AGENTDB_E2E_BIN"

// ---- serve 子进程管理 ----

type serveProc struct {
	cmd     *exec.Cmd
	url     string
	dataDir string
	log     *bytes.Buffer
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func launchServe(t *testing.T, bin, dataDir string, port int) *serveProc {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	cmd := exec.Command(bin, "serve",
		"--addr", fmt.Sprintf("127.0.0.1:%d", port),
		"--data", dataDir,
		"--snapshot-interval", "0s") // 0 = 仅关闭时落盘, 重启验证确定
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 agentdb serve: %v", err)
	}
	p := &serveProc{cmd: cmd, url: url, dataDir: dataDir, log: &buf}
	if err := p.waitHealth(20 * time.Second); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("serve 未就绪: %v\nlog:\n%s", err, buf.String())
	}
	t.Logf("serve 已就绪: %s data=%s", url, dataDir)
	return p
}

func (p *serveProc) waitHealth(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(p.url + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("health 检查超时")
}

// stop 发 SIGTERM (graceful: saveAll 落盘 + 关 journal), 等进程退出。
func (p *serveProc) stop(t *testing.T) {
	t.Helper()
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
	t.Logf("serve 已停止 (pid=%d)", p.cmd.Process.Pid)
}

// ---- 数据面: 层1 + 分布式 (M3) ----

type e2eVal struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testDataPlane(t *testing.T, c *agentdbclient.Client) {
	t.Helper()
	ctx := context.Background()
	kv := c.KV("e2e-kv")

	// KV 往返 + Keys 有序 + Delete 幂等
	var got e2eVal
	if ok, err := kv.Get("a", &got); err != nil || ok {
		t.Fatalf("未写入 Get 应 (false,nil), ok=%v err=%v", ok, err)
	}
	want := e2eVal{Name: "alpha", Count: 42}
	if err := kv.Put("a", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ok, err := kv.Get("a", &got); err != nil || !ok || got != want {
		t.Fatalf("KV 往返失败: ok=%v got=%+v err=%v", ok, got, err)
	}
	for _, k := range []string{"z", "m"} {
		if err := kv.Put(k, e2eVal{Name: k}); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := kv.Keys()
	if err != nil || len(keys) != 3 || keys[0] != "a" || keys[2] != "z" {
		t.Fatalf("Keys 应有序, got=%v err=%v", keys, err)
	}
	if err := kv.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err := kv.Delete("a"); err != nil {
		t.Fatalf("重复 Delete 应幂等: %v", err)
	}

	// CAS: 冲突 / 字节级匹配 / nil 前置
	if err := c.KV("cas").Put("x", e2eVal{Name: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.CAS(ctx, "cas", "x", []byte(`{"name":"999","count":0}`), mustJSON(t, e2eVal{Name: "v2"})); !errors.Is(err, agentdbclient.ErrCASConflict) {
		t.Fatalf("CAS 旧值不匹配应冲突, got %v", err)
	}
	if err := c.CAS(ctx, "cas", "x", mustJSON(t, e2eVal{Name: "v1"}), mustJSON(t, e2eVal{Name: "v2"})); err != nil {
		t.Fatalf("CAS 匹配应成功: %v", err)
	}
	if err := c.CAS(ctx, "cas", "fresh", nil, mustJSON(t, e2eVal{Name: "v"})); err != nil {
		t.Fatalf("CAS(nil) 新键应成功: %v", err)
	}

	// Tx: 跨 bucket 原子批写
	if err := c.Tx(ctx, []agentdbclient.TxOp{
		{Bucket: "ta", Key: "k1", Value: mustJSON(t, e2eVal{Name: "a1"})},
		{Bucket: "tb", Key: "k2", Value: mustJSON(t, e2eVal{Name: "b2"})},
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	for _, tc := range []struct{ b, k string }{{"ta", "k1"}, {"tb", "k2"}} {
		var v e2eVal
		if ok, _ := c.KV(tc.b).Get(tc.k, &v); !ok {
			t.Fatalf("Tx 后 %s/%s 应存在", tc.b, tc.k)
		}
	}

	// Log: JSONL 追加 + 顺序读回
	lg := c.Log("e2e-log")
	for i := 0; i < 5; i++ {
		if err := lg.Append(map[string]int{"seq": i}); err != nil {
			t.Fatalf("Log.Append: %v", err)
		}
	}
	var lines [][]byte
	if err := lg.ReadAll(func(line []byte) error {
		lines = append(lines, append([]byte(nil), line...))
		return nil
	}); err != nil {
		t.Fatalf("Log.ReadAll: %v", err)
	}
	if len(lines) != 5 || string(lines[3]) != `{"seq":3}` {
		t.Fatalf("Log 应 5 行且第4行={\"seq\":3}, got %d 行", len(lines))
	}

	// Blob: sha256 内容寻址 + 去重
	blob := c.Blob()
	data := []byte("e2e distributed payload")
	h1, err := blob.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := blob.Put(data)
	if err != nil || h1 != h2 || len(h1) != 64 {
		t.Fatalf("Blob 去重应同 hash: %s != %s err=%v", h1, h2, err)
	}
	if got, err := blob.Get(h1); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Blob.Get: err=%v", err)
	}
	if !blob.Has(h1) {
		t.Fatal("Blob.Has 应为 true")
	}
}

func testDistributed(t *testing.T, c *agentdbclient.Client) {
	t.Helper()
	ctx := context.Background()

	// 租约: 抢占冲突 / 续租 / 释放 / 过期重派 (worker 崩溃 → 任务重派)
	l, err := c.Acquire(ctx, "e2e-worker", "node-a", time.Minute)
	if err != nil || l.Holder != "node-a" {
		t.Fatalf("Acquire: l=%+v err=%v", l, err)
	}
	if _, err := c.Acquire(ctx, "e2e-worker", "node-b", time.Minute); err == nil {
		t.Fatal("node-b 抢占应被拒")
	}
	if _, err := c.Renew(ctx, "e2e-worker", "node-a", time.Minute); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if err := c.Release(ctx, "e2e-worker", "node-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := c.Acquire(ctx, "e2e-worker", "node-b", time.Minute); err != nil {
		t.Fatalf("释放后 node-b 应可抢占: %v", err)
	}
	if _, err := c.Acquire(ctx, "e2e-ephemeral", "dead-worker", 150*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if l2, err := c.Acquire(ctx, "e2e-ephemeral", "live-worker", time.Minute); err != nil || l2.Holder != "live-worker" {
		t.Fatalf("过期后接管: l=%+v err=%v", l2, err)
	}

	// 任务认领: 双 worker CAS 抢同一 pending 任务 → 恰 1 个 winner
	if err := c.KV("dist-tasks").Put("task-1", map[string]string{"state": "pending", "owner": ""}); err != nil {
		t.Fatal(err)
	}
	claim := func(worker string) error {
		for {
			var cur map[string]string
			ok, err := c.KV("dist-tasks").Get("task-1", &cur)
			if err != nil {
				return err
			}
			if !ok || cur["state"] != "pending" {
				return errors.New("task already claimed")
			}
			err = c.CAS(ctx, "dist-tasks", "task-1", mustJSON(t, cur), mustJSON(t, map[string]string{"state": "claimed", "owner": worker}))
			if errors.Is(err, agentdbclient.ErrCASConflict) {
				continue
			}
			return err
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, w := range []string{"worker-1", "worker-2"} {
		wg.Add(1)
		go func(w string) {
			defer wg.Done()
			results <- claim(w)
		}(w)
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("任务认领应恰 1 个 winner, got %d", winners)
	}
	var final map[string]string
	if ok, _ := c.KV("dist-tasks").Get("task-1", &final); !ok || final["state"] != "claimed" {
		t.Fatalf("任务终态=%+v", final)
	}
}

// ---- 语义面: 层2 ----

func testSemantic(t *testing.T, sem *agentdbsemantic.Client) {
	t.Helper()
	ctx := context.Background()

	// 记忆写入 (minilm 服务端真实语义嵌入) → 语义检索召回
	content := "agentdb 分布式任务认领使用 lease 与 cas 原语"
	if _, err := sem.StoreMemory(ctx, agentdbsemantic.Memory{
		Content:   content,
		Type:      agentdbsemantic.MemoryProcedural,
		SessionID: "e2e-sess",
		Tags:      []string{"agentdb", "distributed"},
	}); err != nil {
		t.Fatalf("StoreMemory: %v", err)
	}
	hits, err := sem.SearchMemory(ctx, "分布式任务如何认领", 5, 0)
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("语义检索应召回相关记忆 (minilm), got 0 hits")
	}
	if _, err := sem.SearchMemory(ctx, content, 5, 0.02); err != nil {
		t.Fatalf("SearchMemory(decay) 应可用: %v", err)
	}

	// 文件: 索引 → BM25 检索 → 内容重组。BM25 按空白切词精确匹配,
	// 故查询短语须作为独立词出现。
	filePath := "e2e/deploy.go"
	if _, err := sem.IndexFile(ctx, filePath, []byte(
		"package deploy\n// DeployAgentDB 集群部署\n// 分布式任务认领 示例\ntype DeployAgentDB struct{}\n")); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}
	fhits, err := sem.SearchFiles(ctx, "DeployAgentDB", 5)
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(fhits) == 0 || !strings.Contains(fhits[0].Meta.Path, "deploy.go") {
		t.Fatalf("BM25 应召回 deploy.go, got %+v", fhits)
	}
	raw, err := sem.GetFileContent(ctx, fhits[0].Meta.Path)
	if err != nil || !strings.Contains(string(raw), "DeployAgentDB") {
		t.Fatalf("内容重组应含原文: err=%v", err)
	}

	// 跨源检索 (memory+file) + RRF 组装
	res, err := sem.RetrieveQuery(ctx, "分布式任务认领", []agentdbsemantic.Source{
		agentdbsemantic.SourceMemory, agentdbsemantic.SourceFile,
	}, 5)
	if err != nil {
		t.Fatalf("RetrieveQuery: %v", err)
	}
	var memHits, fileHits int
	for _, it := range res.Items {
		switch it.Source {
		case agentdbsemantic.SourceMemory:
			memHits++
		case agentdbsemantic.SourceFile:
			fileHits++
		}
	}
	if memHits == 0 || fileHits == 0 {
		t.Fatalf("跨源检索应 memory+file 双命中, items=%+v", res.Items)
	}
	if res.SourceCounts[agentdbsemantic.SourceMemory] == 0 {
		t.Fatalf("source_counts 应含 memory, got %+v", res.SourceCounts)
	}
	asm, err := sem.AssembleContext(ctx, "分布式任务认领", 600, 200, res.Items)
	if err != nil || asm.Body == "" {
		t.Fatalf("AssembleContext: asm=%+v err=%v", asm, err)
	}
}

// ---- 引擎注入 (MemoryInject agentDB 后端, M2 验收) ----

func testMemoryInject(t *testing.T, sem *agentdbsemantic.Client) {
	t.Helper()
	ctx := context.Background()
	content := "用户偏好使用 Go 编写高并发服务"
	if _, err := sem.StoreMemory(ctx, agentdbsemantic.Memory{Content: content}); err != nil {
		t.Fatalf("StoreMemory: %v", err)
	}

	h := internal_hook.NewAgentDBSemanticInjectHook(sem, 5, 600, 200)
	hc := &internal_hook.HookContext{
		Ctx:          ctx,
		Phase:        internal_hook.PhasePreRequest,
		TurnCount:    0,
		Messages:     []types.Message{{Type: types.MessageTypeUser, Content: []types.ContentBlock{{Type: types.ContentBlockText, Text: content}}}},
		SystemPrompt: []string{"你是软件工程师助手。"},
	}
	res, err := h.Execute(hc)
	if err != nil {
		t.Fatalf("hook Execute: %v", err)
	}
	joined := strings.Join(res.SystemPrompt, "\n")
	if !strings.Contains(joined, `<memory_context source="agentdb"`) || !strings.Contains(joined, content) {
		t.Fatalf("system prompt 应注入 agentdb 记忆上下文, got %q", joined)
	}
	t.Logf("注入成功, system prompt 片段:\n%s", joined)
}

// ---- 主入口 ----

func TestAgentDBDeployedE2E(t *testing.T) {
	bin := os.Getenv(e2eBinEnv)
	if bin == "" {
		t.Skip("AGENTDB_E2E_BIN 未设置, 跳过部署型 E2E (先 build agentdb 二进制)")
	}
	dataDir := t.TempDir()
	srv := launchServe(t, bin, dataDir, freePort(t))
	t.Cleanup(func() { srv.stop(t) })

	// 阶段 0: 预留重启持久化标记 (数据面 + 语义面)
	ctx := context.Background()
	dc := agentdbclient.New(srv.url)
	marker := e2eVal{Name: "restart-marker", Count: 7}
	if err := dc.KV("persist").Put("marker", marker); err != nil {
		t.Fatal(err)
	}
	if err := dc.Log("persist-log").Append(map[string]string{"ev": "before-restart"}); err != nil {
		t.Fatal(err)
	}
	sem0 := agentdbsemantic.New(srv.url)
	memContent := "重启后应仍然存在的记忆标记"
	if _, err := sem0.StoreMemory(ctx, agentdbsemantic.Memory{Content: memContent}); err != nil {
		t.Fatal(err)
	}
	blobHash, err := dc.Blob().Put([]byte("persist blob payload"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("层1 数据面 (KV/CAS/Tx/Log/Blob)", func(t *testing.T) { testDataPlane(t, dc) })
	t.Run("层2 语义面 (memory/retrieve/file)", func(t *testing.T) { testSemantic(t, agentdbsemantic.New(srv.url)) })
	t.Run("分布式横切 (租约/CAS 认领)", func(t *testing.T) { testDistributed(t, dc) })
	t.Run("MemoryInject 检索注入", func(t *testing.T) { testMemoryInject(t, agentdbsemantic.New(srv.url)) })

	// 阶段 5: SIGTERM 优雅关闭 → 重启同数据目录 → 验证状态存活 (P0-3 持久化)
	t.Run("重启持久化", func(t *testing.T) {
		srv.stop(t)
		srv2 := launchServe(t, bin, dataDir, freePort(t))
		t.Cleanup(func() { srv2.stop(t) })
		c2 := agentdbclient.New(srv2.url)

		var got e2eVal
		if ok, err := c2.KV("persist").Get("marker", &got); err != nil || !ok || got != marker {
			t.Fatalf("KV 重启后应存活: ok=%v got=%+v err=%v", ok, got, err)
		}
		var logLines [][]byte
		if err := c2.Log("persist-log").ReadAll(func(l []byte) error {
			logLines = append(logLines, append([]byte(nil), l...))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(logLines) != 1 || !strings.Contains(string(logLines[0]), "before-restart") {
			t.Fatalf("Log 重启后应存活, got %q", logLines)
		}
		if got, err := c2.Blob().Get(blobHash); err != nil || string(got) != "persist blob payload" {
			t.Fatalf("Blob 重启后应存活: err=%v", err)
		}
		sem2 := agentdbsemantic.New(srv2.url)
		hits, err := sem2.SearchMemory(ctx, memContent, 5, 0)
		if err != nil || len(hits) == 0 {
			t.Fatalf("记忆重启后应存活 (memories.json+journal), hits=%v err=%v", hits, err)
		}
		t.Logf("重启持久化验证通过: KV/Log/Blob/Memory 全部存活")
	})
}
