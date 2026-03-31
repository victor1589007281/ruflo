package guidance

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	"github.com/ruflo/ruflo-go/api"
)

var (
	reRuleID = regexp.MustCompile(`(?m)^\s*(R\d{3})\s*:`)
	reRisk   = regexp.MustCompile(`(?i)risk:\s*(critical|high|medium|low|info)\b`)
	reDomain = regexp.MustCompile(`@([a-z][a-z0-9_-]*)`)
	reTool   = regexp.MustCompile(`\[([a-z][a-z0-9_-]*)\]`)
	reIntent = regexp.MustCompile(`#(bug-fix|feature|refactor|security|performance|testing|docs)\b`)
	reScope  = regexp.MustCompile(`(?i)scope:\s*(\S+)`)
)

// GuidanceCompiler parses CLAUDE.md-style markdown into a PolicyBundle.
type GuidanceCompiler struct{}

// NewGuidanceCompiler returns a stateless compiler.
func NewGuidanceCompiler() *GuidanceCompiler {
	return &GuidanceCompiler{}
}

// Compile merges root and local markdown; local rules override on same ID.
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

func hashID(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
}

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
