package hooks

import (
	"strings"

	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// runner_groups.go —— Runner 上的**可装卸 hook 组** (21.8.12 P3「hook 进池」的运行时半边)。
//
// ---------------------------------------------------------------------------
// 它补的是什么
// ---------------------------------------------------------------------------
//
// 改造前 Runner 拿到的 `configs` 是一个**构造后不可变**的切片: 会话开始时由
// config.json 的 hooks 段 (或资产声明) 一次性装进去, 从此只读。这一条是刻意的
// (它让 findHooks 不必加锁), 代价是: "本会话想临时开一条 hook" 在结构上做不到 ——
// 而池的全部语义就是"注册不懒、广告懒", 落到 hook 上就是"**装上是懒的**"。
//
// 所以这里加的是**一层**东西, 不是把 configs 变成可变的:
//
//	基线 (configs)     构造时给定, 之后**一个字节都不动**
//	组   (groups)      运行期可装可卸, 按名字分组
//
// 为什么要分成两层而不是"允许往 configs 里 append": 基线与组的**来源与权限不同**。
// 基线来自操作员的配置 (config.json / 会话装配), 组来自模型经 `pool_load` 的请求。
// 混成一个切片之后, "把模型装的东西全部卸掉" 这件事就没有下手处 —— 而那正是会话
// 结束或治理回退时唯一要走的一步。分层让 `RemoveHookGroup` **结构上**碰不到基线。
//
// ---------------------------------------------------------------------------
// 装卸是真正生效的, 不是读数装饰
// ---------------------------------------------------------------------------
//
// 装上之后的下一次 `findHooks` 就会看到它 —— 也就是说 `pool_load` 一条 PreToolUse
// hook 之后, 本会话后续的工具调用真的会过它。这一点必须成立, 否则"hook 进池"就只是
// 让 pool_search 多列一行: 声明在里面、读数是 wired、而没有任何行为变化。
//
// ---------------------------------------------------------------------------
// 锁是承重的, 不是防御性代码
// ---------------------------------------------------------------------------
//
// configs 之所以可以不加锁, 是因为它**构造后只读**。groups 破了这个前提: 工具调用
// 是**并发批次**执行的 (pkg/tool/orchestration.go 的 runConcurrentBatch, 信号量
// MaxToolUseConcurrency), 每个并发分支都会调 findHooks; 而 pool_load 会在另一个
// goroutine 上改 groups。没有锁, 这是标准的数据竞争 (map 并发读写会直接 fatal)。
//
// 所以 cfgMu **不可以**当成"万一并发呢"的保险删掉 —— 删了在 `-race` 下必然报警,
// 而在不带 race 的构建里表现为偶发的 map 崩溃或读到半更新状态。
//
// 锁的粒度刻意只包住"取匹配列表"这一小段: findHooks 返回的是**值的拷贝**切片,
// 执行 (executeHook, 可能 spawn 进程 / 发 HTTP) 全部发生在锁外。把执行放进锁里会让
// 一条慢 hook 阻塞所有并发工具调用 —— 那是比数据竞争更难查的性能故障。

// groupSinkNames 是组名的规范形: 去空白。空名一律拒绝 —— 空名组会让
// RemoveHookGroup("") 意外卸掉某个组, 而"卸掉了什么"从名字上看不出来。
func normGroupName(name string) string { return strings.TrimSpace(name) }

// InstallHookGroup 装入 (或整体替换) 一组 hook 配置, 返回装入的条数。
//
// **同名的旧组被整体替换**而不是追加: `pool_load` 是幂等的, 重复调用同一条不该把
// 同一条 hook 装上两遍 —— 装上两遍的表现是这条 hook 被执行两次 (PreToolUse 的
// 命令跑两遍、注入的 AdditionalContext 出现两次), 而那看起来像"配置里写了两条"。
//
// 空配置组**不建立**: 装入一个零条的组会让 HasHookGroup 报 true、GroupNames 里
// 多一个名字, 而它没有任何行为 —— 又是一个"读数说装了, 实际什么都没装"。
// 返回 0 表示没装。
func (r *Runner) InstallHookGroup(name string, cfgs []types.HookConfig) int {
	if r == nil {
		return 0
	}
	name = normGroupName(name)
	if name == "" || len(cfgs) == 0 {
		return 0
	}
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	if r.groups == nil {
		r.groups = make(map[string][]types.HookConfig)
	}
	if _, exists := r.groups[name]; !exists {
		r.groupOrder = append(r.groupOrder, name)
	}
	r.groups[name] = append([]types.HookConfig(nil), cfgs...)
	return len(cfgs)
}

// UninstallHookGroup 卸下一组, 返回是否真的卸掉了东西 (false = 本来就没装)。
//
// 返回值必须被调用方用起来: `pool_release` 拿它区分"已卸载"与"没装过", 而这两句
// 对模型是不同的事实 —— "卸掉了"意味着行为变了, "没装过"意味着什么都没发生。
// 一律回同一句"已释放"会让模型以为它关掉了一条一直在跑的 hook。
//
// **只作用于组**: 基线 configs 一个字节都碰不到 (见文件头)。
func (r *Runner) UninstallHookGroup(name string) bool {
	if r == nil {
		return false
	}
	name = normGroupName(name)
	if name == "" {
		return false
	}
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	if _, ok := r.groups[name]; !ok {
		return false
	}
	delete(r.groups, name)
	for i, n := range r.groupOrder {
		if n == name {
			r.groupOrder = append(r.groupOrder[:i], r.groupOrder[i+1:]...)
			break
		}
	}
	return true
}

// HasHookGroup 报告某组是否装着。
func (r *Runner) HasHookGroup(name string) bool {
	if r == nil {
		return false
	}
	name = normGroupName(name)
	if name == "" {
		return false
	}
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	_, ok := r.groups[name]
	return ok
}

// GroupNames 返回已装组名 (装入序)。
//
// 顺序是**稳定**的而不是 map 遍历序: 组内 hook 按装入序执行, 而事件上挂多条 hook 时
// 执行顺序会改变结果 (前一条的 deny 与后一条的 deny 谁先返回)。map 序意味着同一个
// 配置两次运行可能给出不同的行为 —— 与 pkg/agent.roles.go 那个未排序的 Names 是
// 同一类毛病, 不在这里重复。
func (r *Runner) GroupNames() []string {
	if r == nil {
		return nil
	}
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	if len(r.groupOrder) == 0 {
		return nil
	}
	return append([]string(nil), r.groupOrder...)
}

// GroupConfigs 返回某组装着的配置 (副本; 不存在返回 nil)。
func (r *Runner) GroupConfigs(name string) []types.HookConfig {
	if r == nil {
		return nil
	}
	name = normGroupName(name)
	if name == "" {
		return nil
	}
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	cfgs, ok := r.groups[name]
	if !ok {
		return nil
	}
	return append([]types.HookConfig(nil), cfgs...)
}

// BaselineCount 返回**基线**配置的条数 (不含组)。
//
// 与 Count 分开是刻意的: Count 是"这个 Runner 一共挂了几条", 基线数是"配置/装配给了
// 几条"。两个数在读数上回答不同的问题, 而合成一个数之后, "本会话临时装了几条"
// 就得靠减法去猜 —— 猜错的方向恰恰是"以为基线变了"。
func (r *Runner) BaselineCount() int {
	if r == nil {
		return 0
	}
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return len(r.configs)
}

// describeAll 组装 (基线 + 全部组) 的快照, 供 Describe 用。
//
// Index 的含义是"在本 Runner 的**生效序**里的下标" —— 基线在前、组按装入序在后。
// 同一事件上挂多条同名 hook 时它是唯一的区分手段, 所以它必须覆盖组里那些,
// 否则两条来自不同组的同型 hook 会报出同一个 Index。
func (r *Runner) describeAll() []HookSummary {
	if r == nil {
		return nil
	}
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	out := make([]HookSummary, 0, len(r.configs))
	appendOne := func(c types.HookConfig, group string) {
		s := HookSummary{
			Event:        string(c.Event),
			Type:         string(effectiveHookType(c)),
			Index:        len(out),
			HasCondition: c.If != "",
			Timeout:      c.Timeout,
		}
		s.Group = group
		out = append(out, s)
	}
	for _, c := range r.configs {
		appendOne(c, "")
	}
	for _, name := range r.groupOrder {
		for _, c := range r.groups[name] {
			appendOne(c, name)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 编译期断言: 本类型满足 pkg/tool 的装卸接口
// ---------------------------------------------------------------------------
//
// 断言写在**实现方** (这里) 而不是消费方, 理由与文件末尾的 `var _ tool.HookRunner`
// 那条一样: 接口在 pkg/tool 定义 (它不能引 pkg/hooks, 否则成环), 于是"两边的签名
// 漂了"只有在这里才报得出来。漂了的后果是池那边注入 sink 时类型断言失败 ——
// 而那条路径如果写成 `if s, ok := ...; ok {}`, 失败就是**静默不接线**: 池里列着
// hook、加载返回成功、而 Runner 一条都没装上。
//
// 断言放在这里, 编译期就挡住。这也是"接口定义方与实现方必须有一处强制对齐"的
// 具体形态 —— 只有一处断言而没有测试的话, 漂移靠的是运气。
var _ tool.HookGroupSink = (*Runner)(nil)
