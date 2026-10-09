package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/pluginrun"
)

// plugin_handlers.go: 插件包 (plugin bundle) 资产的查看 / 创建 / 删除 / 草稿生成。
//
// 与 agent_handlers.go 同形, 管的是六种资产里的 KindPlugin 这一层:
//
//	agent   <root>/agents/<名>.md                    扁平单文件
//	插件包  <root>/plugins/<名>/PLUGIN.md            目录形式: 清单 + 成员子目录
//	        <root>/plugins/<名>/agents/*.md          成员 agent
//	        <root>/plugins/<名>/skills/<x>/SKILL.md  成员技能
//	        <root>/plugins/<名>/hooks/...            成员 hook (HOOK.md)
//
// 与 agent 的关键差异只有一个: 插件是**目录形式**的资产, PLUGIN.md 是清单,
// 同目录的子目录里才是成员。所以这一层**不自己算**"这个插件带了几个 agent / 有
// 什么问题" —— 那是 pkg/pluginrun.Expand 的事: 成员归属规则只能有一份, 在展开器
// 里; 这里若再实现一遍, 就会长出"面板数出来的成员"与"运行时实际装载的成员"两份
// 会漂移的口径, 而漂移不报错。
//
// 写入口留在服务端的理由与 agent_handlers.go 包注释相同: PLUGIN.md 的 frontmatter
// 键名是"写方 webapp / 读方 claude-go"两个进程各自解释的契约, 键名只能定义在
// 读它的这一侧。

const pluginAssetsSubdir = "plugins"

// pluginAssetRoot 插件资产的 state 落点: <stateDir>/plugins。
//
// 与 asset.LoadRoots 的 state 来源**逐字节一致** (pkg/asset/load.go 的
// standardLayout[KindPlugin] = "plugins", state 来源的根就是 stateDir 本身)。
// 写成别的路径会得到一个"写进去了但没人读"的目录 —— 那种失败没有任何报错。
func (s *Server) pluginAssetRoot() string { return filepath.Join(s.cfg.StateDir, pluginAssetsSubdir) }

// validPluginName 与 validSkillName 同规则 (小写字母/数字/连字符, ≤64)。
//
// 插件名同时是 (a) 目录名 (b) 资产名 (c) URL 路径段。字符集里没有 '/' '.' 是
// DELETE 防路径穿越的第一道守卫 —— `..` 与 `a/b` 在这一步就被拒掉。
func validPluginName(s string) bool { return validSkillName(s) }

// pluginCountsDTO 成员计数。固定五键的**结构体**而不是 map: 契约要求五个键
// 永远在 (哪怕为 0), map 形态哪天少写一键, 前端读到的是 undefined 而不是 0。
type pluginCountsDTO struct {
	Agents    int `json:"agents"`
	Skills    int `json:"skills"`
	Hooks     int `json:"hooks"`
	Tools     int `json:"tools"`
	Workflows int `json:"workflows"`
}

// pluginDTO 一个插件包的对外表示 (列表与详情的公共部分)。
//
// 显式枚举字段, 不序列化 pluginrun.Bundle 整个出去 —— Bundle 里揣着成员的完整
// *asset.Asset (含正文), 列表接口全带出去会把"给我二十个插件的概览"变成
// "把这台机器上所有资产文件正文发一遍"。
type pluginDTO struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Version     string          `json:"version,omitempty"`
	Status      string          `json:"status,omitempty"`
	Source      string          `json:"source,omitempty"`     // state | project | user | builtin
	SourcePath  string          `json:"sourcePath,omitempty"` // PLUGIN.md (或平铺 .md) 的实际路径
	DirForm     bool            `json:"dirForm"`              // true = 目录形式包; false = 平铺单文件
	Counts      pluginCountsDTO `json:"counts"`
	Problems    []string        `json:"problems"` // 展开问题; 空也必须是 [] 不是 null
	Editable    bool            `json:"editable"` // 是否本机 state 根下的那一份 (前端据此显示删除按钮)
}

