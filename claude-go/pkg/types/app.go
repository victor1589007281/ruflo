// app.go — app（调用方应用）身份的进程内载体。
//
// # 与 owner / source 的正交分工
//
//	platform 里已有两个近邻概念，app 与它们都不重合：
//	  - owner（owner.go）回答"这次运行为哪个租户/用户服务"（R-H6 多租户数据隔离）；
//	  - TaskRecord.Source / LLMCallRecord.Source 回答"请求从哪个**入口通道**进来"
//	    （dashboard / platform-mcp / sync），是 claude-go 自己的接线拓扑；
//	  - app 回答"**哪个外部 APP** 发起了这次调用"（nav 的 studyforge / askforge /
//	    moqi / 指挥台 bot / testforge …）。同一个入口通道上跑着多个 APP
//	    （8+ 下游平台直连 :18080），没有这一维就看不出流量是谁发的。
//
//	三者刻意不合并：owner 是鉴权出的身份，source 是拓扑事实，app 是调用方自报
//	+凭据背书的来源。把 app 拼进 owner（"owner@app"）会让两个维度各自的分组
//	都退化成笛卡尔积。
//
// # 信任级别：verified / claimed / unknown
//
//	app 值有三种形态，下游按前缀即可分辨可信度：
//	  - 纯 appId（如 "studyforge"）：请求带了注册表（pkg/httpauth.AppRegistry）
//	    中登记的 token，身份由凭据背书，可信；
//	  - "claimed:<id>"：调用方只带了 X-App-Id 自报头（或请求体字段），没有
//	    对应凭据。可观测但不可信 —— 与 owner.go 头注释"自报 owner 等于让攻击者
//	    自己选租户"同一道理，所以打上 claimed: 前缀与 verified 区分；
//	  - "unknown"（DefaultAppID）：什么都没带。归一成具体值而不是空串的理由
//	    与 owner 相同：空值会让"来源隔离成立"的断言恒真。
//
// # 为什么载体放在 types
//
//	与 owner 同一理由：app 需要贯穿 httpauth → dashboard/sessionapi/platformmcp
//	→ taskservice → engine → api.Client 各层，而 types 是依赖最底层，
//	"只此一份实现"避免两套 ctx key 互相看不见。
package types

import (
	"context"
	"strings"
)

// DefaultAppID 是未携带来源标识时的 app。
//
// 选具体字符串而不是空串，理由见文件头：空串会让"按 app 分组/隔离"的断言
// 在双方都为空时恒真 —— 恰恰在最需要看见来源的路径上什么都看不见。
const DefaultAppID = "unknown"

// claimedAppPrefix 标记"自报但未凭据背书"的 app 值。
// 由 httpauth 中间件与入口适配器在写入 ctx 时拼接，本包只提供判定与拼接工具，
// 保证前缀拼写全仓唯一。
const claimedAppPrefix = "claimed:"

// NormalizeApp 把空/纯空白 app 归一为 DefaultAppID，其余原样返回。
//
// 只 strip 首尾空白：appId 是注册表里的稳定标识，静默改写会让同一个 APP
// 在不同入口拿到不同 app（进而"按 APP 统计的流量对不上账"）。
// 注意 "claimed:xxx" 原样保留 —— 前缀是信任级别的一部分，不是脏数据。
func NormalizeApp(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultAppID
	}
	return s
}

// ClaimedApp 把自报的 appId 包成 claimed: 形态，表示"可观测但未经凭据背书"。
// 已是 claimed: 前缀的值不重复加（幂等）。
func ClaimedApp(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return DefaultAppID
	}
	if strings.HasPrefix(id, claimedAppPrefix) {
		return id
	}
	return claimedAppPrefix + id
}

// IsClaimedApp 报告 app 值是否为自报形态（claimed: 前缀）。
func IsClaimedApp(app string) bool {
	return strings.HasPrefix(NormalizeApp(app), claimedAppPrefix)
}

type appCtxKey struct{}

// WithApp 把 app 归一化后写入 ctx（nil ctx 返回 nil，与 context 包约定一致）。
// 写入点归一而非只在读取点归一，使"ctx 里的 app 永不为空"成为全局不变量
// （与 WithOwner 同一纪律）。
func WithApp(ctx context.Context, app string) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, appCtxKey{}, NormalizeApp(app))
}

// AppFromContext 读取 ctx 中的 app；缺失（或值类型不对）时返回 DefaultAppID。
// 不返回 (string, bool) 的理由与 OwnerFromContext 相同：调用方几乎总是需要
// 可比较的 app，缺省归一比逼每个调用点写 ok-check 更不容易出错。
func AppFromContext(ctx context.Context) string {
	if ctx == nil {
		return DefaultAppID
	}
	if v, ok := ctx.Value(appCtxKey{}).(string); ok {
		return NormalizeApp(v)
	}
	return DefaultAppID
}

// HasApp 报告 ctx 里是否**显式**写过 app。
// 与 AppFromContext 分开的理由同 HasOwner：鉴权边界上必须能区分
// "这个请求就是没带来源"（记 unknown）与"忘了过 stamping 中间件"（接线 bug）。
func HasApp(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(appCtxKey{}).(string)
	return ok
}

type appRefCtxKey struct{}

// WithAppRef 把调用方业务参考号（如指挥台工单号）写入 ctx。
// 与 app 分开存放：app 是分组维度（要归一、要前缀），AppRef 是透传引用
// （原样保留、可为空、不做任何归一 —— 它是调用方的自由文本，改了就对不上账）。
func WithAppRef(ctx context.Context, ref string) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, appRefCtxKey{}, ref)
}

// AppRefFromContext 读取调用方业务参考号；缺失返回空串。
// 空是正常状态（调用方没给参考号），不是缺失错误。
func AppRefFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(appRefCtxKey{}).(string); ok {
		return v
	}
	return ""
}
