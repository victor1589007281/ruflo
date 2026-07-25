// Package graph 实现 design/01《统一 Agent 编排引擎》§4 的图引擎内核 v1 (M1 范围):
// 纯数据 GraphSpec (§4.1-4.2) + 事件溯源 Journal (§4.3) + 节点级 Loop (§4.4)
// + Hook 总线最小版 (§4.5) + ready-set 并行调度器 (§4.3)。
//
// 设计约束:
//   - 仅依赖标准库 + pkg/trace; 不 import pkg/agent (防循环依赖),
//     节点执行经注入的 NodeRunner 接口 (§4.9 的本地化前身)。
//   - 图是纯数据, 可 JSON 序列化; prompt 占位符 ({objective}/{prev_result}/
//     {node.<id>}) 的替换由 runner 负责, 引擎只透传。
package graph

// GraphSpec 图的纯数据描述 (design/01 §4.1)。可 JSON 序列化, 引擎对其只读。
type GraphSpec struct {
	Name     string        `json:"name"`
	Version  string        `json:"version,omitempty"`
	Nodes    []NodeSpec    `json:"nodes"`
	Edges    []EdgeSpec    `json:"edges"`
	Policies GraphPolicies `json:"policies,omitempty"`
	Meta     GraphMeta     `json:"meta,omitempty"`
}

// GraphMeta 声明式门禁元数据 (design/01 §4.1)。
// 继承 workflow.go 的教训: 门禁凭元数据不凭工作流名白名单。
type GraphMeta struct {
	ProducesCode bool   `json:"produces_code,omitempty"` // 是否产出代码 (取代按工作流名白名单)
	QualityGate  string `json:"quality_gate,omitempty"`  // "content"|"none"
}

// GraphPolicies 图级默认策略 (design/01 §4.1)。
type GraphPolicies struct {
	MaxParallel  int          `json:"max_parallel,omitempty"`  // 并发上限, 0=默认4
	DefaultRetry *RetryPolicy `json:"default_retry,omitempty"` // 节点未声明 Retry 时的图级默认
	// MaxTotalNodes 一次运行的**运行图**节点总数上限 (含动态展开产物与 map 分片)。
	// 0 → DefaultMaxTotalNodes。这是"有界展开"的最后一道闸 (design/01 §4.2):
	// 单个 ExpandSpec 的深度/条数上限只能约束一次展开, 挡不住"每层都只加 3 个
	// 但加了 20 层"这种由 LLM 产出驱动的累积膨胀。
	MaxTotalNodes int `json:"max_total_nodes,omitempty"`
}

// NodeKind 节点形态 (design/01 §4.1 全表 8 种)。
//
// 已实现: agent | gate | map | reduce | loop-group。
// 未实现: router | subgraph | human —— Validate 对它们**显式报错**而不是静默接受
// (静默接受等于把一个什么都不做的节点混进图里, 表现为"整条分支莫名 skipped")。
type NodeKind string

const (
	NodeKindAgent NodeKind = "agent" // LLM agent 节点
	NodeKindGate  NodeKind = "gate"  // 门禁节点 (确定性代码或 LLM 评分, 输出 score)

	// —— 扇出/汇聚 (§4.2 map/reduce) ——
	NodeKindMap    NodeKind = "map"    // 按集合切分并发派生 N 个同构分片
	NodeKindReduce NodeKind = "reduce" // 等其 map 组全部终态后聚合
	// —— 组级循环 (§4.4) ——
	NodeKindLoopGroup NodeKind = "loop-group" // 一组节点 (内嵌子图) 整体循环

	// 以下 Kind 为 design/01 §4.1 全表预留, 尚未实现, Validate 明确拒绝。
	NodeKindRouter   NodeKind = "router"
	NodeKindSubgraph NodeKind = "subgraph"
	NodeKindHuman    NodeKind = "human"
)

