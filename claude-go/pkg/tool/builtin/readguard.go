package builtin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadRootEnv 把 Read/Glob/Grep 限制在一个根目录内。
//
// 为什么需要它: 只读工具集里 Read 接受绝对路径。给 commit-scribe 这类
// "只能看工作区副本"的 agent 只去掉 Bash/Write 是不够的 —— 它仍然可以
// Read("/home/me/repo/.env") 并把内容带进模型上下文。工作区隔离若只在
// 后端"不把敏感文件复制进去"这一层做, 那就不是隔离, 只是没主动递过去。
//
// 约定: 环境变量为空 = 不做限制 (默认行为, 与改造前逐字节一致); 非空 =
// 该进程里所有文件读取都必须落在这个根内。GitInspect 是另一个工具, 它经
// scope 文件按授权路径读仓库, 不经过这里。
const ReadRootEnv = "CLAUDE_GO_READ_ROOT"

// guardReadPath 在设置了 ReadRootEnv 时校验 p 是否在根内。
//
// 同时校验词法路径与真实路径: 工作区里可能存在指向外部的符号链接, 只看
// filepath.Clean 会把它当成"在根内"。目标不存在时逐级向上找已存在祖先的
// 真实路径, 这样 `/root/../etc/passwd` 与"父目录是指向外部的软链接"两类
// 都会被拦下, 而普通的"文件还没创建"仍返回 not-found 而不是越界。
func guardReadPath(p string) error {
	root := strings.TrimSpace(os.Getenv(ReadRootEnv))
	if root == "" {
		return nil
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("读取根无效: %v", err)
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return fmt.Errorf("读取根不可用: %v", err)
	}
	pAbs, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("路径无效: %v", err)
	}
	if !withinDir(rootAbs, pAbs) {
		return fmt.Errorf("路径不在允许的读取根内: %s", pAbs)
	}

	// 目标存在: 解析真实路径后必须仍在根内。
	if real, err := filepath.EvalSymlinks(pAbs); err == nil {
		if !withinDir(rootReal, real) {
			return fmt.Errorf("路径经符号链接逃出读取根: %s", pAbs)
		}
		return nil
	}

	// 目标不存在: 找到最近的已存在祖先, 校验它的真实路径。
	ancestor := pAbs
	for {
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
		ancestor = parent
		real, err := filepath.EvalSymlinks(ancestor)
		if err != nil {
			continue
		}
		if !withinDir(rootReal, real) {
			return fmt.Errorf("路径的父目录经符号链接逃出读取根: %s", pAbs)
		}
		return nil
	}
	return fmt.Errorf("无法校验读取路径: %s", pAbs)
}

// withinDir 判断 p 是否等于 root 或位于 root 之下。
func withinDir(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
