// plugin_materialize.go: 插件成员的**安装物化** —— 把 bundle 目录里的成员资产
// 落到运行时真正读的位置，让"POST /api/plugins"从「存了个包」变成「能力已上线」。
//
// ---------------------------------------------------------------------------
// 为什么需要这一层 (BindingNoConsumer 的服务端补全)
// ---------------------------------------------------------------------------
//
// pkg/pluginrun 的 Expand 只"找出来并解析"，消费方此前只有 CLI 路径
// (cmd/claude-go 的 run --plugin)。:18080 服务端执行团队时从不展开插件，
// 于是 bundle 里的 agents/workflows/skills 写进去了也没人读 ——
// "装了但没生效"与"没装"从外面完全不可区分。
//
// 这里选的接线点是**写时物化**而不是**运行时展开**，理由：
//
//   - 团队阶段的 agent/prompt/skills 解析 (buildStagePromptWithRoles /
//     stageConstraints / stageSkillListing) 全都只认既有注册表
//     (roleRegistry / 资产目录 / 技能注册表)，零改动复用；
//   - 运行期展开要在每次 stage 执行的热路径上做目录合并，而出错的代价
//     是"有时候生效有时候不生效"；写时物化一次装好之后，运行时读的就是
//     普通资产，失败形态只剩"装的时候报错"，可观测、可重试；
//   - DELETE 插件 = 卸载，与安装对称。
//
// 物化目标 = 各类资产的 state 标准落点 (与 asset.standardLayout 逐字节一致)：
//
//	agents/<n>.md        → <stateDir>/agents/<n>.md        (资产目录重扫即入编)
//	skills/<n>/SKILL.md  → <stateDir>/skills/<n>/SKILL.md   (ReloadSkills)
//	workflows/<n>.json   → <stateDir>/workflows/<n>.json   (RegisterWorkflow 热注册)
//
// hooks/tools 成员本版不物化: hook 声明的消费通道在 config，落一个文件不会
// 被读 (与 pluginrun 包注释同一纪律: 没有消费方的形态必须说出来，不静默)。
//
// ---------------------------------------------------------------------------
// 所有权: 同名资产谁说了算
// ---------------------------------------------------------------------------
//
// 物化绝不覆盖"不是我们装"的同名文件。判定靠 sha256 清单：
//
//	<stateDir>/.dashboard/plugin-materialized/<插件>.json
//
//	- 目标不存在            → 拷贝并登记
//	- 目标是我们装的且未改   → 刷新 (插件升级路径)
//	- 目标是我们装的但被改   → 让位 (用户已接管), 记 skipped
//	- 目标存在但非我们装     → 让位 (别的来源), 记 skipped
//
// DELETE 插件时按同一份清单回收：仍等于我们写的哈希才删；被改过的留在盘上
// (那是用户的东西了)，报告里说明。
package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/pluginrun"
)

// materializedFile 清单里的一条安装记录。
type materializedFile struct {
	Path   string `json:"path"`   // 物化目标的绝对路径
	SHA256 string `json:"sha256"` // 我们写入时的内容哈希
}

// materializeReport 安装/卸载的人话结论。两个切片都必须非 nil (契约同 pluginDTO.Problems)。
type materializeReport struct {
	Installed []string `json:"installed"`
	Skipped   []string `json:"skipped"` // 每条带原因, 形如 "agents/x.md: 用户已接管"
}

func (s *Server) materializedIndexDir() string {
	return filepath.Join(s.cfg.StateDir, ".dashboard", "plugin-materialized")
}

func (s *Server) manifestPath(plugin string) string {
	return filepath.Join(s.materializedIndexDir(), plugin+".json")
}

func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func loadManifest(path string) (map[string]materializedFile, error) {
	out := map[string]materializedFile{}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		// 清单坏不阻断安装: 按"没有旧记录"处理，但本次写入会重建清单 ——
		// 上一份记录里"我们装过"的文件从此失去回收凭据，这是坏清单的合理代价。
		return map[string]materializedFile{}, nil
	}
	return out, nil
}

