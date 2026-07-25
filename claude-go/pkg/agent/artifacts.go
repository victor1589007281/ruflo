package agent

// 团队产物统一采集 (为什么需要这个文件)
//
// 团队产物天然散落在两个根, 且两者的包含关系随配置翻转:
//
//	① data 根 = dataDir = <stateDir>/teams/<team>
//	   平台自己写的东西: REPORT.md / team.json / blackboard.json /
//	   checkpoints.json / graph-journal/ / media/ / NOVEL.md ...
//	② work 根 = team.Cwd = 进程级 config.Cwd (注意: 不是 per-team!)
//	   agent 用 Write/Edit/Bash 写的任意文件、media_gen 六个媒体工具
//	   (resolveOutPath 锚定会话 cwd) 产的图/视频/音频、MaterializeCode
//	   落的源码, 全在这里。
//	③ tool 根 = <cwd>/.claude-go/artifacts/tool-results (超大工具结果溢出)
//
// 历史上每个采集方都只扫一处, 漏一处就把"有产物"误判成"无产物": 下游媒锻只扫
// <stateDir>/teams/<team>/, 漏采 pipeline agent 写到团队 cwd 的音乐产物, 于是
// fail-open 把已完成的成品误置成 review。本文件把"两根合并 + 去重 + 归属过滤 +
// 递归"收成唯一实现, 供所有采集方(finish 时的 ARTIFACTS.json、dashboard 端点、
// 下游平台)复用, 避免再各写一份漏一处。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 产物根类别。也是对外 API (dashboard 取文件子路由) 的 root 白名单取值。
const (
	ArtifactRootData = "data" // <stateDir>/teams/<team>: 平台写的报告/黑板/检查点/media
	ArtifactRootWork = "work" // team.Cwd: agent 与媒体工具写的文件 (进程级共享)
	ArtifactRootTool = "tool" // <cwd>/.claude-go/artifacts/tool-results: 大工具结果溢出
)

// 产物来源 (证据强度): materialize = 平台亲手写下并记账的路径, 是硬证据;
// scan = 遍历磁盘按归属判据认领的, 属推断。
const (
	ArtifactSourceScan        = "scan"
	ArtifactSourceMaterialize = "materialize"
)

// ArtifactManifestFile 清单文件名 (落在 dataDir 下)。
const ArtifactManifestFile = "ARTIFACTS.json"

// maxArtifactFiles 单次采集的文件数上限。cwd 常是用户真实 git 仓库, 一次
// npm install / go build 就能让上万文件的 mtime 晚于 StartedAt; 截断并显式标记
// Truncated, 好过让调用方拉一份几十 MB 的清单。
const maxArtifactFiles = 4000

// maxArtifactScanDuration 单个根的遍历时间上限。清单在团队 finish 的关键路径上生成,
// 而 cwd 可能是几十 GB 的真实仓库 (甚至网络盘): 超时就截断并标记 Truncated (清单不完整
// 但仍是"扫过"), 不能让交付卡在一次目录遍历上。
const maxArtifactScanDuration = 15 * time.Second

// ArtifactRoot 一个产物根。
type ArtifactRoot struct {
	Kind string `json:"kind"` // data | work | tool
	Path string `json:"path"` // 绝对路径
}

// ArtifactRef 一个产物文件。
type ArtifactRef struct {
	Root    string    `json:"root"`    // 所属根的 Kind
	Rel     string    `json:"rel"`     // 相对该根的路径 (始终用 / 分隔)
	Size    int64     `json:"size"`    // 字节
	ModTime time.Time `json:"modTime"` // 修改时间
	Source  string    `json:"source"`  // scan | materialize
}

// ArtifactRootStatus 单个根的扫描结果。
// Scanned 与 Error 是 fail-open 语义的关键: 调用方必须能区分"扫过且真空"
// (Scanned=true, Files=0) 与"未扫/出错"(Scanned=false, Error!=""), 否则就会
// 重演媒锻那次误判 —— 把采集失败当成"没有产物"。
type ArtifactRootStatus struct {
	ArtifactRoot
	Exists    bool   `json:"exists"`
	Scanned   bool   `json:"scanned"`
	Truncated bool   `json:"truncated,omitempty"`
	Files     int    `json:"files"`
	Error     string `json:"error,omitempty"`
}

