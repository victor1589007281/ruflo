package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bigContent(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString("2026-09-13 10:00:00 INFO something happened in the pipeline\n")
	}
	return b.String()
}

func TestCompactSpillsOversizeResultWithReadableCoordinate(t *testing.T) {
	dir := t.TempDir()
	content := bigContent(defaultMaxToolResultChars + 5000)

	out := compactToolResultContent("Shell", nil, content, &ToolContext{Cwd: dir})

	if !strings.Contains(out, "[tool_result compacted]") {
		t.Fatalf("超预算结果应被压缩, 得:\n%s", out[:200])
	}
	// 关键: 预览必须给出**可执行**的续读坐标, 而不是只丢一个路径
	if !strings.Contains(out, "read_hint:") || !strings.Contains(out, "offset=1") {
		t.Error("预览应给出 Read 的续读坐标 (read_hint / offset)")
	}
	if !strings.Contains(out, "不要重跑命令") {
		t.Error("预览应明确告知不必重跑命令")
	}

	// 落盘的文件必须真的是完整正文 (不是又一份预览)
	var artifactPath string
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "full_artifact: "); ok {
			artifactPath = strings.TrimSpace(v)
		}
	}
	if artifactPath == "" {
		t.Fatalf("未给出 full_artifact 路径:\n%s", out)
	}
	got, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("artifact 读不到: %v", err)
	}
	if string(got) != content {
		t.Errorf("artifact 内容与原文不一致 (len %d vs %d)", len(got), len(content))
	}
	if !strings.HasPrefix(artifactPath, filepath.Join(dir, ".claude-go", "artifacts", "tool-results")) {
		t.Errorf("artifact 应落在约定的 tool-results 目录, 得 %s", artifactPath)
	}
}

func TestCompactKeepsSmallResultInline(t *testing.T) {
	in := "很小的一段输出"
	if out := compactToolResultContent("Shell", nil, in, &ToolContext{Cwd: t.TempDir()}); out != in {
		t.Errorf("未超预算不应改动, 得 %q", out)
	}
}

// 自指保护: 读 artifact 本身不能再被落成新 artifact。
//
// 没有这道闸, 「Read 大文件 → 落盘 → 读落盘文件 → 又超预算 → 再落盘」会无限递归,
// 每一轮只把上一轮的预览再预览一遍, 正文永远进不了上下文。
func TestCompactDoesNotRespillArtifactReads(t *testing.T) {
	dir := t.TempDir()
	content := bigContent(defaultMaxToolResultChars + 5000)

	// 先制造一份 artifact
	out := compactToolResultContent("Shell", nil, content, &ToolContext{Cwd: dir})
	artifactPath := ""
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "full_artifact: "); ok {
			artifactPath = strings.TrimSpace(v)
		}
	}
	if artifactPath == "" {
		t.Fatal("前置条件失败: 没有 artifact")
	}

	// 模型去 Read 这个 artifact, 读回来的内容同样超预算 —— 不应再落盘
	readInput, _ := json.Marshal(map[string]string{"path": artifactPath})
	got := compactToolResultContent("Read", readInput, content, &ToolContext{Cwd: dir})
	if got != content {
		t.Errorf("读 artifact 不应再被压缩 (会出现逐层预览的递归), 得:\n%s", got[:200])
	}

	// 相对路径同样要认出来
	rel, err := filepath.Rel(dir, artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	relInput, _ := json.Marshal(map[string]string{"path": rel})
	if got := compactToolResultContent("Read", relInput, content, &ToolContext{Cwd: dir}); got != content {
		t.Error("相对路径形式的 artifact 读取同样不应被再压缩")
	}

	// 但读一个普通大文件仍应正常落盘
	normalInput, _ := json.Marshal(map[string]string{"path": filepath.Join(dir, "huge.log")})
	if got := compactToolResultContent("Read", normalInput, content, &ToolContext{Cwd: dir}); !strings.Contains(got, "[tool_result compacted]") {
		t.Error("普通大文件的读取不应被这道闸误伤")
	}
}

// 内容寻址: 同一份正文反复产生只留一个文件, 且路径稳定。
func TestArtifactPathIsContentAddressed(t *testing.T) {
	dir := t.TempDir()
	content := bigContent(defaultMaxToolResultChars + 3000)
	tctx := &ToolContext{Cwd: dir}

	a := writeToolResultArtifact("Shell", content, tctx)
	b := writeToolResultArtifact("Shell", content, tctx)
	if a == "" || a != b {
		t.Errorf("相同内容应复用同一路径: %q vs %q", a, b)
	}
	if c := writeToolResultArtifact("Shell", content+"x", tctx); c == a {
		t.Error("不同内容不应共用一个路径")
	}
	entries, err := os.ReadDir(filepath.Join(dir, ".claude-go", "artifacts", "tool-results"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("同内容应去重, 目录里应只有 2 个文件, 实得 %d", len(entries))
	}
}

// 落盘不可用时必须显式说明拿不到全文, 不能让模型以为还能读。
func TestCompactWithoutCwdSaysArtifactUnavailable(t *testing.T) {
	out := compactToolResultContent("Shell", nil, bigContent(defaultMaxToolResultChars+1000), &ToolContext{})
	if !strings.Contains(out, "full_artifact: (落盘失败") {
		t.Errorf("无 cwd 时应明说落盘失败, 得:\n%s", out[:200])
	}
}
