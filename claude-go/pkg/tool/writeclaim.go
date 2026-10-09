// writeclaim.go —— 并发写同一文件的显式拒绝 (H3-4)。
//
// # 缺口在哪
//
// 编排层用 conflictKeys / writeScopes 把"写同一个文件的两个 leaf"排成依赖, 于是它们
// 先后执行。但那条串行化是**规划期声明**推出来的, 它挡不住两种残余:
//
//   - 规划漏报: 两个 leaf 声明了不同的目标文件, 实际都改了同一个共享文件 (常量表 /
//     注册表 / 生成的 index)。writeScopes 记的是规划者**以为**的写域。
//   - 声明粒度不够: "file:a.go" 与 "dir:pkg/x" 在字符串上并不相等, 于是两个写者被
//     排到了同一时刻 (见 rawStringSetsOverlap 的集合比较)。
//
// 一旦发生, 后果是**静默覆盖**: 两个子代理各自读到旧内容、各自算好新内容、按到达顺序
// 覆盖写。磁盘上留下一份完整的、语法合法的文件, 里面少了另一个子代理的全部改动 ——
// 没有报错, 没有冲突标记, 事后只能靠 diff 猜。这是长程任务里最难归因的一类损失:
// 两个子代理都报告成功, 验证闸门看到的是一个能编译的文件。
//
// # 为什么是"拒绝"而不是"合并"
//
// 方案 §21 的裁决是显式拒绝。除了 pkg/merge 那个包自己的 Deprecated 声明 (它从未被
// 链进二进制, 说明合并这条路已经被判过一次) 之外, 更实质的理由是**合并的语义正确性
// 无法保证**: 两份对同一段代码的改动合出来的结果可以语法合法而语义错误。那种错比覆盖
// 更难发现 —— 覆盖至少少了一大块东西, 合并出来的是一份"看起来都对"的文本。
// 业界共识同样是"扇出读、串行写"。
//
// # 占用的生命周期 = 写者的一生, 而不是一次写的时长
//
// 这是本机制唯一需要想清楚的设计点。把占用做成"写的那一瞬间加锁"是没用的: 两个写者
// 在不同时刻写同一个文件, 串行地进入临界区, 后一个照样覆盖前一个 —— 那只是让写变成
// 原子的, 不是让冲突变成**可见**的。所以占用从第一次写开始, 到写者结束为止。
//
// 代价是: 一旦占用, 该写者在剩下的生命里都不能再写这个文件, 即使持有者早已不碰它。
// 这是刻意的, 保守的那一侧, 而且它与编排层的串行化是同一个判据的两个位置: 正常规划的
// run 里两个写者本就先后执行 (后者依赖前者), 前者结束时释放占用, 后者**永远看不到
// 冲突**; 只有规划漏掉的那一对才会撞上 —— 而那正是我们想要它撞上的情形。
//
// # 覆盖边界 (诚实记账, 三处都是已知的漏)
//
//   - 只挡模型**显式落盘**的工具 (Write / Edit / NotebookEdit)。经 Bash 的 `sed -i`
//     `> f` 或脚本写入的字节不经过这里。
//   - 只在本进程内有效。跨进程 (两个 claude-go 进程) 没有共享的占用表。这与被保护的
//     对象是一致的: 并行子代理是同一进程里的 goroutine (编排层的 `go func`), 不是
//     两个进程。
//   - 按路径字符串比对 (Clean 之后), **不解析符号链接**。同一个文件的两个软链路径会被
//     当成两个文件。与其在这里做一次 stat (每次写多一次系统调用, 且引入 TOCTOU 窗口),
//     不如把这条漏写下来。
//
// 还有一处刻意的不参与: **主会话循环 (queryLoop) 不登记写者**。它的生命周期是整个
// 会话, 登记之后会把一次早年的写变成对后续所有子代理的长期封锁 —— 用一个必然发生的
// 假阳性换一个偶发的真阳性, 不划算。它那一侧的保护是后续项, 不是这一项。
package tool

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// WriteClaimant 一个写者对一批路径的占用。
//
// 只能经 BeginWriter 构造 —— 于是"占用表里有一个谁也没登记过的持有者"这种状态在
// 类型上就不存在。曾经想过把它做成 ToolContext 上的一个字符串 id, 那样每个消费点都
// 要自己保证"填了 id 就必须配对释放", 而漏填的后果是占用永不释放, 也就是**这个文件
// 在本进程内永久不可写** —— 比它要防的静默覆盖更糟 (覆盖面从"偶发"变成"必发")。
// 指针身份把这条契约从"纪律"变成"类型事实": 拿不到句柄就放不进 ToolContext。
type WriteClaimant struct {
	// label 人读身份 (stage/role/key), **只出现在错误消息里**。判等一律用指针,
	// 于是两个同名 stage 的并行 run 是两个写者 (会被拒绝), 报错时又能说清是谁占着。
	label string
}

