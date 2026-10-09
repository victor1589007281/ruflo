// refstore.go — 折叠原文落盘 (文件引用)。
//
// 背景: 折叠 (MicroCompact) 原先只保留原文头 2048 字节, 超出部分**直接丢弃**。
// 模型若需要完整内容, 只能重新执行一次工具 (再花一轮 + 再产生一次同样的输出,
// 又被折掉, 循环)。这里把原文落盘, 让占位可以**引用**全文而不是丢弃它。
//
// 与前缀缓存的关系 (这是设计里最要命的一条):
// 占位文本是 system/messages 前缀的一部分, 一旦同一个 tool_result 在不同轮次
// 折出不同字节, **从该位置起的缓存全废**。所以引用名**必须**是内容的纯函数 ——
// 用 sha256 而不是时间戳/轮次/随机数/序号。名字定死, 占位就定死, 重复折叠产出
// 逐字节相同的文本。
package compact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// ToolResultRefDir 返回折叠原文相对**工作区**的落盘目录。
//
// 选工作区下的子目录而不是系统临时目录: 原文要能被模型的 Read 工具读回, 而
// Read 按工作区解析相对路径; 临时目录还会被系统清理, 留下指向空文件的占位。
//
// **刻意与 pkg/tool 的溢出目录 (artifacts/tool-results) 分开**: 那里是"结果在
// 执行时就超预算"的落点, 这里是"结果执行时没超、后来被压缩期折叠"的落点。
// 分成两个目录, 自指保护才能分别判定 —— 合成一个目录的话, 模型读回溢出 artifact
// 与读回折叠引用无法区分, 而两者的理想行为不同。
func ToolResultRefDir() string {
	return filepath.Join(".claude-go", "tool-results")
}

// ToolResultRefName 返回一条 tool_result 原文的落盘文件名。
//
// **这是纯函数**: 只由内容本身决定 (sha256 前 16 个十六进制字符), 不含时间戳、
// 轮次、调用 id、序号。这条性质是整套文件引用能跟前缀缓存共存的前提 ——
// 同一条 tool_result 无论在哪个回合、被哪条代码路径折叠, 引用的名字都相同,
// 占位字节因而也相同。
//
// 内容寻址还顺带带来两个性质:
//   - 去重: 重复的工具输出 (同一个文件被 Read 两次) 落在同一个文件上, 不涨盘。
//   - 幂等: 重放同一段历史不会产生第二份文件。
func ToolResultRefName(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:8]) + ".txt"
}

// RefDirReadCallIDs 返回 [from, to) 区间内**读了折叠引用目录**的 tool_use 的 id 集合。
//
// 用途是折叠的**自指保护**, 与 pkg/tool 的 readsArtifact 是同一件事在两个落点上的
// 体现: 引用是"把超大结果送出上下文"的目的地, 而读回来的正文**比原文更大** (Read 给
// 每行加了 `%5d|` 行号前缀), 必然再次超过阈值。对这类结果再折一次, 得到的是"指向引用的
// 引用"—— 每读一次多一层, 完整原文永远进不了上下文 (实测: 20770 字节的引用读回后折成
// 21511 字节的新引用, 原文仍不可见), 模型最后只能改用 Bash 绕开, 文件引用白做。
//
// 返回**集合**而不是布尔: 要跳过的是"读引用"这一条结果, 同一批里其它大结果照常折叠并
// 给出引用 —— 整段关掉会让别的 tool_result 白白丢掉回读能力。
//
// 判据取"这次调用的入参路径是否落在引用目录内", 而不是"目录里有没有文件": 后者在折叠
// 发生前就已为真 (上一轮落过盘), 会让落盘从第二轮起自我关闭。
// dirs 是所有"落盘回读目录": 本包折叠引用的目录, 加上 pkg/tool 执行期溢出的目录。
// 两者都要认 —— 模型读哪一份, 那份内容就不该再被折叠吞掉。
func RefDirReadCallIDs(messages []types.Message, from, to int, dirs ...string) map[string]bool {
	var nonEmpty []string
	for _, d := range dirs {
		if strings.TrimSpace(d) != "" {
			nonEmpty = append(nonEmpty, d)
		}
	}
	if len(nonEmpty) == 0 {
		return nil
	}
	if from < 0 {
		from = 0
	}
	if to < 0 || to > len(messages) {
		to = len(messages)
	}
	var ids map[string]bool
	for i := from; i < to; i++ {
		for _, block := range messages[i].Content {
			if block.Type != types.ContentBlockToolUse || block.ID == "" {
				continue
			}
			p := toolUseInputPath(block.Input)
			inAny := false
			for _, d := range nonEmpty {
				if jsonPathInDir(p, d) {
					inAny = true
					break
				}
			}
			if !inAny {
				continue
			}
			if ids == nil {
				ids = make(map[string]bool)
			}
			ids[block.ID] = true
		}
	}
	return ids
}

