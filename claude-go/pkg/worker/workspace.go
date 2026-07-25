package worker

// workspace.go —— cwd 三档位 local/pvc/git (design/02 §3.3 "工作区(cwd)问题")。
//
// # 这一档补的是哪一段断链
//
// 上一轮只做了 fail-closed 的"能不能提供"判定 (worker.checkWorkspace): 任务要求的
// 工作区与 worker 声明的不一致就拒绝执行。那个判定是对的, 但它只是**闸门**, 不是
// **通路** —— 控制面从来没有真的把"代码该落在哪"这件事声明出来 (factory 压根不填
// RuntimeNodeTask.Workspace), 于是远程 worker 一律在自己的 cwd 里产码, 而编译门禁
// 跑在控制面的 <team.Cwd>/go.mod 上 (pkg/agent/teams.go:1091,1105) —— 远程写的文件
// 门禁看不见, 跨机产码工作流不可用 (design/02 §六 风险④)。
//
// 本文件 + gitws.go 提供三个档位, **建在既有 fail-closed 判定之上而不是替换它**:
//
//	local  worker 用自己的本地目录。只在团队亲和成立 (同团队节点落同一 worker) 时
//	       才正确。语义 = 上一轮的隐含默认, 这里把它显式化成一个可声明的档位。
//	       路径一致只证明"路径相同", **不证明同一份数据** (同路径不同磁盘照样通过)
//	       —— 所以跨机产码不要用 local, 用 pvc/git。
//	pvc    控制面与 worker 挂同一个 ReadWriteMany 卷。声明 (卷名+路径) 之外还做
//	       **双向握手**: 控制面派任务前在共享卷上写 <path>/.claude-go-ws/<taskID>.ctl,
//	       worker 必须读到它才肯执行; worker 执行完写 .worker 回执, 控制面必须读到
//	       它才认这次成功。这一步是真的在验证"同一份数据"——只靠双方各自声明路径,
//	       两个 Pod 各挂一个 emptyDir 到 /workspace 也会双双"声明一致"。
//	git    worker 在自己的本地目录工作, 阶段结束把产物 commit/push 回约定的 git 位置,
//	       下一阶段的 worker 从那里拉 (见 gitws.go)。不需要共享存储。
//
// # 档位靠既有能力标签路由 (pkg/cluster 零改动)
//
// worker 上报 `ws:<档位>` (pvc 再加 `wsvol:<卷名>`), 控制面派任务时把它写进
// RequireCaps —— 于是"local 档的 worker 绝不会拉到 pvc 档的任务"用队列既有的
// RequireCaps ⊆ workerCaps 过滤就实现了。worker 侧的模式校验是纵深防御 (注册表
// 不同步/协议漂移时不许硬跑)。
//
// # 默认行为不变
//
// 控制面**未配置** WorkspacePolicy 时: StageTask 的档位字段全空, RequireCaps 不含
// ws:*, worker 侧走的还是原来那条 checkWorkspace 分支 —— 与改造前一字不变。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// WorkspaceMode cwd 档位。
type WorkspaceMode string

const (
	// WorkspaceModeUnset 未声明。行为 = local (即改造前的隐含默认)。
	WorkspaceModeUnset WorkspaceMode = ""
	// WorkspaceModeLocal worker 用自己的本地目录 (只在团队亲和成立时正确)。
	WorkspaceModeLocal WorkspaceMode = "local"
	// WorkspaceModePVC 控制面与 worker 共享 RWX 卷。
	WorkspaceModePVC WorkspaceMode = "pvc"
	// WorkspaceModeGit worker 本地工作 + commit/push 回约定 git 位置。
	WorkspaceModeGit WorkspaceMode = "git"
)

