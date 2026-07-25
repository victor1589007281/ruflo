package worker

// gitws.go —— cwd 的 git 档位: worker 本地目录工作 + 产物 commit/push 回约定 git 位置,
// 下一阶段的 worker (可能在另一台机器上) 从那里拉。
//
// # 为什么这一档最有价值
//
// pvc 档要求控制面与所有 worker 挂同一个 ReadWriteMany 卷 —— 很多集群没有 RWX
// 存储类, 跨机房更不可能。git 档只要求一个双方都能访问的 git 位置 (bare 仓/内网
// GitLab/一个 NFS 上的 --bare 目录都行), 而且**天然并发隔离**: 每个 worker 有自己的
// 工作树, 只在 push 那一刻汇合。
//
// # 一次 git 档阶段的完整时序
//
//	worker 侧 prepare:  ensureRepo → 抢救脏树 → fetch → checkout <branch>
//	                    ↑ 任何一步失败 = 阶段失败。绝不在"没有上游代码的目录"里开跑。
//	执行体真跑 (工具的 cwd 就是这个目录, 见 Options.Workspace 的注释)
//	worker 侧 finish:   git add -A → 大小闸 → commit → push (被拒则 fetch+rebase 重试)
//	控制面侧 sync:      fetch → ff-only 合入 team.Cwd → 校验 worker 报的提交真在里面
//	                    ↑ 这一步是"编译门禁能看到远程产码"的唯一保证 (门禁跑在
//	                      <team.Cwd>/go.mod 上, 不同步过来它就什么都看不见)
//
// # 三条不许含糊的语义 (逐条都有测试)
//
//	冲突      **绝不 force push, 绝不 --allow-empty, 绝不静默丢弃**。push 被拒 =
//	          有别的分片先推了 → fetch + rebase; rebase 冲突 → 阶段**失败**并列出
//	          冲突文件 (本地提交仍在, 什么都没丢)。静默覆盖会让另一个分片的代码
//	          凭空消失, 静默丢弃会让本阶段的产出凭空消失 —— 两者都是"阶段成功但
//	          代码不见了"。
//	空提交    没有文件改动就**不提交**(评审/分析类阶段本来就不产文件), 据实回报
//	          changed=false。不许用 --allow-empty 制造"看起来干了活"的提交。
//	大文件    单文件超过 MaxFileBytes ⇒ 阶段**失败**并列出路径, 不是悄悄跳过。
//	          悄悄跳过 = 下一阶段找不到那个文件, 正是本包要挡的故障。二进制产物
//	          (png/pptx/wav) 本身允许提交, 只受大小闸约束 —— git LFS 未接线,
//	          大对象进了历史就永久留在那里。
//	          构建产物请靠仓库自己的 .gitignore 排除 (git add -A 尊重它) —— 那是
//	          **显式声明**的排除, 与静默丢弃不同。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// git 操作超时。网络类操作 (fetch/push) 给足时间, 本地操作短。
const (
	gitLocalTimeout   = 60 * time.Second
	gitNetworkTimeout = 5 * time.Minute
	// gitPushRetries push 被拒后的 fetch+rebase 重试次数。
	// 有限重试: 一个持续被别人抢先的分片应该失败, 而不是无限重试把租约耗光。
	gitPushRetries = 3
)

