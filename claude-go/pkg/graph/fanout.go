package graph

// fanout.go —— map / reduce: 真正的扇出→汇聚 (design/01 §4.2, §5 "fanout 首次真正实现")。
//
// map 节点
//  1. 取集合: MapPolicy.Source (prev / prev:<id> / param:<键> / objective);
//  2. 确定性切分: MapPolicy.Split (lines / paragraphs / json_array / whole),
//     截到 MaxShards, 不足 MinShards 即 failed;
//  3. 并发派生 N 个**同构**分片: 分片 NodeSpec = map 节点的副本, ID = <node>#<i>,
//     Kind 降为 agent (它就是一次普通 agent 执行), Map/Expand 清空 ——
//     分片再扇出/再展开是第二条不受控的膨胀路径, 直接从形态上禁掉;
//  4. 并发受 run 级 nestSem (容量 = MaxParallel) 约束; map 节点自己不占票
//     (否则 MaxParallel=1 时它占着唯一的票、自己的分片永远拿不到 → 死锁);
//  5. **重试是分片级**: 每个分片独立走 retry 环 (node.Retry / 图级 DefaultRetry)。
//     组级重试会把已成功的分片连带重跑 —— 对 LLM 就是白烧 N 倍 token, 且分片产出
//     不幂等 (重跑不会得到同一份文本), 汇聚结果会随重试次数漂移;
//  6. 节点级 Loop 若声明, 作用在**每个分片内部** (同一分片自评改写);
//     "整组重来"是 loop-group 的职责, 两者互不重叠;
//  7. journal: map.expanded (分片集, resume 真源) + 每个分片自己的完整节点事件;
//     hook: 分片发 node pre/post, 载荷带 shard_of / shard_index;
//  8. 终态: ≥1 分片 completed ⇒ completed; 全 failed ⇒ failed;
//     空集合或全 skipped ⇒ **skipped** (未走的分支不是失败, 与 or-join 同一口径)。
//
// reduce 节点
//  1. 等其 map 组全部终态 —— 不需要额外等待机制: map 节点在全部分片终态前不会进入
//     终态, 而 reduce 有 map→reduce 入边, ready-set 天然满足"等齐";
//  2. 分片结果经 NodeInput.Shards 下发 (含每片输入/产出/状态) 供 runner 聚合;
//  3. Strategy=concat/longest 由引擎**直接算出**, 零 LLM, 但仍走完整节点生命周期;
//  4. RequireAll=true 时任一分片非 completed 即 failed; 零 completed 分片一律 failed
//     (整个 map 组跑完却没有一份可用产出, 那是真失败, 不是"未走的分支")。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// runMapNode 执行 map 节点: 切分 → 并发分片 → 汇总。
// 返回 (节点结果, 累计轮数, 形态特有的 hook 载荷)。
func (e *Engine) runMapNode(ctx context.Context, rc *runCtx, scope execScope, node NodeSpec, in NodeInput) (NodeResult, int, map[string]any) {
	evID := scope.evID(node.ID)
	pol := node.Map
	if pol == nil { // Validate 已拦, 防御性兜底
		return NodeResult{Status: NodeStatusFailed, Err: "map 节点缺少 map 策略"}, 1, nil
	}

	// —— 1. 分片集: resume 时**按 journal 重建, 不重新切分** ——
	// 上游产出来自 LLM, 重切可能得到不同的分片集 (行数/顺序都可能变), 那样恢复出来
	// 的运行图就与首跑不是同一张图, 事件溯源失效。
	var (
		shardVals   []string
		truncated   int
		fromJournal bool
	)
	if rc.replay != nil {
		if vals, ok := rc.replay.MapShards[evID]; ok && len(vals) > 0 {
			shardVals, fromJournal = vals, true
		}
	}
	if !fromJournal {
		all := splitShards(mapSourceValue(pol.Source, in), pol.Split)
		if len(all) > pol.MaxShards {
			truncated = len(all) - pol.MaxShards
			all = all[:pol.MaxShards]
		}
		shardVals = all
	}

	// —— 2. 运行图总量配额 (§4.2 有界): 分片同样算运行图节点 ——
	if n := len(shardVals); n > 0 {
		allowed := rc.reserveNodes(n)
		switch {
		case allowed == 0:
			err := fmt.Sprintf("map: 运行图节点总数已达上限 %d, 无法派发 %d 个分片", rc.maxTotalNodes(), n)
			return NodeResult{Status: NodeStatusFailed, Err: err}, 1, map[string]any{"shards": 0}
		case allowed < n:
			truncated += n - allowed
			shardVals = shardVals[:allowed]
		}
	}

	// —— 3. 空集合 = 未走的分支 (skipped), 不是失败 ——
	if len(shardVals) == 0 {
		reason := fmt.Sprintf("map: 输入集合为空 (source=%s split=%s), 无分片可派发",
			orDefault(pol.Source, SourcePrev), orDefault(pol.Split, SplitLines))
		return NodeResult{Status: NodeStatusSkipped, Err: reason}, 0, map[string]any{"shards": 0}
	}
	if pol.MinShards > 0 && len(shardVals) < pol.MinShards {
		err := fmt.Sprintf("map: 只切出 %d 个分片, 少于 min_shards=%d", len(shardVals), pol.MinShards)
		return NodeResult{Status: NodeStatusFailed, Err: err}, 0, map[string]any{"shards": len(shardVals)}
	}

	ids := make([]string, len(shardVals))
	for i := range shardVals {
		ids[i] = shardNodeID(node.ID, i)
	}
	// journal map.expanded —— 只在**真的做了切分**时记账。从 journal 重建的那次不再
	// 记, 否则 resume 后 journal 里会出现第二条 map.expanded, 事件序看起来像"展开了两次"。
	if !fromJournal {
		rc.appendEv(EvMapExpanded, evID, scope.with(map[string]any{
			"count":     len(shardVals),
			"shards":    mustJSON(shardVals),
			"ids":       strings.Join(ids, ","),
			"truncated": truncated,
			"source":    orDefault(pol.Source, SourcePrev),
			"split":     orDefault(pol.Split, SplitLines),
		}))
	}

	// —— 4. 并发派发分片 ——
	shardScope := execScope{
		prefix: scope.prefix,
		nested: true, // 分片是嵌套层叶子: 取 nestSem
		depth:  scope.depth,
		extra:  scope.with(map[string]any{"shard_of": node.ID}),
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		iters int
	)
	results := make([]ShardResult, len(shardVals))
	for i, val := range shardVals {
		sn := shardNodeSpec(node, i)
		si := in // 继承 objective/params/prev/group 轮次
		si.nested = true
		si.Shards = nil
		si.Shard = &ShardInput{NodeID: sn.ID, MapID: node.ID, Index: i, Total: len(shardVals), Value: val}
		si.NodeRef = shardScope.evID(sn.ID)
		// resume: 上一轮已 completed 的分片直接吃缓存 (崩溃恰好发生在扇出中途的场景)。
		if rc.replay != nil {
			if cached, ok := rc.replay.Completed[scope.evID(sn.ID)]; ok && cached.Status == NodeStatusCompleted {
				results[i] = ShardResult{NodeID: sn.ID, MapID: node.ID, Index: i, Input: val,
					Status: cached.Status, Output: cached.Output, Score: cached.Score}
				continue
			}
		}
		wg.Add(1)
		go func(idx int, spec NodeSpec, input NodeInput, shardVal string) {
			defer wg.Done()
			// 复用 execNode: 分片因此拥有与普通节点完全一致的生命周期
			// (hook pre/post、journal started/completed/failed/retried、超时、重试)。
			// 这是"分片有独立 NodeID 以便 journal/trace 归因"的落点。
			r := e.execNode(ctx, rc, shardScope, spec, input)
			mu.Lock()
			iters++
			mu.Unlock()
			results[idx] = ShardResult{NodeID: spec.ID, MapID: node.ID, Index: idx, Input: shardVal,
				Status: r.Status, Output: r.Output, Score: r.Score, Err: r.Err}
		}(i, sn, si, val)
	}
	wg.Wait()

	// —— 5. 汇总 ——
	var (
		okOuts    []string
		scoreSum  float64
		nOK       int
		nFail     int
		nSkip     int
		firstErrs []string
	)
	for _, r := range results {
		switch r.Status {
		case NodeStatusCompleted:
			nOK++
			scoreSum += r.Score
			if r.Output != "" {
				okOuts = append(okOuts, r.Output)
			}
		case NodeStatusSkipped:
			nSkip++
		default:
			nFail++
			if len(firstErrs) < 3 {
				firstErrs = append(firstErrs, r.NodeID+": "+r.Err)
			}
		}
	}
	extra := map[string]any{
		"shards": len(results), "shards_ok": nOK, "shards_failed": nFail,
		"shards_skipped": nSkip, "truncated": truncated,
	}
	res := NodeResult{Shards: results}
	switch {
	case nOK > 0:
		res.Status = NodeStatusCompleted
		// Output 给未声明 reduce 的图直接用 (拼接); 声明了 reduce 的图走 Shards。
		res.Output = strings.Join(okOuts, DefaultReduceSeparator)
		res.Score = scoreSum / float64(nOK) // 分片评分均值: 评审团场景可直接挂条件边
		if nFail > 0 {
			res.Err = fmt.Sprintf("map: %d/%d 个分片失败 (%s)", nFail, len(results), strings.Join(firstErrs, "; "))
		}
	case nFail > 0:
		res.Status = NodeStatusFailed
		res.Err = fmt.Sprintf("map: 全部 %d 个分片失败 (%s)", len(results), strings.Join(firstErrs, "; "))
	default: // 全 skipped (例如 hook 逐片 deny): 未走的分支, 不记失败
		res.Status = NodeStatusSkipped
		res.Err = fmt.Sprintf("map: 全部 %d 个分片被跳过", len(results))
	}
	if iters == 0 {
		iters = 1 // 全缓存命中时至少记 1 轮, 避免 hook 载荷里出现 iterations=0
	}
	return res, iters, extra
}

