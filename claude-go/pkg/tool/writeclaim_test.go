// writeclaim_test.go —— H3-4 并发写拒绝的验收。
//
// 方案 §21 的验收原文是:
//
//	**「两个子代理并行写同一文件 ⇒ 结果要么两者都保留 (合并)、要么第二个被明确
//	拒绝并报错; 不允许静默覆盖。」**
//
// 本方案采纳的是第二个分支 (理由见 writeclaim.go 顶部)。所以判据是**两条一起**:
//
//  1. 第二个写者**被拒绝并拿到错误** —— 只有这一条还不够: 一个"先写再报错"的守卫
//     同样满足它, 而磁盘上已经是覆盖后的内容了;
//  2. **第一个写者的内容仍然在磁盘上** —— 这条才排除静默覆盖。文件内容的断言在
//     filewrite_test.go 那边 (那里才有"磁盘上是什么"这个读数), 本文件判的是占用
//     语义本身。
//
// # 占用的反面同样要判
//
// 一个"什么写都拒绝"的实现能让上面两条全绿, 而它会把并行编辑彻底废掉。所以每条
// 拒绝臂都配一条**必须放行**的臂: 同一写者重复写、不同路径、释放之后、以及未装配
// 占用的普通调用 (改造前的行为) —— 后者是回归臂, 少了它这个能力就从"加了一层保护"
// 变成"改了所有人的默认语义"。
package tool

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestWriteClaim_第二个写者被拒 最核心的一条: A 占住的路径, B 申请即被拒。
func TestWriteClaim_第二个写者被拒(t *testing.T) {
	a := BeginWriter("stage=a")
	defer a.Release()
	b := BeginWriter("stage=b")
	defer b.Release()

	path := filepath.Join(t.TempDir(), "shared.go")
	if err := a.Claim(path); err != nil {
		t.Fatalf("第一个写者不该被拒: %v", err)
	}
	err := b.Claim(path)
	if err == nil {
		t.Fatal("第二个写者占同一路径必须被拒 —— 放行就是静默覆盖的入口")
	}
	if !IsWriteConflict(err) {
		t.Fatalf("拒绝必须是 WriteConflictError (上层据此重排任务, 而不是修环境), 实得 %T: %v", err, err)
	}
	// 报错要说清**是谁占着, 我是谁** —— 只说"冲突"的报错没法让人去重排任务。
	msg := err.Error()
	if !strings.Contains(msg, "stage=a") || !strings.Contains(msg, "stage=b") {
		t.Fatalf("冲突消息里应同时出现持有者与本次写者身份, 实得: %s", msg)
	}
}

// TestWriteClaim_同一写者重复写放行.
//
// 这是最要紧的反面臂。一个 coder 在 20 轮里反复 Edit 同一个文件是**常态**,
// 若把它也判成冲突, 这个能力一上线就会把正常的长程任务打死 —— 而且打死的方式
// 是"模型每次编辑都被拒", 看起来像模型出错。
func TestWriteClaim_同一写者重复写放行(t *testing.T) {
	a := BeginWriter("stage=a")
	defer a.Release()
	path := filepath.Join(t.TempDir(), "own.go")

	for i := 0; i < 5; i++ {
		if err := a.Claim(path); err != nil {
			t.Fatalf("第 %d 次重复占用自己被占的路径必须放行, 实得: %v", i+1, err)
		}
	}
}

// TestWriteClaim_不同路径互不影响 扇出读的并行性靠这条活着。
func TestWriteClaim_不同路径互不影响(t *testing.T) {
	dir := t.TempDir()
	a := BeginWriter("stage=a")
	defer a.Release()
	b := BeginWriter("stage=b")
	defer b.Release()

	if err := a.Claim(filepath.Join(dir, "a.go")); err != nil {
		t.Fatalf("a 占 a.go 失败: %v", err)
	}
	if err := b.Claim(filepath.Join(dir, "b.go")); err != nil {
		t.Fatalf("两个写者写不同文件不该冲突 (否则并行编辑全废): %v", err)
	}
}