// NodeSpec 节点描述 (design/01 §4.1)。所有 Kind 共享同一 Agent 载体。
type NodeSpec struct {
	ID    string       `json:"id"`
	Kind  NodeKind     `json:"kind"`
	Agent AgentSpec    `json:"agent"`
	Loop  *LoopPolicy  `json:"loop,omitempty"`  // 节点级循环 (§4.4)
	Retry *RetryPolicy `json:"retry,omitempty"` // 唯一一层重试 (§4.3 重试单层化)
	// TimeoutSec 节点**总预算**(秒), 0=不限 (由 runner 自己的角色级超时兜底)。
	// 注意作用域: 引擎在 retry 环与 loop 环**之外**施加这个 deadline (engine.go
	// execNode), 即它包住"全部重试 + 全部循环轮次"的墙钟总时长, 不是单次尝试的
	// 上限——填单次尝试的超时会让第二次重试必然撞墙。
	TimeoutSec int `json:"timeout_sec,omitempty"`

	// Map 仅 Kind=="map" 有效: 分片来源与切分策略 (§4.2)。
	Map *MapPolicy `json:"map,omitempty"`
	// Reduce 仅 Kind=="reduce" 有效: 聚合来源与策略 (§4.2)。
	Reduce *ReducePolicy `json:"reduce,omitempty"`
	// Group 仅 Kind=="loop-group" 有效: 组内子图 + 组级循环 (§4.4)。
	Group *GroupPolicy `json:"group,omitempty"`
	// Spawn 声明本节点**可以**派生子图 (§4.8 SpawnSubgraph)。
	// 未声明时 NodeInput.Spawn 为 nil —— 授权缺失表现为"没有这个能力", 而不是
	// 调用了才报错。与 Expand 同一原则: 派生能力必须由图显式授予, 否则任何 runner
	// 都能凭工具调用往编排里塞执行单元。
	Spawn *SpawnSpec `json:"spawn,omitempty"`
	// Expand 声明本节点**可以**动态展开子图 (§4.2)。
	// 未声明时 runner 返回的 Expansion 被引擎忽略 —— 展开能力必须由图显式授予,
	// 否则任何 runner 都能凭产出往运行图里塞节点。
	Expand *ExpandSpec `json:"expand,omitempty"`
}

// —— map/reduce (design/01 §4.2 "对集合展开 N 个并行子节点 / 聚合") ——

// 分片切分策略 (MapPolicy.Split)。
const (
	SplitLines     = "lines"      // 按行 (去空行), 默认
	SplitParagraph = "paragraphs" // 按空行分段
	SplitJSONArray = "json_array" // 解析为 JSON 数组 (元素为字符串 / 对象则原样序列化)
	SplitWhole     = "whole"      // 整体作为唯一分片 (退化为 1 分片, 便于灰度对比)
)

// 分片来源 (MapPolicy.Source)。前缀式来源见 SourcePrevPrefix / SourceParamPrefix。
const (
	SourcePrev        = "prev"      // 全部 completed 上游中最长的那份产出 (默认)
	SourceObjective   = "objective" // 整图目标
	SourcePrevPrefix  = "prev:"     // prev:<节点ID> 指定上游
	SourceParamPrefix = "param:"    // param:<键> 取 RunOpts.Params
)

// MapPolicy map 节点的扇出策略 (design/01 §4.2)。
//
// 引擎只做**确定性**切分: 集合从哪来 (Source)、怎么切 (Split)、最多切几片
// (MaxShards)。分片内容如何进 prompt 由 runner 负责 (引擎经 NodeInput.Shard 透传),
// 与"占位符替换归 runner"的既有分工一致。
type MapPolicy struct {
	Source string `json:"source,omitempty"` // 见 Source* 常量, 空=prev
	Split  string `json:"split,omitempty"`  // 见 Split* 常量, 空=lines
	// MaxShards 分片数硬上限, 必填 >0, Validate 强制。
	// 为什么必填: 集合来自上游 LLM 产出, "一行一片"遇到 800 行的产出就是 800 次
	// LLM 调用。超出上限的部分被丢弃并记 journal (map.expanded 带 truncated)。
	MaxShards int `json:"max_shards"`
	// MinShards 分片数下限 (0=不限)。切出的分片少于此数即整节点 failed,
	// 用于"必须至少扇出 N 路"的评审团/集成抽取场景。
	MinShards int `json:"min_shards,omitempty"`
}

// 聚合策略 (ReducePolicy.Strategy)。
const (
	ReduceRunner  = "runner"  // 默认: 分片经 NodeInput.Shards 交给 runner 聚合 (LLM)
	ReduceConcat  = "concat"  // 确定性: 按分片序拼接 (零 LLM)
	ReduceLongest = "longest" // 确定性: 取最长分片 (零 LLM)
	// ReduceVote 确定性: 把每个分片产出解析为「节点/边」抽取文档, 按同名节点与
	// 同 (src,dst,type) 边投票融合, confidence = 命中路数/有效路数 (零 LLM)。
	// 这是 ensemble_extract 的融合算法 (pkg/agent/workflow_ensemble.go fuseExtractions)
	// 搬进内核后的形态, 使 map[N 路抽取]→reduce(投票) 成为真 map→reduce。
	ReduceVote = "vote"
	// ReduceTrimmedMean 确定性: 把每个分片产出解析为「多维评审」文档, 每个维度做
	// 截尾均值 (剔除最高最低后取均值, 抗单个离群评审), overall 取中位数,
	// 共识度 = 1-平均极差/100, 批注取并集 (零 LLM)。
	// 对应 review_panel 的 fuseReviews。**样本不足时不给数**, 见 ReducePolicy.MinSamples。
	ReduceTrimmedMean = "trimmed_mean"
)

