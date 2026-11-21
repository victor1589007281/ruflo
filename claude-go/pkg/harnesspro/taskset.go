package harnesspro

// taskset.go — 可注册、可复现的外部任务集。
//
// 一个任务集目录至少有 taskset.json:
//   {"name":"support-json","version":"v1","tasks":[...]}
// 也可把任务放在同目录 tasks.json, 并在 manifest 里写 "tasks_file":"tasks.json"。
// 任务仍使用本包的声明式 prompt + 冻结断言考场; 这不是 Terminal-Bench 2 的
// 工具型执行器适配, source 元数据不会改变执行体能力边界。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TaskSet 一份命名、带版本的外部考场。
type TaskSet struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source,omitempty"`
	TasksFile   string `json:"tasks_file,omitempty"`
	Hash        string `json:"hash"`
	TaskCount   int    `json:"task_count"`
	ImportedAt  int64  `json:"imported_at,omitempty"`
	Tasks       []Task `json:"tasks,omitempty"`
}

// TaskSetRef 是运行报告与版本观测里使用的不可变任务集坐标。
type TaskSetRef struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Hash      string `json:"hash"`
	TaskCount int    `json:"task_count"`
}

// Ref 取任务集的稳定坐标 (不随注册时间变化)。
func (s TaskSet) Ref() TaskSetRef {
	return TaskSetRef{Name: s.Name, Version: s.Version, Hash: s.Hash, TaskCount: len(s.Tasks)}
}

// IdentifyTaskSet 计算任务内容 hash。任务顺序属于考场定义 (DPP 平局与切分可复现)。
func IdentifyTaskSet(name, version string, tasks []Task) TaskSetRef {
	b, _ := json.Marshal(tasks)
	sum := sha256.Sum256(b)
	return TaskSetRef{
		Name: name, Version: version,
		Hash: "ts-" + hex.EncodeToString(sum[:]), TaskCount: len(tasks),
	}
}

// ValidateTaskPool 是 CLI、HTTP 与任务集注册共用的唯一任务池校验。
func ValidateTaskPool(pool []Task) error {
	if len(pool) < 2 {
		return fmt.Errorf("任务池至少 2 题 (训练/验证需要切分), got %d", len(pool))
	}
	seen := map[string]bool{}
	for i, t := range pool {
		t.ID = strings.TrimSpace(t.ID)
		if t.ID == "" {
			return fmt.Errorf("pool[%d] 缺 id", i)
		}
		if seen[t.ID] {
			return fmt.Errorf("pool 题 id 重复: %q", t.ID)
		}
		seen[t.ID] = true
		if strings.TrimSpace(t.Prompt) == "" {
			return fmt.Errorf("pool[%d] (%s) 缺 prompt —— 任务描述是契约的唯一来源", i, t.ID)
		}
	}
	return nil
}

// validTaskSetPart 同时约束显示名与磁盘路径, 拒绝分隔符、dot 路径和控制符。
func validTaskSetPart(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// LoadTaskSet 从目录或 JSON 文件加载并校验。外部文件只作为数据读取, 不执行其中任何内容。
func LoadTaskSet(path string) (TaskSet, error) {
	var set TaskSet
	info, err := os.Stat(path)
	if err != nil {
		return set, fmt.Errorf("读任务集失败: %w", err)
	}
	manifest := path
	if info.IsDir() {
		manifest = filepath.Join(path, "taskset.json")
	}
	b, err := os.ReadFile(manifest)
	if err != nil {
		return set, fmt.Errorf("读任务集 manifest 失败: %w", err)
	}
	if err := json.Unmarshal(b, &set); err != nil {
		return set, fmt.Errorf("任务集 manifest JSON 错误: %w", err)
	}
	if len(set.Tasks) == 0 && set.TasksFile != "" {
		if !validTaskSetPart(set.TasksFile) {
			return set, fmt.Errorf("tasks_file 只能是当前目录下的简单文件名: %q", set.TasksFile)
		}
		tb, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), set.TasksFile))
		if err != nil {
			return set, fmt.Errorf("读 tasks_file 失败: %w", err)
		}
		if err := json.Unmarshal(tb, &set.Tasks); err != nil {
			return set, fmt.Errorf("tasks_file JSON 错误 (应为任务数组): %w", err)
		}
	}
	if err := ValidateTaskSet(set); err != nil {
		return TaskSet{}, err
	}
	ref := IdentifyTaskSet(set.Name, set.Version, set.Tasks)
	set.Hash, set.TaskCount = ref.Hash, ref.TaskCount
	return set, nil
}

