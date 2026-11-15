// evo_experience_cmd.go — 经验库共享存储的运维命令。
//
//	claude-go evo experience stats   [--agentdb URL] [--bucket B]
//	claude-go evo experience import  --dir <path[,path...]> [--dry-run] [--include-fixtures]
//	claude-go evo experience compact [--ttl 2160h]
//
// import 用于把历史上散落在 <state>/evolution 与各 pod 目录里的 experiences.json
// 合并进 agentDB（按条目 ID 去重、同 ID 取较新版本），之后所有副本共享同一份经验库。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/spf13/cobra"
)

var fixtureExperienceID = regexp.MustCompile(`^(exp|e)[0-9]+$`)

func evoExperienceCmd(resolveStateDir func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "experience",
		Short: "共享经验库运维: 导入历史文件 / 查看统计 / 清理墓碑 (agentDB)",
	}
	cmd.AddCommand(
		evoExperienceStatsCmd(),
		evoExperienceImportCmd(resolveStateDir),
		evoExperienceCompactCmd(),
	)
	return cmd
}

func experienceStoreFlags(cmd *cobra.Command) (agentdb, bucket *string) {
	agentdb = cmd.Flags().String("agentdb", "", "agentDB 地址 (默认 $CLAUDE_GO_AGENTDB_URL / $AGENTDB_URL)")
	bucket = cmd.Flags().String("bucket", "", "bucket 名 (默认 $CLAUDE_GO_EXPERIENCE_BUCKET / claude-go-experience)")
	return agentdb, bucket
}

func resolveExperienceStore(agentdbFlag, bucketFlag string) (*agent.AgentDBExperienceStore, error) {
	base := strings.TrimSpace(agentdbFlag)
	if base == "" {
		base = strings.TrimSpace(os.Getenv("CLAUDE_GO_AGENTDB_URL"))
	}
	if base == "" {
		base = strings.TrimSpace(os.Getenv("AGENTDB_URL"))
	}
	if base == "" {
		return nil, fmt.Errorf("需要 agentDB 地址: --agentdb 或 CLAUDE_GO_AGENTDB_URL/AGENTDB_URL")
	}
	bucket := strings.TrimSpace(bucketFlag)
	if bucket == "" {
		bucket = strings.TrimSpace(os.Getenv("CLAUDE_GO_EXPERIENCE_BUCKET"))
	}
	return agent.NewAgentDBExperienceStore(base, bucket), nil
}

func experienceContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Minute)
}

// ─── stats ────────────────────────────────────────────────────────────────────

func evoExperienceStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "统计 agentDB 中的经验库",
		RunE: func(cmd *cobra.Command, _ []string) error {
			agentdbFlag, _ := cmd.Flags().GetString("agentdb")
			bucketFlag, _ := cmd.Flags().GetString("bucket")
			store, err := resolveExperienceStore(agentdbFlag, bucketFlag)
			if err != nil {
				return err
			}
			ctx, cancel := experienceContext()
			defer cancel()
			snap, err := store.Load(ctx)
			if err != nil {
				return fmt.Errorf("读取经验库失败: %w", err)
			}

			fmt.Printf("agentDB %s bucket=%s\n", strings.TrimRight(agentdbFlagOrEnv(agentdbFlag), "/"), store.Bucket())
			fmt.Printf("有效经验 %d 条, 墓碑 %d 条\n", snap.LiveCount(), snap.TombstoneCount())

			byCategory := map[string]int{}
			byLifecycle := map[string]int{}
			byRole := map[string]int{}
			var quality float64
			var triadic int
			for _, e := range snap.Records {
				byCategory[e.Category]++
				byLifecycle[defaultString(e.Lifecycle, "(none)")]++
				if e.Role != "" {
					byRole[e.Role]++
				}
				quality += e.Quality
				if e.IsTriadic() {
					triadic++
				}
			}
			printCountMap("category", byCategory)
			printCountMap("lifecycle", byLifecycle)
			printCountMap("role(top15)", topN(byRole, 15))
			if len(snap.Records) > 0 {
				fmt.Printf("平均质量 %.3f, 三段式覆盖 %d/%d (%.0f%%)\n",
					quality/float64(len(snap.Records)), triadic, len(snap.Records),
					100*float64(triadic)/float64(len(snap.Records)))
			}
			return nil
		},
	}
	experienceStoreFlags(cmd)
	return cmd
}

// ─── compact ──────────────────────────────────────────────────────────────────