// toolUseInputPath 从 tool_use 入参里取盘上路径。
//
// 字段名与 pkg/tool.toolInputPath 保持一致 —— 那里是执行期溢出的判据入口, 两处若
// 分叉, 就会出现"执行期认得出、折叠期认不出"的半保护 (pkg/tool 不能反向 import 本包,
// 会成环, 所以这份名单只能复制)。
func toolUseInputPath(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"path", "file_path", "notebook_path", "file"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// jsonPathInDir 判断一个路径是否落在 dir 目录内 (含目录自身)。
//
// 绝对路径走 filepath.Rel 判包含, 这样 `.claude-go/tool-results-evil` 这类兄弟前缀
// 不会被误判成目录内; 两者都相对时退化为字符串前缀比较 (同工作区下的常见形态)。
func jsonPathInDir(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	cp, cd := filepath.Clean(p), filepath.Clean(dir)
	if !filepath.IsAbs(cp) || !filepath.IsAbs(cd) {
		return cp == cd || strings.HasPrefix(cp, cd+string(filepath.Separator))
	}
	rel, err := filepath.Rel(cd, cp)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ToolResultRefStore 折叠原文的落盘存储。
//
// 零值不可用; 用 NewToolResultRefStore 构造。**(nil) 方法全部安全** —— 调用方
// 不必到处判空, nil 存储的语义是"不落盘, 退化为纯截断"。
type ToolResultRefStore struct {
	dir string

	// spillDir 是 pkg/tool 执行期溢出的落盘目录, 仅用于自指保护的判定 (本存储不写它)。
	//
	// 由工作区推导而不是额外传参: 两个目录同源于同一个 cwd, 分开传就会出现
	// "落盘用 A、判定用 B" 的错配 —— 而错配是静默的, 保护只是不生效。
	spillDir string

	// mu 保护下面两个字段。Put 会被并发的 run 调用 (各自持有 hook, 但指向同一个
	// 工作区目录), 而 issued 是一张只增不删的 map —— 不加锁读会撞上写。
	mu sync.Mutex
	// issued 本进程发出过的引用名 (见 refsweep.go: 这些是**活引用**, 清理不碰)。
	issued map[string]bool
	// lastSweep 最近一次清理的读数 (Ran=false 表示本存储还没扫过)。
	lastSweep RefSweepStats
}

// NewToolResultRefStore 创建落盘存储。dir 为空 (或纯空白) 时返回 nil ——
// 没配目录就一个字节都不写, 调用方据此走纯截断分支。
func NewToolResultRefStore(dir string) *ToolResultRefStore {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	return &ToolResultRefStore{dir: dir}
}

// NewToolResultRefStoreForCwd 由**工作区路径**创建落盘存储, 并一并记下
// pkg/tool 的溢出目录供自指保护判定。
//
// 这是生产路径该用的构造器 (引擎只掌握 cwd)。NewToolResultRefStore 保留给
// 直接指定目录的测试场景, 那种情况下 spillDir 为空, 只保护折叠引用目录。
func NewToolResultRefStoreForCwd(cwd string) *ToolResultRefStore {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil
	}
	return &ToolResultRefStore{
		dir:      filepath.Join(cwd, ToolResultRefDir()),
		spillDir: tool.ToolResultArtifactDir(cwd),
	}
}

// RefDirs 返回自指保护要认的全部回读目录 (nil 安全)。
func (s *ToolResultRefStore) RefDirs() []string {
	if s == nil {
		return nil
	}
	return []string{s.dir, s.spillDir}
}

// Dir 返回落盘目录 (nil 安全, nil 返回空串)。
func (s *ToolResultRefStore) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Path 返回引用文件的可读路径 (nil 安全)。这是占位文本里要展示的路径。
func (s *ToolResultRefStore) Path(name string) string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.dir, name)
}

// Put 写入原文, 返回引用文件名。
//
// 幂等: 名字是内容的纯函数, 同一条 tool_result 重复写落在同一路径同一字节,
// 不产生第二份文件, 也不改动既有文件。
//
// 权限: 目录 0o700 / 文件 0o600。工具结果里经常带凭据与隐私数据 (Read 一个
// .env、cat 一个配置文件都会走到这里), 默认的 0o644 会让同宿主机上其它用户
// 可读 —— 比折叠前"只存在于进程内存"的状态更差, 所以收紧。
//
// nil 存储返回错误而不是静默成功: 调用方 (ToolResultStubWithRef) 靠 err 判断
// "没给出引用", 返回 nil 错误会让它写出一条指向空路径的占位。
func (s *ToolResultRefStore) Put(content string) (string, error) {
	if s == nil {
		return "", os.ErrInvalid
	}
	name := ToolResultRefName(content)
	path := filepath.Join(s.dir, name)

	// **先记账再落盘**: 从这一刻起这个名字就是"发出去了"。顺序不是细节 ——
	// 反过来的话, 写盘成功与记账之间有一个窗口, 并发的清理可能把刚写出去、马上
	// 要被占位引用的文件删掉。先记账最多多保护一个写失败的名字 (无害: 它本来
	// 就不存在, 保护一个不存在的名字不产生任何动作)。
	s.markIssued(name)

	// 已存在**且字节数一致**就直接复用。
	//
	// 字节数相等 + 名字是内容哈希 => 内容相同 (sha256 碰撞在 64 位截断下对
	// 折叠场景不构成现实风险)。省掉重复写盘, 也避免改动既有文件的 mtime。
	if fi, err := os.Stat(path); err == nil && fi.Size() == int64(len(content)) {
		return name, nil
	}

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	// 清理排在写盘**之后**: 目录刚多了一条, 现在扫才能把这次增长算进去。
	// 路径上没有任何地方会回头碰这条刚写完的文件 —— issued 已经把它挡住了。
	maybeSweepRefs(s)
	return name, nil
}