// 截尾均值的样本下限缺省值 (ReducePolicy.MinSamples 未声明时)。
//
// 为什么只有 trimmed_mean 有非 1 的缺省: 样本 < 3 时"剔除最高最低后取均值"这个
// 操作根本执行不了 (剔完就空了), 它会静默退化成**另一种算法**(普通均值), 抗离群
// 这个它唯一存在理由的性质消失, 但产出的数字看起来完全正常, 下游 `score >= N`
// 的条件边分辨不出来。vote 不同: 它的 confidence 分母就是有效样本数, 2 路投票得出
// 的 1.0 自述为"两路都认", 不会伪装成更强的证据, 故缺省下限为 1。
const (
	DefaultVoteMinSamples        = 1
	DefaultTrimmedMeanMinSamples = 3
)

// ReducePolicy reduce 节点的聚合策略 (design/01 §4.2)。
type ReducePolicy struct {
	// From 指定聚合哪些 map 节点 (空=全部 map 类直接前驱)。
	From []string `json:"from,omitempty"`
	// Strategy 见 Reduce* 常量, 空=runner。
	// concat/longest 是**确定性**策略: 引擎直接算出结果, 零 LLM 调用, 但仍走完整
	// 节点生命周期 (hook/journal/预算), 与 design/01 §4.1 对 Deterministic 的要求一致。
	Strategy string `json:"strategy,omitempty"`
	// Separator concat 分隔符, 空=DefaultReduceSeparator。
	Separator string `json:"separator,omitempty"`
	// RequireAll true=任一分片非 completed 即 reduce 失败;
	// false (默认)=尽力聚合已成功的分片 (扇出场景里单片失败不该毁掉整轮)。
	RequireAll bool `json:"require_all,omitempty"`
	// MinSamples 融合所需的**有效样本**下限 (仅 vote/trimmed_mean 生效, 0=取策略缺省:
	// vote=1, trimmed_mean=3)。有效样本 = completed 且能解析出该策略所需 schema 的分片
	// —— 与 RequireAll 是两把不同的闸: RequireAll 管"分片执行是否全成功",
	// MinSamples 管"融合是否有足够的独立观测"。一个 completed 但吐了散文的分片对
	// RequireAll 是成功的, 对融合是零。
	//
	// 不满足下限时 reduce **failed 并在 Err 里写清样本数**, 不降级出数:
	// 降级出的数看起来正常, 但条件边与平台都无从分辨它没有抗离群/无人佐证。
	// 需要低样本也出数的图必须显式写小 MinSamples —— 那时产出 JSON 里带
	// samples/trimmed 字段自述, 责任在图作者。
	MinSamples int `json:"min_samples,omitempty"`
}

// GroupPolicy loop-group 组级循环 (design/01 §4.4)。
//
// 与节点级 Loop 的边界 (谁管谁), 这是本形态最容易搞混的地方:
//   - 节点级 Loop: **单个节点**自我迭代, Until 对该节点自己的结果求值,
//     journal 记 loop.iteration。典型 = 同一 agent 反复自评改写。
//   - loop-group: **一组节点**(组内保持 DAG 语义) 整体反复, Until 对组产出节点
//     (ResultFrom) 的结果求值, journal 记 loop.group.iteration。典型 = 对抗模式
//     "生成→评审"两节点整轮重来。
//
// 两者**不允许并存于同一节点** (Validate 强制): 组节点自己再套一层 Loop 会让轮次
// 相乘 (MaxIterations×MaxIterations), 预算无法推理。组内也不允许再嵌套 loop-group,
// 同理。
type GroupPolicy struct {
	// Nodes/Edges 组内子图。内嵌 (而非引用外部节点 ID) 是刻意的: 组成员若是顶层
	// 节点, 顶层 Validate 的"入口可达 / 无环"检查会把它们当孤岛报错, 且调度器会
	// 把它们当普通节点各跑一次。内嵌后组内子图按同一套规则**递归校验**。
	Nodes []NodeSpec `json:"nodes"`
	Edges []EdgeSpec `json:"edges,omitempty"`
	// Loop 组级循环策略 (MaxIterations 必填 >0; Until 对 ResultFrom 结果求值;
	// Feedback 的 {prev_output} 替换为上一轮 ResultFrom 的产出, 下发给**全部**成员)。
	Loop LoopPolicy `json:"loop"`
	// ResultFrom 组产出取哪个成员节点。空 = 组内唯一出度 0 节点;
	// 有多个出度 0 节点时 Validate 强制显式声明 (否则"组产出是谁"取决于声明顺序,
	// 是个隐蔽的不确定性)。
	ResultFrom string `json:"result_from,omitempty"`
}

