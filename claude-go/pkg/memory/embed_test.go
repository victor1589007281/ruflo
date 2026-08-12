package memory

import (
	"context"
	"testing"
)

// fakeEmbedder 按预设表回向量 (测试用)。
type fakeEmbedder struct {
	vecs map[string][]float32
}

func (f fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := f.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0}, nil // 未知文本 → 零向量
}

func TestEmbedderFromEnv(t *testing.T) {
	if e := EmbedderFromEnv(func(string) string { return "" }); e != nil {
		t.Error("未配置应返回 nil (纯 BM25 现状)")
	}
	e := EmbedderFromEnv(func(k string) string {
		if k == "CLAUDE_GO_MEMORY_EMBEDDER" {
			return "ollama:bge-m3"
		}
		return ""
	})
	if e == nil {
		t.Error("ollama:bge-m3 应构造成功")
	}
	if e2 := EmbedderFromEnv(func(k string) string {
		if k == "CLAUDE_GO_MEMORY_EMBEDDER" {
			return "openai:text-embedding-3"
		}
		return ""
	}); e2 != nil {
		t.Error("未知 provider 应返回 nil")
	}
}

func TestTieredStoreEmbedBlend(t *testing.T) {
	// 两条内容几乎不同的记忆; BM25 对 "部署" 命中 A, 但向量应把语义相近的 B 拉上来
	emb := fakeEmbedder{vecs: map[string][]float32{
		"部署端口是多少":            {1, 0},
		"服务监听的端口配置":          {0.9, 0.1}, // 与查询语义相近
		"今天天气很好适合出门散步":    {0, 1},     // 不相关
	}}
	s := NewTieredStore()
	s.SetEmbedder(emb)
	s.Add(&MemoryEntry{Content: "服务监听的端口配置", Importance: 0.5})
	s.Add(&MemoryEntry{Content: "今天天气很好适合出门散步", Importance: 0.5})

	// Add 时应已写入向量
	for _, e := range s.episodic {
		if len(e.Embedding) == 0 {
			t.Error("Add 应向量化记忆内容")
		}
	}

	// 查询与 A 无词面重叠 (BM25≈0) 但语义近 A; 混合后 A 必须排第一
	got := s.Retrieve("部署端口是多少", 1)
	if len(got) == 0 {
		t.Fatal("应有检索结果")
	}
	if got[0].Content != "服务监听的端口配置" {
		t.Errorf("向量混合应把语义相近项提到第一, 得 %q", got[0].Content)
	}
}

func TestTieredStoreEmbedderFailureDegrades(t *testing.T) {
	// embedder 全错 → 静默退化为纯 BM25, 不报错不丢检索
	s := NewTieredStore()
	s.SetEmbedder(fakeEmbedder{vecs: map[string][]float32{}}) // 全部零向量
	s.Add(&MemoryEntry{Content: "端口配置是 18765", Importance: 0.5})
	got := s.Retrieve("端口", 1)
	if len(got) == 0 || got[0].Content != "端口配置是 18765" {
		t.Error("embedder 失效应静默退化纯 BM25")
	}
}
