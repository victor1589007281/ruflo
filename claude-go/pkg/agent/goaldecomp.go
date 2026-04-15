// goaldecomp.go — 层级目标分解系统 (Hierarchical Goal Decomposition, HGD)。
//
// 解决超长上下文下的复杂系统开发问题:
//   - 目标层级化: 将大目标拆解为可管理的子目标 (O(log n) 深度)
//   - 持久化状态: 跨 session/multi-day 保持目标树 + 进度
//   - 增量上下文: 每次只加载当前子目标及其祖先链的摘要 (O(depth) 而非 O(n))
//   - 自动检查点: 每个子目标完成后快照, 支持断点续作
//
// 参考:
//   - HTN Planning (Hierarchical Task Network, ICAPS 2023)
//   - LATS (Language Agent Tree Search, Zhou et al. 2024)
//   - SWE-agent 的 RepoMap (上下文窗口管理, Princeton 2024)
//   - Devin AI 的 multi-step planning (Cognition 2024)
//   - Chain-of-Thought 的层级变体 (Tree-of-Thoughts, Yao et al. 2023)
//   - 人类项目管理: WBS (Work Breakdown Structure, PMI/PMBOK)
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// GoalStatus 目标状态
type GoalStatus string

const (
	GoalPending    GoalStatus = "pending"
	GoalActive     GoalStatus = "active"
	GoalCompleted  GoalStatus = "completed"
	GoalFailed     GoalStatus = "failed"
	GoalBlocked    GoalStatus = "blocked"
	GoalDecomposed GoalStatus = "decomposed" // 已分解为子目标
)

// GoalNode 目标树中的一个节点。
// 参考 HTN (Hierarchical Task Network): 每个目标可分解为子任务或直接执行。
type GoalNode struct {
	ID          string     `json:"id"`
	ParentID    string     `json:"parentId,omitempty"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Status      GoalStatus `json:"status"`
	Owner       string     `json:"owner,omitempty"`
	Priority    int        `json:"priority"`

	// 上下文摘要: 完成后由 LLM 生成, 用于向子目标/后续目标传递关键信息
	// 参考: SWE-agent RepoMap — 只传递必要的上下文而非全量历史
	Summary string `json:"summary,omitempty"`

	// 验收标准 (从架构师的设计文档中提取)
	AcceptCriteria []string `json:"acceptCriteria,omitempty"`

	// 关联 V2 Task ID (与 TaskStore DAG 联动)
	TaskIDs []string `json:"taskIds,omitempty"`

	Children  []string  `json:"children,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// 检查点: 完成到哪一步了 (断点续作用)
	Checkpoint string `json:"checkpoint,omitempty"`
}

// GoalTree 层级目标树, 持久化到磁盘。
// 参考: Tree-of-Thoughts (Yao et al. 2023) — 结构化搜索空间
type GoalTree struct {
	RootID string               `json:"rootId"`
	Nodes  map[string]*GoalNode `json:"nodes"`

	mu      sync.Mutex
	dataDir string
}

// NewGoalTree 创建或加载目标树。
func NewGoalTree(dataDir string) *GoalTree {
	gt := &GoalTree{
		Nodes:   make(map[string]*GoalNode),
		dataDir: dataDir,
	}
	gt.load()
	return gt
}

// AddRoot 创建根目标 (项目级目标)。
func (gt *GoalTree) AddRoot(title, description string) *GoalNode {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	id := fmt.Sprintf("goal-%d", time.Now().UnixNano()%1000000)
	node := &GoalNode{
		ID:          id,
		Title:       title,
		Description: description,
		Status:      GoalActive,
		Priority:    2,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	gt.Nodes[id] = node
	gt.RootID = id
	gt.save()
	return node
}

// Decompose 将一个目标分解为子目标 (HTN decomposition)。
// subGoals: [{title, description, owner, acceptCriteria}]
func (gt *GoalTree) Decompose(parentID string, subGoals []SubGoalDef) []*GoalNode {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	parent, ok := gt.Nodes[parentID]
	if !ok {
		return nil
	}

	var created []*GoalNode
	for i, sg := range subGoals {
		id := fmt.Sprintf("%s-%d", parentID, i+1)
		node := &GoalNode{
			ID:             id,
			ParentID:       parentID,
			Title:          sg.Title,
			Description:    sg.Description,
			Owner:          sg.Owner,
			AcceptCriteria: sg.AcceptCriteria,
			Status:         GoalPending,
			Priority:       sg.Priority,
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		}
		gt.Nodes[id] = node
		parent.Children = append(parent.Children, id)
		created = append(created, node)
	}
	parent.Status = GoalDecomposed
	parent.UpdatedAt = time.Now()
	gt.save()
	return created
}

// SubGoalDef 子目标定义
type SubGoalDef struct {
	Title          string
	Description    string
	Owner          string
	AcceptCriteria []string
	Priority       int
}

// Complete 标记目标完成, 并记录摘要。
func (gt *GoalTree) Complete(goalID, summary string) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	if node, ok := gt.Nodes[goalID]; ok {
		node.Status = GoalCompleted
		node.Summary = summary
		node.UpdatedAt = time.Now()

		// 自动检查: 如果所有兄弟目标都完成, 父目标也标记为完成
		if node.ParentID != "" {
			gt.checkParentCompletion(node.ParentID)
		}
		gt.save()
	}
}

