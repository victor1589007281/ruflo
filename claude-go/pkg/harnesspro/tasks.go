package harnesspro

// tasks.go —— 内置进化任务池与 h0 基座 harness。
//
// 任务池是"被进化对象"的考场: 每道题的 Prompt 是契约的唯一来源 (论文纪律一:
// 任务描述与公开文件定义契约)。12 题的规模服从论文 0.1 的量级精神 (10 题训练),
// 题类覆盖 JSON 处理 / 文本变换 / 结构化生成 / 数值计算 / 格式纪律 —— 这是
// claude-go 单发补全执行体能真实完成、且冻结测试可机械判定的闭集。

import "math"

// DefaultPool 内置任务池 (12 题)。
func DefaultPool() []Task {
	return []Task{
		{
			ID: "json-extract-errors", Category: "extract", Difficulty: 0.45,
			EstFeatures: []string{"log-parsing", "json-output"},
			Prompt: `下面是一段服务日志, 请提取所有级别为 ERROR 的行, 把每行的"时间戳 + 错误消息"以 JSON 数组返回, 元素形如 {"ts": "...", "msg": "..."}。
2026-11-10T08:00:01Z INFO server started
2026-11-10T08:00:03Z ERROR connection refused by upstream db1
2026-11-10T08:00:04Z INFO retry scheduled
2026-11-10T08:00:09Z ERROR timeout reading shard 7
2026-11-10T08:00:10Z WARN latency p99 above budget`,
		},
		{
			ID: "csv-mean-compute", Category: "compute", Difficulty: 0.35,
			EstFeatures: []string{"csv", "numeric"},
			Prompt: `计算下面 CSV 中 score 列的平均值, 只输出一个数字, 保留两位小数, 不要输出任何其他文字。
name,score
alice,87
bob,92
carol,78
dave,90
erin,85`,
		},
		{
			ID: "reverse-words-sentence", Category: "text", Difficulty: 0.25,
			EstFeatures: []string{"string-transform"},
			Prompt: `把下面这句话的单词顺序逆序 (单词本身不变, 标点跟着原单词), 只输出逆序后的句子:
the quick brown fox jumps over the lazy dog.`,
		},
		{
			ID: "kv-to-markdown-table", Category: "format", Difficulty: 0.40,
			EstFeatures: []string{"markdown", "table"},
			Prompt: `把下面的键值列表转成 Markdown 表格 (两列: 名称/版本), 只输出表格本身:
nginx 1.25.4
redis 7.2.4
postgres 16.2
etcd 3.5.12`,
		},
		{
			ID: "classify-intent-label", Category: "format", Difficulty: 0.50,
			EstFeatures: []string{"classification", "fixed-format"},
			Prompt: `把下面的用户请求分类到恰好一个标签: BUG / FEATURE / QUESTION / OTHER。第一行只输出标签, 第二行用一句话说明理由。
"我升级之后所有定时任务都不触发了, 回退版本也没用, 之前配置的 cron 全部消失。"`,
		},
		{
			ID: "acronym-expand", Category: "text", Difficulty: 0.30,
			EstFeatures: []string{"expansion"},
			Prompt: `展开下面的缩写并给出全称, 输出 JSON 对象, 键是缩写, 值是全称 (英文全称):
API, DB, RAID, ACID, FIFO`,
		},
		{
			ID: "dedupe-sort-ints", Category: "compute", Difficulty: 0.30,
			EstFeatures: []string{"dedup", "sort"},
			Prompt: `对下面的整数列表去重并升序排序, 只输出逗号分隔的结果, 不要空格与其他文字:
5, 3, 9, 3, 1, 7, 5, 9, 2`,
		},
		{
			ID: "bracket-balance-check", Category: "compute", Difficulty: 0.45,
			EstFeatures: []string{"parsing", "boolean"},
			Prompt: `检查下面的括号串是否平衡 (四种括号 () [] {} <> 都参与), 只输出 YES 或 NO:
([{<>}]({}))<[]>`,
		},
		{
			ID: "date-shift-days", Category: "compute", Difficulty: 0.55,
			EstFeatures: []string{"date", "arithmetic"},
			Prompt:      `把日期 2026-11-16 往后推 100 天, 只输出结果日期 (YYYY-MM-DD), 不要输出其他文字。注意 2026 年不是闰年之后的年份规则自行判断, 但本题不涉及 2 月 29 日。`,
		},
		{
			ID: "summarize-3bullets-zh", Category: "text", Difficulty: 0.40,
			EstFeatures: []string{"summary", "bullet", "chinese"},
			Prompt: `把下面的段落总结成恰好 3 条要点, 每条以 "-" 开头独占一行, 用简体中文:
Raft 是一种共识算法, 它把一致性问题分解成领导者选举、日志复制和安全性三个子问题。任何时刻至多一个领导者, 领导者负责接收客户端请求并向跟随者复制日志条目; 跟随者只接受来自合法领导者的写入。选举使用随机超时, 任期号单调递增,  split vote 时通过新一轮选举化解。安全性由"领导者完备性"保证: 一条日志条目只有在它被复制到多数派之后才会被提交, 而任何新领导者的日志都包含所有已提交的条目。`,
		},
		{
			ID: "json-config-validate", Category: "json", Difficulty: 0.50,
			EstFeatures: []string{"json", "validation"},
			Prompt: `下面的 JSON 配置声称遵循"必须包含 name(string) 与 port(1-65535 的整数) 两个字段"的契约。检查它是否合规: 输出 JSON {"ok": true/false, "reason": "..."}。
{"name": "gateway", "port": 8080, "debug": false}`,
		},
		{
			ID: "regex-rewrite-phone", Category: "text", Difficulty: 0.45,
			EstFeatures: []string{"rewrite", "masking"},
			Prompt: `把下面的文本中的手机号打码: 11 位数字且以 1 开头的串, 保留前 3 位与后 4 位, 中间 4 位替换为 ****。其他数字串不动。只输出打码后的文本:
联系人: 张三 13812345678, 李四 电话 19900001111, 座机 010-88886666`,
		},
	}
}

