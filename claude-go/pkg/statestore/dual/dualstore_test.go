package dual

import (
	"errors"
	"testing"

	"github.com/anthropic/claude-go/pkg/statestore"
)

func TestDualWriteFanout(t *testing.T) {
	f := statestore.NewMemStore()
	a := statestore.NewMemStore()
	d := NewDual(f, a, ReadFile)

	if err := d.KV("b").Put("k", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	// 双写: 两侧都有
	var out struct {
		N int `json:"n"`
	}
	ok, _ := f.KV("b").Get("k", &out)
	if !ok || out.N != 1 {
		t.Error("文件侧未落盘")
	}
	ok, _ = a.KV("b").Get("k", &out)
	if !ok || out.N != 1 {
		t.Error("agentDB 侧未双写")
	}

	// Log 双写
	_ = d.Log("l").Append(map[string]string{"e": "1"})
	c1, c2 := 0, 0
	_ = f.Log("l").ReadAll(func([]byte) error { c1++; return nil })
	_ = a.Log("l").ReadAll(func([]byte) error { c2++; return nil })
	if c1 != 1 || c2 != 1 {
		t.Errorf("Log 双写失败: file=%d agent=%d", c1, c2)
	}

	// Blob 双写 + 哈希一致
	h, err := d.Blob().Put([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Blob().Has(h) || !a.Blob().Has(h) {
		t.Error("Blob 双写失败")
	}
}

func TestDualReadSources(t *testing.T) {
	f := statestore.NewMemStore()
	a := statestore.NewMemStore()
	// 制造分叉: 两侧同 key 不同值
	_ = f.KV("b").Put("k", "file-val")
	_ = a.KV("b").Put("k", "agent-val")

	var out string
	// file 档
	d := NewDual(f, a, ReadFile)
	d.KV("b").Get("k", &out)
	if out != "file-val" {
		t.Errorf("ReadFile 应读文件侧, 得 %s", out)
	}
	// agentdb 严格档
	d = NewDual(f, a, ReadAgentDB)
	d.KV("b").Get("k", &out)
	if out != "agent-val" {
		t.Errorf("ReadAgentDB 应读 agentDB 侧, 得 %s", out)
	}
	// fallback 档: agentDB 命中
	d = NewDual(f, a, ReadFallback)
	d.KV("b").Get("k", &out)
	if out != "agent-val" {
		t.Errorf("ReadFallback 命中时应读 agentDB, 得 %s", out)
	}
	// fallback 档: agentDB 未命中回退文件
	_ = f.KV("b").Put("only-file", "x")
	d = NewDual(f, a, ReadFallback)
	out = ""
	ok, _ := d.KV("b").Get("only-file", &out)
	if !ok || out != "x" {
		t.Error("ReadFallback 未命中应回退文件")
	}
}

func TestDualAgentFailureFailOpen(t *testing.T) {
	f := statestore.NewMemStore()
	d := NewDual(f, &failStore{}, ReadFile)
	// agentDB 全挂: 写仍成功 (fail-open), 文件侧有数据
	if err := d.KV("b").Put("k", 1); err != nil {
		t.Errorf("agentDB 失败不应阻断写入: %v", err)
	}
	var out int
	ok, _ := f.KV("b").Get("k", &out)
	if !ok || out != 1 {
		t.Error("文件侧应已落盘")
	}
}

func TestDualNilAgentDegrades(t *testing.T) {
	f := statestore.NewMemStore()
	d := NewDual(f, nil, ReadFallback) // nil agent → 强制 file 档
	if d.read != ReadFile {
		t.Error("nil agent 应强制 ReadFile")
	}
	if err := d.KV("b").Put("k", 1); err != nil {
		t.Fatal(err)
	}
	var out int
	ok, _ := d.KV("b").Get("k", &out)
	if !ok {
		t.Error("纯文件读写应正常")
	}
}

// failStore 全方法报错的 StateStore (模拟 agentDB 不稳定)。
type failStore struct{}

func (failStore) KV(string) statestore.KVStore   { return failKV{} }
func (failStore) Log(string) statestore.AppendLog { return failLog{} }
func (failStore) Blob() statestore.BlobStore      { return failBlob{} }

var errDown = errors.New("agentdb down")

type failKV struct{}

func (failKV) Get(string, any) (bool, error) { return false, errDown }
func (failKV) Put(string, any) error         { return errDown }
func (failKV) Delete(string) error           { return errDown }
func (failKV) Keys() ([]string, error)       { return nil, errDown }

type failLog struct{}

func (failLog) Append(any) error                       { return errDown }
func (failLog) ReadAll(func([]byte) error) error       { return errDown }

type failBlob struct{}

func (failBlob) Put([]byte) (string, error) { return "", errDown }
func (failBlob) Get(string) ([]byte, error) { return nil, errDown }
func (failBlob) Has(string) bool            { return false }