// ExpandSpec 动态展开授权与边界 (design/01 §4.2)。
//
// 有界是硬要求: 展开的内容来自 LLM 产出 (GoalTree HTN 分解 / WBS / swarm 分解),
// 无界展开等于把运行图的规模交给模型即兴决定。四道闸:
//  1. MaxDepth   展开深度 (父→子→孙…), 默认 DefaultExpandDepth=1;
//  2. MaxNodes   单次展开可追加的节点数 (必填 >0);
//  3. MaxTotalNodes (图级) 运行图节点总数;
//  4. 单调收窄  展开产物只继承/收窄父节点约束, 不得放宽 (见 expand.go)。
type ExpandSpec struct {
	MaxDepth int `json:"max_depth,omitempty"` // 0 → DefaultExpandDepth
	MaxNodes int `json:"max_nodes"`           // 必填 >0
}

// 展开/扇出的内置上限与默认值。
const (
	DefaultExpandDepth      = 1                // ExpandSpec.MaxDepth 缺省: 只允许展开一层
	DefaultMaxTotalNodes    = 200              // GraphPolicies.MaxTotalNodes 缺省
	DefaultReduceSeparator  = "\n\n---\n\n"    // ReducePolicy.Separator 缺省
	ShardIDSep              = "#"              // 分片 NodeID: <map节点>#<序号>
	GroupIterIDInfix        = "#it"            // 组内 NodeID: <组节点>#it<轮次>/<成员>
	GroupMemberIDSep        = "/"              //
	expandedNodeIDSeparator = GroupMemberIDSep // 展开产物 NodeID: <父节点>/<子节点>
)

// AgentSpec 节点的 Agent 载体 (design/01 §4.1)。
type AgentSpec struct {
	Role        string `json:"role"`
	Prompt      string `json:"prompt,omitempty"`       // 模板, 支持 {objective}/{prev_result}/{node.<id>} 占位 (替换由 runner 负责, 引擎只透传)
	ToolProfile string `json:"tool_profile,omitempty"` // 显式声明工具画像, 替代角色名子串匹配
	MaxTurns    int    `json:"max_turns,omitempty"`
	// Deterministic gate 节点纯代码策略 (零 LLM, design/01 §4.1)。
	// 引擎不区分, runner 自解释——但仍走同一节点生命周期 (hook/journal/trace)。
	Deterministic bool `json:"deterministic,omitempty"`
	// Placement 本节点的远程放置约束 (design/01 §4.9)。
	// nil = 不声明, 由宿主的进程级默认策略决定 (与改造前行为一致)。
	// 引擎自己**不解释**它 (调度内核不认识 runtime), 只做形状校验 + 经
	// NodeExecHints 透传给宿主 —— 与 ToolProfile/MaxTurns 同一分工。
	Placement *PlacementSpec `json:"placement,omitempty"`
}

