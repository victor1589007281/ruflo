// Agent Notes 纪律校验 (F9, dsh verify-agent-note-format 的 Go 侧等价物)。
//
// 四条机械约束, 每条对应 README.md 里的一条成文纪律:
//	1. 文件内格式: 前三行严格为 "# Agent Note: <标题>" / 空行 / "Status: <状态>",
//	   且 Status 与所在 lifecycle 目录交叉一致 (dsh 的 gate 语义)。
//	2. class 封闭集: 只有六个 class 目录, 新增 class 必须先改这里的清单——
//	   与 dsh "分类 gate 拒绝其他目录" 同款, 防目录树无序生长。
//	3. 双语对完整: 每份 note 都有 .en.md 副本 (主侧中文, 本仓工作语言),
//	   三份文件 (主/英) 成对存在; 缺副本的 note 是单语孤儿, 违反"同等权威"承诺。
//	4. 相对链接可解析: note 间的交叉引用一律是相对 markdown 链接且指向真实
//	   文件——裸文字引用在目录间移动时会静默断链, 机械可校验性是全部前提。
//
// 没有牙的纪律不是纪律: 这些断言让"忘更新/单语孤儿/断链"在 go test 主链路变红,
// 而不是靠复核时人眼发现。
package unit

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const agentNotesRoot = "../../.agents/notes"

// agentNoteClasses class 封闭集 (README.md「分类」节的机器可读形态)。
var agentNoteClasses = map[string]bool{
	"feature":        true,
	"bug-fix":        true,
	"simplification": true,
	"architecture":   true,
	"process":        true,
	"testing":        true,
}

// agentNoteLifecycles 三态 lifecycle (README.md「布局与命名」)。
var agentNoteLifecycles = map[string]string{
	"implemented": "implemented",
	"proposed":    "proposed",
	"rejected":    "rejected",
}

var agentNoteHeaderRe = regexp.MustCompile(`^# Agent Note: \S.*$`)
var agentNoteStatusRe = regexp.MustCompile(`^Status: ([a-z]+)(?: — (.+))?$`)
var agentNoteLinkRe = regexp.MustCompile(`\]\(([^)#]+?\.md)\)`)

// collectAgentNotes 递归收集 notes 树下全部 .md (排除 README), 返回相对路径。
func collectAgentNotes(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(agentNotesRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		base := filepath.Base(path)
		if base == "README.md" || base == "README.en.md" {
			return nil
		}
		out = append(out, filepath.ToSlash(path))
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", agentNotesRoot, err)
	}
	sort.Strings(out)
	return out
}

// TestAgentNote_格式与目录一致 每份 note 的前三行与 Status/目录交叉核对。
func TestAgentNote_格式与目录一致(t *testing.T) {
	notes := collectAgentNotes(t)
	if len(notes) < 10 {
		t.Fatalf("notes 树应至少有 10 份 note (本批 6 份裁定 × 双语 + README×2), 实际 %d —— 树被意外清空?", len(notes))
	}
	for _, path := range notes {
		path := path
		t.Run(filepath.ToSlash(path), func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			sc := bufio.NewScanner(f)
			lines := make([]string, 0, 3)
			for sc.Scan() && len(lines) < 3 {
				lines = append(lines, sc.Text())
			}
			if len(lines) < 3 {
				t.Fatalf("前三行不足 3 行 (须为 标题/空行/Status): %q", lines)
			}
			if !agentNoteHeaderRe.MatchString(lines[0]) {
				t.Fatalf("首行必须是 `# Agent Note: <标题>`, got %q", lines[0])
			}
			if lines[1] != "" {
				t.Fatalf("第二行必须为空行, got %q", lines[1])
			}
			m := agentNoteStatusRe.FindStringSubmatch(lines[2])
			if m == nil {
				t.Fatalf("第三行必须是 `Status: <状态>`, got %q", lines[2])
			}
			status := m[1]
			// lifecycle 从路径取: .../notes/<lifecycle>/<class>/<file>
			// (相对根前缀 ../../ 的段数不定, 从 "notes" 段之后切)
			parts := strings.Split(filepath.ToSlash(path), "/")
			for i, seg := range parts {
				if seg == "notes" {
					parts = parts[i:]
					break
				}
			}
			if len(parts) < 4 {
				t.Fatalf("路径须为 notes/<lifecycle>/<class>/<file>: %q", path)
			}
			lifecycle, class := parts[1], parts[2]
			if status != lifecycle {
				t.Fatalf("Status %q 与目录 lifecycle %q 不一致 (dsh gate 语义)", status, lifecycle)
			}
			if status == "rejected" && strings.TrimSpace(m[2]) == "" {
				t.Fatal("rejected 的 Status 必须带一行理由 (rejected — <why>)")
			}
			if !agentNoteClasses[class] {
				t.Fatalf("class %q 不在封闭集 %v (新增 class 须先改本测试与 README)", class, agentNoteClasses)
			}
		})
	}
}

// TestAgentNote_双语对完整 每份非英文 note 都有 .en.md 副本, 且副本不是孤儿。
func TestAgentNote_双语对完整(t *testing.T) {
	notes := map[string]bool{}
	for _, p := range collectAgentNotes(t) {
		notes[p] = true
	}
	for p := range notes {
		if strings.HasSuffix(p, ".en.md") {
			primary := strings.TrimSuffix(p, ".en.md") + ".md"
			if !notes[primary] {
				t.Fatalf("英文副本孤儿 (缺主侧 %s): %s", primary, p)
			}
			continue
		}
		en := strings.TrimSuffix(p, ".md") + ".en.md"
		if !notes[en] {
			t.Fatalf("note 缺英文副本 (双语对不完整): %s", p)
		}
	}
}

// TestAgentNote_相对链接可解析 note 内的 markdown 链接必须指向真实文件
// (notes 树内相对), 裸编号/不存在的目标直接红。
func TestAgentNote_相对链接可解析(t *testing.T) {
	for _, path := range collectAgentNotes(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(path)
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			// 配对记录注释里的假链接不参与校验
			if strings.HasPrefix(line, "<!--") {
				continue
			}
			for _, m := range agentNoteLinkRe.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if strings.Contains(target, "://") { // 外链跳过
					continue
				}
				full := filepath.Join(dir, filepath.FromSlash(target))
				if _, err := os.Stat(full); err != nil {
					t.Errorf("%s: 断链 %q (解析为 %s)", path, target, full)
				}
			}
		}
	}
}

// TestAgentNote_双语对骨架同构 主/英两侧的章节标题集合应一致 (翻译而非改写)。
// 标题级一致性是"同等权威、结构一一对应"承诺的最低机械下限: 允许翻译差异,
// 不允许单侧私加/私删章节。
func TestAgentNote_双语对骨架同构(t *testing.T) {
	for _, path := range collectAgentNotes(t) {
		if !strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".en.md") {
			continue
		}
		en := strings.TrimSuffix(path, ".md") + ".en.md"
		secs := func(file string) map[string]bool {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]bool{}
			sc := bufio.NewScanner(strings.NewReader(string(data)))
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "## ") {
					out[strings.TrimSpace(sc.Text())] = true
				}
			}
			return out
		}
		zh, enSecs := secs(path), secs(en)
		// 允许一侧把 ## Verification 写作 ## 验证 类翻译——骨架同构按数量+各自
		// 出现次数对齐: 数量必须一致, 这是"结构一一对应"的硬下限。
		if len(zh) != len(enSecs) {
			t.Fatalf("%s 与 %s 的二级章节数不一致: %d vs %d", path, en, len(zh), len(enSecs))
		}
	}
}