// pluginDetailDTO GET /api/plugins/:name 的插件对象: pluginDTO + 正文 + 成员清单。
// 嵌入 pluginDTO 让字段**平铺**在 plugin 对象上 (契约形状), 而不是多一层嵌套。
type pluginDetailDTO struct {
	pluginDTO
	Body    string           `json:"body"`
	Members pluginMembersDTO `json:"members"`
}

// pluginMembersDTO 成员清单。每类必须是 [] 不是 null (同 pluginDTO.Problems)。
type pluginMembersDTO struct {
	Agents    []pluginMemberDTO `json:"agents"`
	Skills    []pluginMemberDTO `json:"skills"`
	Hooks     []pluginMemberDTO `json:"hooks"`
	Tools     []pluginMemberDTO `json:"tools"`
	Workflows []pluginMemberDTO `json:"workflows"`
}

// pluginMemberDTO 成员的摘要条目。成员详情 (正文/frontmatter) 不在这里出 —
// 它们各自有 /api/agents /api/skills 的详情入口, 这里只回答"这个包里有谁"。
type pluginMemberDTO struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// pluginDTOFromBundle 把展开结果投影成对外表示。
func (s *Server) pluginDTOFromBundle(b *pluginrun.Bundle) pluginDTO {
	d := pluginDTO{
		Name:     b.Name,
		Problems: append([]string{}, b.Problems...),
	}
	if b.Plugin != nil {
		d.Description = b.Plugin.Description
		d.Version = b.Plugin.Version
		d.Status = b.Plugin.Status
		d.Source = b.Plugin.Source
		d.SourcePath = b.Plugin.SourcePath
		// dirForm 以展开器的 Bundle.Dir 为准: 目录形式的判定规则只有一份, 在
		// pluginrun 里 (bundleDirOf), 这里不再从 SourcePath 推导第二份。
		d.DirForm = b.Dir != ""
		// editable 与 agent 列表的 sourcePath 判定同一形态: 用前缀匹配而不是重新推导
		// "它是不是 state 来源" —— state 来源的根就是 pluginAssetRoot(), 两者等价,
		// 而前缀匹配在三来源优先级调整之后不会静默错判"哪一份能删"。
		d.Editable = strings.HasPrefix(b.Plugin.SourcePath, s.pluginAssetRoot()+string(filepath.Separator))
	}
	c := b.Counts()
	d.Counts = pluginCountsDTO{
		Agents: c["agents"], Skills: c["skills"], Hooks: c["hooks"],
		Tools: c["tools"], Workflows: c["workflows"],
	}
	return d
}

// pluginIsDirForm 这份插件资产是不是目录形式的包 —— **只在 Expand 失败的兜底
// 分支用** (那时拿不到 Bundle.Dir); 正常路径一律以展开器的判定 (b.Dir != "") 为准。
// 两套判据在所有能被 LoadDir 装进索引的路径上一致 (bundleDirOf 认"父目录≠plugins
// 的 PLUGIN.md/README.md", 而 LoadDir 只从 plugins 根下一层装载, 两者等价)。
//
// 判据就是 LoadDir 认的目录布局本身: <某 root>/plugins/<名>/PLUGIN.md|README.md。
// 不拿"父目录名 == 插件名"当判据 —— FM 里 `name:` 可以覆盖目录名, 那种文件
// 父目录名与资产名不等, 但它仍是目录形式的包。kind 目录固定叫 "plugins"
// (standardLayout 那张表), 所以"祖父目录是 plugins + 文件名是插件文档名"是精确判据,
// 平铺在 plugins/ 下的 <名>.md 与 plugins/PLUGIN.md 都判 false。
func pluginIsDirForm(sourcePath string) bool {
	if sourcePath == "" {
		return false
	}
	switch strings.ToUpper(filepath.Base(sourcePath)) {
	case "PLUGIN.MD", "README.MD":
	default:
		return false
	}
	return filepath.Base(filepath.Dir(filepath.Dir(sourcePath))) == pluginAssetsSubdir
}

// handlePlugins GET /api/plugins (列表) | POST /api/plugins (创建/覆盖)
func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handlePluginCreate(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"plugins": s.pluginAssetList()})
}