// PlacementSpec 放置约束的**图本地镜像** (对应 pkg/agent.Placement)。
//
// 为什么抄一份结构体而不是 import pkg/agent:
// 放置的求解方 (RuntimeRegistry.Pick)、能力标签的取值域 (RuntimeCaps)、团队亲和
// 的分组键 (RunMetadata.Team) 全部是 agent 语义。让 pkg/graph import pkg/agent
// 会把"纯调度内核"变成认识 agent/团队/工具画像的东西, 是依赖倒挂 (design/01 §4.9
// 已为此把 AgentRuntime 定在 pkg/agent 而非 pkg/graph)。镜像的代价是两个结构体要
// 手工保持同步, 收益是内核继续只依赖标准库 + pkg/trace。
//
// 字段语义与 agent.Placement 逐条一致 (JSON 名也一致, 于是同一份 JSON 两边都能解):
// Require 是**硬**约束 (不满足即 Pick 失败, 绝不降级); Prefer/Affinity 是软偏好。
type PlacementSpec struct {
	// Require 硬约束: runtime 必须具备全部这些能力标签, 否则被过滤掉。
	// 取值域由宿主的 RuntimeCaps 定义 (bash|browser|k8s-sandbox|gpu|自定义标签),
	// 引擎不校验标签名 —— 内核不该持有一张会与宿主漂移的能力白名单。
	Require []string `json:"require,omitempty"`
	// Prefer 软偏好: "local" | "remote:<name>" | "any" (空 = 不表达偏好)。
	// **非法值必须被 Validate 拒绝而不是当成 any**: 写错的 "remote" (漏了冒号和名字)
	// 若被当成 any, 表现是"节点随机落在任意机器上"却毫无报错, 而作者以为自己钉住了。
	Prefer string `json:"prefer,omitempty"`
	// Affinity "team" = 同一分组键的节点尽量落同一 runtime (共享 cwd)。空 = 不亲和。
	Affinity string `json:"affinity,omitempty"`
	// AffinityKey 亲和分组键。留空时由宿主用团队名/用途/RunID 补齐 (见
	// pkg/worker/factory.go)；声明了 AffinityKey 却没声明 Affinity 是死配置, Validate 拒。
	AffinityKey string `json:"affinity_key,omitempty"`
}

// 放置偏好的合法取值 (PlacementSpec.Prefer)。
const (
	PlacementPreferLocal  = "local"
	PlacementPreferAny    = "any"
	PlacementPreferRemote = "remote:" // remote:<runtime 名>
	// PlacementAffinityTeam 目前唯一合法的亲和口径 (同团队共享 cwd)。
	PlacementAffinityTeam = "team"
)

// EdgeSpec 有向边 (design/01 §4.2)。Condition 为空表示无条件恒真;
// 语法见 condition.go (v1 极简确定性条件集, 非 CEL)。
type EdgeSpec struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

// LoopPolicy 节点级循环 (design/01 §4.4)。
type LoopPolicy struct {
	// MaxIterations 循环硬上限, 必填 >0, Validate 强制 (无界循环违法, design/01 §4.4)。
	MaxIterations int `json:"max_iterations"`
	// Until 对本节点结果求值的退出条件 (语法见 condition.go)。
	// 空 = 无退出条件, 循环直到 MaxIterations 耗尽。
	Until string `json:"until,omitempty"`
	// Feedback 每轮回灌上一轮产出的模板, 含 {prev_output} 占位
	// (引擎替换为上一轮 Output 后经 NodeInput.Feedback 传给 runner)。
	Feedback string `json:"feedback,omitempty"`
	// Terminator 可插拔终止器 (design/01 §4.4 的自适应退出)。
	// nil = 不启用, 退出条件只看 Until + MaxIterations (**与改造前逐字节一致**)。
	//
	// 为什么需要它而不是把 Until 写成 `score >= 6`:
	// 生产里 5 个 mode (creative_media/app_composite/game_composite/novel_writing/
	// swarm_novel) 的退出条件是 AdaptiveTerminator 的**五路信号组合** ——
	// 达标 / 收敛 / 退化 / 策略转换 / best-of-N 回滚。其中三路是**跨轮**判断
	// (收敛看相邻两轮差, 退化看连续下降计数, 回滚看历史最高分), 而 Until 只对
	// **当前轮**的单个结果求值, 表达不了任何一路。用 `score >= 6` 顶替的实际后果是:
	// 收敛与退化两条早停路径消失 → 图在无望的方向上一直跑满 MaxIterations 轮,
	// 回滚路径消失 → 明明第 2 轮最好却交付第 5 轮的劣化产出。
	//
	// Terminator 与 Until **不得并存于同一 Loop** (Validate 强制): 两者都能决定
	// 退出, 谁先谁后是纯实现细节, 并存等于把"第几轮停"交给求值顺序。
	Terminator *TerminatorSpec `json:"terminator,omitempty"`
}