// ParseWorkspaceMode 解析档位名。未知档位必须报错而不是静默降级成 local ——
// 把 "--workspace-mode pvcc" 这种拼写错误洗成"本地跑"正是本包要挡的那类故障。
func ParseWorkspaceMode(s string) (WorkspaceMode, error) {
	switch m := WorkspaceMode(strings.ToLower(strings.TrimSpace(s))); m {
	case WorkspaceModeUnset, WorkspaceModeLocal, WorkspaceModePVC, WorkspaceModeGit:
		return m, nil
	default:
		return WorkspaceModeUnset, fmt.Errorf("worker: 未知工作区档位 %q (可选 local/pvc/git)", s)
	}
}

// Effective 未声明视作 local。
func (m WorkspaceMode) Effective() WorkspaceMode {
	if m == WorkspaceModeUnset {
		return WorkspaceModeLocal
	}
	return m
}

// 档位/卷的能力标签前缀 (与 CapWorkerPrefix 同一套机制: 用队列既有的标签过滤做路由)。
const (
	CapWorkspacePrefix = "ws:"
	CapVolumePrefix    = "wsvol:"
)

// WorkspaceModeCap 档位能力标签。
func WorkspaceModeCap(m WorkspaceMode) string { return CapWorkspacePrefix + string(m.Effective()) }

// WorkspaceVolumeCap 卷身份能力标签。
func WorkspaceVolumeCap(v string) string {
	return CapVolumePrefix + strings.ToLower(strings.TrimSpace(v))
}

// GitWorkspace git 档位的约定位置 (随任务下发)。
//
// Remote/Branch 是**控制面配置**的, 不是从图规格推出来的 —— 见 WorkspacePolicy 的
// 注释: "约定 git 位置"这个上游概念在图层不存在, 由部署方注入。
type GitWorkspace struct {
	// Remote 约定 git 位置 (bare 仓的 URL 或本机路径)。
	Remote string `json:"remote"`
	// Branch 本团队/本次运行的工作区分支 (所有阶段共用它接力)。
	Branch string `json:"branch"`
	// BaseRef 分支不存在时的起点 (空 → origin/HEAD; 远端为空仓则起自空树)。
	BaseRef string `json:"base_ref,omitempty"`
	// MaxFileBytes 单文件大小上限 (0 → DefaultGitMaxFileBytes)。超限**拒绝提交并让
	// 阶段失败**: 静默跳过大文件 = 下一阶段找不到它, 正是本包要挡的故障。
	MaxFileBytes int64 `json:"max_file_bytes,omitempty"`
}