// ArtifactManifest 一次采集的完整结果, 也是 ARTIFACTS.json 的落盘格式。
type ArtifactManifest struct {
	Team      string               `json:"team"`
	Roots     []ArtifactRootStatus `json:"roots"`
	Files     []ArtifactRef        `json:"files"`
	ScannedAt time.Time            `json:"scannedAt"`
	StartedAt time.Time            `json:"startedAt"`
	Scanned   bool                 `json:"scanned"` // 所有根都有确定结论 (扫过或确定不存在)
	Empty     bool                 `json:"empty"`   // Scanned 且零产物 = "扫过且真空"
	Truncated bool                 `json:"truncated,omitempty"`
	Error     string               `json:"error,omitempty"` // 非空 = 至少一个根未扫成, 清单不完整
}

// ArtifactScanSpec 一次采集的输入。与 ProductionTeam 解耦, 让只有磁盘状态、拿不到
// 活体 team 对象的调用方 (dashboard 读 team.json、下游平台) 也能复用同一套
// 去重 / 归属 / 递归规则。
type ArtifactScanSpec struct {
	Team         string
	DataDir      string    // 落点 A
	Cwd          string    // 落点 B (进程级共享)
	StartedAt    time.Time // 归属时间窗下界; 零值 = 无判据, 共享根标记为未扫
	FinishedAt   time.Time // 归属时间窗上界; 零值 = 团队仍在跑, 上界开放
	Materialized []string  // MaterializeCode 返回的 written (相对 Cwd, 也接受绝对路径)
}

// artifactFinishSlack 归属时间窗上界的宽限。团队标记 FinishedAt 之后仍可能有收尾
// 写盘 (异步装配、缓冲 flush), 留一点余量以免把自己的产物判给"别人"。
const artifactFinishSlack = 30 * time.Second

// artifactSkipDir 遍历时应整棵剪掉的目录。
//
// 为什么剪这些: .git 是版本库内部状态 (agent 一次 commit 会刷新上千个 object 的
// mtime, 全部会被 mtime 判据误认成产物); node_modules / __pycache__ 是依赖与字节码
// 缓存; .claude-go 是平台自己的状态目录 —— 剪掉它同时顺手完成了默认配置下的去重
// (默认无 stateDir 时 dataDir = <cwd>/.claude-go/teams/<n>, 嵌套在 cwd 内, 若不剪
// 就会把 REPORT.md / team.json 当成 cwd 工作产物重复计一遍)。
//
// 判据与 workspaceFileManifest (注入 coder prompt 的工作区清单) 共用同一份实现,
// 避免两处判据漂移。
func artifactSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".claude-go", "__pycache__":
		return true
	}
	return false
}

// artifactBelongsToTeam cwd 侧的归属判据。
//
// team.Cwd 是【进程级】共享目录 (teams.go 里 team.Cwd = ptm.cwd, 且只能经
// main.go 的 --cwd / 飞书 /cwd set 全局设置), 同一 cwd 下先后或并发跑的多个团队
// 产物混在一起, 没有 <cwd>/<team>/ 命名空间可用。全代码库里可用的归属判据只有两个:
// ① 文件 mtime 晚于本团队 StartedAt; ② MaterializeCode 返回的 written 列表。
// 这里是 ①, 与 workspaceFileManifest 复用同一实现。
func artifactBelongsToTeam(mod, since time.Time) bool {
	return mod.After(since)
}

// artifactInAttributionWindow 完整归属时间窗 [StartedAt, FinishedAt+slack]。
//
// 只有下界不足以把同一 cwd 下【先后】跑的两个团队分开: B 在 A 之后跑, B 的文件
// mtime 必然也晚于 A.StartedAt, 只用下界就会把 B 的产物算进 A 的清单。上界用
// A.FinishedAt (已有的序列化字段) 把它切掉; 团队仍在跑 (FinishedAt 为零) 时上界开放。
func artifactInAttributionWindow(mod, started, finished time.Time) bool {
	if !artifactBelongsToTeam(mod, started) {
		return false
	}
	if finished.IsZero() {
		return true
	}
	return !mod.After(finished.Add(artifactFinishSlack))
}

// ArtifactAttributed 判断某个共享根 (work / tool) 下的文件是否可归属给本次采集的团队。
// 供外部入口 (dashboard 取文件时的归属校验) 复用同一判据。
func ArtifactAttributed(spec ArtifactScanSpec, mod time.Time) bool {
	if spec.StartedAt.IsZero() {
		return false // 没有基线就不敢认领 (宁可说"不知道", 不要瞎认)
	}
	return artifactInAttributionWindow(mod, spec.StartedAt, spec.FinishedAt)
}