// TestWriteClaim_路径规范化后比对.
//
// "./x.go" 与 "x.go" 与 "x.go/" 是同一个文件。不规范化的话, 两个写者绕一下路径
// 就能各占一份、双双放行 —— 这条守卫的成本是一次 Clean, 而它挡掉的正是"换个写法
// 就能躲开"的失效。
func TestWriteClaim_路径规范化后比对(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "norm.go")

	a := BeginWriter("stage=a")
	defer a.Release()
	b := BeginWriter("stage=b")
	defer b.Release()

	if err := a.Claim(target); err != nil {
		t.Fatalf("a 占用失败: %v", err)
	}
	// 同一文件的几种写法, 逐条都必须被认成同一个路径。
	for _, alt := range []string{
		filepath.Join(dir, ".", "norm.go"),
		filepath.Join(dir, "sub", "..", "norm.go"),
	} {
		if err := b.Claim(alt); err == nil {
			t.Fatalf("路径 %q 与 %q 是同一个文件, 换个写法就放行等于守卫可绕过", alt, target)
		}
	}
}

// TestWriteClaim_释放后可被他人占用 写者结束必须让位, 否则串行执行的后继任务永远写不了。
func TestWriteClaim_释放后可被他人占用(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.go")

	a := BeginWriter("stage=a")
	if err := a.Claim(path); err != nil {
		t.Fatalf("a 占用失败: %v", err)
	}
	b := BeginWriter("stage=b")
	defer b.Release()
	if err := b.Claim(path); err == nil {
		t.Fatal("a 还活着时 b 不该能占")
	}

	a.Release()

	if err := b.Claim(path); err != nil {
		t.Fatalf("a 已释放, b 必须能占 (否则编排层串行化过的后继任务全被锁死): %v", err)
	}
}

// TestWriteClaim_Release只释放自己的.
//
// 一次 Release 把整张表清空是个很容易写出来的实现 (遍历删全部), 而它会让
// 另一个仍在跑的写者的占用凭空消失 —— 那之后它的路径对所有人开放, 静默覆盖
// 又回来了, 且这次连冲突都不会报。
func TestWriteClaim_Release只释放自己的(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	bPath := filepath.Join(dir, "b.go")

	a := BeginWriter("stage=a")
	b := BeginWriter("stage=b")
	defer b.Release()

	if err := a.Claim(aPath); err != nil {
		t.Fatalf("a 占用失败: %v", err)
	}
	if err := b.Claim(bPath); err != nil {
		t.Fatalf("b 占用失败: %v", err)
	}

	a.Release()

	// b 的占用必须还在: 第三个写者碰 b 的路径应当仍被拒。
	c := BeginWriter("stage=c")
	defer c.Release()
	if err := c.Claim(bPath); err == nil {
		t.Fatal("a 的 Release 把 b 的占用也清掉了 —— 那之后 b 的路径对所有人开放")
	}
	// a 的占用必须没了。
	if err := c.Claim(aPath); err != nil {
		t.Fatalf("a 已释放, c 应能占: %v", err)
	}
}

// TestWriteClaim_未装配时恒放行 回归臂: nil 占用 = 改造前的行为。
//
// 主会话循环 (queryLoop) 刻意不登记写者, 工具被单独调用时也没有引擎装配。
// 这两条路都必须与改造前逐字节相同 —— 否则这个改动就从"给并行子代理加保护"
// 变成"给所有人改了默认语义"。
func TestWriteClaim_未装配时恒放行(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nil.go")

	var nilClaim *WriteClaimant
	if err := nilClaim.Claim(path); err != nil {
		t.Fatalf("nil 占用者必须放行 (未装配 = 未参与), 实得: %v", err)
	}
	// 零值 ToolContext 也一样 —— 走方法而不是在每个调用点判 nil 就是为了这条。
	var nilCtx *ToolContext
	if err := nilCtx.ClaimWrite(path); err != nil {
		t.Fatalf("零值 *ToolContext 的 ClaimWrite 必须放行, 实得: %v", err)
	}
	(&ToolContext{}).ClaimWrite(path) // WriteClaim 为 nil: 同样放行
}

