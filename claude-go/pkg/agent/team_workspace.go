package agent

// team_workspace.go —— 进程内团队的**独占工作区**（design/02 §1.2 单机假设清单里
// 「团队 cwd 仍进程级共享一份」那条）。
//
// ---------------------------------------------------------------------------
// 缺陷长什么样
// ---------------------------------------------------------------------------
//
// `CreateTeam` 给每个团队都写同一个 `Cwd: ptm.cwd`（teams.go 那一行）。于是**两个并发
// 产码团队写的是同一个目录**：
//
//	团队 A 的 coder 写 main.go → 团队 B 的 coder 也写 main.go → 互相覆盖;
//	编译门禁 (runCompileGate) 在 team.Cwd 上跑 `go build`, 看到的是两个团队混在一起的树,
//	于是 A 的门禁可能因为 B 写坏的文件而失败 —— 而报告会说是 A 的代码有问题。
//
// git 档（`pkg/worker/workspace.go`）**已经**按团队隔离（分支模板 `claude-go/ws/{team}`，
// worker 侧各自 clone/worktree），所以这条只剩**进程内直跑**这一档。那个文件的注释里
// 也点名了这处上游边界（"团队只有一个进程级 Cwd"）。
//
// ---------------------------------------------------------------------------
// 为什么默认关
// ---------------------------------------------------------------------------
//
// 打开后产物落点从 `<cwd>/…` 变成 `<cwd>/<团队名>/…`，而**下游平台按约定路径采产物**
// （本仓的媒锻/织叙等平台会扫团队工作目录）。默认打开等于在不通知下游的情况下改了
// 交付物的位置 —— 那不是"修 bug"，那是行为变更。
//
// 所以：默认逐字节不变（`Cwd` 仍是 `ptm.cwd`）；`CLAUDE_GO_TEAM_WORKSPACE=1` 时每个团队
// 独占 `<cwd>/<安全化团队名>`。并发产码的部署应当开它，单团队部署无需。
//
// ---------------------------------------------------------------------------
// 为什么不用更直觉的两种做法
// ---------------------------------------------------------------------------
//
//  1. **不用 `os.MkdirTemp` 给每次运行一个新目录**：同一团队的多次 run（含精修、图层
//     重试）必须**接力同一份代码**，临时目录会让重试从空目录开始跑编译门禁 —— 与
//     git 档"按团队而不是按 run 分支"是同一条理由（见 workspace.go 的
//     `DefaultGitBranchTemplate` 注释）。
//  2. **不在 `runCompileGate` 里临时切目录**：那只修了编译门禁一处，而写文件的是
//     `MaterializeCode`、读文件的是契约门禁与产物采集，各自都用 `team.Cwd`。
//     隔离必须发生在**Cwd 被赋值的那一处**，否则就是"修了一半"。

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropic/claude-go/pkg/logging"
)

// teamWorkspaceEnabled 团队独占工作区开关（默认关，理由见文件头）。
func teamWorkspaceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_GO_TEAM_WORKSPACE"))) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

// safeTeamDirName 把团队名压成一个安全的单层目录名。
//
// 必须做这一步: 团队名来自用户输入（飞书消息 / HTTP 载荷），未净化就拼进路径等于把
// `../../etc` 这类值直接交给 filepath.Join —— 而 Join 会**如实**上溯父目录。
// 只保留字母数字与 `-_.`，其余一律换成 `-`；`.`/`..` 这类纯点名字整体拒掉。
func safeTeamDirName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), ".-")
	if s == "" {
		return ""
	}
	// 兜一层: 全点名字（"." / ".."）经上面的 Trim 后已为空, 这里防的是 "..foo" 之类
	// 仍含前导点的残留形态被当成隐藏目录。
	return strings.TrimLeft(s, ".")
}

// teamWorkspaceDir 计算团队的工作目录。
//
// 返回值即 `ProductionTeam.Cwd`：
//   - 开关关 → 原样返回进程 cwd（逐字节等价于改造前）
//   - 开关开 → `<进程 cwd>/<安全化团队名>`，并**当场建目录**
//
// 建不出目录时**退回进程 cwd 而不是报错**: 这条路径在 CreateTeam 里，为一个工作区
// 隔离的增强让"建团队"整个失败是不成比例的（交付侧 fail-open）。但会留一行日志 ——
// 静默退回会让人以为隔离生效了，那比不隔离更危险。
func teamWorkspaceDir(procCwd, teamName string) string {
	if !teamWorkspaceEnabled() || procCwd == "" {
		return procCwd
	}
	dir := safeTeamDirName(teamName)
	if dir == "" {
		return procCwd
	}
	full := filepath.Join(procCwd, dir)
	// 净化后仍必须落在 procCwd 之下 —— 这是**结构性**检查, 不依赖上面的字符过滤是否
	// 想全了所有形态。
	if rel, err := filepath.Rel(procCwd, full); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return procCwd
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		logging.For("teams").Warn("团队独占工作区建目录失败, 退回进程 cwd",
			"team", teamName, "dir", full, "err", err)
		return procCwd
	}
	return full
}