// pluginAssetList 返回插件包列表。
//
// ★ 取哪一份索引与 agentAssetList 完全同策略 (进程内运行时单例优先, nil 则现扫),
//
//	且就是**同一份**索引 —— agentCatalog() 返回的是六种资产全量的 *asset.Set,
//	不是 agent 专属的 (方法名是历史遗留)。独立 dashboard (:7777) 单例恒为 nil,
//	此时退化成现扫, 否则会出现"创建 200、文件在、紧接着列表是空的" (实测撞到过,
//	见 agentAssetList 的注释)。
func (s *Server) pluginAssetList() []pluginDTO {
	cat := s.agentCatalog()
	names := cat.Names(asset.KindPlugin)
	out := make([]pluginDTO, 0, len(names))
	for _, n := range names {
		b, err := pluginrun.Expand(cat, n)
		if err != nil {
			// 一个插件展开失败**不该让整个列表 500** —— 与 asset.Set 把装载问题做成
			// 收集品同一哲学: 坏的这一个以"带 problem 的条目"出现, 其余照常。
			d := pluginDTO{Name: n, Problems: []string{err.Error()}}
			if a := cat.Get(asset.KindPlugin, n); a != nil {
				d.Description = a.Description
				d.Version = a.Version
				d.Status = a.Status
				d.Source = a.Source
				d.SourcePath = a.SourcePath
				d.DirForm = pluginIsDirForm(a.SourcePath)
				d.Editable = strings.HasPrefix(a.SourcePath, s.pluginAssetRoot()+string(filepath.Separator))
			}
			out = append(out, d)
			continue
		}
		out = append(out, s.pluginDTOFromBundle(b))
	}
	return out
}

// pluginCreateInput 新建/覆盖一个插件包。Scaffold 可空: 带骨架时成员文件随清单
// 一并落盘 (AI 生成 → 审核 → 保存的闭环), 不带则只写清单 PLUGIN.md。
type pluginCreateInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Body        string          `json:"body"`
	Version     string          `json:"version,omitempty"`
	Scaffold    *pluginScaffold `json:"scaffold,omitempty"`
}

// handlePluginCreate POST /api/plugins → 写 <stateDir>/plugins/<名>/PLUGIN.md。
//
// 目录形式, 不是扁平文件: 插件包的价值就在"清单 + 成员子目录"这个形状上,
// 落成扁平 <名>.md 的话, 之后往里加成员时还得先做一次"平铺 → 目录"的迁移。
func (s *Server) handlePluginCreate(w http.ResponseWriter, r *http.Request) {
	var in pluginCreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !validPluginName(in.Name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name 必须为小写字母/数字/连字符 (它同时是目录名与资产名)"))
		return
	}
	if strings.TrimSpace(in.Description) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("description 不能为空 (它进的是插件清单, 用户据此决定要不要启用它)"))
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("body 不能为空 (它就是 PLUGIN.md 的正文)"))
		return
	}

	md := buildPluginMarkdown(in)
	dir := filepath.Join(s.pluginAssetRoot(), in.Name)
	dst := filepath.Join(dir, "PLUGIN.md")
	// ★ 先验后写 (与 handleAgentCreate 同一纪律), 而且验的是**全部**内容:
	//   清单与 scaffold 成员任何一个解析不过, 都 400 且盘上一片不留 ——
	//   "插件建好了但成员丢了一半"从外面看与"骨架根本没生成"不可区分。
	if _, err := asset.ParseContent(md, dst, "state", asset.KindPlugin); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("生成的资产无法解析 (这是个缺陷, 请上报): %w", err))
		return
	}
	members, err := buildScaffoldFiles(dir, in.Scaffold)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.WriteFile(dst, []byte(md), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, m := range members {
		if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err := os.WriteFile(m.path, []byte(m.content), 0o644); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	// ReloadAgents 刷的是**全量资产索引** (装配处接到的是 Bot.DashboardReloadAssets:
	// LoadStandard 全六种重扫 + 两处持有者同换), 插件与 agent 共用这一个钩子,
	// 不需要也不应该再有第二个"只刷 plugins 目录"的回调。
	if s.cfg.ReloadAgents != nil {
		s.cfg.ReloadAgents()
	}
	// 回读不回显入参 (与 agent 同一纪律), 而且读的是**展开器口径**: 清单从盘上重解析,
	// 成员计数走 pluginrun.Expand —— 用户看到的数字就是运行时实际会装载的数字,
	// 两份口径在写入口就合上, 不靠"它们应该一致"。
	back, err := asset.ParseFile(dst, "state", asset.KindPlugin)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("写入成功但回读失败: %w", err))
		return
	}
	fresh := asset.NewSet()
	asset.LoadDir(s.pluginAssetRoot(), "state", asset.KindPlugin, fresh)
	b, err := pluginrun.Expand(fresh, in.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("写入成功但展开失败: %w", err))
		return
	}
	d := s.pluginDTOFromBundle(b)
	d.Description = back.Description
	d.Version = back.Version
	writeJSON(w, http.StatusOK, map[string]interface{}{"plugin": d})
}

