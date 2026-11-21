package harnesspro

// versions.go — 跨运行 harness 版本仓库。
//
// VersionID = hash(run_id, candidate_id, instructions): 唯一谱系节点。
// ContentID = hash(instructions): 同一指令内容跨运行的共同坐标。
// 每个版本一个原子替换的 JSON 文件; rollout/eval 的聚合观测追加到该版本,
// 原始题级/断言级轨迹仍留在各 run 的 journal/report, 两边用相同版本 ID 连接。

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
	"sync"
	"time"
)

// HarnessContentID 指令全文的内容寻址 ID。
func HarnessContentID(instructions string) string {
	sum := sha256.Sum256([]byte(instructions))
	return "hc-" + hex.EncodeToString(sum[:])
}

// HarnessVersionID 运行内候选谱系的全局唯一 ID。
func HarnessVersionID(runID, candidateID, instructions string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + candidateID + "\x00" + instructions))
	return "hv-" + hex.EncodeToString(sum[:16])
}

// IdentifyHarness 补齐 harness 的版本坐标; 已带坐标时校验而非静默改写。
func IdentifyHarness(h Harness, runID string) (Harness, error) {
	h.RunID = runID
	contentID := HarnessContentID(h.Instructions)
	versionID := HarnessVersionID(runID, h.ID, h.Instructions)
	if h.ContentID != "" && h.ContentID != contentID {
		return h, fmt.Errorf("harness %s content_id 与指令全文不符", h.ID)
	}
	if h.VersionID != "" && h.VersionID != versionID {
		return h, fmt.Errorf("harness %s version_id 与 run/candidate/指令不符", h.ID)
	}
	h.ContentID, h.VersionID = contentID, versionID
	return h, nil
}

// VersionObservation 一次可比较的运行效果观测。
type VersionObservation struct {
	Phase         string   `json:"phase"` // floor | train_rollout | eval
	Epoch         int      `json:"epoch"`
	PassRate      float64  `json:"pass_rate"`
	PassFrac      float64  `json:"pass_frac"`
	CostTokens    int64    `json:"cost_tokens"`
	N             int      `json:"n"`
	Frontier      bool     `json:"frontier,omitempty"`
	Filtered      bool     `json:"filtered,omitempty"`
	Fallback      bool     `json:"fallback,omitempty"`
	SurvivorTiers []string `json:"survivor_tiers,omitempty"`
	RecordedAt    int64    `json:"recorded_at"`
}

// HarnessVersion 一个不可变指令版本及其跨阶段运行观测。
type HarnessVersion struct {
	VersionID       string               `json:"version_id"`
	ContentID       string               `json:"content_id"`
	CandidateID     string               `json:"candidate_id"`
	ParentVersionID string               `json:"parent_version_id,omitempty"`
	RunID           string               `json:"run_id"`
	Epoch           int                  `json:"epoch"`
	Origin          string               `json:"origin"`
	Model           string               `json:"model"`
	TaskSet         TaskSetRef           `json:"task_set"`
	CreatedAt       int64                `json:"created_at"`
	Instructions    string               `json:"instructions"`
	Observations    []VersionObservation `json:"observations,omitempty"`
}

// VersionRegistry 以目录为持久化边界; 无 Dir 的内存测试不启用。
type VersionRegistry struct {
	Dir string
	mu  sync.Mutex
}

func NewVersionRegistry(dir string) (*VersionRegistry, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 harness 版本仓库失败: %w", err)
	}
	return &VersionRegistry{Dir: dir}, nil
}

func (r *VersionRegistry) path(versionID string) (string, error) {
	if r == nil || r.Dir == "" {
		return "", fmt.Errorf("harness 版本仓库未配置")
	}
	if !strings.HasPrefix(versionID, "hv-") || len(versionID) != 35 {
		return "", fmt.Errorf("非法 harness version_id: %q", versionID)
	}
	for _, c := range strings.TrimPrefix(versionID, "hv-") {
		if c < '0' || c > '9' && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("非法 harness version_id: %q", versionID)
		}
	}
	return filepath.Join(r.Dir, versionID+".json"), nil
}