// gitCmd 一次 git 调用。
//
// 三处刻意的环境处理:
//   - GIT_TERMINAL_PROMPT=0: 凭据缺失时**立刻失败**而不是挂在终端提示上等到超时
//     (容器里没有终端, 挂住的表现是"阶段莫名超时", 归因极难)。
//   - -c user.name/-c user.email: 容器镜像里没有全局 git 配置, 不带它 commit 会
//     直接失败 ("Please tell me who you are")。
//   - -c core.hooksPath=<不存在>: 被检出的仓库自带的 hooks 不该在 worker 上执行。
func gitCmd(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := []string{
		"-c", "user.name=claude-go",
		"-c", "user.email=claude-go@localhost",
		"-c", "core.hooksPath=/nonexistent-claude-go-hooks",
		"-c", "core.quotepath=false",
		"-c", "advice.detachedHead=false",
		// 宿主/镜像里的全局配置不该左右 worker 的提交: 签名要私钥 (没有就直接失败),
		// 自动 gc 会在阶段中途占住仓库锁。
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
		"-c", "gc.auto=0",
	}
	cmd := exec.CommandContext(cctx, "git", append(base, args...)...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return s, fmt.Errorf("git %s 超时 (%s): %s", strings.Join(args, " "), timeout, s)
		}
		return s, fmt.Errorf("git %s 失败: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

func gitOK(ctx context.Context, dir string, args ...string) bool {
	_, err := gitCmd(ctx, dir, gitLocalTimeout, args...)
	return err == nil
}

// ─────────────────────────────────────────────────────────────────────────────
// worker 侧
// ─────────────────────────────────────────────────────────────────────────────

// prepareGit git 档的工作区准备。任何一步失败都必须让阶段失败 —— 在一个没有上游
// 代码的目录里"成功"跑完是本包首要要挡的故障。
func (w *Worker) prepareGit(st StageTask) (*workspaceSession, error) {
	g := st.Git
	if g == nil || strings.TrimSpace(g.Remote) == "" || strings.TrimSpace(g.Branch) == "" {
		return nil, fmt.Errorf("git 档任务缺少 remote/branch 声明; 拒绝执行 (worker %s)", w.opt.Name)
	}
	dir := strings.TrimSpace(w.opt.Workspace)
	if dir == "" {
		return nil, fmt.Errorf("worker %s 未声明工作区 (--workspace), 无法提供 git 档工作区; 拒绝执行", w.opt.Name)
	}
	// 串行化: git 档下同一个工作目录不能被两个任务同时用 (会互相 checkout/reset)。
	// New() 已强制 git 档 MaxParallel=1, 这把锁是纵深防御。
	w.gitMu.Lock()
	unlock := func() { w.gitMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	release := func() { cancel(); unlock() }

	if err := gitEnsureRepo(ctx, dir, g.Remote, false); err != nil {
		release()
		return nil, err
	}
	if err := gitWriteLocalExcludes(dir); err != nil {
		w.opt.Logf("[worker] git 档: 写 .git/info/exclude 失败 (运行时垃圾可能被提交): %v", err)
	}
	if note, err := gitSalvageDirty(ctx, dir); err != nil {
		release()
		return nil, err
	} else if note != "" {
		w.opt.Logf("[worker] git 档: %s", note)
	}
	if _, err := gitCmd(ctx, dir, gitNetworkTimeout, "fetch", "--prune", "origin"); err != nil {
		release()
		// 拉取失败必须真失败: 否则就在一个只有上一次残留 (或空) 的目录里开跑。
		return nil, fmt.Errorf("git 档拉取失败 (remote=%s): %w; 拒绝在没有上游代码的目录执行", g.Remote, err)
	}
	coNote, err := gitCheckoutWorkBranch(ctx, dir, g)
	if err != nil {
		release()
		return nil, err
	}
	w.opt.Logf("[worker] git 档: %s (工作区 %s)", coNote, dir)
	head, _ := gitCmd(ctx, dir, gitLocalTimeout, "rev-parse", "--verify", "-q", "HEAD")

	sess := &workspaceSession{mode: WorkspaceModeGit, dir: dir}
	sess.fn = func(ok bool) (WorkspaceReport, error) {
		defer release()
		rep := WorkspaceReport{Mode: WorkspaceModeGit, Dir: dir}
		if !ok {
			// 阶段失败: 不提交、不推送。脏树留在原地, 下一次 prepare 会把它抢救成
			// patch 再清干净 (不是静默丢弃)。
			rep.Note = "阶段失败, 未提交"
			return rep, nil
		}
		return gitPublish(ctx, dir, st, g, strings.TrimSpace(head))
	}
	return sess, nil
}

// gitEnsureRepo 确保 dir 是一个指向 remote 的 git 仓库。
//
// allowInitNonEmpty: 控制面侧的 team.Cwd 天然是个非空目录 (bot 的工作目录), 允许在
// 其上 git init (非破坏性); worker 侧则**拒绝**在非空非仓目录上初始化 —— 那意味着
// 有人把 worker 的 --workspace 指到了一个已经有内容的目录, 后续 checkout 会与既有
// 文件撞车, 与其半路失败不如一开始就说清。
func gitEnsureRepo(ctx context.Context, dir, remote string, allowInitNonEmpty bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("git 档工作区 %s 无法创建: %w", dir, err)
	}
	isRepo := gitOK(ctx, dir, "rev-parse", "--git-dir")
	if !isRepo {
		if !allowInitNonEmpty {
			ents, err := os.ReadDir(dir)
			if err != nil {
				return fmt.Errorf("git 档工作区 %s 不可读: %w", dir, err)
			}
			if len(ents) > 0 {
				return fmt.Errorf("git 档工作区 %s 非空且不是 git 仓库; 拒绝在其上初始化 "+
					"(会与既有 %d 个条目撞车)。请给 worker 一个空目录或一个已 clone 的目录", dir, len(ents))
			}
		}
		if _, err := gitCmd(ctx, dir, gitLocalTimeout, "init", "-q"); err != nil {
			return fmt.Errorf("git 档初始化 %s 失败: %w", dir, err)
		}
	}
	cur, err := gitCmd(ctx, dir, gitLocalTimeout, "remote", "get-url", "origin")
	if err != nil {
		if _, aerr := gitCmd(ctx, dir, gitLocalTimeout, "remote", "add", "origin", remote); aerr != nil {
			return fmt.Errorf("git 档配置 remote 失败: %w", aerr)
		}
		return nil
	}
	if !sameGitRemote(cur, remote) {
		// 不静默改指向: 一个已经指着别的仓的工作区被悄悄重定向, 会把两个项目的
		// 代码搅到一起。
		return fmt.Errorf("git 档工作区 %s 的 origin 是 %q, 与任务要求的 %q 不符; 拒绝静默改指向",
			dir, cur, remote)
	}
	return nil
}

func sameGitRemote(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.TrimSuffix(s, "/")
		s = strings.TrimSuffix(s, ".git")
		return s
	}
	return norm(a) == norm(b)
}

// gitSalvageDirty 处理上一次阶段留下的脏树。
//
// 脏树的来源是"上一个阶段失败了但已经写了文件"。直接 reset --hard 会静默丢弃那些
// 文件; 直接失败又会让 worker 永久卡住。折中: 先把改动导成 patch 存进
// .git/claude-go-salvage/ (git clean 不会碰 .git), 再清干净。什么都没丢, 也不卡住。
func gitSalvageDirty(ctx context.Context, dir string) (string, error) {
	status, err := gitCmd(ctx, dir, gitLocalTimeout, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("git 档读取工作区状态失败: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return "", nil
	}
	hasHEAD := gitOK(ctx, dir, "rev-parse", "--verify", "-q", "HEAD")
	if _, err := gitCmd(ctx, dir, gitLocalTimeout, "add", "-A"); err != nil {
		return "", fmt.Errorf("git 档暂存残留改动失败: %w", err)
	}
	patch, perr := gitCmd(ctx, dir, gitLocalTimeout, "diff", "--cached", "--binary")
	saved := ""
	if perr == nil && strings.TrimSpace(patch) != "" {
		sdir := filepath.Join(dir, ".git", "claude-go-salvage")
		if mkErr := os.MkdirAll(sdir, 0o755); mkErr == nil {
			p := filepath.Join(sdir, fmt.Sprintf("dirty-%d.patch", time.Now().UnixMilli()))
			if wErr := os.WriteFile(p, []byte(patch+"\n"), 0o644); wErr == nil {
				saved = p
			}
		}
	}
	if hasHEAD {
		if _, err := gitCmd(ctx, dir, gitLocalTimeout, "reset", "--hard", "-q", "HEAD"); err != nil {
			return "", fmt.Errorf("git 档清理残留改动失败: %w", err)
		}
	} else if _, err := gitCmd(ctx, dir, gitLocalTimeout, "rm", "-r", "-q", "--cached", "."); err != nil {
		return "", fmt.Errorf("git 档清理残留暂存失败 (空仓): %w", err)
	}
	if _, err := gitCmd(ctx, dir, gitLocalTimeout, "clean", "-fdq"); err != nil {
		return "", fmt.Errorf("git 档清理未跟踪文件失败: %w", err)
	}
	if saved != "" {
		return fmt.Sprintf("上一阶段残留的改动已导出到 %s 后清理 (未丢弃)", saved), nil
	}
	return "已清理上一阶段的残留改动", nil
}

// gitCheckoutWorkBranch 检出工作区分支, 返回一句可观测的说明。
//
// 起点选择顺序 (每一步都是"能解析就用, 解析不到再退"):
//
//	① origin/<branch>            —— 本团队上一阶段推的东西, 接力就靠它
//	② BaseRef                    —— 部署方显式指定的基线
//	③ 远端默认分支 (ls-remote --symref) —— 单产品仓的自然基线
//	④ 空树                       —— 新分支, 与远端其它分支无血缘关系
//
// ④ 曾经被写成"远端有引用就拒绝", 真机第一次跑第二个团队就被它挡住了: 一个
// 裸仓上放着别的团队的分支 (claude-go/ws/其它团队), 而 origin/HEAD 在 init+fetch
// 的流程里**从来不会被设置** (它只在 clone 时写)。新建一条分支从空树起步并不会
// 删掉任何东西 (其它分支照旧), 所以正确做法是**起空树 + 大声说明**, 而不是拒绝。
// 真正危险的"从空树覆盖上游"发生在分支已存在却被忽略时 —— 那由 ① 挡住。
func gitCheckoutWorkBranch(ctx context.Context, dir string, g *GitWorkspace) (string, error) {
	br := strings.TrimSpace(g.Branch)
	if gitOK(ctx, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+br) {
		if _, err := gitCmd(ctx, dir, gitLocalTimeout, "checkout", "-q", "-B", br, "origin/"+br); err != nil {
			return "", fmt.Errorf("git 档检出 %s 失败: %w", br, err)
		}
		return "接上 origin/" + br, nil
	}
	for _, cand := range gitBaseCandidates(ctx, dir, g) {
		if cand == "" || !gitOK(ctx, dir, "rev-parse", "--verify", "-q", cand) {
			continue
		}
		if _, err := gitCmd(ctx, dir, gitLocalTimeout, "checkout", "-q", "-B", br, cand); err != nil {
			return "", fmt.Errorf("git 档以 %s 为起点创建 %s 失败: %w", cand, br, err)
		}
		return fmt.Sprintf("新建分支 %s (起点 %s)", br, cand), nil
	}
	// symbolic-ref 在"尚未有任何提交"的仓库上也能把 HEAD 指到目标分支。
	if _, err := gitCmd(ctx, dir, gitLocalTimeout, "symbolic-ref", "HEAD", "refs/heads/"+br); err != nil {
		return "", fmt.Errorf("git 档在空仓上创建分支 %s 失败: %w", br, err)
	}
	if gitOK(ctx, dir, "rev-parse", "--verify", "-q", "HEAD") {
		// HEAD 可解析 = 本地已有同名分支且它有提交 (上一次 push 没成功), 内容就是
		// 我们要接着干的东西, 不动。
		return "接上本地已有分支 " + br + " (上次推送未成功)", nil
	}
	// 清空索引与工作树。同一个 worker 会先后服务不同团队 (每团队一条分支): 上一个
	// 团队的文件此刻是"未跟踪文件"躺在工作树里, 不清掉就会被 git add -A 扫进**这个**
	// 团队的提交 —— 跨团队串味。清掉是安全的: 那些文件已在上一条分支的历史里
	// (推成功了在远端, 没推成功也还在本地那条分支上), 不是丢弃。
	if _, err := gitCmd(ctx, dir, gitLocalTimeout, "reset", "-q"); err != nil {
		return "", fmt.Errorf("git 档清空索引失败: %w", err)
	}
	if _, err := gitCmd(ctx, dir, gitLocalTimeout, "clean", "-fdq"); err != nil {
		return "", fmt.Errorf("git 档清理上一条分支的残留文件失败: %w", err)
	}
	note := fmt.Sprintf("新建分支 %s, 起自空树", br)
	if others, _ := gitCmd(ctx, dir, gitLocalTimeout,
		"for-each-ref", "--format=%(refname:short)", "refs/remotes/origin"); strings.TrimSpace(others) != "" {
		// 不是错误但必须说清: 若本意是从某条产品分支起步, 要显式给 base_ref。
		note += fmt.Sprintf(" —— 远端已有其它分支 [%s] 但解析不到默认起点; "+
			"如需以某条分支为基线请设 --workspace-git-base", strings.Join(splitLines(others), " "))
	}
	return note, nil
}

// gitBaseCandidates 候选起点 (按优先级)。
func gitBaseCandidates(ctx context.Context, dir string, g *GitWorkspace) []string {
	var out []string
	if b := strings.TrimSpace(g.BaseRef); b != "" {
		out = append(out, "refs/remotes/origin/"+b, b)
	}
	// 远端默认分支: init+fetch 的流程里 refs/remotes/origin/HEAD 不会被写 (那是 clone
	// 才做的事), 所以主动问一次远端。
	if out2, err := gitCmd(ctx, dir, gitNetworkTimeout, "ls-remote", "--symref", "origin", "HEAD"); err == nil {
		for _, ln := range splitLines(out2) {
			if strings.HasPrefix(ln, "ref:") {
				f := strings.Fields(ln)
				if len(f) >= 2 {
					out = append(out, "refs/remotes/origin/"+strings.TrimPrefix(f[1], "refs/heads/"))
				}
			}
		}
	}
	out = append(out, "refs/remotes/origin/HEAD")
	return out
}

// gitPublish 把本阶段的产物提交并推回约定位置。
//
// headBefore 是执行**之前**的 HEAD, 用来识别一件真实会发生的事: **执行体自己
// commit 了**。Agent 有 Bash 工具, 产码阶段里它常顺手 `git add && git commit` ——
// 此时 `git add -A` 什么都暂存不到, 若据此判定"无改动、不推送", 它的提交就永远
// 留在 worker 本地, 下一阶段与门禁都看不见 (真机验证第一次就撞上了这一幕)。
func gitPublish(ctx context.Context, dir string, st StageTask, g *GitWorkspace, headBefore string) (WorkspaceReport, error) {
	rep := WorkspaceReport{Mode: WorkspaceModeGit, Dir: dir, Commit: headBefore}
	// 执行体可能切了分支/弄成了游离 HEAD。那时 push HEAD 会把不确定的内容推上去,
	// 必须拒绝而不是猜。
	// 用 symbolic-ref 而不是 rev-parse --abbrev-ref: 前者在"分支还没有任何提交"
	// (首个阶段 + 空裸仓) 时也能给出分支名, 后者会报 ambiguous argument 'HEAD';
	// 而游离 HEAD 时 symbolic-ref 失败 —— 那正是要拦的情况。
	if cur, err := gitCmd(ctx, dir, gitLocalTimeout, "symbolic-ref", "--short", "-q", "HEAD"); err != nil {
		return rep, fmt.Errorf("git 档: 执行体让工作区处于游离 HEAD (%v), 拒绝推送不确定的内容 "+
			"(本地改动仍在 %s, 未丢弃)", err, dir)
	} else if strings.TrimSpace(cur) != strings.TrimSpace(g.Branch) {
		return rep, fmt.Errorf("git 档: 执行体把工作区从 %s 切到了 %q, 拒绝推送不确定的内容 "+
			"(本地改动仍在 %s, 未丢弃)", g.Branch, strings.TrimSpace(cur), dir)
	}
	if _, err := gitCmd(ctx, dir, gitLocalTimeout, "add", "-A"); err != nil {
		return rep, fmt.Errorf("git 档暂存产物失败: %w", err)
	}
	files, err := gitStagedFiles(ctx, dir)
	if err != nil {
		return rep, err
	}
	headNow, _ := gitCmd(ctx, dir, gitLocalTimeout, "rev-parse", "--verify", "-q", "HEAD")
	selfCommitted := strings.TrimSpace(headNow) != "" && strings.TrimSpace(headNow) != headBefore

	if len(files) == 0 && !selfCommitted {
		// 空提交政策: 不造提交。评审/分析类阶段本来就不产文件, 造一个空提交只会
		// 让历史里出现"看起来干了活"的噪音。
		rep.Changed = false
		rep.Note = "本阶段无文件改动, 未提交 (分支保持 " + shortSHA(headBefore) + ")"
		return rep, nil
	}
	rep.Changed = true
	rep.Files = len(files)
	if len(files) > 0 {
		if err := gitCheckFileSizes(dir, files, g.MaxFileBytes); err != nil {
			return rep, err
		}
		rep.Binary = gitCountBinary(ctx, dir)
		msg := fmt.Sprintf("[claude-go] %s: %s\n\nrun: %s\nnode: %s\nworkspace-mode: git\n",
			firstNonEmptyStr(st.Role, "stage"), gitSubject(st), st.RunID, st.NodeID)
		if _, err := gitCmd(ctx, dir, gitLocalTimeout, "commit", "-q", "-m", msg); err != nil {
			return rep, fmt.Errorf("git 档提交失败: %w", err)
		}
	}

	pushed, note, err := gitPushWithRebase(ctx, dir, g.Branch)
	if head, herr := gitCmd(ctx, dir, gitLocalTimeout, "rev-parse", "HEAD"); herr == nil {
		rep.Commit = strings.TrimSpace(head)
	}
	rep.Pushed = pushed
	rep.Note = note
	if selfCommitted && note != "" {
		rep.Note = note + " (含执行体自行创建的提交)"
	}
	if err != nil {
		return rep, err
	}
	return rep, nil
}

// gitLocalExcludes 写进 .git/info/exclude 的 worker 侧运行时垃圾。
//
// 为什么不是"静默丢弃": 这几项**不是 Agent 的产物**, 而是 claude-go 自己在 cwd 下的
// 运行期记账 (沙箱 stdout/stderr、握手文件)。真机验证第一次就把
// .claude-go/sandboxes/*/stdout.log 提交进了仓库。用 .git/info/exclude 而不是改仓库的
// .gitignore: 前者是**本地克隆**的排除, 不动任何被跟踪的文件, 也不会推到远端。
var gitLocalExcludes = []string{".claude-go/", handshakeDir + "/"}

// gitWriteLocalExcludes 幂等地补上本地排除项。失败只记不阻断 (最坏结果是多提交几个
// 日志文件, 不该因此让阶段失败)。
func gitWriteLocalExcludes(dir string) error {
	p := filepath.Join(dir, ".git", "info")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return err
	}
	f := filepath.Join(p, "exclude")
	cur, _ := os.ReadFile(f)
	body := string(cur)
	add := ""
	for _, e := range gitLocalExcludes {
		if !strings.Contains(body, e) {
			add += e + "\n"
		}
	}
	if add == "" {
		return nil
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		add = "\n" + add
	}
	return os.WriteFile(f, []byte(body+"# claude-go worker 运行时垃圾 (非 Agent 产物)\n"+add), 0o644)
}

// gitPushWithRebase push, 被拒则 fetch+rebase 后重试。
//
// 冲突处理的全部要点在这里:
//   - 从不 --force / --force-with-lease: 覆盖别人的提交 = 静默丢代码。
//   - rebase 而不是 merge: 保持线性, 于是控制面侧可以永远用 ff-only 合入 (ff-only
//     失败就是真出事了, 不会被 merge commit 掩盖)。
//   - rebase 冲突 = 阶段失败, 并列出冲突文件。abort 之后本地提交仍在 (可人工接管),
//     远端也没被动过 —— 两边都没丢东西。
func gitPushWithRebase(ctx context.Context, dir, branch string) (bool, string, error) {
	target := "HEAD:refs/heads/" + branch
	var lastOut string
	for attempt := 0; attempt < gitPushRetries; attempt++ {
		out, err := gitCmd(ctx, dir, gitNetworkTimeout, "push", "origin", target)
		if err == nil {
			note := "已推送到 " + branch
			if attempt > 0 {
				note = fmt.Sprintf("与并发分片变基后推送到 %s (第 %d 次尝试)", branch, attempt+1)
			}
			return true, note, nil
		}
		lastOut = out
		if !gitPushRejected(out) {
			// 不是"被别人抢先"而是别的错 (鉴权/网络/远端不存在): 直接失败。
			return false, "", fmt.Errorf("git 档推送失败: %w", err)
		}
		// 被拒 = 有并发分片先推了。拉过来变基, 不覆盖它。
		if _, ferr := gitCmd(ctx, dir, gitNetworkTimeout, "fetch", "origin", branch); ferr != nil {
			return false, "", fmt.Errorf("git 档推送被拒后拉取失败: %w", ferr)
		}
		if rout, rerr := gitCmd(ctx, dir, gitLocalTimeout, "rebase", "FETCH_HEAD"); rerr != nil {
			conflicts, _ := gitCmd(ctx, dir, gitLocalTimeout, "diff", "--name-only", "--diff-filter=U")
			_, _ = gitCmd(ctx, dir, gitLocalTimeout, "rebase", "--abort")
			list := strings.Join(splitLines(conflicts), ", ")
			if list == "" {
				list = "(未能列出, 见 git 输出)"
			}
			return false, "", fmt.Errorf("git 档: 与并发分片在同一文件上冲突, 无法自动合并 [%s]; "+
				"本阶段的提交仍在 worker 本地 (未丢弃), 远端未被改动。git: %s", list, firstLine(rout))
		}
	}
	return false, "", fmt.Errorf("git 档: 推送连续 %d 次被并发分片抢先, 放弃 (最后一次: %s)",
		gitPushRetries, firstLine(lastOut))
}

func gitPushRejected(out string) bool {
	l := strings.ToLower(out)
	return strings.Contains(l, "non-fast-forward") || strings.Contains(l, "fetch first") ||
		strings.Contains(l, "! [rejected]")
}

// gitStagedFiles 已暂存的文件列表 (-z: 路径含空格/中文也不会解析错)。
func gitStagedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := gitCmd(ctx, dir, gitLocalTimeout, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return nil, fmt.Errorf("git 档读取暂存清单失败: %w", err)
	}
	var files []string
	for _, p := range strings.Split(out, "\x00") {
		if p = strings.TrimSpace(p); p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}

// gitCheckFileSizes 大小闸。超限**失败**而不是跳过 —— 跳过就等于下一阶段找不到它。
func gitCheckFileSizes(dir string, files []string, limit int64) error {
	if limit <= 0 {
		limit = DefaultGitMaxFileBytes
	}
	var over []string
	for _, f := range files {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			continue // 删除的文件没有大小
		}
		if fi.Size() > limit {
			over = append(over, fmt.Sprintf("%s (%.1f MiB)", f, float64(fi.Size())/(1<<20)))
		}
	}
	if len(over) > 0 {
		return fmt.Errorf("git 档: %d 个文件超过单文件上限 %.1f MiB, 拒绝提交 [%s]。"+
			"如属构建产物请加入 .gitignore; 如确需入库请提高 max_file_bytes (git 未接 LFS, "+
			"大对象进历史后不可移除)", len(over), float64(limit)/(1<<20), strings.Join(over, ", "))
	}
	return nil
}