// scaffoldFile 一个待落盘的成员文件 (先验后写: 全部构建并解析通过前, 一个都不写)。
type scaffoldFile struct {
	path    string
	content string
}

// buildScaffoldFiles 把成员骨架变成盘上文件清单, 全部合法才返回 files。
//
// 布局与运行时的读取口径一一对应, 错一格运行时就看不见:
//   - agents → agents/<名>.md            (LoadDir 扁平形式)
//   - skills → skills/<名>/SKILL.md      (skills.Registry 只认目录形式)
//   - hooks  → hooks/<名>.md             (声明行进 frontmatter 的 hooks: 列表)
//
// 任一成员名字非法 / 重名 / 解析失败 / hook 声明行拼错 → error 整体失败,
// 不挑"能写一半"的中间态: 保存是用户在 AI 草稿上签的字, 落一半等于改了签的内容。
func buildScaffoldFiles(dir string, sc *pluginScaffold) ([]scaffoldFile, error) {
	if sc == nil {
		return nil, nil
	}
	var files []scaffoldFile
	seen := map[string]bool{}
	checkName := func(kind, n string) error {
		n = strings.TrimSpace(n)
		if !validPluginName(n) {
			return fmt.Errorf("scaffold.%s 成员名 %q 非法 (小写字母/数字/连字符)", kind, n)
		}
		if seen[kind+":"+n] {
			return fmt.Errorf("scaffold.%s 成员 %q 重名", kind, n)
		}
		seen[kind+":"+n] = true
		return nil
	}
	fm := func(name, desc string) string {
		return "---\nname: " + name + "\ndescription: " + oneLine(desc) + "\n---\n\n"
	}
	for _, a := range sc.Agents {
		n := strings.TrimSpace(a.Name)
		if err := checkName("agents", n); err != nil {
			return nil, err
		}
		p := filepath.Join(dir, "agents", n+".md")
		c := fm(n, a.Description) + strings.TrimSpace(a.Body) + "\n"
		if _, err := asset.ParseContent(c, p, "state", asset.KindAgent); err != nil {
			return nil, fmt.Errorf("scaffold.agents/%s 解析失败: %w", n, err)
		}
		files = append(files, scaffoldFile{p, c})
	}
	for _, sk := range sc.Skills {
		n := strings.TrimSpace(sk.Name)
		if err := checkName("skills", n); err != nil {
			return nil, err
		}
		p := filepath.Join(dir, "skills", n, "SKILL.md")
		c := fm(n, sk.Description) + strings.TrimSpace(sk.Body) + "\n"
		if _, err := asset.ParseContent(c, p, "state", asset.KindSkill); err != nil {
			return nil, fmt.Errorf("scaffold.skills/%s 解析失败: %w", n, err)
		}
		files = append(files, scaffoldFile{p, c})
	}
	for _, h := range sc.Hooks {
		n := strings.TrimSpace(h.Name)
		if err := checkName("hooks", n); err != nil {
			return nil, err
		}
		var b strings.Builder
		b.WriteString("---\nname: " + n + "\ndescription: " + oneLine(h.Description) + "\n")
		if len(h.Decls) > 0 {
			b.WriteString("hooks:\n")
			for _, d := range h.Decls {
				d = strings.TrimSpace(d)
				if d == "" || strings.ContainsAny(d, "\n\r") {
					return nil, fmt.Errorf("scaffold.hooks/%s 的声明行 %q 非法", n, d)
				}
				b.WriteString("  - " + d + "\n")
			}
		}
		b.WriteString("---\n\n" + strings.TrimSpace(h.Body) + "\n")
		p := filepath.Join(dir, "hooks", n+".md")
		c := b.String()
		parsed, err := asset.ParseContent(c, p, "state", asset.KindHook)
		if err != nil {
			return nil, fmt.Errorf("scaffold.hooks/%s 解析失败: %w", n, err)
		}
		// 声明行不仅要写进去, 还要能被 hook 解析器真parse出来 —— 两件事都验,
		// 缺一件就是"文件在但 hook 永远不触发"的静默。
		if _, probs := hooks.ParseAssetHooks(parsed); len(probs) > 0 {
			return nil, fmt.Errorf("scaffold.hooks/%s 声明解析失败: %s", n, strings.Join(probs, "; "))
		}
		files = append(files, scaffoldFile{p, c})
	}
	return files, nil
}