// TestWriteClaim_空路径不进表.
//
// 空路径不是一条可以占用的资源, 而把它放进表里会让**所有**空路径的写入互相冲突
// (它们都映射到同一个键), 报出的错还是"并发冲突"这种误导性的原因。
func TestWriteClaim_空路径不进表(t *testing.T) {
	before := Holders()
	a := BeginWriter("stage=a")
	defer a.Release()
	b := BeginWriter("stage=b")
	defer b.Release()

	for _, p := range []string{"", "   ", "."} {
		if err := a.Claim(p); err != nil {
			t.Fatalf("空路径 %q 不该报冲突: %v", p, err)
		}
		if err := b.Claim(p); err != nil {
			t.Fatalf("空路径 %q 对第二个写者也不该报冲突: %v", p, err)
		}
	}
	if got := Holders(); got != before {
		t.Fatalf("空路径不该进占用表: 之前 %d 条, 现在 %d 条", before, got)
	}
}

// TestWriteClaim_并发申请恰好一个赢 race 下占用表的正确性。
//
// 这一条是给 -race 与互斥本身用的: 32 个 goroutine 抢同一个路径, 必须**恰好一个**
// 拿到, 其余全部被拒。多一个或少一个都说明表在竞争下算错了 —— 少一个尤其危险
// (两个写者同时以为自己独占)。
func TestWriteClaim_并发申请恰好一个赢(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contended.go")
	const n = 32

	var (
		mu       sync.Mutex
		winners  int
		conflict int
		other    []error
		start    = make(chan struct{})
		wg       sync.WaitGroup
	)
	claims := make([]*WriteClaimant, n)
	for i := range claims {
		claims[i] = BeginWriter("writer")
	}
	defer func() {
		for _, c := range claims {
			c.Release()
		}
	}()

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(c *WriteClaimant) {
			defer wg.Done()
			<-start
			err := c.Claim(path)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case IsWriteConflict(err):
				conflict++
			default:
				other = append(other, err)
			}
		}(claims[i])
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("出现了非冲突类错误: %v", other)
	}
	if winners != 1 {
		t.Fatalf("同一路径必须恰好一个写者拿到占用, 实得 %d 个 (多于 1 = 两个写者都以为独占, 正是要防的)", winners)
	}
	if conflict != n-1 {
		t.Fatalf("其余 %d 个都应被明确拒绝, 实得 %d 个", n-1, conflict)
	}
}

// TestWriteConflictError_可被 errors.As 拆出 上层要靠类型分派, 不靠字符串。
func TestWriteConflictError_可被errorsAs拆出(t *testing.T) {
	dir := t.TempDir()
	a := BeginWriter("held")
	defer a.Release()
	b := BeginWriter("me")
	defer b.Release()

	if err := a.Claim(filepath.Join(dir, "x.go")); err != nil {
		t.Fatalf("a 占用失败: %v", err)
	}
	err := b.Claim(filepath.Join(dir, "x.go"))

	var wc *WriteConflictError
	if !errors.As(err, &wc) {
		t.Fatalf("应从错误里拆出 *WriteConflictError, 实得 %T", err)
	}
	if wc.Holder != "held" || wc.Self != "me" {
		t.Fatalf("冲突错误应带上双方身份, 实得 holder=%q self=%q", wc.Holder, wc.Self)
	}
	if !IsWriteConflict(err) {
		t.Fatal("IsWriteConflict 应为真")
	}
	// 反向: 一个寻常的 I/O 错误不能被认成冲突 (否则上层会把环境问题当成并发问题重排)。
	if IsWriteConflict(errors.New("permission denied")) {
		t.Fatal("普通错误不该被判成写冲突")
	}
}

// TestWriteClaimant_无身份时消息不空
//
// 编排层某些路径 (CLI 单跑) 没有 stage/team, Label 会是空的。消息里必须写成
// "(未知)" 而不是留白 —— 留白会让报错读成"路径 X 已被 占用", 看不出是谁。
func TestWriteClaimant_无身份时消息不空(t *testing.T) {
	dir := t.TempDir()
	a := BeginWriter("")
	defer a.Release()
	b := BeginWriter("")
	defer b.Release()

	if err := a.Claim(filepath.Join(dir, "anon.go")); err != nil {
		t.Fatalf("a 占用失败: %v", err)
	}
	err := b.Claim(filepath.Join(dir, "anon.go"))
	if err == nil {
		t.Fatal("无身份不影响占用判定 (判等用指针), b 应被拒")
	}
	if !strings.Contains(err.Error(), "(未知)") {
		t.Fatalf("无身份时应显式写成 (未知), 实得: %s", err)
	}
}