// memberTargets 展开结果 → 待物化清单。key = bundle 内相对路径 (清单主键)，
// 值为标准 state 落点。hooks/tools 不在表里: 本版无消费方，见包注释。
func (s *Server) memberTargets(b *pluginrun.Bundle) []struct {
	key  string // 稳定主键: <kind>/<文件名>
	src  string
	dst  string
	kind string
} {
	type T = struct {
		key  string
		src  string
		dst  string
		kind string
	}
	out := []T{}
	// SourcePath 是 bundle 目录里的实际文件。
	for _, a := range b.Agents {
		if a.SourcePath == "" {
			continue
		}
		base := filepath.Base(a.SourcePath)
		out = append(out, T{key: "agents/" + base, src: a.SourcePath, dst: filepath.Join(s.cfg.StateDir, "agents", base), kind: "agent"})
	}
	for _, a := range b.Workflows {
		if a.SourcePath == "" {
			continue
		}
		base := filepath.Base(a.SourcePath)
		// 运行时 workflow 注册表只认 JSON 定义; workflows/*.md 是声明式成员
		// (steps: 等元数据, 由所属应用消费), 拷进 workflows/ 只会让
		// hotRegisterWorkflow 在 JSON 解析上失败。跳过并报告, 不装假文件。
		if !strings.EqualFold(filepath.Ext(base), ".json") {
			continue
		}
		out = append(out, T{key: "workflows/" + base, src: a.SourcePath, dst: filepath.Join(s.cfg.StateDir, "workflows", base), kind: "workflow"})
	}
	for _, a := range b.Skills {
		if a.SourcePath == "" {
			continue
		}
		// skills 的成员资产 SourcePath 指向 SKILL.md; 标准落点 skills/<名>/SKILL.md
		skillName := a.Name
		out = append(out, T{key: "skills/" + skillName + "/SKILL.md", src: a.SourcePath, dst: filepath.Join(s.cfg.StateDir, "skills", skillName, "SKILL.md"), kind: "skill"})
	}
	return out
}

// materializePlugin 把 bundle 成员装进标准 state 根。幂等，可重入 (re-POST 插件 = 升级)。
func (s *Server) materializePlugin(b *pluginrun.Bundle) (materializeReport, error) {
	rep := materializeReport{Installed: []string{}, Skipped: []string{}}
	if b == nil {
		return rep, fmt.Errorf("插件为空，没有可物化成员")
	}
	if b.ExternalRuntime() {
		// 创建路径已提前分支; 这里是第二道闸: 任何未来调用方 (重装/自愈)
		// 都不能绕过 runtime 声明把成员装进全局目录。
		rep.Skipped = append(rep.Skipped, "runtime: external —— 成员由所属应用消费，claude-go 不物化")
		return rep, nil
	}
	if b.Dir == "" {
		return rep, fmt.Errorf("插件 %q 不是目录形式，没有可物化成员", b.Name)
	}
	manifest, err := loadManifest(s.manifestPath(b.Name))
	if err != nil {
		return rep, err
	}

	targets := s.memberTargets(b)
	for _, a := range b.Workflows {
		if a != nil && a.SourcePath != "" && !strings.EqualFold(filepath.Ext(a.SourcePath), ".json") {
			rep.Skipped = append(rep.Skipped, "workflows/"+filepath.Base(a.SourcePath)+
				": 声明式成员 (.md)，本版本只物化运行时 JSON 定义，未安装")
		}
	}
	reloadAgents, reloadSkills := false, false
	for _, t := range targets {
		cur, err := fileSHA256(t.dst)
		owned, wasInstalled := manifest[t.key]
		targetExists := err == nil

		switch {
		case !targetExists:
			// 无人认领: 直接装
		case wasInstalled && owned.SHA256 == cur:
			// 我们装的、没人动: 刷新路径 (插件升级/重启 re-ensure)
		case wasInstalled:
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: 目标已被改动 (用户接管)，插件版本不再覆盖", t.key))
			manifest[t.key] = materializedFile{Path: t.dst, SHA256: cur} // 转交所有权记录，卸载时也不删
			continue
		default:
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: 同名资产已存在且非本插件所装，让位", t.key))
			continue
		}

		data, err := os.ReadFile(t.src)
		if err != nil {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: 读取成员失败: %v", t.key, err))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(t.dst), 0o755); err != nil {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: 建目录失败: %v", t.key, err))
			continue
		}
		if err := os.WriteFile(t.dst, data, 0o644); err != nil {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: 写入失败: %v", t.key, err))
			continue
		}
		sum, _ := fileSHA256(t.dst)
		manifest[t.key] = materializedFile{Path: t.dst, SHA256: sum}
		rep.Installed = append(rep.Installed, t.key)
		switch t.kind {
		case "agent":
			reloadAgents = true
		case "skill":
			reloadSkills = true
		}

		if t.kind == "workflow" {
			if err := s.hotRegisterWorkflow(t.dst, wasInstalled && owned.SHA256 == cur); err != nil {
				// 文件已装但热注册失败: 重启时会从目录装载，不算丢 —— 但必须说出来
				rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: 已落盘但进程内热注册失败 (重启后生效): %v", t.key, err))
			}
		}
	}

	if err := os.MkdirAll(s.materializedIndexDir(), 0o755); err != nil {
		return rep, err
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(s.manifestPath(b.Name), mb, 0o644); err != nil {
		return rep, err
	}
	// 物化新装了 agent/skill 成员后刷新对应索引 —— create 处理器里的 ReloadAgents
	// 在物化**之前**调用 (它刷的是 bundle 落盘), 这里补的是物化目标目录的刷新。
	// workflow 成员不需要: hotRegisterWorkflow 已把定义直接写进进程内注册表。
	if reloadAgents && s.cfg.ReloadAgents != nil {
		s.cfg.ReloadAgents()
	}
	if reloadSkills && s.cfg.ReloadSkills != nil {
		s.cfg.ReloadSkills()
	}
	sort.Strings(rep.Installed)
	sort.Strings(rep.Skipped)
	return rep, nil
}