// ValidateTaskSet 校验 manifest 与任务池; hash 由加载/注册时重算, 不信外部申报值。
func ValidateTaskSet(set TaskSet) error {
	if !validTaskSetPart(set.Name) {
		return fmt.Errorf("任务集 name 非法 (仅字母数字 ._- , 1-128 字符): %q", set.Name)
	}
	if !validTaskSetPart(set.Version) {
		return fmt.Errorf("任务集 version 非法 (仅字母数字 ._- , 1-128 字符): %q", set.Version)
	}
	return ValidateTaskPool(set.Tasks)
}

// TaskSetStore 是 <StateDir>/harness-evo/tasksets 下的目录注册表。
type TaskSetStore struct {
	Root string
}

func NewTaskSetStore(root string) *TaskSetStore { return &TaskSetStore{Root: root} }

// Register 原子写入 name/version; 同坐标不同内容拒绝覆盖, 防止旧运行结果被换考场。
func (s *TaskSetStore) Register(set TaskSet) error {
	if s == nil || strings.TrimSpace(s.Root) == "" {
		return fmt.Errorf("任务集注册目录未配置")
	}
	if err := ValidateTaskSet(set); err != nil {
		return err
	}
	ref := IdentifyTaskSet(set.Name, set.Version, set.Tasks)
	set.Hash, set.TaskCount = ref.Hash, ref.TaskCount
	set.TasksFile = "" // 注册后的 manifest 自包含, 不再依赖源目录
	if set.ImportedAt == 0 {
		set.ImportedAt = time.Now().Unix()
	}
	path := filepath.Join(s.Root, set.Name, set.Version, "taskset.json")
	if old, err := LoadTaskSet(path); err == nil {
		if old.Hash != set.Hash {
			return fmt.Errorf("任务集 %s@%s 已存在且内容不同 (old=%s new=%s), 请提升 version",
				set.Name, set.Version, old.Hash, set.Hash)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("已注册任务集 %s@%s 不可读, 拒绝覆盖: %w", set.Name, set.Version, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".taskset-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// List 返回注册摘要 (不携带完整 prompt), 名称/版本/导入时间稳定排序。
func (s *TaskSetStore) List() ([]TaskSet, error) {
	out := []TaskSet{}
	if s == nil || s.Root == "" {
		return out, nil
	}
	names, err := os.ReadDir(s.Root)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if !name.IsDir() {
			continue
		}
		versions, err := os.ReadDir(filepath.Join(s.Root, name.Name()))
		if err != nil {
			return nil, err
		}
		for _, version := range versions {
			if !version.IsDir() {
				continue
			}
			set, err := LoadTaskSet(filepath.Join(s.Root, name.Name(), version.Name(), "taskset.json"))
			if err != nil {
				return nil, fmt.Errorf("已注册任务集 %s/%s 损坏: %w", name.Name(), version.Name(), err)
			}
			set.Tasks = nil
			out = append(out, set)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].ImportedAt != out[j].ImportedAt {
			return out[i].ImportedAt > out[j].ImportedAt
		}
		return out[i].Version > out[j].Version
	})
	return out, nil
}

// Get 解析 name@version; version 省略时取最近导入的版本。
func (s *TaskSetStore) Get(ref string) (TaskSet, error) {
	var zero TaskSet
	name, version, _ := strings.Cut(strings.TrimSpace(ref), "@")
	if !validTaskSetPart(name) || version != "" && !validTaskSetPart(version) {
		return zero, fmt.Errorf("任务集引用应为 name[@version]: %q", ref)
	}
	if version != "" {
		set, err := LoadTaskSet(filepath.Join(s.Root, name, version, "taskset.json"))
		if err != nil {
			return zero, fmt.Errorf("任务集 %s 未注册或不可读: %w", ref, err)
		}
		return set, nil
	}
	all, err := s.List()
	if err != nil {
		return zero, err
	}
	for _, summary := range all {
		if summary.Name == name {
			return s.Get(name + "@" + summary.Version)
		}
	}
	return zero, fmt.Errorf("任务集 %s 未注册", name)
}
