package agent

import (
	"context"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent/modelconfig"
)

// PlanConfigKey 是 context.Context 中保存解析后 plan 配置的 key。
// 已废弃: 请改用 ModelConfigKey。
type PlanConfigKey struct{}

// ModelConfigKey 是 context.Context 中保存 modelconfig.ResolvedConfig 的 key。
// 用于传递更丰富的模型配置 (alias, provider, maxTokens 等)。
type ModelConfigKey struct{}

// RunMetadataKey 是 context.Context 中保存团队/角色运行标签的 key。
type RunMetadataKey struct{}

// RunMetadata 描述一次 Agent Runner 调用的业务来源, 用于 LLM prompt 账本指标。
type RunMetadata struct {
	Source   string
	Purpose  string
	Workflow string
	Role     string
	Team     string
	Stage    string
	// Cwd 本次执行所属团队的工作目录 (team.Cwd)。
	//
	// 为什么账本结构体里要有它: 放置感知的执行工厂 (pkg/worker.RuntimeFactory) 需要
	// 知道"代码该落在哪"才能给远程 worker 下发工作区声明 —— 编译门禁跑在
	// <team.Cwd>/go.mod 上 (teams.go:1091,1105), 这是唯一能把它传到工厂的既有通道
	// (CreateAgentFunc 的签名是 (ctx, role, systemPrompt), 团队对象传不进去)。
	// 空 = 不是团队阶段 / 未配置 cwd, 此时工作区档位不启用。
	Cwd string
}

// WithRunMetadata 将团队运行标签写入 context。
func WithRunMetadata(ctx context.Context, meta RunMetadata) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, RunMetadataKey{}, meta)
}

// RunMetadataFromContext 读取团队运行标签。
func RunMetadataFromContext(ctx context.Context) RunMetadata {
	if ctx == nil {
		return RunMetadata{}
	}
	if meta, ok := ctx.Value(RunMetadataKey{}).(RunMetadata); ok {
		return meta
	}
	return RunMetadata{}
}

// stageKey 承载"本 stage 叫什么"。
type stageKey struct{}

// withStage 标注当前 ctx 属于哪个 stage。
//
// 为什么不加参数: runAgent 有 10+ 个调用方 (workflow / adversarial / novel v2/v3 ...),
// 逐个加一个 stage 参数会逼着每个调用方**编**一个名字出来 —— 而编出来的名字一旦
// 被引擎当成身份用, 两名角色就会因为随手编了同一个字符串而共享中间态。走 ctx
// 则相反: 只有一个调用点 (executeStage) 知道真正的 stage 名, 也只有它需要标注;
// 其余调用方不加, 引擎收到空 stage, 身份自然退化 (更保守, 后果只是少一次恢复)。
func withStage(ctx context.Context, stage string) context.Context {
	if ctx == nil || stage == "" {
		return ctx
	}
	return context.WithValue(ctx, stageKey{}, stage)
}

// stageFrom 读回 withStage 标注的 stage 名 (未标注时为空串)。
func stageFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(stageKey{}).(string)
	return s
}

// PlanConfigResolver 按 plan + role 层级解析最终 API 连接参数。
// 现在是 modelconfig.ConfigResolver 的轻量包装，保持向后兼容的接口。
type PlanConfigResolver struct {
	modelResolver *modelconfig.ConfigResolver
}

// NewPlanConfigResolver 从 modelconfig 模块创建解析器 (唯一构造方式)。
func NewPlanConfigResolver(resolver *modelconfig.ConfigResolver) *PlanConfigResolver {
	return &PlanConfigResolver{
		modelResolver: resolver,
	}
}

// Resolve 按 plan + role 层级解析最终配置。
func (r *PlanConfigResolver) Resolve(plan, role string) modelconfig.ResolvedConfig {
	if r.modelResolver != nil {
		return r.modelResolver.Resolve(plan, role)
	}
	return modelconfig.ResolvedConfig{}
}

// ResolveAlias 把一个**具体别名**解析成完整连接参数 (21.8.12 P4 / X1 的消费方)。
//
// 与 Resolve 的分工: Resolve 回答"这个 team + 这个 role 该用哪个模型" (走
// plan/role 层级 + 全局默认兜底); 本方法回答"这个别名指向什么" ——
// StageDef.Model 声明的正是后者 (一个别名), 不是"另一个 role"。
//
// 复用 modelconfig.ConfigResolver.ResolveAlias 而不是在这里再写一遍别名解析:
// 别名 -> (provider, baseURL, apiKey, protocol, proxy, 限流参数) 的映射是本仓最容易
// 漏字段的一处 —— 漏了 protocol 会把 openai 协议的模型按 anthropic 发, 漏了 proxy
// 会让必须走代理的模型直连失败。只有一份实现才谈得上"不漏"。
//
// 未注入 resolver 时返回零值: 调用方据此判定"解析不出来"并**保持原配置 + 留痕**,
// 而不是把零值当成一个有效配置套上去 (那会把 baseURL/apiKey 清空)。
func (r *PlanConfigResolver) ResolveAlias(alias string) modelconfig.ResolvedConfig {
	if r == nil || r.modelResolver == nil || strings.TrimSpace(alias) == "" {
		return modelconfig.ResolvedConfig{}
	}
	return r.modelResolver.ResolveAlias(strings.TrimSpace(alias))
}

// Registry 返回内部的 ProviderRegistry。
func (r *PlanConfigResolver) Registry() *modelconfig.ProviderRegistry {
	if r.modelResolver != nil {
		// ConfigResolver 没有直接暴露 registry 的方法，
		// 但 PlanConfigResolver 在初始化时通常同时持有 registry 引用。
		// 如需访问 registry，请通过外部持有的引用直接访问。
	}
	return nil
}