// hotRegisterWorkflow 物化的 workflow 成员即时进进程内注册表。
// update=true 表示此前已物化过 (走版本守卫的 UpdateWorkflow)，否则 RegisterWorkflow。
func (s *Server) hotRegisterWorkflow(path string, update bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var def agent.WorkflowDef
	if err := json.Unmarshal(data, &def); err != nil {
		return fmt.Errorf("workflow JSON 解析失败: %w", err)
	}
	roles := s.roleRegistry()
	if update {
		for _, w := range agent.ListWorkflows() {
			if w.Name == def.Name {
				if _, err := agent.UpdateWorkflow(&def, roles, w.Revision); err != nil {
					return err
				}
				// UpdateWorkflow 只改进程内注册表; 盘上的文件上面已经写过 (内容即 def 来源)
				return nil
			}
		}
		// 曾物化过但注册表里没了 (重启/清理): 落到注册分支
	}
	if err := agent.RegisterWorkflow(&def, roles); err != nil {
		return err
	}
	return agent.SaveWorkflowToDir(filepath.Join(s.cfg.StateDir, "workflows"), &def)
}

// dematerializePlugin 回收插件物化出去的成员。仍等于我们写入哈希的才删；
// 被用户改过的留在盘上 (转交报告说明)。workflows 同时从进程内注册表摘除。
func (s *Server) dematerializePlugin(plugin string) (materializeReport, error) {
	rep := materializeReport{Installed: []string{}, Skipped: []string{}}
	manifest, err := loadManifest(s.manifestPath(plugin))
	if err != nil {
		return rep, err
	}
	keys := make([]string, 0, len(manifest))
	for k := range manifest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rec := manifest[k]
		cur, err := fileSHA256(rec.Path)
		if err != nil || cur != rec.SHA256 {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: 内容已变更或已删除，保留现状 (归用户所有)", k))
			continue
		}
		if err := os.Remove(rec.Path); err != nil {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: 删除失败: %v", k, err))
			continue
		}
		rep.Installed = append(rep.Installed, k) // 回收也算 installed 口径: "按预期移除了"
		// 清空空目录 (skills/<n>/ 这类成员目录)
		dir := filepath.Dir(rec.Path)
		if entries, _ := os.ReadDir(dir); err == nil && len(entries) == 0 {
			_ = os.Remove(dir)
		}
		if strings.HasPrefix(k, "workflows/") {
			name := strings.TrimSuffix(strings.TrimPrefix(k, "workflows/"), ".json")
			if err := agent.UnregisterWorkflow(name); err != nil {
				rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: 进程内摘除失败 (重启后消失): %v", k, err))
			}
		}
	}
	os.Remove(s.manifestPath(plugin))
	return rep, nil
}