// shardNodeID 分片节点 ID: <map节点>#<序号> (journal/trace 归因用)。
func shardNodeID(mapID string, idx int) string {
	return mapID + ShardIDSep + strconv.Itoa(idx)
}

// shardNodeSpec 分片的节点声明 = map 节点副本 (同构子任务)。
// Kind 降为 agent、Map/Expand 清空: 分片就是一次普通 agent 执行, 不得再扇出或展开。
// Loop/Retry/TimeoutSec/Agent 全部保留 —— 循环与重试因此天然落在分片粒度。
func shardNodeSpec(node NodeSpec, idx int) NodeSpec {
	sn := node
	sn.ID = shardNodeID(node.ID, idx)
	sn.Kind = NodeKindAgent
	sn.Map = nil
	sn.Expand = nil
	sn.Reduce = nil
	sn.Group = nil
	return sn
}

// mapSourceValue 按 Source 声明取待切分的原文。
func mapSourceValue(source string, in NodeInput) string {
	switch {
	case source == SourceObjective:
		return in.Objective
	case strings.HasPrefix(source, SourceParamPrefix):
		return in.Params[strings.TrimSpace(strings.TrimPrefix(source, SourceParamPrefix))]
	case strings.HasPrefix(source, SourcePrevPrefix):
		return in.PrevOutputs[strings.TrimSpace(strings.TrimPrefix(source, SourcePrevPrefix))]
	default: // "" / prev: 取最长的上游产出 (同长按节点 ID 字典序取小, 保证确定性)
		best, bestKey := "", ""
		for k, v := range in.PrevOutputs {
			if len(v) > len(best) || (len(v) == len(best) && (bestKey == "" || k < bestKey)) {
				best, bestKey = v, k
			}
		}
		return best
	}
}

