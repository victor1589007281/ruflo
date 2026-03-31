package hooks

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ruflo/ruflo-go/api"
	"github.com/ruflo/ruflo-go/pkg/embeddings"
	"github.com/ruflo/ruflo-go/pkg/memory"
	"github.com/ruflo/ruflo-go/pkg/neural"
	"github.com/ruflo/ruflo-go/pkg/security"
)

var workerDeps struct {
	Memory memory.MemoryService
	SONA   *neural.SONACoordinator
}

// SetHookWorkerDeps wires optional memory and SONA instances used by background workers.
func SetHookWorkerDeps(mem memory.MemoryService, sona *neural.SONACoordinator) {
	workerDeps.Memory = mem
	workerDeps.SONA = sona
}

func attachRealHandlers(cfgs []WorkerConfig) []WorkerConfig {
	handlers := map[string]func(context.Context, WorkerContext) WorkerResult{
		"ultralearn":  workerUltralearn,
		"optimize":    workerOptimize,
		"consolidate": workerConsolidate,
		"predict":     workerPredict,
		"audit":       workerAudit,
		"map":         workerMap,
		"preload":     workerPreload,
		"deepdive":    workerDeepdive,
		"document":    workerDocument,
		"refactor":    workerRefactor,
		"benchmark":   workerBenchmark,
		"testgaps":    workerTestGaps,
	}
	for i := range cfgs {
		if h, ok := handlers[cfgs[i].Name]; ok {
			cfgs[i].Handler = h
		}
	}
	return cfgs
}

func argString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func workerRoot(wc WorkerContext) string {
	r := argString(wc.Args, "root")
	if r != "" {
		return r
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func workerUltralearn(ctx context.Context, wc WorkerContext) WorkerResult {
	query := argString(wc.Args, "query")
	if query == "" {
		query = wc.Trigger
	}
	data := map[string]any{"query": query, "suggestions": []string{}}
	if workerDeps.Memory != nil {
		res, err := workerDeps.Memory.Search(query, api.SearchOptions{K: 8, EF: 64})
		if err != nil {
			return WorkerResult{OK: false, Message: err.Error(), Data: data}
		}
		var sug []string
		for _, r := range res {
			if r.Entry != nil {
				sug = append(sug, fmt.Sprintf("%s/%s: %s", r.Entry.Namespace, r.Entry.Key, truncate(r.Entry.Value, 120)))
			}
		}
		data["suggestions"] = sug
		data["hits"] = len(sug)
		return WorkerResult{OK: true, Message: "memory pattern suggestions", Data: data}
	}
	// Heuristic tips from trigger keywords
	kw := strings.Fields(strings.ToLower(query))
	tips := []string{"capture successful edits as reusable patterns", "store task context in namespace 'patterns' before long runs"}
	for _, k := range kw {
		if strings.Contains(k, "test") {
			tips = append(tips, "mirror production paths in test layout")
		}
		if strings.Contains(k, "sec") {
			tips = append(tips, "validate inputs at CLI and MCP boundaries")
		}
	}
	data["suggestions"] = tips
	return WorkerResult{OK: true, Message: "learning suggestions (no memory backend)", Data: data}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func workerOptimize(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	var files int64
	var total int64
	var largest string
	var maxSz int64
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if skipExt(filepath.Ext(path)) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		files++
		sz := fi.Size()
		total += sz
		if sz > maxSz {
			maxSz = sz
			largest = path
		}
		return nil
	})
	avg := int64(0)
	if files > 0 {
		avg = total / files
	}
	return WorkerResult{OK: true, Message: "file size scan", Data: map[string]any{
		"root": root, "file_count": files, "total_bytes": total, "avg_bytes": avg,
		"largest_file": largest, "largest_bytes": maxSz,
	}}
}

func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "build":
		return true
	default:
		return false
	}
}

func skipExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".png", ".jpg", ".jpeg", ".gif", ".zip", ".tar", ".gz":
		return true
	default:
		return false
	}
}