// normalizeArtifactPath 归一成干净的绝对路径, 便于做"同一路径只算一次"的去重
// 与"嵌套根剪枝"的字符串比较。
func normalizeArtifactPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// RootsForScan 返回本次采集涉及的全部产物根 (已按路径去重)。
func RootsForScan(spec ArtifactScanSpec) []ArtifactRoot {
	var roots []ArtifactRoot
	seen := map[string]bool{}
	add := func(kind, path string) {
		p := normalizeArtifactPath(path)
		// 同一路径只出现一次: dataDir == cwd 这种病态配置下不能返回两条同路径根,
		// 否则每个文件都会被计两遍。
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		roots = append(roots, ArtifactRoot{Kind: kind, Path: p})
	}
	add(ArtifactRootData, spec.DataDir)
	if cwd := normalizeArtifactPath(spec.Cwd); cwd != "" {
		add(ArtifactRootWork, cwd)
		// tool 根只在真的存在时才列: 它是 orchestration 溢出超大工具结果时才创建的
		// 可选目录, 且嵌套在 work 根内 (work 侧遍历会剪掉 .claude-go), 恒列会在清单
		// 里留下一个永不命中的幽灵根, 干扰"扫过且真空"的判断和白名单校验。
		toolDir := filepath.Join(cwd, ".claude-go", "artifacts", "tool-results")
		if st, err := os.Stat(toolDir); err == nil && st.IsDir() {
			add(ArtifactRootTool, toolDir)
		}
	}
	return roots
}

// ArtifactRoots 返回团队的产物根 (data / work / tool)。
func ArtifactRoots(team *ProductionTeam) []ArtifactRoot {
	return RootsForScan(ArtifactSpecForTeam(team))
}

// CollectArtifacts 采集团队在【所有】落点上的产物。
//
// error 非空 = 至少一个根没扫成 (权限/IO/缺归属基线), 清单不完整: 调用方此时
// 【不得】把空清单当成"没有产物"(这正是媒锻 fail-open 误置 review 的根因)。
// error 为空且返回空切片 = 扫过且确实零产物。
func CollectArtifacts(team *ProductionTeam) ([]ArtifactRef, error) {
	return CollectArtifactsFor(ArtifactSpecForTeam(team))
}

// CollectArtifactsFor 同 CollectArtifacts, 但输入是解耦的 spec。
func CollectArtifactsFor(spec ArtifactScanSpec) ([]ArtifactRef, error) {
	m := BuildArtifactManifestFor(spec)
	if m.Error != "" {
		return m.Files, errors.New(m.Error)
	}
	return m.Files, nil
}

// BuildArtifactManifest 生成团队产物清单。
func BuildArtifactManifest(team *ProductionTeam) ArtifactManifest {
	return BuildArtifactManifestFor(ArtifactSpecForTeam(team))
}

