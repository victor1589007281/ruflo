// index_ops.go — init/update/reindex 的统一索引操作。
//
// init/update/reindex 本质是同一操作(gitnexus analyze + graphify update),
// 此前该逻辑及"失败/部分"状态判定在 MCP transport / builtin 工具 / 各 case 里
// 重复多份("MCP,CLIENT,API 都要维护 index")。这里收敛为单一来源, 各入口只
// 负责用自己的格式化器(safeMCPResult / safeMap 等)渲染结果。
package codeintel

import "sync"

// IndexOutcome 一次全量索引的原始结果, 供各入口统一执行、各自格式化。
type IndexOutcome struct {
	GitNexus *QueryResult
	GNErr    error
	Graphify *QueryResult
	GFErr    error
}

// RunIndex 对已配置好的 gitnexus + graphify 执行全量索引(analyze + update),
// 两者相互独立故并行执行(此前 builtin 并行、MCP 串行, 现统一为并行)。
// 调用方负责按各自上下文构造 gn/gf(IndexBaseDir、MaxHeapMB 等)。
func RunIndex(gn *GitNexus, gf *Graphify) IndexOutcome {
	var o IndexOutcome
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); o.GitNexus, o.GNErr = gn.Analyze() }()
	go func() { defer wg.Done(); o.Graphify, o.GFErr = gf.Update(true) }()
	wg.Wait()
	return o
}

// StatusOr 依据两工具错误返回 ok(全成功)/ "partial"(部分成功)/ "failed"(全失败)。
func (o IndexOutcome) StatusOr(ok string) string {
	switch {
	case o.GNErr != nil && o.GFErr != nil:
		return "failed"
	case o.GNErr != nil || o.GFErr != nil:
		return "partial"
	default:
		return ok
	}
}
