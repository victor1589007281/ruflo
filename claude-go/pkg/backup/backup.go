// Package backup 负责 Claude-Go 运行态关键数据的打包/列举/恢复。
//
// 备份覆盖范围 (位于 stateDir, 默认 .claude-go):
//   - teams/           团队快照 + blackboard + report
//   - memory/          长期记忆 + dreaming 产出
//   - metrics/         指标事件与 summary
//   - evolution/       进化引擎记录 (可选)
//   - statestore/      轨迹底座: KV/Log/Blob (含 TraceStore Span 与正文 blob)
//   - swarm_intel/     群体智能预测与信息素
//   - tasks.json       DAG 任务
//   - cron_jobs.json   定时任务
//   - blackboard.json  历史黑板 (若存在于 stateDir 根)
//   - config.json      项目层 dashboard 配置 (若存在)
//
// 归档位于 stateDir/backups/<timestamp>-<label>.tar.gz
// 备份包内保留原始子路径结构, 恢复时按 "覆盖 | 合并" 语义写回。
//
// 设计目标:
//   - 单一可执行 (Go 原生 archive/tar + compress/gzip, 无外部依赖)
//   - 在 dashboard 进程 (只读) 与 claude-go 主进程 (写) 两侧都可调用
//   - 恢复前会自动产生一份 safety-<timestamp>.tar.gz 作为现场快照
package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Default subsystems (按依赖顺序) — 若目录/文件不存在会静默跳过。
var defaultTargets = []string{
	"teams",
	"memory",
	"metrics",
	"evolution",
	"statestore", // 轨迹 Span + Blob + KV (design/03 §4.1); 漏收会丢学习制品
	"swarm_intel",
	"blackboard",
	"tasks.json",
	"cron_jobs.json",
	"dreaming",
	"config.json",
}

// Manifest 一次备份的元信息, 被写入归档内的 MANIFEST.json。
type Manifest struct {
	Version     string    `json:"version"`
	CreatedAt   time.Time `json:"createdAt"`
	StateDir    string    `json:"stateDir"`
	Label       string    `json:"label,omitempty"`
	Targets     []string  `json:"targets"`
	FileCount   int       `json:"fileCount"`
	BytesTotal  int64     `json:"bytesTotal"`
	Host        string    `json:"host,omitempty"`
	ClaudeGoVer string    `json:"claudeGoVersion,omitempty"`
}

// CreateOptions 创建备份时的选项。
type CreateOptions struct {
	// StateDir 数据根目录 (.claude-go)。必填。
	StateDir string
	// OutPath 自定义归档输出路径。为空时写入 StateDir/backups/。
	OutPath string
	// Label 人类可读标签, 会出现在文件名中 (安全化: 只保留 [-_a-zA-Z0-9])。
	Label string
	// Targets 覆盖默认子系统列表。nil = 使用 defaultTargets。
	Targets []string
	// IncludeReports 是否包含 team/*/REPORT.md (通常体积较大但有价值)。默认 true。
	IncludeReports bool
}

// CreateResult 成功后返回。
type CreateResult struct {
	Path     string   `json:"path"`
	Size     int64    `json:"size"`
	Manifest Manifest `json:"manifest"`
}

// Create 生成备份归档。线程安全 (读-only StateDir)。
func Create(opts CreateOptions) (*CreateResult, error) {
	if opts.StateDir == "" {
		return nil, fmt.Errorf("stateDir 不能为空")
	}
	if info, err := os.Stat(opts.StateDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("stateDir %s 不存在或不是目录", opts.StateDir)
	}
	if len(opts.Targets) == 0 {
		opts.Targets = defaultTargets
	}
	ts := time.Now()
	label := sanitizeLabel(opts.Label)
	if label == "" {
		label = "auto"
	}
	outPath := opts.OutPath
	if outPath == "" {
		backupsDir := filepath.Join(opts.StateDir, "backups")
		if err := os.MkdirAll(backupsDir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir backups: %w", err)
		}
		outPath = filepath.Join(backupsDir,
			fmt.Sprintf("%s-%s.tar.gz", ts.Format("20060102-150405"), label))
	}

	f, err := os.Create(outPath)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", outPath, err)
	}
	defer f.Close()

	gzw := gzip.NewWriter(f)
	defer gzw.Close()
	tw := tar.NewWriter(gzw)
	defer tw.Close()

	manifest := Manifest{
		Version:   "1.0",
		CreatedAt: ts,
		StateDir:  opts.StateDir,
		Label:     label,
		Targets:   opts.Targets,
	}
	host, _ := os.Hostname()
	manifest.Host = host

	for _, t := range opts.Targets {
		src := filepath.Join(opts.StateDir, t)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		n, bytes, err := addPathToTar(tw, src, t, opts)
		if err != nil {
			return nil, fmt.Errorf("archive %s: %w", t, err)
		}
		manifest.FileCount += n
		manifest.BytesTotal += bytes
	}

	// 写入 MANIFEST.json
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	hdr := &tar.Header{
		Name:    "MANIFEST.json",
		Mode:    0o644,
		Size:    int64(len(mb)),
		ModTime: ts,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	if _, err := tw.Write(mb); err != nil {
		return nil, err
	}
	_ = tw.Close()
	_ = gzw.Close()

	stat, _ := os.Stat(outPath)
	size := int64(0)
	if stat != nil {
		size = stat.Size()
	}
	return &CreateResult{
		Path:     outPath,
		Size:     size,
		Manifest: manifest,
	}, nil
}