// TerminatorSpec 终止器的**纯数据**声明 (JSON 可序列化, 与图一起存档)。
//
// Name 经进程内注册表 (RegisterTerminator) 解析为一个 LoopTerminator 实例。
// 为什么是"名字 + 参数"而不是直接放一个函数字段: GraphSpec 必须可 JSON 序列化并
// 进 journal —— 函数不可序列化, 而"这张图当时用的是哪个终止器、什么参数"是事后
// 解释"为什么第 3 轮就停了"的一半证据。
//
// 内置终止器 TerminatorAdaptive ("adaptive") 复刻 pkg/agent.AdaptiveTerminator
// 的五路信号。它的四个判据阈值**必须显式填写, 没有缺省** —— 见各字段注释。
type TerminatorSpec struct {
	// Name 注册表里的终止器名 (必填, 未注册的名字 Validate 直接拒)。
	Name string `json:"name"`

	// PassScore 质量达标线: 本轮 Score >= 它即停 (信号 quality_pass)。必须 > 0。
	//
	// 为什么不给 6.0 这个缺省 (AdaptiveTerminator 的原值): **分数量纲是图作者的选择**,
	// 引擎无从推断。本仓两种量纲都在用 —— AdaptiveTerminator 是 0-10 加权分,
	// 而条件边里写的是 `score >= 75` (gate 节点的 0-100 分)。给 6.0 当缺省会让
	// 0-100 量纲的图在第一轮就"达标"退出, 且**没有任何报错**。
	PassScore float64 `json:"pass_score"`
	// ConvergeEpsilon 收敛判据: 相邻两轮分差落在 [0, ε) 即视为改进饱和。必须 > 0。
	// 同样不设缺省: ε=0.5 在 0-10 量纲上是"半分", 在 0-100 量纲上是"半分之一",
	// 后者几乎永不满足 → 收敛早停静默失效, 表现为永远跑满轮次。
	ConvergeEpsilon float64 `json:"converge_epsilon"`
	// DegradeDelta 退化判据: 相邻两轮**下降**超过它才计一次退化 (小于它算噪声)。
	// 必须 > 0, 不设缺省, 理由同 ConvergeEpsilon。
	DegradeDelta float64 `json:"degrade_delta"`
	// DegradeRounds 连续退化多少轮即终止 (AdaptiveTerminator 原值 2)。必须 >= 1。
	DegradeRounds int `json:"degrade_rounds"`

	// MaxStrategyShifts 收敛时允许的"策略转换"次数 (AdaptiveTerminator 原值 2)。
	// 0 = 不做策略转换, 一收敛就退出。**不设非零缺省**: 0 与"未填"在 JSON 里
	// 不可区分, 给它一个 2 的缺省等于让明确写了 0 的图悄悄多跑两轮。
	MaxStrategyShifts int `json:"max_strategy_shifts,omitempty"`
	// StrategyShiftFeedback 策略转换时**追加**到下一轮回灌末尾的文案
	// (空 = 只记信号不改回灌, 策略转换退化为"再给一轮机会")。
	// 为什么是追加而不是整体替换 Loop.Feedback: Feedback 模板是图作者的交接契约,
	// 让终止器整份替换等于它能静默关掉作者的回灌。也为什么内核不内置一句默认文案:
	// 引擎不产提示词 (与"占位符替换归 runner"同一分工)。
	StrategyShiftFeedback string `json:"strategy_shift_feedback,omitempty"`
	// MinRounds 最少完成的轮数 (0 = 不限): 未达此数一律不停, 保证充分探索。
	MinRounds int `json:"min_rounds,omitempty"`

	// AllowRollback 是否允许 best-of-N 回滚 (默认 false)。
	//
	// 这是**唯一会改变"节点产出是哪一轮的"**的开关, 所以必须显式打开:
	// 回滚打开后 Loop 的返回值不再必然是最后一轮 (终止器可指定回到历史最高分那轮),
	// 下游 PrevOutputs/条件边/journal 里的 node.completed 全部跟着变。终止器给了
	// 回滚建议但本开关为 false 时, 引擎**忽略并记 journal** (rollback=suppressed)
	// —— 静默忽略会让"终止器明明说了回滚却没生效"完全不可考。
	AllowRollback bool `json:"allow_rollback,omitempty"`

	// Params 自定义终止器的自由参数 (内置 adaptive 不读它)。
	// 走 map[string]string 而不是 any: 图要能 JSON 往返且进 journal 后仍可比对,
	// any 经一次往返就退化成 map[string]any, 参数指纹不再可靠。
	Params map[string]string `json:"params,omitempty"`
}

// RetryPolicy 节点重试策略 (design/01 §4.3 重试单层化: 重试只存在于此层)。
type RetryPolicy struct {
	MaxRetries int `json:"max_retries"`
	BackoffSec int `json:"backoff_sec,omitempty"` // 指数退避基数 (秒), BackoffSec*2^attempt, 默认2
}
