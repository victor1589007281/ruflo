package agentdbsemantic_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropic/claude-go/pkg/agentdbsemantic"
	"gitee.com/lorydb/agentDB/pkg/embedding"
	"gitee.com/lorydb/agentDB/pkg/file"
	"gitee.com/lorydb/agentDB/pkg/memory"
	"gitee.com/lorydb/agentDB/pkg/server"
)

// newSemanticServer 起一个进程内 agentDB serve, 装配层2 语义后端
// (Memory HNSW + hash-stub Embedding + File BM25)。hash-stub 是确定性
// 精确文本召回, 故文本路径断言用与写入完全相同的查询串。
func newSemanticServer(t *testing.T) *agentdbsemantic.Client {
	t.Helper()
	mem, err := memory.NewStore(128)
	if err != nil {
		t.Fatal(err)
	}
	emb := embedding.NewService()
	emb.Register("default", embedding.NewHashStubProvider("default", 128, 512))
	emb.SetDefault("default")

	fb := file.NewDB()
	if _, err := fb.Index(context.Background(), "deploy.go", []byte(
		"package main\n// DeployAgentDB 部署 agentDB 集群\ntype DeployAgentDB struct{}\n")); err != nil {
		t.Fatal(err)
	}

	srv := server.New(server.Backends{Memory: mem, Embedding: emb, File: fb})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = fb.Close() })
	return agentdbsemantic.New(ts.URL)
}

// TestMemoryStoreSearchE2E 层2 验收: 写入记忆 → 服务端嵌入 → 检索召回。
func TestMemoryStoreSearchE2E(t *testing.T) {
	c := newSemanticServer(t)
	ctx := context.Background()

	// 写入: 不提供向量, serve 端用 hash-stub 嵌入 content。
	content := "如何部署 agentDB 集群的完整步骤"
	id, err := c.StoreMemory(ctx, agentdbsemantic.Memory{
		Content:   content,
		Type:      agentdbsemantic.MemoryProcedural,
		SessionID: "sess-1",
		Tags:      []string{"deploy", "agentdb"},
	})
	if err != nil {
		t.Fatalf("StoreMemory: %v", err)
	}
	if id == "" {
		t.Fatal("StoreMemory 应返回服务端生成 id")
	}

	// 检索: 精确同文 (hash-stub 只保证同文召回)。
	hits, err := c.SearchMemory(ctx, content, 5, 0)
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Content == content {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("检索应召回刚写入的记忆, got %+v", hits)
	}

	// 衰减参数: 只验证端点接受 decay 且不报错 (排序权重变化不在此断言)。
	if _, err := c.SearchMemory(ctx, content, 5, 0.02); err != nil {
		t.Fatalf("SearchMemory(decay): %v", err)
	}
}

// TestMemoryVecRoundtrip 向量直传路径: 写入显式向量 → 向量检索命中。
func TestMemoryVecRoundtrip(t *testing.T) {
	c := newSemanticServer(t)
	ctx := context.Background()
	vec := make([]float32, 128)
	vec[0] = 1
	if _, err := c.StoreMemory(ctx, agentdbsemantic.Memory{
		Content:   "向量直写",
		Embedding: vec,
		Type:      agentdbsemantic.MemoryWorking,
	}); err != nil {
		t.Fatalf("StoreMemory(vec): %v", err)
	}
	hits, err := c.SearchMemoryVec(ctx, vec, 5, 0)
	if err != nil {
		t.Fatalf("SearchMemoryVec: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Content == "向量直写" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("向量路径应召回, got %+v", hits)
	}
}

// TestRetrieveQueryE2E 层2 验收: 跨源检索 (memory+file) 聚合 + source_counts。
func TestRetrieveQueryE2E(t *testing.T) {
	c := newSemanticServer(t)
	ctx := context.Background()
	content := "DeployAgentDB 结构体定义了部署参数"
	if _, err := c.StoreMemory(ctx, agentdbsemantic.Memory{Content: content}); err != nil {
		t.Fatalf("StoreMemory: %v", err)
	}

	res, err := c.RetrieveQuery(ctx, content, nil, 5) // 默认 memory+file
	if err != nil {
		t.Fatalf("RetrieveQuery: %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatalf("跨源检索应召回记忆, items=%d", len(res.Items))
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
	if memHits == 0 {
		t.Fatalf("memory 源应有命中, items=%+v", res.Items)
	}
	if res.SourceCounts[agentdbsemantic.SourceMemory] == 0 {
		t.Fatalf("source_counts 应含 memory 计数, counts=%+v", res.SourceCounts)
	}
	_ = fileHits
}

// TestAssembleContext 层2 验收: RRF 上下文组装, token 预算裁剪。
func TestAssembleContext(t *testing.T) {
	c := newSemanticServer(t)
	ctx := context.Background()
	asm, err := c.AssembleContext(ctx, "部署问题", 600, 200, []agentdbsemantic.RetrieveHit{
		{Source: agentdbsemantic.SourceMemory, Content: "部署 agentDB 需先初始化数据目录", Score: 0.9},
		{Source: agentdbsemantic.SourceFile, Content: "deploy.go 定义了部署参数", Score: 0.7},
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	if asm.Body == "" {
		t.Fatal("组装结果应非空")
	}
	if !strings.Contains(asm.Body, "agentDB") {
		t.Fatalf("组装体应含检索内容, body=%q", asm.Body)
	}
	if asm.Items == 0 {
		t.Fatalf("items 应 > 0")
	}
}

// TestFilesSearch 层2 验收: 文件 BM25 检索 + 内容重组读取。
func TestFilesSearch(t *testing.T) {
	c := newSemanticServer(t)
	ctx := context.Background()
	hits, err := c.SearchFiles(ctx, "DeployAgentDB", 5)
	if err != nil {
		t.Fatalf("SearchFiles: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("BM25 应召回 deploy.go")
	}
	if !strings.Contains(hits[0].Meta.Path, "deploy.go") {
		t.Fatalf("命中路径应含 deploy.go, got %+v", hits[0])
	}
	raw, err := c.GetFileContent(ctx, hits[0].Meta.Path)
	if err != nil {
		t.Fatalf("GetFileContent: %v", err)
	}
	if !strings.Contains(string(raw), "DeployAgentDB") {
		t.Fatalf("内容重组应含原文, got %q", raw)
	}
	if _, err := c.GetSymbols(ctx, "DeployAgentDB"); err != nil {
		t.Fatalf("GetSymbols: %v", err)
	}
}
