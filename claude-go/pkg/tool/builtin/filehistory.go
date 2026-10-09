// filehistory.go 实现文件级检查点 (file-level checkpoint)。
//
// 把文件的每一个修订版本留一份裸字节副本, 供事后回退。磁盘布局与 Claude Code
// 的 file-history 逐位一致 (两条都是拿它自己的磁盘快照实测出来的, 不是推断):
//
//	<stateDir>/file-history/<sessionID>/<sha256(absPath)[:16]>@v<N>
//	<stateDir>/file-history/<sessionID>/index.json     path → 版本次账
//
// 命名是**按路径**寻址, 不是按内容: 锚定某会话 manifest.json 的 ref 为
// "21b2927e60d9342c@v2", 而 sha256(该绝对路径)[:16] 恰为 21b2927e60d9342c。
// 按内容寻址会得到另一套名字, 两边目录无法对读。
//
// 版本语义 (同样来自实测): v<N> 存的是该文件第 N 个**修订状态**, 不是"第 N 次
// 写前的状态"。锚定: 对 manifest.json 的一次 Write 之后产生 v2, 其内容 sha 与该
// 次写入的新内容 sha 逐位相同 (cd9de19e2cd0) —— 写入后的状态也被记为版本。
// 于是不变式是: **当前文件内容 == 最高版本的内容**, 回退一步 = 恢复次高版本。
// 记成"只存写前内容"会让最高版本落后于磁盘, 回退语义随之错位。
//
// 由此, 每次写入只新增一个版本而非两个: 写前状态在上一次写入后已经记过, 去重
// 命中即复用。存储量与"每份不同内容存一次"等价。
//
// fail-soft: 存档失败只记日志并放行写入 —— 检查点是旁路能力, 不能因为留备份
// 失败把正常的写文件搞失败。
package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/basedir"
	"github.com/anthropic/claude-go/pkg/logging"
	"github.com/anthropic/claude-go/pkg/tool"
)

// FileHistoryMaxVersionsPerFile 单文件保留的最大修订数 (超出淘汰最旧)。
// CC 全盘 58MB / 1618 版本且无回收; 长会话里这个量只增不减, 故此处封顶。
const FileHistoryMaxVersionsPerFile = 50

// fileHistoryIndexFile 版本次账文件名 (会话目录内)。
const fileHistoryIndexFile = "index.json"

// FileHistoryEntry 单个路径的版本次账。
//
// History 逐条记下该路径的每个版本文件名 —— 版本号虽在文件名里, 但目录里还混着
// 别的文件的版本, 光看目录分不出归属, 淘汰必须按这份清单来。
type FileHistoryEntry struct {
	Ref     string   `json:"ref"`     // 最新版本文件名
	Version int      `json:"version"` // 最新版本号 (单调递增)
	Hash    string   `json:"hash"`    // 最新版本内容 sha256[:16] (去重用)
	History []string `json:"history"` // 该路径全部版本文件名 (升序)
}

// FileHistory 按会话维护文件修订版本。
type FileHistory struct {
	root string // stateDir/file-history

	mu   sync.Mutex
	sess string                      // 当前会话目录名 (空 = 尚未落定)
	idx  map[string]FileHistoryEntry // absPath → 次账
}

// NewFileHistory 以 stateDir 为根创建。stateDir 为空时按 basedir.ResolveDefault
// 规则解析 —— 与 evotools.resolveStateDir 同一套, 保证同项目落在同一处。
func NewFileHistory(stateDir, cwd string) *FileHistory {
	if stateDir == "" {
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		stateDir = basedir.ResolveDefault("", cwd)
	}
	return &FileHistory{root: filepath.Join(stateDir, "file-history")}
}

// 包级实例。由装配方 (CLI/飞书) 经 SetFileHistory 注入。
//
// 走包级注入而非 ToolContext 字段: FileHistory 在 pkg/tool/builtin 而 ToolContext
// 在 pkg/tool, 放进 ToolContext 会形成 tool ↔ builtin 的反向依赖。与 evoTierFactory
// 同一套注入风格。nil = 未启用检查点, 全部调用 fail-open 直通 (老行为)。
var (
	fileHistoryMu sync.RWMutex
	fileHistory   *FileHistory
)