func workerConsolidate(ctx context.Context, wc WorkerContext) WorkerResult {
	if workerDeps.SONA == nil {
		return WorkerResult{OK: true, Message: "no SONA coordinator configured", Data: map[string]any{"merged": 0}}
	}
	n := workerDeps.SONA.ConsolidatePatterns(0.92)
	return WorkerResult{OK: true, Message: fmt.Sprintf("merged %d duplicate pattern(s)", n), Data: map[string]any{"merged": n}}
}

func workerPredict(ctx context.Context, wc WorkerContext) WorkerResult {
	query := argString(wc.Args, "query")
	if query == "" {
		query = wc.Trigger
	}
	data := map[string]any{"query": query}
	if workerDeps.Memory != nil {
		hits, err := workerDeps.Memory.Search(query, api.SearchOptions{K: 5})
		if err != nil {
			return WorkerResult{OK: false, Message: err.Error()}
		}
		var preds []string
		for _, h := range hits {
			if h.Entry != nil {
				preds = append(preds, h.Entry.Value)
			}
		}
		data["predictions"] = preds
		return WorkerResult{OK: true, Message: "memory-based predictions", Data: data}
	}
	if workerDeps.SONA != nil {
		vec := embeddings.HashEmbed384(query)
		sim := workerDeps.SONA.FindSimilarPatterns(vec, 5)
		var preds []string
		for _, p := range sim {
			preds = append(preds, p.Content)
		}
		data["predictions"] = preds
		return WorkerResult{OK: true, Message: "SONA similarity predictions", Data: data}
	}
	data["predictions"] = []string{"configure memory or SONA for richer predictions"}
	return WorkerResult{OK: true, Message: "fallback prediction stub", Data: data}
}

func workerAudit(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return WorkerResult{OK: false, Message: err.Error()}
	}
	pv := security.NewPathValidator(absRoot)
	v := security.NewInputValidator()
	secretLike := regexp.MustCompile(`(?i)(api[_-]?key|secret|password|bearer)\s*[=:]\s*['\"]?[a-z0-9]{16,}`)
	var issues []string
	var filesScanned int
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".json") {
			return nil
		}
		if _, err := pv.Validate(path, false); err != nil {
			issues = append(issues, fmt.Sprintf("path %s: %v", path, err))
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		filesScanned++
		s := string(b)
		if secretLike.MatchString(s) {
			issues = append(issues, fmt.Sprintf("%s: possible hardcoded credential pattern", path))
		}
		res := v.ValidateString("file", s[:min(len(s), 8192)])
		for _, iss := range res.Issues {
			if iss.Severity == security.SeverityBlock {
				issues = append(issues, fmt.Sprintf("%s: %s", path, iss.Message))
			}
		}
		return nil
	})
	return WorkerResult{OK: len(issues) == 0, Message: fmt.Sprintf("scanned %d files", filesScanned), Data: map[string]any{
		"root": absRoot, "files_scanned": filesScanned, "issues": issues,
	}}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type fileMapEntry struct {
	Path  string `json:"path"`
	Lines int    `json:"lines"`
	Bytes int64  `json:"bytes"`
}

func workerMap(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	var entries []fileMapEntry
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := 0
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			lines++
		}
		rel, _ := filepath.Rel(root, path)
		entries = append(entries, fileMapEntry{Path: rel, Lines: lines, Bytes: int64(len(b))})
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return WorkerResult{OK: true, Message: "code map", Data: map[string]any{"root": root, "files": entries, "count": len(entries)}}
}