// handlePluginDetail GET/DELETE /api/plugins/{name}
func (s *Server) handlePluginDetail(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/plugins/"), "/")
	if name == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("plugin name required"))
		return
	}
	if !validPluginName(name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("非法的插件名 %q", name))
		return
	}

	if r.Method == http.MethodDelete {
		s.handlePluginDelete(w, name)
		return
	}

	// 与列表读**同一份**索引 —— 分开取的话会出现"列表里有它、点进去 404"的自相矛盾。
	cat := s.agentCatalog()
	a := cat.Get(asset.KindPlugin, name)
	if a == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("plugin %q not found", name))
		return
	}
	d := pluginDetailDTO{Body: a.Body}
	// members 五类先归一成 []: Expand 失败的分支不动它们, 零值 nil 切片会序列化成
	// null —— "插件在但展开坏了"与"前端白页"不该是因果关系。
	d.Members = pluginMembersDTO{
		Agents: []pluginMemberDTO{}, Skills: []pluginMemberDTO{}, Hooks: []pluginMemberDTO{},
		Tools: []pluginMemberDTO{}, Workflows: []pluginMemberDTO{},
	}
	b, err := pluginrun.Expand(cat, name)
	if err != nil {
		// 插件在、展开失败: 仍返回 200 与清单本体, 把失败放进 problems。
		// 展开问题不是"插件不存在", 用 500 会把"清单能看"与"成员展开坏了"混成一件事。
		d.pluginDTO = pluginDTO{
			Name: a.Name, Description: a.Description, Version: a.Version,
			Status: a.Status, Source: a.Source, SourcePath: a.SourcePath,
			DirForm:  pluginIsDirForm(a.SourcePath),
			Problems: []string{err.Error()},
			Editable: strings.HasPrefix(a.SourcePath, s.pluginAssetRoot()+string(filepath.Separator)),
		}
	} else {
		d.pluginDTO = s.pluginDTOFromBundle(b)
		if b.Plugin != nil {
			d.Body = b.Plugin.Body
		}
		d.Members = pluginMembersDTO{
			Agents:    pluginMemberList(b.Agents),
			Skills:    pluginMemberList(b.Skills),
			Hooks:     pluginMemberList(b.Hooks),
			Tools:     pluginMemberList(b.Tools),
			Workflows: pluginMemberList(b.Workflows),
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"plugin": d})
}

// pluginMemberList 投影成员摘要; 恒返回非 nil 切片 (nil 序列化成 null, 前端
// list.map 当场白页 —— 本仓反复出现过的形态)。
func pluginMemberList(assets []*asset.Asset) []pluginMemberDTO {
	out := make([]pluginMemberDTO, 0, len(assets))
	for _, a := range assets {
		if a == nil {
			continue
		}
		out = append(out, pluginMemberDTO{Name: a.Name, Description: a.Description})
	}
	return out
}