// SetFileHistory 注入检查点实例 (幂等, 后者覆盖前者; 传 nil 关闭)。
func SetFileHistory(h *FileHistory) {
	fileHistoryMu.Lock()
	defer fileHistoryMu.Unlock()
	fileHistory = h
}

// CurrentFileHistory 取当前注入的实例 (可能为 nil)。
func CurrentFileHistory() *FileHistory {
	fileHistoryMu.RLock()
	defer fileHistoryMu.RUnlock()
	return fileHistory
}

// sessionName 会话目录名。为空时退化到进程级 PID —— 这**不**保证一个进程内多
// 会话的隔离 (同进程共用目录), 只保证不跨进程串。宁可目录粗一点, 也不把不同
// 会话的版本混成一份无法归因的乱账。
func (h *FileHistory) sessionName(sessionID string) string {
	if sessionID != "" {
		return sessionID
	}
	return fmt.Sprintf("pid-%d", os.Getpid())
}

// dir 返回当前会话目录, 首次调用时落定、建目录并载入次账。
func (h *FileHistory) dir(sessionID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sess == "" {
		h.sess = h.sessionName(sessionID)
		_ = os.MkdirAll(filepath.Join(h.root, h.sess), 0o755)
		h.idx = h.loadIndexLocked()
	}
	return filepath.Join(h.root, h.sess)
}

// loadIndexLocked 读次账。损坏/缺失一律当空账起步: 最坏后果是版本号从头计数
// (旧版本仍在磁盘上), 而不是阻断写入。调用方须持锁。
func (h *FileHistory) loadIndexLocked() map[string]FileHistoryEntry {
	idx := make(map[string]FileHistoryEntry)
	data, err := os.ReadFile(filepath.Join(h.root, h.sess, fileHistoryIndexFile))
	if err != nil {
		return idx
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		logging.For("tool").Warn("file-history index 损坏, 按空账起步", "err", err)
		return make(map[string]FileHistoryEntry)
	}
	return idx
}

// saveIndexLocked 原子落次账 (临时文件 + rename), 避免写一半崩溃留下半个 JSON。
// 调用方须持锁。
func (h *FileHistory) saveIndexLocked() {
	dir := filepath.Join(h.root, h.sess)
	data, err := json.MarshalIndent(h.idx, "", "  ")
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, fileHistoryIndexFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		logging.For("tool").Warn("file-history index 写入失败", "err", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, fileHistoryIndexFile)); err != nil {
		logging.For("tool").Warn("file-history index 落盘失败", "err", err)
	}
}

// contentHash 内容 → sha256 前 8 字节 (16 hex), 用于版本内容去重。
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// hashPath 绝对路径 → 版本文件名前缀。与 CC 同名规则。
func hashPath(absPath string) string {
	sum := sha256.Sum256([]byte(absPath))
	return hex.EncodeToString(sum[:8])
}

// recordVersion 把 data 记为该路径的下一个修订版本, 返回版本引用与是否新增。
//
// 内容与最新已记版本相同则复用, 不推进版本 —— 这是"每次写入只新增一个版本"
// 的关键: 写入前会调一次 (记下当前状态), 写入后再调一次 (记下新状态), 前者
// 在绝大多数情况下与上一次写入后记下的内容相同, 于是自然去重。
func (h *FileHistory) recordVersion(absPath string, data []byte, sessionID string) (string, bool) {
	sum := contentHash(data)

	h.mu.Lock()
	if h.sess == "" {
		h.mu.Unlock()
		h.dir(sessionID) // 落定会话目录并载入次账
		h.mu.Lock()
	}
	rec := h.idx[absPath]
	if rec.Ref != "" && rec.Hash == sum {
		h.mu.Unlock()
		return rec.Ref, false // 该状态已在档
	}

	dir := filepath.Join(h.root, h.sess)
	next := rec.Version + 1
	if rec.Version == 0 {
		// 次账为空 (首次或损坏重置): 从磁盘上同前缀的既有版本续号, 避免覆盖。
		next = maxExistingVersion(dir, hashPath(absPath)) + 1
	}
	ref := fmt.Sprintf("%s@v%d", hashPath(absPath), next)

	dest := filepath.Join(dir, ref)
	if _, err := os.Stat(dest); err != nil {
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			h.mu.Unlock()
			logging.For("tool").Warn("file-history 版本写入失败", "path", absPath, "err", err)
			return "", false
		}
	}
	rec.Ref, rec.Version, rec.Hash = ref, next, sum
	rec.History = append(rec.History, ref)
	h.idx[absPath] = rec
	h.pruneLocked(dir, absPath)
	h.saveIndexLocked()
	h.mu.Unlock()
	return ref, true
}

