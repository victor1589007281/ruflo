// bashguard.go —— Bash 目标合并闸 (run 级)。
//
// 背景: 调研型 agent 会为同一个 PDF/URL 连开多条 Bash —— 先 curl 下载, 再
// pdftotext 转文本, 再 grep/sed 抽段。每次都是一整轮 LLM 调用, 而真正的工作量
// 只有几行 shell。实测一个 CBAM 任务 25 次 Bash 里有 18 次在重复访问同 4 份 PDF,
// 回合预算就是这么烧掉的。
//
// 这里不做"Bash 最多 N 次"的硬上限 (那会误伤需要多目标并行的任务), 而是按
// **目标**判重: 同一份资源第二次被单独访问时拒绝, 要求把下载/解析/提取合并成
// 一条命令。允许一次合并重试 (第二次访问必须本身就是合并命令), 第三次起拒绝。
//
// 判据落在"这次命令要碰哪些资源"上:
//   - URL (含其 path basename, 用来把 https://x/a.pdf 与本地 a.pdf 关联起来);
//   - 文档类文件路径 (pdf/html/xml/json/csv/txt/md/zip/tar/xls/doc);
//   - 以及这些路径的 basename (同一个文件用相对/绝对路径写法不同也要能认出来)。
//
// 只对含下载/解析/提取动作的命令记账: `ls`、`go test` 这类不会因为重复而拒绝。
package tool

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// BashMergeGuard 记录一次 run 内 Bash 已访问过的资源。
//
// 零值不可用; 用 NewBashMergeGuard 构造。RunIsolated 每次运行建一个, 经
// ToolContext.BashMerge 传进工具 —— 不能挂在 BashTool 上: BashTool 是全局共享的
// 无状态实例, 挂上去会让并发的两个 run 互相把对方的目标算成"已访问"。
type BashMergeGuard struct {
	mu   sync.Mutex
	seen map[string]int
}

// NewBashMergeGuard 构造一个空的 run 级闸门。
func NewBashMergeGuard() *BashMergeGuard {
	return &BashMergeGuard{seen: make(map[string]int)}
}

// bashDocExtRE 只认"文档/归档"类扩展名。脚本 (.sh/.py/.go) 不算: 同一个脚本被
// 反复调用是正常用法, 不是重复抓取。
var bashDocExtRE = regexp.MustCompile(`(?i)[A-Za-z0-9_.~/-]+\.(?:pdf|html?|xml|json|csv|tsv|txt|md|zip|tgz|gz|tar|xlsx?|docx?)`)

// bashURLRE 抓命令里的 http(s) URL。尾部标点单独裁, 因为 shell 里常见
// `curl 'https://x/a.pdf',` 或 `见 https://x/a` 这类写法。
var bashURLRE = regexp.MustCompile(`https?://[^\s'"<>()]+`)

// bashTarget 一个待判重的资源。key 用于跨命令判等, display 用于错误信息。
type bashTarget struct {
	key     string
	display string
}