// writeClaims 进程内的占用表: 路径 → 持有者。
//
// 键是 Clean 之后的路径。值是**指针**, "是不是我自己占的"用 == 判, 不比字符串。
var writeClaims = struct {
	mu     sync.Mutex
	byPath map[string]*WriteClaimant
}{byPath: make(map[string]*WriteClaimant)}

// BeginWriter 登记一个写者, 返回它的占用句柄。
//
// 调用方**必须** defer Release —— 占用不释放等于该路径在本进程内永久不可写。
func BeginWriter(label string) *WriteClaimant {
	return &WriteClaimant{label: strings.TrimSpace(label)}
}

// Label 返回人读身份 (报错用)。
func (c *WriteClaimant) Label() string {
	if c == nil {
		return ""
	}
	return c.label
}

// Release 释放本写者占用的全部路径 (写者结束时调用)。
//
// 按**持有者指针**扫一遍而不是另记一份"我占过哪些路径"的清单: 两份状态就要保持同步,
// 而漏同步的那一侧会让占用泄漏。表很小 (一次 run 写过的文件数), 扫一遍是 O(路径数)。
func (c *WriteClaimant) Release() {
	if c == nil {
		return
	}
	writeClaims.mu.Lock()
	defer writeClaims.mu.Unlock()
	for path, holder := range writeClaims.byPath {
		if holder == c {
			delete(writeClaims.byPath, path)
		}
	}
}

// Claim 尝试占用 path。返回 nil = 可以写。
//
// 接收者为 nil 时恒返回 nil —— 这是"未装配占用"的表示 (工具被单独调用 / 主会话循环),
// 语义与改造前逐字节相同。nil 不是错误情形, 是**未参与**。
func (c *WriteClaimant) Claim(path string) error {
	if c == nil {
		return nil
	}
	key := filepath.Clean(strings.TrimSpace(path))
	if key == "" || key == "." {
		// 空路径不是一条可以占用的资源: 让它在更下游 (创建目录/写文件) 报它自己的错,
		// 而不是在这里冒充一个"并发冲突"。
		return nil
	}
	writeClaims.mu.Lock()
	defer writeClaims.mu.Unlock()
	if prev, ok := writeClaims.byPath[key]; ok && prev != c {
		return &WriteConflictError{Path: key, Holder: prev.label, Self: c.label}
	}
	writeClaims.byPath[key] = c
	return nil
}

// Holders 返回当前占用的路径数 (诊断/测试用)。**不返回持有者身份** —— 那是别人 run
// 的内部状态, 唯一需要它的地方是报错消息, 而那条路走 WriteConflictError。
func Holders() int {
	writeClaims.mu.Lock()
	defer writeClaims.mu.Unlock()
	return len(writeClaims.byPath)
}

// ClaimWrite 落盘类工具申请写占用 (未装配占用时恒放行)。
//
// 做成 *ToolContext 上的方法而不是让每个工具自己判 tctx.WriteClaim != nil:
// 拿零值 *ToolContext 调工具在测试与内部调用里都很常见, 写成一个方法可以让 nil 接收者
// 也安全 —— 否则每个调用点都要写一遍 `if tctx != nil && tctx.WriteClaim != nil`,
// 而漏掉前一半的那一处会在 nil 上 panic, 且只在"工具被单独调用"时暴露。
func (t *ToolContext) ClaimWrite(path string) error {
	if t == nil {
		return nil
	}
	return t.WriteClaim.Claim(path)
}

// WriteConflictError 一条路径已被另一个仍然活着的写者占用。
type WriteConflictError struct {
	Path string
	// Holder 占用者的身份 (人读)。空串表示占用者没登身份 —— 消息里会显式写成"(未知)"。
	Holder string
	// Self 本次写者自己的身份 (人读)。
	Self string
}

func (e *WriteConflictError) Error() string {
	holder := e.Holder
	if holder == "" {
		holder = "(未知)"
	}
	self := e.Self
	if self == "" {
		self = "(未知)"
	}
	return fmt.Sprintf("路径 %s 已被并行的写者占用 (持有者: %s; 本次: %s)", e.Path, holder, self)
}

// IsWriteConflict 判定一个错误是不是并发写拒绝。
//
// 存在的意义是让上层能把"撞上并行写者"与"写盘失败"分开: 前者要的是重排任务,
// 后者要的是修环境。两者的处置完全不同, 而在 error 字符串上做前缀匹配的判据
// 会在消息被改一次之后静默失效。
func IsWriteConflict(err error) bool {
	var wc *WriteConflictError
	return errors.As(err, &wc)
}