// maxExistingVersion 扫描目录里同前缀的版本号取最大。仅用于次账缺失时续号。
func maxExistingVersion(dir, prefix string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	best := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix+"@v") {
			continue
		}
		if n := refVersion(name); n > best {
			best = n
		}
	}
	return best
}

// prunedRefs 累计淘汰次数 (观测用)。淘汰会删掉真正的历史状态, 计数暴露出来便于
// 判断"上限是不是设小了", 而不是让它静默发生。
var prunedRefs = struct {
	sync.Mutex
	n int
}{}

// PrunedRefs 返回本进程累计淘汰的版本数。
func PrunedRefs() int {
	prunedRefs.Lock()
	defer prunedRefs.Unlock()
	return prunedRefs.n
}

// pruneLocked 把该路径的版本数压到上限内, 淘汰最旧。调用方须持锁。
//
// 只动本路径 History 里列出的版本 —— 目录里还有别的文件的版本, 且版本号按路径
// 分配、跨路径不可比, 按号全局淘汰会把别人的当前版本当"旧版本"删掉。
func (h *FileHistory) pruneLocked(dir, path string) {
	rec := h.idx[path]
	if len(rec.History) <= FileHistoryMaxVersionsPerFile {
		return
	}
	drop := rec.History[:len(rec.History)-FileHistoryMaxVersionsPerFile]
	removed := 0
	for _, ref := range drop {
		if os.Remove(filepath.Join(dir, ref)) == nil {
			removed++
		}
	}
	rec.History = rec.History[len(drop):]
	h.idx[path] = rec
	if removed > 0 {
		prunedRefs.Lock()
		prunedRefs.n += removed
		prunedRefs.Unlock()
		logging.For("tool").Info("file-history 淘汰旧版本",
			"path", path, "removed", removed, "keep", FileHistoryMaxVersionsPerFile)
	}
}

// Snapshot 在写入 path **之前**调用: 把文件当前状态记为版本, 返回该版本引用。
//
// 返回空串表示无版本可记 —— 文件不存在, 或存档失败。两种情况调用方都照常写入:
// "文件原本不存在"没有修订状态可留 (回退到不存在由删除语义承担), 存档失败则是
// fail-soft 的既定行为。
func (h *FileHistory) Snapshot(path, sessionID string) string {
	if h == nil || path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	before, err := os.ReadFile(abs)
	if err != nil {
		return "" // 新文件 / 无权限
	}
	ref, _ := h.recordVersion(abs, before, sessionID)
	return ref
}

// Record 在写入 path **成功之后**调用: 把新状态记为版本, 使最高版本始终等于
// 磁盘上的当前内容。失败或未启用时静默返回空串 —— 写本身已经成功了。
func (h *FileHistory) Record(path, sessionID string) string {
	if h == nil || path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	after, err := os.ReadFile(abs)
	if err != nil {
		return ""
	}
	ref, _ := h.recordVersion(abs, after, sessionID)
	return ref
}