func evoExperienceCompactCmd() *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "compact",
		Short: "清理超过 TTL 的墓碑（物理删除已删条目）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			agentdbFlag, _ := cmd.Flags().GetString("agentdb")
			bucketFlag, _ := cmd.Flags().GetString("bucket")
			store, err := resolveExperienceStore(agentdbFlag, bucketFlag)
			if err != nil {
				return err
			}
			if ttl <= 0 {
				ttl = agent.TombstoneTTLFromEnv()
			}
			ctx, cancel := experienceContext()
			defer cancel()
			n, err := store.Compact(ctx, ttl)
			if err != nil {
				return fmt.Errorf("清理墓碑失败 (已清理 %d 条): %w", n, err)
			}
			fmt.Printf("清理墓碑 %d 条 (TTL %s)\n", n, ttl)
			return nil
		},
	}
	experienceStoreFlags(cmd)
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "墓碑保留时长 (默认 $CLAUDE_GO_EXPERIENCE_TOMBSTONE_TTL / 2160h)")
	return cmd
}

// ─── import ───────────────────────────────────────────────────────────────────

type sourcedExperience struct {
	record *agent.Experience
	path   string
}

func evoExperienceImportCmd(resolveStateDir func() string) *cobra.Command {
	var dirs []string
	var dryRun, includeFixtures bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "把历史 experiences.json（含各 pod 残留目录）合并导入 agentDB",
		Long: "递归扫描 --dir 下的 experiences.json，按条目 ID 合并（同 ID 取 UpdatedAt 较新者），\n" +
			"默认跳过测试夹具（id 形如 exp1/e1 且无三段式内容）。默认 dry-run 关闭时会真实写入。",
		RunE: func(cmd *cobra.Command, _ []string) error {
			agentdbFlag, _ := cmd.Flags().GetString("agentdb")
			bucketFlag, _ := cmd.Flags().GetString("bucket")
			if len(dirs) == 0 {
				dirs = []string{filepath.Join(resolveStateDir(), "evolution")}
			}
			return runExperienceImport(agentdbFlag, bucketFlag, dirs, dryRun, includeFixtures)
		},
	}
	experienceStoreFlags(cmd)
	cmd.Flags().StringArrayVar(&dirs, "dir", nil, "要扫描的文件或目录 (可重复; 默认 <state>/evolution)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "只扫描/合并并打印结果，不写 agentDB")
	cmd.Flags().BoolVar(&includeFixtures, "include-fixtures", false, "连测试夹具（exp1..exp5/e1..e5）一起导入")
	return cmd
}

func runExperienceImport(agentdbFlag, bucketFlag string, dirs []string, dryRun, includeFixtures bool) error {
	files, err := collectExperienceFiles(dirs)
	if err != nil {
		return err
	}
	fmt.Printf("扫描 %d 个 experiences.json\n", len(files))

	var (
		total, parseFailed, fixtures int
		merged                       = map[string]sourcedExperience{}
		conflicts                    []string
		fixtureIDs                   = map[string]struct{}{}
	)
	for _, path := range files {
		records, err := readExperienceFile(path)
		if err != nil {
			parseFailed++
			fmt.Printf("  ⚠️  %s: %v\n", path, err)
			continue
		}
		total += len(records)
		for _, rec := range records {
			if rec == nil || rec.ID == "" {
				continue
			}
			if isFixtureExperience(rec) {
				fixtures++
				fixtureIDs[rec.ID] = struct{}{}
				if !includeFixtures {
					continue
				}
			}
			prev, ok := merged[rec.ID]
			if !ok {
				merged[rec.ID] = sourcedExperience{record: rec, path: path}
				continue
			}
			pat, ptie := agent.ExperienceVersion(prev.record)
			at, tie := agent.ExperienceVersion(rec)
			if at.After(pat) || (at.Equal(pat) && tie > ptie) {
				merged[rec.ID] = sourcedExperience{record: rec, path: path}
			}
			if agent.ExperienceFingerprint(prev.record) != agent.ExperienceFingerprint(rec) {
				if len(conflicts) < 20 {
					conflicts = append(conflicts, fmt.Sprintf("%s (%s ↔ %s)", rec.ID, prev.path, path))
				}
			}
		}
	}

	fmt.Printf("读取 %d 条, 解析失败文件 %d 个, 跳过测试夹具 %d 条 (唯一 ID %d)\n",
		total, parseFailed, fixtures, len(fixtureIDs))
	if includeFixtures {
		fmt.Printf("  ⚠️  --include-fixtures 已开启，夹具也计入导入\n")
	}
	fmt.Printf("按 ID 合并后 %d 条, 同 ID 内容冲突 %d 处\n", len(merged), len(conflicts))
	for _, c := range conflicts {
		fmt.Printf("  · %s\n", c)
	}
	if dryRun {
		fmt.Printf("dry-run: 未写入 agentDB\n")
		return nil
	}

	store, err := resolveExperienceStore(agentdbFlag, bucketFlag)
	if err != nil {
		return err
	}
	records := make([]*agent.Experience, 0, len(merged))
	for _, s := range merged {
		records = append(records, s.record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })

	ctx, cancel := experienceContext()
	defer cancel()
	res, err := store.Sync(ctx, records, nil)
	if err != nil {
		return fmt.Errorf("写入 agentDB 失败 (已写 %d/跳过 %d): %w", res.Written, res.Skipped, err)
	}
	fmt.Printf("写入 agentDB %s bucket=%s: 写 %d / 跳过(远端更新) %d\n",
		strings.TrimRight(agentdbFlagOrEnv(agentdbFlag), "/"), store.Bucket(), res.Written, res.Skipped)
	return nil
}