// WorkspaceReport worker 对本次工作区处理的据实回报 (随 StageResult 回控制面)。
//
// 为什么要回报: git 档位下"有没有真的推上去"是控制面能否看到代码的唯一依据。
// 没有它, 控制面只能猜, 而猜错的方向恰好是"以为推了其实没推"= 静默成功。
type WorkspaceReport struct {
	Mode    WorkspaceMode `json:"mode,omitempty"`
	Dir     string        `json:"dir,omitempty"`     // worker 侧真实工作目录
	Commit  string        `json:"commit,omitempty"`  // git: 推上去的提交
	Pushed  bool          `json:"pushed,omitempty"`  // git: 是否真的 push 了
	Changed bool          `json:"changed,omitempty"` // 本阶段有无文件改动
	Files   int           `json:"files,omitempty"`   // 提交涉及的文件数
	Binary  int           `json:"binary,omitempty"`  // 其中二进制文件数 (可观测)
	Note    string        `json:"note,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// 控制面侧: WorkspacePolicy → 每次执行的工作区声明
// ─────────────────────────────────────────────────────────────────────────────

// WorkspacePolicy 控制面的进程级工作区策略 (由部署方注入, 与 Placement 默认策略
// 同一个注入点)。
//
// # 诚实说明: "约定 git 位置"从哪来
//
// design/02 §3.3 写的是"worker 各自 clone/worktree + 结果以 commit/patch 回传",
// 但**图规格里没有任何地方描述那个 remote**: `graph.AgentSpec` 没有工作区字段,
// 团队 (`agent.ProductionTeam`) 只有一个进程级 Cwd (teams.go:609 `Cwd: ptm.cwd`),
// 也没有"这个团队对应哪个仓"的概念。所以 remote 只能由部署方配置, 分支按团队名
// 模板化。这是本档位的上游边界, 不是实现偷懒。
type WorkspacePolicy struct {
	// Mode 档位。空 = 不启用 (StageTask 不带任何档位字段, 行为与改造前一致)。
	Mode WorkspaceMode
	// Volume pvc 档: 卷身份。worker 必须声明同名卷 (wsvol:<v>) 才可能拉到任务。
	// 卷名只是"两边说的是同一个卷"的声明; 真正证明同一份数据的是握手文件。
	Volume string
	// GitRemote git 档: 约定 git 位置 (bare 仓 URL / 路径)。
	GitRemote string
	// GitBranch git 档: 分支模板, 支持 {team} / {run} 占位。
	// 空 → DefaultGitBranchTemplate。
	GitBranch string
	// GitBaseRef git 档: 分支首次创建时的起点 (空 → origin/HEAD)。
	GitBaseRef string
	// GitMaxFileBytes git 档: 单文件上限 (0 → DefaultGitMaxFileBytes)。
	GitMaxFileBytes int64
}

// DefaultGitBranchTemplate 默认分支模板: 一个团队一条工作区分支。
//
// 为什么按团队而不是按 run: 同一团队的多次 run (含图层重试) 要接力同一份代码,
// 按 run 分支会让重试从空目录开始。按团队分支 = 与 Placement.Affinity:"team" 同粒度。
const DefaultGitBranchTemplate = "claude-go/ws/{team}"

// DefaultGitMaxFileBytes 单文件上限默认 32 MiB。
// 取值理由: 足够放下 pptx/png/小型 wav 这类真实产物, 又能挡住误提交的二进制大对象
// (git 没有 LFS 接线, 大对象进仓就永久留在历史里)。
const DefaultGitMaxFileBytes int64 = 32 << 20

// Enabled 策略是否启用。
func (p *WorkspacePolicy) Enabled() bool { return p != nil && p.Mode != WorkspaceModeUnset }

// Validate 校验策略自洽。缺项必须在启动时报错 —— 一个"启用了 pvc 档但没写卷名"的
// 控制面会把所有任务派成永远没人能拉的 pending。
func (p *WorkspacePolicy) Validate() error {
	if !p.Enabled() {
		return nil
	}
	switch p.Mode {
	case WorkspaceModeLocal:
	case WorkspaceModePVC:
		if strings.TrimSpace(p.Volume) == "" {
			return fmt.Errorf("worker: pvc 档位必须声明卷名 (WorkspacePolicy.Volume)")
		}
	case WorkspaceModeGit:
		if strings.TrimSpace(p.GitRemote) == "" {
			return fmt.Errorf("worker: git 档位必须声明约定 git 位置 (WorkspacePolicy.GitRemote)")
		}
	default:
		return fmt.Errorf("worker: 未知工作区档位 %q", p.Mode)
	}
	return nil
}

// RequireCaps 本策略对 worker 的硬要求 (进队列 RequireCaps, 由既有过滤器路由)。
func (p *WorkspacePolicy) RequireCaps() []string {
	if !p.Enabled() {
		return nil
	}
	out := []string{WorkspaceModeCap(p.Mode)}
	if p.Mode == WorkspaceModePVC && strings.TrimSpace(p.Volume) != "" {
		out = append(out, WorkspaceVolumeCap(p.Volume))
	}
	return out
}

var branchSanitize = regexp.MustCompile(`[^A-Za-z0-9._/-]+`)

// resolveBranch 按团队/run 展开分支模板。
func (p *WorkspacePolicy) resolveBranch(team, runID string) string {
	tpl := strings.TrimSpace(p.GitBranch)
	if tpl == "" {
		tpl = DefaultGitBranchTemplate
	}
	key := strings.TrimSpace(team)
	if key == "" {
		// 没有团队名时退回 run: 宁可一次 run 一条分支 (重试从头) 也不能让所有团队
		// 共用一条分支 —— 那会把互不相关的团队的代码搅在一起。
		key = strings.TrimSpace(runID)
	}
	if key == "" {
		key = "default"
	}
	br := strings.ReplaceAll(tpl, "{team}", key)
	br = strings.ReplaceAll(br, "{run}", strings.TrimSpace(runID))
	br = branchSanitize.ReplaceAllString(br, "-")
	br = strings.Trim(br, "/-")
	if br == "" {
		br = "claude-go/ws/default"
	}
	return br
}

// workspaceRequest 控制面对一次执行的工作区声明 (broker 内部形态)。
type workspaceRequest struct {
	Mode      WorkspaceMode
	Path      string // 控制面侧的团队 cwd (= 编译门禁跑的地方)
	Volume    string
	Handshake string // pvc: 握手令牌 (= 任务 ID)
	Git       *GitWorkspace
}

// resolve 把策略 + 本次执行的上下文展开成声明。path 为控制面侧团队 cwd。
func (p *WorkspacePolicy) resolve(path, team, runID, taskID string) (*workspaceRequest, error) {
	if !p.Enabled() {
		return nil, nil
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	req := &workspaceRequest{Mode: p.Mode, Path: strings.TrimSpace(path)}
	switch p.Mode {
	case WorkspaceModeLocal, WorkspaceModePVC:
		// 这两档的身份就是路径: 没有路径就无从校验, 也无从让门禁看到代码。
		if req.Path == "" {
			return nil, fmt.Errorf("worker: %s 档位要求声明工作区路径, 但控制面没有提供团队 cwd "+
				"(RunMetadata.Cwd 为空: 该次执行不是团队阶段?)", p.Mode)
		}
		if p.Mode == WorkspaceModePVC {
			req.Volume = strings.TrimSpace(p.Volume)
			req.Handshake = taskID
		}
	case WorkspaceModeGit:
		// git 档不要求控制面有 cwd: 没有 cwd 就没有编译门禁 (teams.go:849 以
		// team.Cwd != "" 为门禁前提), 此时 git 档退化成纯 worker 间接力 —— 仍然成立,
		// 只是控制面不同步、也无从校验提交可达 (settleWorkspace 里如实处理)。
		req.Git = &GitWorkspace{
			Remote:       strings.TrimSpace(p.GitRemote),
			Branch:       p.resolveBranch(team, runID),
			BaseRef:      strings.TrimSpace(p.GitBaseRef),
			MaxFileBytes: p.GitMaxFileBytes,
		}
	}
	return req, nil
}

// apply 把声明写进任务载荷。
func (r *workspaceRequest) apply(st *StageTask) {
	if r == nil {
		return
	}
	st.WorkspaceMode = r.Mode
	st.Workspace = r.Path
	st.WorkspaceVolume = r.Volume
	st.WorkspaceHandshake = r.Handshake
	st.Git = r.Git
}

// ─────────────────────────────────────────────────────────────────────────────
// pvc 握手: 证明"同一份数据", 而不是"同一个路径字符串"
// ─────────────────────────────────────────────────────────────────────────────

// handshakeDir 握手文件目录 (放在工作区内 —— 它要能被双方看见, 这正是被验证的东西)。
const handshakeDir = ".claude-go-ws"

// handshakeTTL 陈旧握手文件的清理阈值。
const handshakeTTL = 2 * time.Hour

type handshakeNote struct {
	Volume string `json:"volume,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	Side   string `json:"side"` // control / worker
	Who    string `json:"who,omitempty"`
	Dir    string `json:"dir,omitempty"`
	TS     int64  `json:"ts"`
}