// handlePluginDelete DELETE /api/plugins/{name}: 只删 state 根下的那一份。
func (s *Server) handlePluginDelete(w http.ResponseWriter, name string) {
	root := s.pluginAssetRoot()
	dir := filepath.Join(root, name)
	// 防路径穿越第二道 (第一道是 validPluginName 的字符集): RemoveAll 不可逆,
	// 落点必须显式确认在 state 的 plugins 根之内 —— 与 handleTeamMedia 的守卫同形态。
	absRoot, err := filepath.Abs(root)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if absDir != absRoot && !strings.HasPrefix(absDir, absRoot+string(os.PathSeparator)) {
		writeError(w, http.StatusForbidden, fmt.Errorf("path traversal blocked"))
		return
	}

	deleted := false
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		// 目录形式: 整包删除 (PLUGIN.md + 成员子目录)。
		if err := os.RemoveAll(dir); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		deleted = true
	} else {
		// 平铺形态 <root>/<名>.md 也是 LoadDir 认的合法布局, 列表同样把它标成
		// editable —— 这里只认目录的话, 会出现"面板说能删、点了却 404"的自相矛盾。
		flat := filepath.Join(root, name+".md")
		if _, err := os.Stat(flat); err == nil {
			if err := os.Remove(flat); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			deleted = true
		}
	}
	if !deleted {
		// ★ 只删 state 根下的那一份, 且只认"在不在"。报 404 而不是"删成功":
		//   同名插件可能还有 user/project 根里的一份仍在生效, 那种情况下说"已删除"
		//   会让用户以为它没了。删除后剩余来源的同名资产会按覆盖语义自然浮现,
		//   那是正常结果不是残留 (与 handleAgentDetail 的删除注释同理)。
		writeError(w, http.StatusNotFound, fmt.Errorf(
			"state 资产目录里没有 %q (user/project 根里的同名资产不在本接口的删除范围内)", name))
		return
	}
	if s.cfg.ReloadAgents != nil {
		s.cfg.ReloadAgents()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "deleted": name, "scope": "state"})
}

// buildPluginMarkdown 把入参渲染成一份 PLUGIN.md。
//
// 输出形状与 pkg/asset 的解析器逐键对应 (扁平 frontmatter, kebab-case 与单值键)。
// version 只在**有值**时写: "没写 version"与"写了空 version"在资产语义里是两件事,
// 无条件写一行空值会把"未声明"变成"声明为空"。
func buildPluginMarkdown(in pluginCreateInput) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("kind: plugin\n")
	b.WriteString("name: " + in.Name + "\n")
	b.WriteString("description: " + oneLine(in.Description) + "\n")
	// status 显式写 active: IsActive() 对空值也是 true, 但显式写出来让这份文件在
	// 人读时能一眼看出"它是故意在这里的"。
	b.WriteString("status: active\n")
	if v := strings.TrimSpace(in.Version); v != "" {
		b.WriteString("version: " + oneLine(v) + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(in.Body))
	b.WriteString("\n")
	return b.String()
}

// pluginGenerateInput 需求驱动的插件包草稿生成。
type pluginGenerateInput struct {
	Objective string `json:"objective"`
}

// pluginScaffoldMember 草稿里的一个成员骨架。body 是该成员资产文件的正文
// (agent 的系统提示词 / SKILL.md 正文 / HOOK.md 正文)。
type pluginScaffoldMember struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Body        string `json:"body,omitempty"`
	// Decls 仅 hooks 成员用: 扁平三/四段式 hooks 声明行 (event:type:target[:if])。
	// hook 的生效声明住在 frontmatter 的 hooks: 列表里, 写在 body 里运行时看不到 ——
	// 所以声明必须有自己的字段, 不能混在正文里让人以为写了就算数。
	Decls []string `json:"decls,omitempty"`
}

// pluginScaffold 草稿的成员骨架。只有 agents/skills/hooks 三类 —— 它们是插件包里
// 最常见的成员; tools/workflows 用户可以在保存后自己补, 草稿阶段不替人把架子搭满。
type pluginScaffold struct {
	Agents []pluginScaffoldMember `json:"agents"`
	Skills []pluginScaffoldMember `json:"skills"`
	Hooks  []pluginScaffoldMember `json:"hooks"`
}

// pluginDraft LLM 生成的插件包草稿。
type pluginDraft struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Body        string         `json:"body"`
	Scaffold    pluginScaffold `json:"scaffold"`
}