func workerPreload(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	dir := filepath.Join(root, ".claude-flow")
	fi, err := os.ReadDir(dir)
	if err != nil {
		return WorkerResult{OK: true, Message: "no .claude-flow directory", Data: map[string]any{"dir": dir, "entries": []string{}}}
	}
	var names []string
	for _, e := range fi {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return WorkerResult{OK: true, Message: "preload manifest", Data: map[string]any{"dir": dir, "entries": names}}
}

func workerDeepdive(ctx context.Context, wc WorkerContext) WorkerResult {
	path := argString(wc.Args, "file")
	if path == "" {
		path = argString(wc.Args, "path")
	}
	if path == "" {
		return WorkerResult{OK: false, Message: "missing args.file or args.path"}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return WorkerResult{OK: false, Message: err.Error()}
	}
	s := string(b)
	lines := strings.Count(s, "\n") + 1
	funcs := regexp.MustCompile(`(?m)^func\s+(\([^)]+\)\s+)?(\w+)\s*\(`).FindAllString(s, -1)
	longLines := 0
	for _, line := range strings.Split(s, "\n") {
		if len(line) > 120 {
			longLines++
		}
	}
	return WorkerResult{OK: true, Message: "complexity snapshot", Data: map[string]any{
		"path": path, "bytes": len(b), "lines": lines, "func_decls": len(funcs),
		"long_lines_gt_120": longLines,
	}}
}

var goFuncSigRE = regexp.MustCompile(`(?m)^func\s+(\([^)]*\)\s+)?(\w+)\s*\([^)]*\)`)

func workerDocument(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	var sigs []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range goFuncSigRE.FindAllString(string(b), -1) {
			rel, _ := filepath.Rel(root, path)
			sigs = append(sigs, rel+": "+strings.TrimSpace(m))
		}
		return nil
	})
	sort.Strings(sigs)
	if len(sigs) > 200 {
		sigs = sigs[:200]
	}
	return WorkerResult{OK: true, Message: "signatures", Data: map[string]any{"signatures": sigs, "count": len(sigs)}}
}

func workerRefactor(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	ifPat := regexp.MustCompile(`\bif\b[^\n]{200,}`)
	longParams := regexp.MustCompile(`func\s+\w+\s*\([^)]{120,}\)`)
	var hints []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		s := string(b)
		if ifPat.MatchString(s) {
			hints = append(hints, filepath.Base(path)+": very long if condition — consider guard clauses")
		}
		if longParams.MatchString(s) {
			hints = append(hints, filepath.Base(path)+": long parameter list — consider a struct")
		}
		if strings.Contains(s, "interface{}") && !strings.Contains(path, "vendor") {
			hints = append(hints, filepath.Base(path)+": replace interface{} with any or a concrete type")
		}
		return nil
	})
	return WorkerResult{OK: true, Message: "refactor hints", Data: map[string]any{"hints": hints}}
}

func workerBenchmark(ctx context.Context, wc WorkerContext) WorkerResult {
	dim := embeddings.HashEmbeddingDim
	idx := memory.NewHNSWIndex(dim, memory.CosineDistance)
	n := 2000
	if s := argString(wc.Args, "vectors"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			n = v
		}
	}
	if n > 10000 {
		n = 10000
	}
	if n < 100 {
		n = 100
	}
	for i := 0; i < n; i++ {
		vec := embeddings.HashEmbed384(fmt.Sprintf("bench-%d", i))
		if err := idx.Insert(uint64(i+1), vec); err != nil {
			return WorkerResult{OK: false, Message: err.Error()}
		}
	}
	q := embeddings.HashEmbed384("bench-query")
	start := time.Now()
	hits := idx.Search(q, 10, 64)
	elapsed := time.Since(start)
	return WorkerResult{OK: true, Message: "HNSW search timing", Data: map[string]any{
		"vectors": n, "neighbors": len(hits), "duration_ns": elapsed.Nanoseconds(),
		"duration_ms": float64(elapsed.Microseconds()) / 1000,
	}}
}

func workerTestGaps(ctx context.Context, wc WorkerContext) WorkerResult {
	root := workerRoot(wc)
	allGo := map[string]bool{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			rel, _ := filepath.Rel(root, path)
			allGo[rel] = true
		}
		return nil
	})
	var missing []string
	for f := range allGo {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		testF := strings.TrimSuffix(f, ".go") + "_test.go"
		if !allGo[testF] {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	return WorkerResult{OK: true, Message: "test gap scan", Data: map[string]any{"missing_test_for": missing, "count": len(missing)}}
}