func handshakePath(root, token, side string) string {
	return filepath.Join(root, handshakeDir, token+"."+side)
}

// writeHandshake 写一侧的握手文件。
func writeHandshake(root, token, side string, note handshakeNote) error {
	dir := filepath.Join(root, handshakeDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("工作区 %s 不可写 (pvc 档要求双方都能写同一个卷): %w", root, err)
	}
	note.Side = side
	note.TS = time.Now().UnixMilli()
	b, err := json.Marshal(note)
	if err != nil {
		return err
	}
	// 先写临时文件再 rename: 对端可能正好在读, 半个 JSON 会被读成"握手损坏"。
	tmp := filepath.Join(dir, "."+token+"."+side+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("写握手文件失败 (%s): %w", tmp, err)
	}
	if err := os.Rename(tmp, handshakePath(root, token, side)); err != nil {
		return fmt.Errorf("落地握手文件失败: %w", err)
	}
	return nil
}

// readHandshake 读一侧的握手文件。
func readHandshake(root, token, side string) (handshakeNote, error) {
	var n handshakeNote
	b, err := os.ReadFile(handshakePath(root, token, side))
	if err != nil {
		return n, err
	}
	if err := json.Unmarshal(b, &n); err != nil {
		return n, fmt.Errorf("握手文件内容损坏: %w", err)
	}
	return n, nil
}