// addPathToTar 把一个文件或整棵目录树写进 tar, 返回 (文件数, 字节数, 错误)。
//
// 算法解读：
//
//	原理：src 是磁盘上的真实路径, rel 是它在包内的相对前缀。单文件直接入包; 目录则 filepath.Walk
//	      遍历, 每个条目按 Rel(src, path) 换算成包内名, 目录写成 tar.TypeDir 条目 (带尾部 "/")。
//	关键不变量：包内名必须**始终**基于 rel 而不是绝对路径 —— tar 条目名一旦泄漏宿主绝对路径,
//	      解包时就会按那个路径落盘 (路径穿越)。同理跳过 socket/pipe 等特殊文件 (ModeType != 0):
//	      tar 无法忠实表示它们, 强行写入会得到解包失败或语义错误的普通文件。
//	代价与边界：整棵树串行遍历 + 逐文件 io.Copy, 与备份体量线性; 遍历中任一文件读失败即整体失败
//	      (不跳过), 因为"备份少了一个文件却报成功"比备份失败更危险。
//	设计权衡：刻意不跟随符号链接 (Walk 本身不跟随) —— 备份要的是当时的树形状, 跟随链接会把
//	      目标目录整棵复制进来, 体积和语义都不可控。
//
// 注意: 跳过 report.md 的两个条件里, 第二个 (HasSuffix(..., "report.md")) 已覆盖第一个
// (HasSuffix(..., "/report.md")) —— 前者是死条件, 不改变行为。
func addPathToTar(tw *tar.Writer, src, rel string, opts CreateOptions) (fileCount int, totalBytes int64, err error) {
	info, err := os.Stat(src)
	if err != nil {
		return 0, 0, err
	}
	if !info.IsDir() {
		// 单文件直接入包
		n, err := writeFileToTar(tw, src, rel, info)
		return 1, n, err
	}
	err = filepath.Walk(src, func(path string, i os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rp, _ := filepath.Rel(src, path)
		name := filepath.ToSlash(filepath.Join(rel, rp))
		if name == rel {
			name = rel + "/"
		}
		// 跳过报告文件 (非必要且大)
		if !opts.IncludeReports {
			if strings.HasSuffix(strings.ToLower(path), "/report.md") ||
				strings.HasSuffix(strings.ToLower(path), "report.md") {
				return nil
			}
		}
		// 目录也必须写 Header: tar 靠条目名尾部的 "/" 与 TypeDir 标识目录, 漏掉它解包时
		// 目录不会被创建, 后续文件条目会因父目录不存在而失败。
		if i.IsDir() {
			hdr := &tar.Header{
				Name:     name,
				Mode:     0o755,
				ModTime:  i.ModTime(),
				Typeflag: tar.TypeDir,
			}
			return tw.WriteHeader(hdr)
		}
		// 跳过 socket / pipe 等特殊文件
		if i.Mode()&os.ModeType != 0 {
			return nil
		}
		n, err := writeFileToTar(tw, path, name, i)
		if err != nil {
			return err
		}
		fileCount++
		totalBytes += n
		return nil
	})
	return fileCount, totalBytes, err
}

// writeFileToTar 把一个普通文件按 name 写进 tar, 返回写入字节数。
//
// 注意: Header 只填 Name/Mode/Size/ModTime, 不设 Typeflag —— 零值在 archive/tar 里就是普通文件,
// 这也是它只适用于普通文件的原因 (目录/链接走 addPathToTar 里各自的 Header)。
func writeFileToTar(tw *tar.Writer, src, name string, info os.FileInfo) (int64, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	hdr := &tar.Header{
		Name:    name,
		Mode:    int64(info.Mode().Perm()),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, err
	}
	return io.Copy(tw, f)
}

// ListEntry 列表条目。
type ListEntry struct {
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	Size      int64     `json:"size"`
	Label     string    `json:"label,omitempty"`
}