// BuildArtifactManifestFor 生成产物清单: 逐根扫描, 合并去重, 记录每个根是否真的
// 扫过, 让"扫过且真空"与"未扫/出错"在结果里可分辨。
func BuildArtifactManifestFor(spec ArtifactScanSpec) ArtifactManifest {
	roots := RootsForScan(spec)
	m := ArtifactManifest{
		Team:      spec.Team,
		ScannedAt: time.Now(),
		StartedAt: spec.StartedAt,
		Files:     []ArtifactRef{},
		Roots:     []ArtifactRootStatus{},
	}

	// 物化清单归一成 work 根下的相对路径: 它比 mtime 更硬 —— 平台亲手写的文件,
	// 不依赖时间猜测, 也不该被 .git/node_modules 剪枝规则连带丢掉。
	workPath := ""
	for _, r := range roots {
		if r.Kind == ArtifactRootWork {
			workPath = r.Path
		}
	}
	materialized := map[string]bool{}
	if workPath != "" {
		for _, f := range spec.Materialized {
			if rel := relUnderRoot(workPath, f); rel != "" {
				materialized[rel] = true
			}
		}
	}

	var problems []string
	for _, root := range roots {
		st := ArtifactRootStatus{ArtifactRoot: root}
		info, err := os.Stat(root.Path)
		switch {
		case err == nil && info.IsDir():
			st.Exists = true
		case err != nil && os.IsNotExist(err):
			// 根不存在 = 确定性的空 (平台从没往这里写过), 是"扫过且真空"而非"未扫"。
			st.Scanned = true
			m.Roots = append(m.Roots, st)
			continue
		case err == nil:
			st.Error = "不是目录: " + root.Path
			problems = append(problems, fmt.Sprintf("%s(%s): %s", root.Kind, root.Path, st.Error))
			m.Roots = append(m.Roots, st)
			continue
		default:
			// stat 失败 (权限/IO): 必须让调用方看出"不知道有没有产物"。
			st.Error = err.Error()
			problems = append(problems, fmt.Sprintf("%s(%s): %v", root.Kind, root.Path, err))
			m.Roots = append(m.Roots, st)
			continue
		}

		// work / tool 根是【进程级共享】目录 (混着别的团队和用户自己的文件), 必须过
		// 归属时间窗; data 根是本团队专属目录, 全量收。
		shared := root.Kind != ArtifactRootData
		if shared && spec.StartedAt.IsZero() {
			// 没有 StartedAt 就没有任何归属判据。与其把整个用户仓库当成本团队产物
			// (污染) 或反过来当成"无产物"(误判), 明确标成"未扫 + 原因"。
			st.Error = "缺少 startedAt: 无法判定共享目录内文件的归属, 已跳过该根"
			problems = append(problems, fmt.Sprintf("%s(%s): %s", root.Kind, root.Path, st.Error))
			m.Roots = append(m.Roots, st)
			continue
		}

		// 去重: 剪掉嵌套在本根内的其它根 (默认配置下 dataDir/tool 根都在 cwd 内),
		// 它们由各自那一轮单独扫, 否则同一个文件会以两个 root 出现两次。
		prune := map[string]bool{}
		for _, other := range roots {
			if other.Path != root.Path && isUnderPath(root.Path, other.Path) {
				prune[other.Path] = true
			}
		}

		refs, truncated, walkErr := scanArtifactRoot(root, spec, shared, prune, materialized)
		st.Scanned = walkErr == nil
		st.Truncated = truncated
		st.Files = len(refs)
		if walkErr != nil {
			st.Error = walkErr.Error()
			problems = append(problems, fmt.Sprintf("%s(%s): %v", root.Kind, root.Path, walkErr))
		}
		if truncated {
			m.Truncated = true
		}
		m.Files = append(m.Files, refs...)
		m.Roots = append(m.Roots, st)
	}

	// 物化过但遍历没采到的文件单独补回 (落在被剪目录里, 或 mtime 被外部工具改回
	// 过去): written 是平台亲手写下的硬证据, 丢了清单就在说谎。
	if workPath != "" && len(materialized) > 0 {
		seen := map[string]bool{}
		for _, f := range m.Files {
			if f.Root == ArtifactRootWork {
				seen[f.Rel] = true
			}
		}
		var rescued []ArtifactRef
		for rel := range materialized {
			if seen[rel] {
				continue
			}
			info, err := os.Stat(filepath.Join(workPath, rel))
			if err != nil || !info.Mode().IsRegular() {
				continue // 已被删除/替换成目录: 不进清单
			}
			rescued = append(rescued, ArtifactRef{
				Root: ArtifactRootWork, Rel: rel, Size: info.Size(),
				ModTime: info.ModTime(), Source: ArtifactSourceMaterialize,
			})
		}
		if len(rescued) > 0 {
			m.Files = append(m.Files, rescued...)
			for i := range m.Roots {
				if m.Roots[i].Kind == ArtifactRootWork {
					m.Roots[i].Files += len(rescued)
				}
			}
		}
	}

	sort.Slice(m.Files, func(i, j int) bool {
		if m.Files[i].Root != m.Files[j].Root {
			return m.Files[i].Root < m.Files[j].Root
		}
		return m.Files[i].Rel < m.Files[j].Rel
	})

	m.Scanned = true
	for _, st := range m.Roots {
		if !st.Scanned {
			m.Scanned = false
		}
	}
	if len(problems) > 0 {
		m.Error = strings.Join(problems, "; ")
	}
	m.Empty = m.Scanned && len(m.Files) == 0
	return m
}

