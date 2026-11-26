package codeintel

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateQueryTarget 钉住"空目标必须报参数错"这条不变量。
//
// 背景：find_refs 带空 symbol 时，Engine 会降级到 Native.FindReferences，
// 后者把空 pattern 交给 `rg -w ''` —— 匹配全仓每一行（agentDB 实测 count:21100，
// 首条是 deploy/build-and-load.sh:1 的 shebang）。返回体 engine=native_references、
// 无 error 字段，调用方据此以为"查到了"。参数拼错（JSON 里写 query 而不是 symbol）
// 就该在分派前被点出来。
func TestValidateQueryTarget(t *testing.T) {
	cases := []struct {
		name       string
		queryType  string
		symbol     string
		filePath   string
		targetSym  string
		wantErr    bool
		wantSubstr string
	}{
		{name: "find_refs 空 symbol", queryType: "find_refs", wantErr: true, wantSubstr: "missing symbol"},
		{name: "find_refs 纯空白 symbol", queryType: "find_refs", symbol: "   ", wantErr: true, wantSubstr: "missing symbol"},
		{name: "find_refs 有 symbol", queryType: "find_refs", symbol: "handleKVPut"},
		{name: "navigate 空 symbol", queryType: "navigate", wantErr: true, wantSubstr: "missing symbol"},
		{name: "explain 空 symbol", queryType: "explain", wantErr: true, wantSubstr: "missing symbol"},
		{name: "cross_shard 空 symbol", queryType: "cross_shard", wantErr: true, wantSubstr: "missing symbol"},
		{name: "impact 空 file_path", queryType: "impact", symbol: "ignored", wantErr: true, wantSubstr: "missing file_path"},
		{name: "impact 有 file_path", queryType: "impact", filePath: "pkg/agentdb/agentdb.go"},
		{name: "path 缺 target_symbol", queryType: "path", symbol: "a", wantErr: true, wantSubstr: "missing target_symbol"},
		{name: "path 完整", queryType: "path", symbol: "a", targetSym: "b"},
		{name: "communities 无目标要求", queryType: "communities"},
		{name: "god_nodes 无目标要求", queryType: "god_nodes"},
		{name: "surprises 无目标要求", queryType: "surprises"},
		{name: "未知类型直通", queryType: "not-a-type"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateQueryTarget(tc.queryType, tc.symbol, tc.filePath, tc.targetSym)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				if !errors.Is(err, ErrEmptyQueryTarget) {
					t.Fatalf("want ErrEmptyQueryTarget, got %v", err)
				}
				if !strings.Contains(err.Error(), tc.wantSubstr) {
					t.Fatalf("want error containing %q, got %q", tc.wantSubstr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestNativeFindReferencesRejectsEmptySymbol 确认绕过 Engine 直接调 Native 也不会
// 把空 symbol 变成全仓扫描。
func TestNativeFindReferencesRejectsEmptySymbol(t *testing.T) {
	e := NewNativeEngine("/nonexistent")
	qr, err := e.FindReferences("", 50)
	if err == nil {
		t.Fatalf("want error for empty symbol, got result %#v", qr)
	}
	if qr != nil {
		t.Fatalf("want nil result on rejected query, got %#v", qr)
	}
	if !errors.Is(err, ErrEmptyQueryTarget) {
		t.Fatalf("want ErrEmptyQueryTarget, got %v", err)
	}
}
