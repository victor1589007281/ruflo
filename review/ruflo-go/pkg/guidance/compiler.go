// Package guidance 实现 Ruflo 治理（Guidance）控制面：将 CLAUDE.md 类 Markdown 编译为结构化 PolicyBundle，
// 经 EnforcementGates 在命令/编辑/工具调用前做门控，RunLedger 记录运行审计，ProofChain 提供哈希链证明等。
// 各子模块见 gates、ledger、optimizer、persistence、memory_gate、continue_gate、analyzer、trust、manifest_validator、evolution。
package guidance

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

// Markdown 规则解析用正则：规则 ID、风险等级、领域标签、工具白名单片段、任务意图、路径 scope。
var (
	reRuleID = regexp.MustCompile(`(?m)^\s*(R\d{3})\s*:`)                                            // 形如 R001:
	reRisk   = regexp.MustCompile(`(?i)risk:\s*(critical|high|medium|low|info)\b`)                   // risk: critical 等
	reDomain = regexp.MustCompile(`@([a-z][a-z0-9_-]*)`)                                             // @security 类标签
	reTool   = regexp.MustCompile(`\[([a-z][a-z0-9_-]*)\]`)                                          // [read] 工具名
	reIntent = regexp.MustCompile(`#(bug-fix|feature|refactor|security|performance|testing|docs)\b`) // #feature
	reScope  = regexp.MustCompile(`(?i)scope:\s*(\S+)`)                                              // scope: glob
)

// GuidanceCompiler 无状态编译器：将根目录与本地 Markdown 合并为单一 PolicyBundle。
type GuidanceCompiler struct{}

// NewGuidanceCompiler 返回编译器实例（无内部可变状态）。
func NewGuidanceCompiler() *GuidanceCompiler {
	return &GuidanceCompiler{}
}

// Compile 解析 rootMD 与 localMD：按 R\d{3} 抽取规则块，local 同 ID 覆盖 root；前 60 行非空行并入 Constitution；
// 生成 default 分片、Manifest 统计与基于全文哈希的 bundle ID。
func (c *GuidanceCompiler) Compile(rootMD, localMD string) PolicyBundle {
	rootRules, rootConst := parseMarkdownRules(rootMD, "root")
	localRules, localConst := parseMarkdownRules(localMD, "local")
	byID := make(map[string]GuidanceRule)
	for _, r := range rootRules {
		byID[r.ID] = r
	}
	for _, r := range localRules {
		byID[r.ID] = r
	}
	merged := make([]GuidanceRule, 0, len(byID))
	for _, r := range byID {
		merged = append(merged, r)
	}
	constLines := append(append([]string{}, rootConst.Lines...), localConst.Lines...)
	constitution := Constitution{
		Lines:  uniqueStrings(constLines),
		Source: "root+local",
	}
	shard := RuleShard{
		ShardID: "default",
		Rules:   merged,
		Version: time.Now().UTC().Format("20060102T150405Z"),
	}
	bundleID := hashID(rootMD + "\n---\n" + localMD)
	manifest := buildManifest(bundleID, shard.Version, []RuleShard{shard})
	return PolicyBundle{
		ID:           bundleID,
		Version:      shard.Version,
		Constitution: constitution,
		Shards:       []RuleShard{shard},
		Manifest:     manifest,
		CreatedAt:    time.Now().UTC(),
	}
}

// parseMarkdownRules 扫描 md：前 60 行构建 Constitution；按 "\n## " 分块，块内需含 R\d{3} 才视为规则；
// 从正文提取 Risk、Domain、Tools、Intent、ScopeGlob，并生成 api.GuidanceRule（严重度由 body 关键字 block/warn 推断）。
func parseMarkdownRules(md, source string) ([]GuidanceRule, Constitution) {
	lines := strings.Split(md, "\n")
	var constLines []string
	for i, ln := range lines {
		if i >= 60 {
			break
		}
		t := strings.TrimSpace(ln)
		if t != "" {
			constLines = append(constLines, t)
		}
	}
	var rules []GuidanceRule
	blocks := strings.Split(md, "\n## ")
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		firstLine, rest, _ := strings.Cut(block, "\n")
		title := strings.TrimSpace(strings.TrimPrefix(firstLine, "#"))
		if !reRuleID.MatchString(rest) && !reRuleID.MatchString(title+"\n"+rest) {
			continue
		}
		text := title + "\n" + rest
		id := ""
		if m := reRuleID.FindStringSubmatch(text); len(m) > 1 {
			id = m[1]
		}
		if id == "" {
			continue
		}
		r := GuidanceRule{
			GuidanceRule: apiRule(id, title, rest, source),
		}
		if m := reRisk.FindStringSubmatch(text); len(m) > 1 {
			r.RiskClass = RiskClass(strings.ToLower(m[1]))
		}
		if m := reDomain.FindStringSubmatch(text); len(m) > 1 {
			r.Domain = m[1]
		}
		for _, t := range reTool.FindAllStringSubmatch(text, -1) {
			if len(t) > 1 {
				r.Tools = append(r.Tools, t[1])
			}
		}
		if m := reIntent.FindStringSubmatch(text); len(m) > 1 {
			r.Intent = TaskIntent(m[1])
		}
		if m := reScope.FindStringSubmatch(text); len(m) > 1 {
			r.ScopeGlob = m[1]
		}
		rules = append(rules, r)
	}
	return rules, Constitution{Lines: constLines, Source: source}
}

// apiRule 构造 API 层 GuidanceRule：Metadata.source 标记来源，Severity 由 body 是否含 block/warn 决定。
func apiRule(id, title, body, source string) api.GuidanceRule {
	sev := "info"
	if strings.Contains(strings.ToLower(body), "block") {
		sev = "block"
	} else if strings.Contains(strings.ToLower(body), "warn") {
		sev = "warn"
	}
	return api.GuidanceRule{
		ID:          id,
		Name:        title,
		Description: strings.TrimSpace(body),
		Severity:    sev,
		Metadata:    map[string]string{"source": source},
	}
}

// buildManifest 遍历分片统计 RuleCount、按 RiskClass 与 Intent 聚合计数，并写入创建时间。
func buildManifest(bundleID, ver string, shards []RuleShard) RuleManifest {
	m := RuleManifest{
		BundleID:   bundleID,
		Version:    ver,
		ShardCount: len(shards),
		ByRisk:     make(map[string]int),
		ByIntent:   make(map[string]int),
		CreatedAt:  time.Now().UTC(),
	}
	for _, sh := range shards {
		for _, r := range sh.Rules {
			m.RuleCount++
			if r.RiskClass != "" {
				m.ByRisk[string(r.RiskClass)]++
			}
			if r.Intent != "" {
				m.ByIntent[string(r.Intent)]++
			}
		}
	}
	return m
}

// hashID 对输入字符串做 SHA256 并取前 12 字节十六进制作为短 ID。
func hashID(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
}

// uniqueStrings 保持首次出现顺序去重。
func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
