package coder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Verifier 客观终态判定。退出码 0 = 通过。
//
// 抽成接口的理由与 Agent 相同 (可单测), 但还有一个更硬的理由:
// **验证必须由驱动侧执行**, 而不是读 agent 的自述。实测过一次静默失败 ——
// 进程 exit=0、is_error=false、模型说了一段像样的话, 而工作区一个字节没动。
// 只要判据取自 agent 的自我报告, 那种失败就永远是绿的。
type Verifier interface {
	Verify(ctx context.Context, dir string) (passed bool, output string, err error)
}

// CommandVerifier 在工作目录里跑一条命令, 用退出码判定。
type CommandVerifier struct {
	Cmd     []string
	Timeout time.Duration
}

// Verify 见 Verifier。
//
// 注意这里**不**把"命令本身起不来"当成通过: 起不来是 err, 不是 passed=false ——
// 两者在下游会被区别对待 (err 会让任务停下来报警, passed=false 会让它继续迭代)。
// 把它们混成一个 false, 会让"环境坏了"伪装成"代码还没写好", 白烧完整轮额度。
func (v CommandVerifier) Verify(ctx context.Context, dir string) (bool, string, error) {
	if len(v.Cmd) == 0 {
		return false, "", fmt.Errorf("验证命令为空")
	}
	cctx := ctx
	if v.Timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, v.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(cctx, v.Cmd[0], v.Cmd[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()

	if err == nil {
		return true, truncate(out, 8000), nil
	}

	// 顺序很重要: **先**看 context, 再看 ExitError。
	//
	// exec.CommandContext 在超时/取消时是**杀掉**子进程, 于是 cmd.Run() 返回的是
	// 一个普通的 *exec.ExitError (signal: killed) —— 与"程序自己退出码 3"逐字节同形。
	// 先判 ExitError 会把超时归成"代码还没写好", 于是运行时会拿着一条永远跑不完的
	// 验证命令, 每轮都烧掉一整轮额度, 最后报"没通过"而真凶是超时 (实测踩到)。
	if cerr := cctx.Err(); cerr != nil {
		if cerr == context.DeadlineExceeded {
			return false, truncate(out, 8000), fmt.Errorf(
				"验证命令超时 (%s) —— 这是环境问题, 不是代码问题", v.Timeout)
		}
		return false, truncate(out, 8000), fmt.Errorf("验证被中止: %w", cerr)
	}

	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		// 非零退出 = 没通过。这是**正常**路径 (任务尚未完成), 不是错误。
		return false, truncate(out, 8000), nil
	}
	// 命令不存在 / 权限 —— 环境问题, 与"代码写没写好"无关。
	return false, truncate(out, 8000), fmt.Errorf("验证命令无法执行: %w", err)
}

// skipDirs 指纹计算时跳过的目录。
//
// 判据是"这个目录会不会在 agent 什么都没干的情况下也变"。会变的必须跳过,
// 否则"工作区动没动"恒为真, 空转止损永不触发 —— 而那正是它存在的理由。
//
//   - `.coder`      本运行时每轮都写 state.json / prompt.md / logs。
//   - `.claude-go`  **agent 进程自己**每轮都写 sandboxes/<id>/stdout.log。
//     实测发现: 一次只创建了个 hello.txt 的 run, 也在工作区里留下了
//     .claude-go/sandboxes/sbx_.../{stdout,stderr}.log。漏掉它, 止损就是摆设。
//   - `.git`        内部状态与"代码变没变"无关。
//   - 依赖目录      内容与任务无关, 且动辄几万文件。
var skipDirs = map[string]bool{
	RuntimeDir:     true,
	".claude-go":   true,
	".git":         true,
	"node_modules": true,
	"vendor":       true,
}

// Fingerprint 计算工作区内容的指纹 (只看会"变成产物"的那些文件)。
//
// 用途是判"这一轮 agent 到底动没动手"。它与验证结果一起构成空转判据。
func Fingerprint(root string) (string, error) {
	h := sha256.New()
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	// 排序: 遍历顺序不稳定会让同一份内容算出不同指纹 (报出一堆假变化)。
	sort.Strings(paths)
	for _, rel := range paths {
		b, rerr := os.ReadFile(filepath.Join(root, rel))
		if rerr != nil {
			continue
		}
		sum := sha256.Sum256(b)
		fmt.Fprintf(h, "%s\x00%d\x00%s\n", rel, len(b), hex.EncodeToString(sum[:]))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ExportClean 把工作区复制到一个干净目录, **排除运行时目录**。
//
// 用途是终态验收: 在导出副本里重新跑一次构建与验证。这么做的理由有两条,
// 都不是洁癖:
//
//  1. 证明结论**不是自报的** —— 副本里没有 agent 的会话、没有它的日志,
//     只有它实际留下的代码。
//  2. 把"能不能跑起来"与"运行时目录在不在"解耦。有些任务的验证会无意依赖
//     .coder 里的东西 (相对路径、临时产物), 在原目录里跑是看不出来的。
func ExportClean(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // 符号链接等一律不拷, 避免把工作区外的路径带出来
		}
		in, oerr := os.Open(p)
		if oerr != nil {
			return oerr
		}
		defer in.Close()
		out, cerr := os.Create(filepath.Join(dst, rel))
		if cerr != nil {
			return cerr
		}
		defer out.Close()
		_, werr := io.Copy(out, in)
		return werr
	})
}