// H0Instructions 初始 harness 指令 (论文里的 h0: 朴素、无机制、可被进化改进)。
// 刻意写得"够用但粗糙" —— 不含动态路由/预算提醒/输出纪律, 给进化留出可改进空间。
func H0Instructions() string {
	return `你是一个任务执行引擎。用户会给你一项明确任务, 你的职责是正确完成任务。
要求: 仔细阅读任务, 按任务要求的格式输出, 不要输出与任务无关的内容。`
}

// SelectTrainTasks DPP-lite 选题: 贪心 MAP 推断在"质量(难度)×多样性(类别/特征)"核上选 n 题。
//
// 核函数: K(i,j) = q_i * sim(i,j) * q_j, 其中 q = 0.5 + difficulty/2 (质量先验),
// sim = RBF(特征距离) (类别不同加性奖励)。贪心: 从空集出发, 每步加入使 det(K_S) 增长最大的题。
// 特征向量 = [category-one-hot..., difficulty, log(1+len(prompt)), len(est_features)]。
// 这是论文 0.1 节 DPP 工程实现的忠实轻量化 (难度用离线先验替代执行器真跑)。
func SelectTrainTasks(pool []Task, n int) []Task {
	if n >= len(pool) {
		return append([]Task(nil), pool...)
	}
	type feat struct {
		t Task
		v []float64
	}
	cats := map[string]int{}
	for _, t := range pool {
		if _, ok := cats[t.Category]; !ok {
			cats[t.Category] = 0
		}
	}
	catNames := make([]string, 0, len(cats))
	for c := range cats {
		catNames = append(catNames, c)
	}
	sortStrings(catNames)
	for i, c := range catNames {
		cats[c] = i
	}
	nCat := len(catNames)
	fs := make([]feat, len(pool))
	for i, t := range pool {
		v := make([]float64, nCat+3)
		v[cats[t.Category]] = 1.0
		v[nCat] = t.Difficulty
		v[nCat+1] = float64(len(t.Prompt)) / 500.0
		v[nCat+2] = float64(len(t.EstFeatures))
		fs[i] = feat{t, v}
	}
	sim := func(a, b []float64) float64 {
		var d2 float64
		for i := range a {
			dd := a[i] - b[i]
			d2 += dd * dd
		}
		return math.Exp(-d2 / 2.0) // RBF, γ=1
	}
	quality := func(t Task) float64 { return 0.5 + t.Difficulty/2 }
	picked := make([]bool, len(pool))
	var sel []int
	// 贪心 MAP: det 增长 = q_i^2 * ∏_{j∈S} (1 - sim(i,j)^2) 的单调代理 (质量×新颖度)。
	for len(sel) < n {
		best, bestGain := -1, -1.0
		for i := range fs {
			if picked[i] {
				continue
			}
			nov := 1.0
			for _, j := range sel {
				s := sim(fs[i].v, fs[j].v)
				nov *= 1 - s*s
			}
			gain := quality(fs[i].t) * quality(fs[i].t) * nov
			if gain > bestGain {
				best, bestGain = i, gain
			}
		}
		if best < 0 {
			break
		}
		picked[best] = true
		sel = append(sel, best)
	}
	out := make([]Task, 0, len(sel))
	for _, i := range sel {
		out = append(out, fs[i].t)
	}
	return out
}

// sortStrings 原地排序 (选题确定性: 类别序不依赖 map 迭代序)。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