// Restore 把 path 恢复为 ref 指向的内容。
//
// ref 为空 / 版本缺失一律报错, 不静默成功 —— 回退失败而谎报成功会直接损坏工作区,
// 这是本能力最不能出的错。
func (h *FileHistory) Restore(path, ref, sessionID string) error {
	if h == nil {
		return fmt.Errorf("file-history 未启用")
	}
	if ref == "" {
		return fmt.Errorf("缺少版本引用")
	}
	if strings.ContainsAny(ref, "/\\") || strings.Contains(ref, "..") {
		return fmt.Errorf("非法版本引用: %q", ref)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(h.dir(sessionID), ref))
	if err != nil {
		return fmt.Errorf("版本 %s 不存在: %w", ref, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, data, 0o644)
}

// Versions 返回 path 的版本引用, 按版本号升序 (最旧在前)。
func (h *FileHistory) Versions(path, sessionID string) []string {
	if h == nil || path == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	h.dir(sessionID)
	h.mu.Lock()
	defer h.mu.Unlock()
	out := append([]string(nil), h.idx[abs].History...)
	sort.Slice(out, func(i, j int) bool { return refVersion(out[i]) < refVersion(out[j]) })
	return out
}

// PreviousVersion 返回"回退一步"应恢复的版本 —— 当前**磁盘内容**所在版本的前一个。
//
// 锚点必须是磁盘内容而不是版本清单的固定下标: 连续回退两次时, 第一次回退把
// 磁盘改回了上一个修订, 此时"前一个版本"应再往前一格。若照清单取倒数第二个,
// 第二次回退会原地打转 (恢复成和现在相同的内容), 看起来像回退成功实则没动。
//
// 磁盘内容不在版本史里 (被外部改过) 时返回空串 —— 没有可安全回退的目标,
// 此时应当报"无版本可退"而不是猜一个。
func (h *FileHistory) PreviousVersion(path, sessionID string) string {
	vers := h.Versions(path, sessionID)
	if len(vers) < 2 {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	cur, err := os.ReadFile(abs)
	if err != nil {
		return ""
	}
	curHash := contentHash(cur)
	dir := h.dir(sessionID)
	for i := len(vers) - 1; i >= 1; i-- {
		data, err := os.ReadFile(filepath.Join(dir, vers[i]))
		if err != nil {
			continue
		}
		if contentHash(data) == curHash {
			return vers[i-1]
		}
	}
	return ""
}

// RefFor 返回 path 当前最新版本引用 (无记录返回空)。
func (h *FileHistory) RefFor(path, sessionID string) string {
	if h == nil || path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	h.dir(sessionID)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.idx[abs].Ref
}

// VersionFor 返回 path 当前版本号 (无记录返回 0)。
func (h *FileHistory) VersionFor(path, sessionID string) int {
	if h == nil || path == "" {
		return 0
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0
	}
	h.dir(sessionID)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.idx[abs].Version
}

// refVersion 解析 "<hash>@v<N>" 的 N。解析失败返回 -1 (排序时置前)。
func refVersion(ref string) int {
	i := strings.LastIndex(ref, "@v")
	if i < 0 {
		return -1
	}
	var n int
	if _, err := fmt.Sscanf(ref[i+2:], "%d", &n); err != nil {
		return -1
	}
	return n
}

// checkpointBeforeWrite 在写入前留版本, 返回引用名 (未启用/无写前状态时为空)。
//
// 调用点纪律: 必须在**所有**拒写分支 (语法护栏/唯一性校验) 之后、os.WriteFile
// **之前** —— 被拒的写入没有产生新修订, 不该在版本史里留下记录。
func checkpointBeforeWrite(filePath string, tctx *tool.ToolContext) string {
	h := CurrentFileHistory()
	if h == nil {
		return ""
	}
	return h.Snapshot(filePath, sessionIDOf(tctx))
}

// checkpointAfterWrite 在写入成功后调用, 让最高版本跟上磁盘。
func checkpointAfterWrite(filePath string, tctx *tool.ToolContext) {
	if h := CurrentFileHistory(); h != nil {
		h.Record(filePath, sessionIDOf(tctx))
	}
}

func sessionIDOf(tctx *tool.ToolContext) string {
	if tctx == nil {
		return ""
	}
	return tctx.SessionID
}