func collectExperienceFiles(dirs []string) ([]string, error) {
	seen := map[string]struct{}{}
	var files []string
	add := func(path string) {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		if _, ok := seen[abs]; ok {
			return
		}
		seen[abs] = struct{}{}
		files = append(files, abs)
	}
	for _, root := range dirs {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		info, err := os.Stat(root)
		if err != nil {
			fmt.Printf("  ⚠️  %s: %v\n", root, err)
			continue
		}
		if !info.IsDir() {
			add(root)
			continue
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // 单个目录不可读不影响其余
			}
			if d.IsDir() {
				name := d.Name()
				// 注意不能跳过所有 "." 开头目录：worker 的状态目录就是
				// <pod>/.claude-go/evolution，跳过它会漏掉全部残留经验。
				if path != root && (name == ".git" || name == "node_modules" || name == "vendor") {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() == "experiences.json" {
				add(path)
			}
			return nil
		})
	}
	sort.Strings(files)
	return files, nil
}

func readExperienceFile(path string) ([]*agent.Experience, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var records []*agent.Experience
	if err := json.Unmarshal(data, &records); err == nil {
		return records, nil
	}
	// 兼容 {"experiences": [...]} 包装。
	var wrapped struct {
		Experiences []*agent.Experience `json:"experiences"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Experiences, nil
}

// isFixtureExperience 识别早期测试种子：ID 为 exp1..exp5 / e1..e5。
// 真实经验的 ID 形如 exp-<unix>-<n>，不会命中这个模式；这些短 ID 全部来自
// 10-22 的测试种库（部分只有 {id,task,lesson}，部分是 team/test 种下的结构化条目）。
// 默认跳过，--include-fixtures 可一并导入。
func isFixtureExperience(e *agent.Experience) bool {
	return e != nil && fixtureExperienceID.MatchString(e.ID)
}

// ─── 小工具 ───────────────────────────────────────────────────────────────────

func agentdbFlagOrEnv(flag string) string {
	if strings.TrimSpace(flag) != "" {
		return flag
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_GO_AGENTDB_URL")); v != "" {
		return v
	}
	return os.Getenv("AGENTDB_URL")
}

func printCountMap(label string, m map[string]int) {
	if len(m) == 0 {
		fmt.Printf("%s: (空)\n", label)
		return
	}
	type kv struct {
		k string
		v int
	}
	items := make([]kv, 0, len(m))
	for k, v := range m {
		items = append(items, kv{k, v})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].v != items[j].v {
			return items[i].v > items[j].v
		}
		return items[i].k < items[j].k
	})
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s=%d", it.k, it.v))
	}
	fmt.Printf("%s: %s\n", label, strings.Join(parts, ", "))
}

func topN(m map[string]int, n int) map[string]int {
	items := make([]kvInt, 0, len(m))
	for k, v := range m {
		items = append(items, kvInt{k, v})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].v != items[j].v {
			return items[i].v > items[j].v
		}
		return items[i].k < items[j].k
	})
	if len(items) > n {
		items = items[:n]
	}
	out := make(map[string]int, len(items))
	for _, it := range items {
		out[it.k] = it.v
	}
	return out
}

type kvInt struct {
	k string
	v int
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