func (gt *GoalTree) checkParentCompletion(parentID string) {
	parent, ok := gt.Nodes[parentID]
	if !ok || parent.Status == GoalCompleted {
		return
	}
	allDone := true
	for _, childID := range parent.Children {
		if child, ok := gt.Nodes[childID]; ok {
			if child.Status != GoalCompleted {
				allDone = false
				break
			}
		}
	}
	if allDone && len(parent.Children) > 0 {
		parent.Status = GoalCompleted
		parent.UpdatedAt = time.Now()
		if parent.ParentID != "" {
			gt.checkParentCompletion(parent.ParentID)
		}
	}
}

// ContextForGoal 为指定目标构建精简上下文 (祖先链摘要 + 兄弟完成状态)。
// 参考: SWE-agent RepoMap — 只传递当前任务相关的上下文, 而非全量代码库。
// 复杂度: O(depth) 而非 O(total_nodes)
func (gt *GoalTree) ContextForGoal(goalID string) string {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	node, ok := gt.Nodes[goalID]
	if !ok {
		return ""
	}

	// 收集祖先链
	var ancestors []*GoalNode
	current := node
	for current.ParentID != "" {
		if parent, ok := gt.Nodes[current.ParentID]; ok {
			ancestors = append([]*GoalNode{parent}, ancestors...)
			current = parent
		} else {
			break
		}
	}

	var ctx string
	ctx += "## 项目目标层级\n\n"
	for i, anc := range ancestors {
		indent := ""
		for j := 0; j < i; j++ {
			indent += "  "
		}
		ctx += fmt.Sprintf("%s- [%s] **%s**: %s\n", indent, anc.Status, anc.Title, anc.Summary)
	}

	// 当前目标的兄弟状态 (知道哪些已完成)
	if node.ParentID != "" {
		if parent, ok := gt.Nodes[node.ParentID]; ok {
			ctx += "\n### 同级目标进度\n"
			for _, sibID := range parent.Children {
				if sib, ok := gt.Nodes[sibID]; ok {
					mark := "⬜"
					switch sib.Status {
					case GoalCompleted:
						mark = "✅"
					case GoalActive:
						mark = "🔵"
					case GoalFailed:
						mark = "❌"
					}
					ctx += fmt.Sprintf("- %s %s (owner: %s)\n", mark, sib.Title, sib.Owner)
					if sib.ID != goalID && sib.Summary != "" {
						summary := sib.Summary
						if len(summary) > 500 {
							summary = summary[:500] + "..."
						}
						ctx += fmt.Sprintf("  摘要: %s\n", summary)
					}
				}
			}
		}
	}

	// 当前目标的详细描述 + 验收标准
	ctx += fmt.Sprintf("\n### 当前目标: %s\n%s\n", node.Title, node.Description)
	if len(node.AcceptCriteria) > 0 {
		ctx += "\n**验收标准:**\n"
		for _, ac := range node.AcceptCriteria {
			ctx += fmt.Sprintf("- [ ] %s\n", ac)
		}
	}

	return ctx
}

// NextGoals 返回下一批可执行的目标 (叶子节点中 status=pending 的)。
func (gt *GoalTree) NextGoals() []*GoalNode {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	var ready []*GoalNode
	for _, node := range gt.Nodes {
		if node.Status == GoalPending && len(node.Children) == 0 {
			ready = append(ready, node)
		}
	}
	// 按优先级排序
	for i := 1; i < len(ready); i++ {
		for j := i; j > 0 && ready[j].Priority > ready[j-1].Priority; j-- {
			ready[j], ready[j-1] = ready[j-1], ready[j]
		}
	}
	return ready
}

// Progress 返回 (completed, total) 的完成进度。
func (gt *GoalTree) Progress() (int, int) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	completed, total := 0, 0
	for _, node := range gt.Nodes {
		if len(node.Children) == 0 { // 只统计叶子节点
			total++
			if node.Status == GoalCompleted {
				completed++
			}
		}
	}
	return completed, total
}

// SaveCheckpoint 保存子目标的中间检查点 (断点续作)。
func (gt *GoalTree) SaveCheckpoint(goalID, checkpoint string) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	if node, ok := gt.Nodes[goalID]; ok {
		node.Checkpoint = checkpoint
		node.UpdatedAt = time.Now()
		gt.save()
	}
}

func (gt *GoalTree) save() {
	if gt.dataDir == "" {
		return
	}
	os.MkdirAll(gt.dataDir, 0755)
	data, _ := json.MarshalIndent(gt, "", "  ")
	_ = os.WriteFile(filepath.Join(gt.dataDir, "goals.json"), data, 0644)
}

func (gt *GoalTree) load() {
	if gt.dataDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(gt.dataDir, "goals.json"))
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, gt)
	if gt.Nodes == nil {
		gt.Nodes = make(map[string]*GoalNode)
	}
}