// writeAtomic 同步落盘后 rename, 读者不会看到半份 JSON。
func (r *VersionRegistry) writeAtomic(path string, v *HarnessVersion) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(r.Dir, ".version-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Register 在 h0/候选产生时立即登记。重入resume保留已积累观测, 但不可变字段必须一致。
func (r *VersionRegistry) Register(h Harness, model string, taskSet TaskSetRef, parentVersionID string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	path, err := r.path(h.VersionID)
	if err != nil {
		return err
	}
	if old, err := r.Get(h.VersionID); err == nil {
		if old.ContentID != h.ContentID || old.RunID != h.RunID || old.CandidateID != h.ID ||
			old.Instructions != h.Instructions || old.ParentVersionID != parentVersionID ||
			old.Epoch != h.Epoch || old.Origin != h.Origin || old.Model != model || old.TaskSet != taskSet {
			return fmt.Errorf("harness 版本 %s 已存在且不可变字段不同", h.VersionID)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	v := &HarnessVersion{
		VersionID: h.VersionID, ContentID: h.ContentID, CandidateID: h.ID,
		ParentVersionID: parentVersionID, RunID: h.RunID, Epoch: h.Epoch,
		Origin: h.Origin, Model: model, TaskSet: taskSet,
		CreatedAt: time.Now().Unix(), Instructions: h.Instructions,
	}
	return r.writeAtomic(path, v)
}

// Observe 追加一次运行观测; 版本未登记是装配错误, 不静默造匿名版本。
func (r *VersionRegistry) Observe(versionID string, obs VersionObservation) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.Get(versionID)
	if err != nil {
		return err
	}
	obs.RecordedAt = time.Now().Unix()
	// 同一版本在同一阶段/轮次只有一条聚合观测; 节点重试/resume 覆盖旧值,
	// 而不是把同一次评估重复计成多次运行。
	replaced := false
	for i := range v.Observations {
		if v.Observations[i].Phase == obs.Phase && v.Observations[i].Epoch == obs.Epoch {
			v.Observations[i] = obs
			replaced = true
			break
		}
	}
	if !replaced {
		v.Observations = append(v.Observations, obs)
	}
	path, err := r.path(versionID)
	if err != nil {
		return err
	}
	return r.writeAtomic(path, v)
}

// ObserveEval 记录地板或验证集效果及选点结果。
func (r *VersionRegistry) ObserveEval(runID string, epoch int, phase string, ev CandidateEval, frontier, filtered bool, tiers []string, fallback ...bool) error {
	if r == nil {
		return nil
	}
	return r.Observe(ev.VersionID, VersionObservation{
		Phase: phase, Epoch: epoch, PassRate: ev.PassRate, PassFrac: ev.PassFrac,
		CostTokens: ev.CostTokens, N: ev.NEval, Frontier: frontier, Filtered: filtered,
		Fallback: len(fallback) > 0 && fallback[0], SurvivorTiers: tiers,
	})
}

// ObserveRollout 记录训练集多次 rollout 的聚合效果。
func (r *VersionRegistry) ObserveRollout(h Harness, epoch int, m CandidateMetrics, passFrac float64) error {
	if r == nil {
		return nil
	}
	return r.Observe(h.VersionID, VersionObservation{
		Phase: "train_rollout", Epoch: epoch, PassRate: m.PassRate, PassFrac: passFrac,
		CostTokens: m.CostTokens, N: m.NRollouts,
	})
}

// Get 读取一个版本。
func (r *VersionRegistry) Get(versionID string) (*HarnessVersion, error) {
	path, err := r.path(versionID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("harness 版本 %s 不存在或不可读: %w", versionID, err)
	}
	var v HarnessVersion
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("harness 版本 %s 文件损坏: %w", versionID, err)
	}
	return &v, nil
}

// List 读取版本仓库。runID/contentID/ids 为空表示不过滤。
func (r *VersionRegistry) List(runID, contentID string, ids map[string]bool) ([]HarnessVersion, error) {
	out := []HarnessVersion{}
	if r == nil {
		return out, nil
	}
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "hv-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if len(ids) > 0 && !ids[id] {
			continue
		}
		v, err := r.Get(id)
		if err != nil {
			return nil, err
		}
		if runID != "" && v.RunID != runID || contentID != "" && v.ContentID != contentID {
			continue
		}
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].VersionID > out[j].VersionID
	})
	return out, nil
}
