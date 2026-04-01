// 安全命令执行器（executor.go）
//
// 设计思路：命令注入的核心来源之一是 Shell 解析与用户可控字符串拼接。本模块通过「逻辑名 → 绝对路径 allowlist」
// 与 exec.CommandContext(name, arg0, args...) 直接 execve 风格调用，完全绕过 Shell，从根上消除引号与元字符解释。
// 参数侧采用黑名单正则拦截已知危险模式（如 rm -rf、重定向、管道、反引号等），相当于对参数再做一层策略过滤
// （与 Shell 转义不同：这里不拼接命令行字符串，而是结构化 argv）。
//
// 整体架构：SafeExecutor 持有 allowlist 与 blockedArgRe；Run 解析逻辑名、Clean 可执行路径、逐参数扫描后构造 *Cmd 与 minimalEnv。
package security

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// SafeExecutor 受限子进程启动器：仅允许预注册的绝对路径二进制，且每个 argv 元素须通过危险模式扫描。
type SafeExecutor struct {
	allowlist    map[string]string // 逻辑名（如 "git"）到文件系统上已解析的可执行文件绝对路径
	blockedArgRe []*regexp.Regexp  // 对「单个参数串」依次尝试匹配，任一命中则拒绝整次 Run
}

// NewSafeExecutor 浅拷贝传入的 allowlist 引用（调用方勿并发修改 map）；并装配默认 blockedArgRe（破坏性命令片段与 Shell 元字符集）。
func NewSafeExecutor(allowed map[string]string) *SafeExecutor {
	if allowed == nil {
		allowed = map[string]string{}
	}
	se := &SafeExecutor{
		allowlist: allowed,
		blockedArgRe: []*regexp.Regexp{
			regexp.MustCompile(`(?i)(rm\s+-rf\b|mkfs\b|dd\s+if=)`),
			regexp.MustCompile(`[;&|$\x60]`),
		},
	}
	return se
}

// RegisterAllow 运行时动态注册或覆盖 allowlist 项；absPath 应在 Run 时由 filepath.Clean 再次规范化。
func (s *SafeExecutor) RegisterAllow(name, absPath string) {
	if s.allowlist == nil {
		s.allowlist = make(map[string]string)
	}
	s.allowlist[name] = absPath
}

// Run 执行流程：① nil 检查；② 逻辑名查表；③ Clean 可执行路径；④ 对每个 args[i] 调用 argBlocked；
// ⑤ exec.CommandContext(ctx, exe, args...) + minimalEnv + Output()。stderr 不合并进返回切片（与标准库 Output 行为一致）。
func (s *SafeExecutor) Run(ctx context.Context, name string, args []string) ([]byte, error) {
	if s == nil {
		return nil, errors.New("security: nil executor")
	}
	exe, ok := s.allowlist[name]
	if !ok {
		return nil, fmt.Errorf("security: command not allowlisted: %s", name)
	}
	exe = filepath.Clean(exe)
	for _, a := range args {
		if s.argBlocked(a) {
			return nil, fmt.Errorf("security: blocked argument pattern")
		}
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = minimalEnv()
	return cmd.Output()
}

// argBlocked 对单参数串顺序匹配 blockedArgRe，短路返回：O(规则数 × 匹配成本)，规则数通常很小。
func (s *SafeExecutor) argBlocked(a string) bool {
	for _, re := range s.blockedArgRe {
		if re.MatchString(a) {
			return true
		}
	}
	return false
}

// minimalEnv 构造极简环境：PATH 限制为 /usr/bin、/bin、/usr/local/bin；LANG 固定 UTF-8。
// 目的：削弱通过 LD_PRELOAD、恶意 PATH 等向量间接影响子进程行为（仍依赖 OS 对子进程环境的隔离）。
func minimalEnv() []string {
	return []string{
		"PATH=" + strings.Join([]string{"/usr/bin", "/bin", "/usr/local/bin"}, string(filepath.ListSeparator)),
		"LANG=C.UTF-8",
	}
}