// CheckAndRecord 判定这条命令是否在重复单独访问已处理过的目标。
//
// 返回非空字符串 = 拒绝执行, 字符串是回给模型的 tool_result (含合并示例)。
// 返回空 = 放行, 并把本命令的目标记入账本。
//
// 拒绝时**不记账**: 模型按提示改成合并命令后重试, 那次应当按"第一次合并访问"
// 处理; 若拒绝也计数, 一次失败的窄命令会把后续正确的合并重试直接顶到上限。
func (g *BashMergeGuard) CheckAndRecord(cwd, command string) string {
	if g == nil {
		return ""
	}
	targets, merged := bashTargets(cwd, command)
	if len(targets) == 0 {
		return ""
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	for _, t := range targets {
		n := g.seen[t.key]
		if n == 0 {
			continue
		}
		// n >= 2: 已经有过一次合并重试, 不再放行。n == 1 只允许合并命令,
		// 不允许把下载与解析拆成两次单独访问。
		if n >= 2 || !merged {
			return fmt.Sprintf(
				"[Bash 合并闸] 目标 %s 已在本 run 的 Bash 中出现过 %d 次, 本次拒绝执行。\n"+
					"不要为同一份 PDF/URL 反复开新的 Bash; 把下载、解析、提取合并成一条命令后重试, 例如:\n"+
					"  curl -sL '<URL>' -o /tmp/x.pdf && pdftotext -layout /tmp/x.pdf - | sed -n '1,220p'\n"+
					"若该目标已经处理完, 直接使用已有结果, 不要再访问。",
				t.display, n)
		}
	}

	for _, t := range targets {
		g.seen[t.key]++
	}
	return ""
}

// bashTargets 从命令里解析目标, 并判断这条命令本身是否已合并。
//
// merged 的判据 (任一成立):
//   - 命令里出现两个及以上不同目标 (一次读多份材料);
//   - 同时含下载与解析/提取动作 (curl && pdftotext);
//   - 同时含解析与提取动作 (pdftotext | grep).
//
// 不含下载/解析/提取动作的命令 (ls / go test / git status) 返回空目标: 它们
// 重复执行没有"重复抓取"的代价, 不该被这条闸门管。
func bashTargets(cwd, command string) ([]bashTarget, bool) {
	ops := bashOps(command)
	if len(ops) == 0 {
		return nil, false
	}

	seenKey := make(map[string]struct{})
	seenResource := make(map[string]struct{})
	var targets []bashTarget
	resources := 0
	add := func(key, display string) {
		if key == "" {
			return
		}
		if _, ok := seenKey[key]; ok {
			return
		}
		seenKey[key] = struct{}{}
		targets = append(targets, bashTarget{key: key, display: display})
	}
	// addResource 数"不同资源", 不是数 key: 一份文件会同时产生 file:<abs>
	// 与 base:<name> 两个 key, 按 key 数会把一条单文件命令误判成"多目标合并".
	addResource := func(key string) {
		if key == "" {
			return
		}
		if _, ok := seenResource[key]; ok {
			return
		}
		seenResource[key] = struct{}{}
		resources++
	}

	// URL: 记完整 URL + path basename。basename 是把 "下载 URL" 与
	// "解析本地同名文件" 关联起来的唯一线索。
	for _, raw := range bashURLRE.FindAllString(command, -1) {
		raw = strings.TrimRight(raw, ".,;:!?)]}'\"")
		if raw == "" {
			continue
		}
		add("url:"+raw, raw)
		addResource("url:" + raw)
		if u, err := url.Parse(raw); err == nil {
			if base := path.Base(u.Path); base != "." && base != "/" && base != "" {
				add("base:"+base, base)
			}
		}
	}

	// 文件路径: 先把 URL 从命令里抹掉, 否则 URL path 会被当成文件路径再记一遍。
	stripped := bashURLRE.ReplaceAllString(command, " ")
	for _, raw := range bashDocExtRE.FindAllString(stripped, -1) {
		raw = strings.Trim(raw, `"'`+"`"+`(),;`)
		if raw == "" || strings.HasPrefix(raw, "-") {
			continue
		}
		abs := raw
		if strings.HasPrefix(abs, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				abs = filepath.Join(home, abs[2:])
			}
		}
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, abs)
		}
		abs = filepath.Clean(abs)
		if abs == "/dev/null" {
			continue
		}
		add("file:"+abs, raw)
		add("base:"+filepath.Base(abs), filepath.Base(abs))
		addResource("file:" + abs)
	}

	merged := resources >= 2
	if (ops["download"] && (ops["parse"] || ops["extract"])) || (ops["parse"] && ops["extract"]) {
		merged = true
	}
	return targets, merged
}

// bashOps 识别命令里出现的动作类别。
//
// 只做词级匹配, 不解析 shell 语法: 这条闸门宁可漏判 (放行) 也不能把
// `grep -r curl .` 这类"参数里带动作名"的命令误判成下载。词级匹配下
// curl 必须是独立 token, 已经能挡住绝大多数误判。
func bashOps(command string) map[string]bool {
	ops := make(map[string]bool)
	lower := strings.ToLower(command)
	has := func(w string) bool { return hasShellWord(lower, w) }

	for _, w := range []string{"curl", "wget", "aria2c"} {
		if has(w) {
			ops["download"] = true
		}
	}
	if strings.Contains(lower, "git clone") {
		ops["download"] = true
	}
	for _, w := range []string{
		"pdftotext", "pdfinfo", "pdfgrep", "pdftoppm", "mutool", "qpdf", "gs",
		"unzip", "tar", "xmllint", "jq", "python", "python3", "node", "ruby", "perl",
	} {
		if has(w) {
			ops["parse"] = true
		}
	}
	for _, w := range []string{
		"grep", "sed", "awk", "head", "tail", "cat", "strings", "wc",
		"cut", "tr", "sort", "uniq", "less", "more",
	} {
		if has(w) {
			ops["extract"] = true
		}
	}
	return ops
}

// hasShellWord 判断 w 是否是 command 里的独立 token。
//
// token 保留字母数字与 `_-./~`: 这样 `curl;`、`curl>`、`curl -sL` 里都能认出
// curl (分隔符是标点/空白), 而 `pdftotext.py`、`https://x/curl` 不会误命中。
// 不解析引号: 闸门宁可漏判也不误杀, 引号内的 `echo "curl"` 命中下载也无害
// (它本来就不带文档目标, 不会进目标表)。
func hasShellWord(command, w string) bool {
	for _, tok := range strings.FieldsFunc(command, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return false
		case r == '_' || r == '-' || r == '.' || r == '~' || r == '/':
			return false
		default:
			return true
		}
	}) {
		if tok == w {
			return true
		}
	}
	return false
}