// normalizeOutput 把验证输出归一化, 用于判"两轮是不是同一份失败"。
//
// 直接比字符串会漏判: 测试输出里带耗时 (`ok smoke 0.002s` / `--- FAIL: ... (0.00s)`),
// 每轮都不同。不归一化的话"输出没变"永远为假, 空转止损又失效了。
func normalizeOutput(s string) string {
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		lines = append(lines, strings.TrimSpace(stripDurations(ln)))
	}
	return strings.Join(lines, "\n")
}

// stripDurations 抹掉耗时片段, 形如 `(0.00s)` / ` 0.123s` / "\t0.002s"。
//
// 分隔符必须同时认空格和**制表符**: `go test` 的汇总行是
// `FAIL\tsmoke\t0.002s` —— 只认空格的实现会留下 tab 后面那一段, 于是"同一个失败"
// 因为耗时不同被当成两份不同的输出, 空转止损失效 (实测踩到)。
//
// 只抹"整段由数字/点/单位字符组成且以单位结尾"的片段 (即要求有单位后缀),
// 不抹裸数字 —— 失败信息里的裸数字往往是有意义的比较值, 抹了会把真的不同的
// 失败判成同一份, 那是往"误止损"的方向错。
func stripDurations(s string) string {
	var sb strings.Builder
	i := 0
	for i < len(s) {
		if isDurationSep(s[i]) {
			open := s[i] == '('
			j := i + 1
			for j < len(s) && isDurationChar(s[j]) {
				j++
			}
			if j > i+1 && isDurationToken(s[i+1:j]) {
				i = j
				if open && i < len(s) && s[i] == ')' {
					i++
				}
				continue
			}
		}
		sb.WriteByte(s[i])
		i++
	}
	return sb.String()
}

func isDurationSep(c byte) bool { return c == ' ' || c == '\t' || c == '(' }

func isDurationChar(c byte) bool {
	return c >= '0' && c <= '9' || c == '.' ||
		c == 'n' || c == 'u' || c == 'm' || c == 's' ||
		c == 0xC2 || c == 0xB5 // µ 的 UTF-8 两字节
}

// isDurationToken 判一个片段是不是耗时: 至少一个数字, 且以时间单位结尾。
func isDurationToken(s string) bool {
	if s == "" {
		return false
	}
	digits := 0
	for i := 0; i < len(s); i++ {
		if !isDurationChar(s[i]) {
			return false
		}
		if s[i] >= '0' && s[i] <= '9' {
			digits++
		}
	}
	if digits == 0 {
		return false
	}
	last := s[len(s)-1]
	return last == 's' || last == 'm'
}