// List 列出 StateDir/backups/ 下所有备份, 按时间倒序。
func List(stateDir string) ([]ListEntry, error) {
	dir := filepath.Join(stateDir, "backups")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ListEntry{}, nil
		}
		return nil, err
	}
	out := []ListEntry{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".tar.gz") && !strings.HasSuffix(name, ".tgz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, name)
		// 从文件名解析时间戳: 20060102-150405-label.tar.gz
		ts := info.ModTime()
		label := ""
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".tar.gz"), ".tgz")
		if len(base) >= 15 && base[8] == '-' {
			if t, err := time.Parse("20060102-150405", base[:15]); err == nil {
				ts = t
			}
			if len(base) > 16 {
				label = base[16:]
			}
		}
		out = append(out, ListEntry{
			Path: path, Name: name, CreatedAt: ts, Size: info.Size(), Label: label,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// RestoreOptions 恢复选项。
type RestoreOptions struct {
	// StateDir 数据根目录 (.claude-go)。必填。
	StateDir string
	// ArchivePath tar.gz 归档绝对路径。必填。
	ArchivePath string
	// SkipSafetyBackup 为 true 时跳过 "恢复前先备份当前现场"。默认 false。
	SkipSafetyBackup bool
	// Overwrite 为 true 时在文件冲突时覆盖写入。默认 true (符合灾难恢复语义)。
	Overwrite bool
}

// RestoreResult 恢复完成后的摘要。
type RestoreResult struct {
	RestoredFiles int       `json:"restoredFiles"`
	SafetyBackup  string    `json:"safetyBackup,omitempty"`
	Manifest      *Manifest `json:"manifest,omitempty"`
}

// Restore 解压归档到 stateDir。
func Restore(opts RestoreOptions) (*RestoreResult, error) {
	if opts.StateDir == "" || opts.ArchivePath == "" {
		return nil, fmt.Errorf("stateDir / archivePath 不能为空")
	}
	if _, err := os.Stat(opts.ArchivePath); err != nil {
		return nil, fmt.Errorf("归档不存在: %w", err)
	}

	res := &RestoreResult{}

	// 安全备份当前 state
	if !opts.SkipSafetyBackup {
		safety, err := Create(CreateOptions{
			StateDir:       opts.StateDir,
			Label:          "safety-before-restore",
			IncludeReports: true,
		})
		if err == nil && safety != nil {
			res.SafetyBackup = safety.Path
		}
		// 不强制失败, 让用户手动补救
	}

	f, err := os.Open(opts.ArchivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	// 防护: 禁止 zip-slip (解压路径越界)。
	root, _ := filepath.Abs(opts.StateDir)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar next: %w", err)
		}
		name := filepath.Clean(hdr.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			continue
		}
		target := filepath.Join(opts.StateDir, name)
		absTarget, _ := filepath.Abs(target)
		if !strings.HasPrefix(absTarget, root+string(filepath.Separator)) && absTarget != root {
			continue
		}

		if hdr.Name == "MANIFEST.json" {
			b, err := io.ReadAll(tr)
			if err == nil {
				var m Manifest
				if json.Unmarshal(b, &m) == nil {
					res.Manifest = &m
				}
			}
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(target, 0o755)
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, err
			}
			// 冲突处理
			if _, err := os.Stat(target); err == nil && !opts.Overwrite {
				// 跳过已有
				continue
			}
			of, err := os.Create(target)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(of, tr); err != nil {
				of.Close()
				return nil, err
			}
			_ = of.Close()
			res.RestoredFiles++
		}
	}
	return res, nil
}

// ReadManifest 仅打开 tar.gz 读取头部的 MANIFEST.json (常用于前端列表展示)。
func ReadManifest(archivePath string) (*Manifest, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == "MANIFEST.json" {
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			var m Manifest
			if err := json.Unmarshal(b, &m); err != nil {
				return nil, err
			}
			return &m, nil
		}
	}
	return nil, fmt.Errorf("MANIFEST.json not found in %s", archivePath)
}

// Delete 安全地删除一个备份文件 (限定在 stateDir/backups 目录内)。
func Delete(stateDir, archivePath string) error {
	root, _ := filepath.Abs(filepath.Join(stateDir, "backups"))
	abs, err := filepath.Abs(archivePath)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return fmt.Errorf("路径越权: %s 不在 %s 之下", abs, root)
	}
	return os.Remove(abs)
}

// ======================================================================
// utils
// ======================================================================

// sanitizeLabel 把用户给的标签净化成安全文件名片段: 只留 [A-Za-z0-9_-], 并截到 32 字节。
//
// 白名单而不是黑名单: 标签会直接拼进 <时间戳>-<label>.tar.gz, 放过 `/` 或 `..` 就能写到
// backups 目录之外。截断按字节做在这里是安全的 —— 白名单已经排除了多字节字符。
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}