// pruneHandshakes 清掉过期握手文件 (best-effort, 防共享卷上无界堆积)。
func pruneHandshakes(root string) {
	dir := filepath.Join(root, handshakeDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cut := time.Now().Add(-handshakeTTL)
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(cut) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// worker 侧: 档位声明与准备
// ─────────────────────────────────────────────────────────────────────────────

// workspaceCaps worker 按自己的档位配置上报的能力标签。
func workspaceCaps(opt Options) []string {
	mode := opt.WorkspaceMode.Effective()
	out := []string{WorkspaceModeCap(mode)}
	if mode == WorkspaceModePVC && strings.TrimSpace(opt.WorkspaceVolume) != "" {
		out = append(out, WorkspaceVolumeCap(opt.WorkspaceVolume))
	}
	return out
}

// workspaceSession 一次任务的工作区上下文。
type workspaceSession struct {
	mode WorkspaceMode
	dir  string
	once sync.Once
	// fn 收尾: ok=true 表示执行体成功。git 档在这里 commit/push, pvc 档在这里写回执。
	// 返回 error ⇒ **整个阶段失败** (产物没能交出去 = 没成功)。
	fn func(ok bool) (WorkspaceReport, error)
}

// finish 收尾一次 (幂等)。
//
// 幂等是必需的: git 档的 finish 里放着工作目录锁的释放, 漏调一次 worker 就永久卡死,
// 多调一次 (execute 的 defer 兜底 + 成功路径显式调用) 就会双重释放。
func (s *workspaceSession) finish(ok bool) (WorkspaceReport, error) {
	rep := WorkspaceReport{Mode: s.mode, Dir: s.dir}
	var err error
	s.once.Do(func() {
		if s.fn != nil {
			rep, err = s.fn(ok)
		}
	})
	return rep, err
}

// abandon 兜底收尾 (execute 的 defer): 已收尾过则什么都不做。
func (s *workspaceSession) abandon() { _, _ = s.finish(false) }

func noopSession(mode WorkspaceMode, dir string) *workspaceSession {
	return &workspaceSession{mode: mode, dir: dir}
}

// prepareWorkspace 按档位准备工作区。
//
// 返回错误 ⇒ 拒绝执行 (fail-closed)。这条规则不可让: 静默在错误目录里产码会得到
// "阶段成功但下一阶段找不到上一阶段的代码", 是最难归因的一类故障。
func (w *Worker) prepareWorkspace(st StageTask) (*workspaceSession, error) {
	want := st.WorkspaceMode.Effective()
	have := w.opt.WorkspaceMode.Effective()
	if want != have {
		// 纵深防御: 队列的 ws:<档位> 标签过滤正常情况下已经挡住了这种投递。
		return nil, fmt.Errorf("任务要求 %s 档工作区, 但 worker %s 提供的是 %s 档; 拒绝执行",
			want, w.opt.Name, have)
	}
	switch want {
	case WorkspaceModeLocal:
		// 与改造前**完全同一条判定** (checkWorkspace 未改): 一致才跑, 不一致诚实失败。
		if err := w.checkWorkspace(st.Workspace); err != nil {
			return nil, err
		}
		return noopSession(WorkspaceModeLocal, strings.TrimSpace(w.opt.Workspace)), nil
	case WorkspaceModePVC:
		return w.preparePVC(st)
	case WorkspaceModeGit:
		return w.prepareGit(st)
	}
	return nil, fmt.Errorf("worker: 未知工作区档位 %q", want)
}

// preparePVC pvc 档: 路径一致 + 卷一致 + 握手文件真的看得见。
func (w *Worker) preparePVC(st StageTask) (*workspaceSession, error) {
	// ① 先做与 local 相同的路径判定 (路径不同必然不是同一份数据)。
	if err := w.checkWorkspace(st.Workspace); err != nil {
		return nil, err
	}
	dir := strings.TrimSpace(w.opt.Workspace)
	// ② 卷身份: 两边说的必须是同一个卷。
	wantVol := strings.ToLower(strings.TrimSpace(st.WorkspaceVolume))
	haveVol := strings.ToLower(strings.TrimSpace(w.opt.WorkspaceVolume))
	if wantVol != "" && wantVol != haveVol {
		return nil, fmt.Errorf("任务要求共享卷 %q, 但 worker %s 挂的是 %q; 拒绝执行",
			wantVol, w.opt.Name, haveVol)
	}
	// ③ 握手: 控制面派任务前写在共享卷上的文件, 必须在这里读得到。
	//    只有这一步能区分"真共享"与"两个 Pod 各挂一个 emptyDir 到同一路径"。
	token := strings.TrimSpace(st.WorkspaceHandshake)
	if token == "" {
		return nil, fmt.Errorf("pvc 档任务缺少握手令牌 (workspace_handshake); 拒绝执行 —— "+
			"没有它就无法证明 worker %s 与控制面看到的是同一份数据", w.opt.Name)
	}
	note, err := readHandshake(dir, token, "control")
	if err != nil {
		return nil, fmt.Errorf("pvc 档握手失败: worker %s 在 %s 下看不到控制面写的 %s "+
			"(%v) ⇒ 该路径不是同一个共享卷, 拒绝在错误目录执行",
			w.opt.Name, dir, filepath.Join(handshakeDir, token+".control"), err)
	}
	if nv := strings.ToLower(strings.TrimSpace(note.Volume)); nv != "" && haveVol != "" && nv != haveVol {
		return nil, fmt.Errorf("pvc 档握手文件声明的卷是 %q, 与 worker %s 挂的 %q 不符; 拒绝执行",
			nv, w.opt.Name, haveVol)
	}
	return &workspaceSession{
		mode: WorkspaceModePVC, dir: dir,
		fn: func(ok bool) (WorkspaceReport, error) {
			rep := WorkspaceReport{Mode: WorkspaceModePVC, Dir: dir}
			if !ok {
				return rep, nil
			}
			// 回执: 让控制面能验证"worker 的写入我这边看得见"。产码类阶段的产物
			// 对门禁可见与否, 就等价于这个文件对控制面可见与否。
			if err := writeHandshake(dir, token, "worker", handshakeNote{
				Volume: haveVol, TaskID: token, Who: w.opt.Name, Dir: dir}); err != nil {
				return rep, fmt.Errorf("pvc 档回执写入失败 (%s): %w", dir, err)
			}
			rep.Note = "共享卷双向握手通过"
			return rep, nil
		},
	}, nil
}