// gitCountBinary 暂存区里的二进制文件数 (numstat 对二进制给 "-"), 纯观测。
func gitCountBinary(ctx context.Context, dir string) int {
	out, err := gitCmd(ctx, dir, gitLocalTimeout, "diff", "--cached", "--numstat")
	if err != nil {
		return 0
	}
	n := 0
	for _, ln := range splitLines(out) {
		if strings.HasPrefix(ln, "-\t-\t") {
			n++
		}
	}
	return n
}

func gitSubject(st StageTask) string {
	s := strings.TrimSpace(st.NodeID)
	if s == "" {
		s = strings.TrimSpace(st.RunID)
	}
	if s == "" {
		s = "stage"
	}
	return s
}

// ─────────────────────────────────────────────────────────────────────────────
// 控制面侧: 把 worker 推上去的代码同步到 team.Cwd (门禁就在那里跑)
// ─────────────────────────────────────────────────────────────────────────────

// gitSyncControl 控制面侧同步。
//
// 这一步是 git 档"跨机产码可用"的最后一环: 编译门禁跑在 <team.Cwd>/go.mod 上
// (pkg/agent/teams.go:1091,1105), worker 推到远端的代码不同步进来, 门禁就还是什么
// 都看不见 —— 那正是上一轮记账的缺口。
//
// wantCommit 非空时**必须**在同步后可达: worker 说自己推了但控制面找不到 = 协议
// 漂移或推到了别的地方, 不许当成功。
//
// 合入用 ff-only: 控制面侧可能有本地改动 (人工介入/本地阶段的产出), 直接
// reset --hard 会毁掉它。ff 不成立就诚实失败, 让人来处理。
func gitSyncControl(ctx context.Context, dir string, g *GitWorkspace, wantCommit string) error {
	if g == nil || strings.TrimSpace(dir) == "" {
		return fmt.Errorf("git 档控制面同步: 缺少工作区或 git 声明")
	}
	br := strings.TrimSpace(g.Branch)
	if err := gitEnsureRepo(ctx, dir, g.Remote, true); err != nil {
		return err
	}
	if _, err := gitCmd(ctx, dir, gitNetworkTimeout, "fetch", "--prune", "origin"); err != nil {
		return fmt.Errorf("git 档控制面拉取失败: %w", err)
	}
	if !gitOK(ctx, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+br) {
		if wantCommit != "" {
			return fmt.Errorf("git 档控制面同步: 远端没有分支 %s, 但 worker 报告已推送 %s",
				br, shortSHA(wantCommit))
		}
		return nil // worker 没有产出, 远端也还没有这条分支: 没什么要同步的
	}
	cur, _ := gitCmd(ctx, dir, gitLocalTimeout, "rev-parse", "--abbrev-ref", "HEAD")
	onBranch := strings.TrimSpace(cur) == br && gitOK(ctx, dir, "rev-parse", "--verify", "-q", "HEAD")
	if !onBranch {
		// 首次同步 (或控制面还在别的分支上): 检出该分支。
		// checkout 撞上未跟踪文件时 git 会拒绝, 那个错误必须原样冒出来 —— 它意味着
		// 控制面的 cwd 里有与上游同名的文件, 静默覆盖会毁掉人的东西。
		if out, err := gitCmd(ctx, dir, gitLocalTimeout, "checkout", "-q", "-B", br, "origin/"+br); err != nil {
			return fmt.Errorf("git 档控制面检出 %s 失败: %w (%s)", br, err, firstLine(out))
		}
	} else if out, err := gitCmd(ctx, dir, gitLocalTimeout, "merge", "--ff-only", "-q", "origin/"+br); err != nil {
		return fmt.Errorf("git 档控制面无法快进合入 %s: %w (%s); "+
			"控制面工作区有本地提交或本地改动 —— 不做 reset --hard 以免毁掉它, 请人工处理",
			br, err, firstLine(out))
	}
	if wantCommit != "" {
		if !gitOK(ctx, dir, "merge-base", "--is-ancestor", wantCommit, "HEAD") {
			return fmt.Errorf("git 档控制面同步后仍找不到 worker 报告的提交 %s (分支 %s); "+
				"产物对编译门禁不可见, 拒绝当成功", shortSHA(wantCommit), br)
		}
	}
	return nil
}

// controlGitLocks 控制面侧按工作区路径串行化 git 操作。
//
// 必须有: 多个远程节点可能同时完成, 它们都要往同一个 team.Cwd 里 fetch/merge。
// 两个 git 进程在同一个工作树上并发操作会撞 index.lock, 表现为随机失败。
type controlGitLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (c *controlGitLocks) lock(path string) func() {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*sync.Mutex{}
	}
	l, ok := c.m[path]
	if !ok {
		l = &sync.Mutex{}
		c.m[path] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// ─────────────────────────────────────────────────────────────────────────────
// 小工具
// ─────────────────────────────────────────────────────────────────────────────

func splitLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func shortSHA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 8 {
		return s[:8]
	}
	if s == "" {
		return "(空)"
	}
	return s
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