// handlePluginGenerate POST /api/plugins/generate {objective} → LLM 出草稿, **不落盘**。
//
// 与 handleAgentGenerate 同一形状, "不落盘"也同样是有意的: 生成出来的是一份会被
// 写进全局资产目录、进而被运行时展开装载的插件包定义。让它在用户看过、改过之前
// 不落盘, 是这个流程里唯一的人工闸门。
func (s *Server) handlePluginGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	if s.cfg.LLMComplete == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("LLM 不可用"))
		return
	}
	var body pluginGenerateInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Objective) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("objective 不能为空"))
		return
	}

	sys := "你是 claude-go 的插件包 (plugin bundle) 设计专家。把一句需求变成一个可装载的插件包草稿。只输出 JSON, 不要解释。"
	user := fmt.Sprintf(`需求: %s

claude-go 的插件包是一个目录, 形状如下:
  plugins/<name>/PLUGIN.md            清单: frontmatter (kind: plugin, name, description, version) + markdown 正文
  plugins/<name>/agents/<x>.md        成员 agent: frontmatter (name, description) + 正文 = 系统提示词
  plugins/<name>/skills/<x>/SKILL.md  成员技能: frontmatter (name, description) + 正文
  plugins/<name>/hooks/<x>.md      成员 hook: frontmatter 的 hooks: 列表逐行写扁平三/四段式声明:
                                      <event>:<type>:<target>[:<if 匹配器>]
                                      例: PreToolUse:command:./scripts/guard.sh:Bash*
                                          SessionStart:http:https://hooks.internal/session
                                      type 可留空 (留空即 command); if 省略即不限制。
                                      event 常用取值: PreToolUse | PostToolUse | SessionStart | SessionEnd | SubagentStop | Stop
                                      ★ 声明必须进 frontmatter 的 hooks: 列表, 写在正文里运行时看不到。

输出插件包草稿的 JSON:
{"name":"小写字母/数字/连字符","description":"一句话写清【做什么】+【何时该用它】",
 "body":"PLUGIN.md 的 markdown 正文: 这个包解决什么问题、成员如何协作、使用边界。不要复述通用编码规范",
 "scaffold":{"agents":[{"name":"...","description":"第三人称, 做什么+何时派它","body":"该 agent 的系统提示词"}],
             "skills":[{"name":"...","description":"做什么+何时用","body":"SKILL.md 正文, 精简可操作"}],
             "hooks":[{"name":"...","description":"这条 hook 防什么","decls":["PreToolUse:command:..."],"body":"HOOK.md 正文: 说明这条 hook 的意图与边界"}]}}

要求: name 只含小写字母/数字/连字符; 成员不需要就给空数组, 不要捏造与需求无关的成员;
hook 的 event 必须从上面的合法取值里选; body 不要写空话。`, oneLine(body.Objective))
	out, err := s.cfg.LLMComplete(context.Background(), sys, user)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	js := extractJSONObj(out)
	if js == "" {
		writeError(w, http.StatusBadGateway, fmt.Errorf("LLM 未返回可解析 JSON"))
		return
	}
	var draft pluginDraft
	if err := json.Unmarshal([]byte(js), &draft); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	// 三类成员归一成非 nil: LLM 省略某一类时 scaffold.agents 会序列化成 null,
	// 前端 members.map 当场白页 (与列表接口同一防线的同一侧)。
	if draft.Scaffold.Agents == nil {
		draft.Scaffold.Agents = []pluginScaffoldMember{}
	}
	if draft.Scaffold.Skills == nil {
		draft.Scaffold.Skills = []pluginScaffoldMember{}
	}
	if draft.Scaffold.Hooks == nil {
		draft.Scaffold.Hooks = []pluginScaffoldMember{}
	}
	// 草稿不落盘, 但校验与创建走同一套 —— 否则会出现"生成说没问题、保存被拒"。
	// 问题作为 warn 回给前端让人修, 不当作 500: 草稿本来就是待审的。
	warn := ""
	switch {
	case !validPluginName(strings.TrimSpace(draft.Name)):
		warn = "name 不符合规范(应小写字母/数字/连字符), 请修正"
	case strings.TrimSpace(draft.Body) == "":
		warn = "body 为空, 请补上 PLUGIN.md 正文"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"plugin": draft, "warn": warn})
}