// splitShards 确定性切分 (引擎只做确定性的事, 语义解析归 runner)。
func splitShards(raw, split string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	switch split {
	case SplitWhole:
		return []string{strings.TrimSpace(raw)}
	case SplitParagraph:
		return nonEmptyTrimmed(strings.Split(raw, "\n\n"))
	case SplitJSONArray:
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &arr); err != nil {
			return nil // 不是合法 JSON 数组: 视为空集合 ⇒ map 节点 skipped (非失败)
		}
		out := make([]string, 0, len(arr))
		for _, el := range arr {
			s := strings.TrimSpace(string(el))
			var str string
			if json.Unmarshal(el, &str) == nil {
				s = strings.TrimSpace(str) // 字符串元素去引号
			}
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default: // SplitLines
		return nonEmptyTrimmed(strings.Split(raw, "\n"))
	}
}

func nonEmptyTrimmed(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// reduce
// ---------------------------------------------------------------------------

// reduceResult 计算 reduce 节点结果。
// handled=false 表示需要交给 runner 聚合 (Strategy=runner), 由调用方走常规 retry 环。
func reduceResult(node NodeSpec, in NodeInput) (NodeResult, bool) {
	pol := node.Reduce
	strategy, sep, requireAll := ReduceRunner, DefaultReduceSeparator, false
	if pol != nil {
		if pol.Strategy != "" {
			strategy = pol.Strategy
		}
		if pol.Separator != "" {
			sep = pol.Separator
		}
		requireAll = pol.RequireAll
	}

	var okOuts []string
	nOK, nBad := 0, 0
	for _, s := range in.Shards {
		if s.Status == NodeStatusCompleted {
			nOK++
			okOuts = append(okOuts, s.Output)
			continue
		}
		nBad++
	}
	if requireAll && nBad > 0 {
		return NodeResult{Status: NodeStatusFailed,
			Err: fmt.Sprintf("reduce: require_all 下有 %d/%d 个分片未成功", nBad, len(in.Shards))}, true
	}
	if nOK == 0 {
		// 整个 map 组跑完却没有一份可用产出: 真失败 (不是"未走的分支")。
		return NodeResult{Status: NodeStatusFailed,
			Err: fmt.Sprintf("reduce: %d 个分片中无一成功, 无可聚合内容", len(in.Shards))}, true
	}
	switch strategy {
	case ReduceConcat:
		return NodeResult{Status: NodeStatusCompleted, Output: strings.Join(okOuts, sep)}, true
	case ReduceLongest:
		best := ""
		for _, s := range okOuts {
			if len(s) > len(best) {
				best = s
			}
		}
		return NodeResult{Status: NodeStatusCompleted, Output: best}, true
	default:
		return NodeResult{}, false // 交给 runner
	}
}

// gatherShards 收集 reduce 节点要聚合的分片结果 (按入边声明序 → 确定性)。
func (dr *dagRun) gatherShards(n NodeSpec) []ShardResult {
	want := map[string]bool{}
	if n.Reduce != nil {
		for _, f := range n.Reduce.From {
			want[f] = true
		}
	}
	var out []ShardResult
	for _, in := range dr.preds[n.ID] {
		p, ok := dr.byID[in.from]
		if !ok || p.Kind != NodeKindMap {
			continue
		}
		if len(want) > 0 && !want[in.from] {
			continue
		}
		if r, ok := dr.state[in.from]; ok {
			out = append(out, r.Shards...)
		}
	}
	return out
}

// mustJSON 把结构编成紧凑 JSON 字符串放进 journal Data。
// 为什么存字符串而不是嵌套 map: journal Data 经 JSON 往返后 []NodeSpec 会退化成
// map[string]any, 再转回结构体要手写一遍反射式解析; 存字符串则 Replay 里一次
// json.Unmarshal 就能拿回强类型。
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