// scanArtifactRoot 递归扫描单个根。递归是刚需: media/sub/deep.png 这类嵌套产物
// 用非递归 ReadDir 永远采不到 (dashboard 的 media 清单就栽在这里)。
func scanArtifactRoot(root ArtifactRoot, spec ArtifactScanSpec, shared bool,
	prune map[string]bool, materialized map[string]bool) ([]ArtifactRef, bool, error) {

	var refs []ArtifactRef
	truncated := false
	deadline := time.Now().Add(maxArtifactScanDuration)
	visited := 0
	err := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, err error) error {
		// 每 512 个条目查一次时钟 (逐条查 time.Now 会拖慢遍历本身)
		if visited++; visited%512 == 0 && time.Now().After(deadline) {
			truncated = true
			return fs.SkipAll
		}
		if err != nil {
			// 单个子目录不可读不该让整根变成"未扫": 跳过它继续走完其余部分。
			if d != nil && d.IsDir() && path != root.Path {
				return fs.SkipDir
			}
			return err
		}
		if d.IsDir() {
			if path != root.Path && (artifactSkipDir(d.Name()) || prune[path]) {
				return fs.SkipDir
			}
			return nil
		}
		// 只收普通文件: 符号链接不跟随 (WalkDir 用 lstat, 避免环与越界读),
		// socket/fifo 不是产物。
		if !d.Type().IsRegular() {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root.Path, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		source := ArtifactSourceScan
		if root.Kind == ArtifactRootWork && materialized[rel] {
			source = ArtifactSourceMaterialize
		}
		// 物化过的文件跳过时间窗判据 (平台亲手写下的硬证据优先于时间推断)。
		if shared && source != ArtifactSourceMaterialize &&
			!artifactInAttributionWindow(info.ModTime(), spec.StartedAt, spec.FinishedAt) {
			return nil
		}
		refs = append(refs, ArtifactRef{
			Root: root.Kind, Rel: rel, Size: info.Size(),
			ModTime: info.ModTime(), Source: source,
		})
		if len(refs) >= maxArtifactFiles {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	return refs, truncated, err
}

// relUnderRoot 把 p (相对或绝对) 归一成 root 下的相对路径; 不在 root 内返回 ""。
func relUnderRoot(root, p string) string {
	p = strings.TrimSpace(p)
	if root == "" || p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	rel, err := filepath.Rel(root, filepath.Clean(p))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// isUnderPath 判断 child 是否严格位于 parent 之内 (用于嵌套根剪枝)。
func isUnderPath(parent, child string) bool {
	if parent == "" || child == "" || parent == child {
		return false
	}
	return strings.HasPrefix(child, strings.TrimSuffix(parent, string(os.PathSeparator))+string(os.PathSeparator))
}

// ArtifactSpecForTeam 从活体 team 取采集输入 (持锁快照, 避免与心跳/物化并发读写)。
func ArtifactSpecForTeam(team *ProductionTeam) ArtifactScanSpec {
	if team == nil {
		return ArtifactScanSpec{}
	}
	team.mu.Lock()
	defer team.mu.Unlock()
	since := team.StartedAt
	if since.IsZero() {
		// 还没 Start 过 (或老版本 team.json 无 startedAt) 时用创建时间兜底:
		// 仍是一个真实的时间下界, 比"没有判据整根不扫"更有用。
		since = team.CreatedAt
	}
	return ArtifactScanSpec{
		Team:         team.Name,
		DataDir:      team.dataDir,
		Cwd:          team.Cwd,
		StartedAt:    since,
		FinishedAt:   team.FinishedAt,
		Materialized: append([]string(nil), team.materializedFiles...),
	}
}

// recordMaterialized 累积 MaterializeCode 写下的文件路径 (相对 team.Cwd)。
// 指标只留"物化了几个文件", 路径一丢, 产物清单就只能靠 cwd 侧 mtime 猜归属。
func (t *ProductionTeam) recordMaterialized(files []string) {
	if t == nil || len(files) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.materializedSeen == nil {
		t.materializedSeen = map[string]bool{}
	}
	for _, f := range files {
		f = strings.TrimSpace(f)
		// 同一文件跨轮次会被反复物化 (对抗循环每轮都重写), 入口去重防清单膨胀。
		if f == "" || t.materializedSeen[f] {
			continue
		}
		t.materializedSeen[f] = true
		t.materializedFiles = append(t.materializedFiles, f)
	}
}

// MaterializedFiles 返回本团队物化过的文件路径 (相对 team.Cwd) 快照。
func (t *ProductionTeam) MaterializedFiles() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.materializedFiles...)
}

// WriteArtifactManifest 在 dataDir 下生成 ARTIFACTS.json。
// 与 REPORT.md 同期落盘, 让"这次跑到底产出了什么、有没有采集失败"成为可归档的证据,
// 而不是每个下游各自重扫一遍还各漏一处。
func WriteArtifactManifest(team *ProductionTeam) (string, error) {
	spec := ArtifactSpecForTeam(team)
	if spec.DataDir == "" {
		return "", errors.New("团队无 dataDir, 无法写产物清单")
	}
	m := BuildArtifactManifestFor(spec)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(spec.DataDir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(spec.DataDir, ArtifactManifestFile)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return path, nil
}

// ReadArtifactManifest 读取 dataDir 下的 ARTIFACTS.json。
// 文件不存在时返回 (nil, nil): 调用方据此区分"没有清单(团队还没结束/老版本)"
// 与"清单说零产物"—— 两者不可混同。
func ReadArtifactManifest(dataDir string) (*ArtifactManifest, error) {
	if dataDir == "" {
		return nil, errors.New("dataDir 为空")
	}
	data, err := os.ReadFile(filepath.Join(dataDir, ArtifactManifestFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m ArtifactManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
