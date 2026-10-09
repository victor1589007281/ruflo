package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropic/claude-go/pkg/agent"
	"github.com/anthropic/claude-go/pkg/asset"
	"github.com/anthropic/claude-go/pkg/tool/builtin"
)

// agent_handlers.go: agent **资产**的查看与创建。
//
// ---------------------------------------------------------------------------
// 为什么这个文件必须存在 (而不是让 webapp 自己往盘上写文件)
// ---------------------------------------------------------------------------
//
// 「自定义 agent」在本仓里是**两种**东西, 不是一个:
//
//	agent 资产   <root>/agents/<名>.md           Markdown + 扁平 frontmatter  ← 这个文件管的
//	角色         <root>/.claude{,-go}/agents/*.json   JSON, RoleDef
//
// 前者优先级更高 (resolveAgentRunSpec 先查资产再兜底角色), 且能声明的东西多得多:
// 正文即系统提示词, 外加 model / max-turns / tool-profile / permission-mode /
// tools / disallowed-tools / skills / hooks。角色只能声明 systemPrompt + skills。
// 所以"新建一个 agent"要落在**资产**这一层。
//
// 改造前资产层**没有任何写入口**: CLI / dashboard / :18080 API 三处都没有, 只能手丢
// 文件。而技能早就有 POST /api/skills 了 —— 这个文件就是把同一形状补到 agent 上。
//
// ★ 为什么不让 webapp 直接写 <stateDir>/agents/<名>.md (那不需要改 Go, 更省事):
//   frontmatter 的键名是一份**会被两个进程各自解释**的契约 —— 写方 webapp、读方
//   claude-go。写方自己攒一份键名表, 就出现了第二份事实; 哪天这边把 `max-turns`
//   改成 `maxTurns`, webapp 会继续写一个 claude-go **静默忽略**的键, 而产物看起来
//   完全正常 (文件在、能解析、只是那个字段不生效 —— 正是 assetb 反复出现的那类缺陷)。
//   键名只能有一处定义, 那处就是读它的这一侧。

const agentAssetsSubdir = "agents"

// agentAssetRoot 资产文件的落点: <stateDir>/agents。
//
// 与 asset.LoadRoots 的 state 来源**逐字节一致** (见 pkg/asset/load.go 的
// standardSources: state 来源的根就是 stateDir 本身, 不再多一层)。写成别的路径会
// 得到一个"写进去了但没人读"的目录 —— 那种失败没有任何报错。
func (s *Server) agentAssetRoot() string { return filepath.Join(s.cfg.StateDir, agentAssetsSubdir) }

// validAgentName 与 validSkillName 同规则 (小写字母/数字/连字符)。
//
// 为什么名字必须守这条: 它要同时当 (a) 文件名 (b) --agent 的取值 (c) --run-label
// 里嵌的片段 (d) 在轨迹页按名子串搜的词。含空格或大写会让 (b) 在命令行上需要引号,
// 含路径分隔符会让 (a) 可以写到目录外面去。
func validAgentName(s string) bool { return validSkillName(s) }

// agentAssetDTO 一份 agent 资产的对外表示。
//
// 这里**显式列出**了前端要用的全部字段, 而不是把 asset.Asset 整个序列化出去:
// Asset 只有一个 Name/Description/Body 的通用形状, 而 agent 这一类的可调项 (model /
// max-turns / tool-profile / …) 全在 FM 里, 序列化 Asset 反而要前端自己去 frontmatter
// 里刨。列出字段这件事本身就是"这一类资产有哪些旋钮"的定义。
type agentAssetDTO struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Body        string   `json:"body,omitempty"`
	Model       string   `json:"model,omitempty"`
	MaxTurns    int      `json:"maxTurns,omitempty"`
	ToolProfile string   `json:"toolProfile,omitempty"`
	PermMode    string   `json:"permissionMode,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Disallowed  []string `json:"disallowedTools,omitempty"`
	Hooks       []string `json:"hooks,omitempty"`
	Status      string   `json:"status,omitempty"`
	Source      string   `json:"source,omitempty"`     // state | project | user
	SourcePath  string   `json:"sourcePath,omitempty"` // 只对 state 来源给出 (可编辑的那一份)
	Active      bool     `json:"active"`
	HasTools    bool     `json:"hasTools,omitempty"` // tools 这个键**写没写**: 写了空列表 = fail-closed 拒一切工具
}

func dtoFromAsset(a *asset.Asset, sourcePath string) agentAssetDTO {
	d := agentAssetDTO{
		Name: a.Name, Description: a.Description, Body: a.Body,
		Model:       a.Meta("model"),
		ToolProfile: a.Meta("tool-profile"),
		PermMode:    a.Meta("permission-mode"),
		Status:      a.Status,
		Source:      a.Source,
		SourcePath:  sourcePath,
		Active:      a.IsActive(),
		Skills:      a.MetaList("skills"),
		Disallowed:  a.MetaList("disallowed-tools"),
		Hooks:       a.MetaList("hooks"),
	}
	if v := strings.TrimSpace(a.Meta("max-turns")); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &d.MaxTurns)
	}
	if tools, ok := a.MetaListSet("tools"); ok {
		d.HasTools = true
		d.Tools = tools
	}
	return d
}

// handleAgents GET /api/agents (列表) | POST /api/agents (创建/覆盖)
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleAgentCreate(w, r)
		return
	}
	writeJSON(w, http.StatusOK, s.agentAssetList())
}

// agentAssetList 返回 agent 资产列表。
//
// ★ 取哪一份索引: **进程里有运行时就取运行时的那份, 没有就现扫一次**。
//   优先级不能反 —— 进程内有 agent.AgentCatalog() 时 (飞书主进程 :18080), 它才是
//   权威: 面板与运行时查的是同一张表, "面板说这个 agent 在、运行说找不到"这类分叉
//   在结构上不存在。
//
//   但独立 dashboard (dashboard run, :7777) 里这个单例**永远是 nil** —— 全仓只有
//   两处 SetAgentCatalog (CLI 的 --agent 路径 / pkg/feishu), 都不在独立 dashboard
//   进程里。此时若直接返回空列表, 表现是: 创建接口 200、文件也确实写进了
//   <stateDir>/agents/, 而**紧接着的列表刷新是空的** —— 用户看到的是"建完就没了",
//   会得出"创建失败了"这个错误结论 (实测撞到过)。
//
//   所以 nil 时退化成现扫。这时进程内不存在"运行时那一份"可供分叉, 而**新起一个
//   claude-go 进程读到的正是这次现扫的结果** —— 现扫反而比"空"更接近真值。
//   代价是每次请求扫一遍目录, 与同包的 freshSkillRegistry (capability_handlers.go)
//   是同一个取舍。
func (s *Server) agentAssetList() []agentAssetDTO {
	cat := s.agentCatalog()
	names := cat.Names(asset.KindAgent)
	out := make([]agentAssetDTO, 0, len(names))
	for _, n := range names {
		a := cat.Get(asset.KindAgent, n)
		if a == nil {
			continue
		}
		d := dtoFromAsset(a, "")
		// root 这一列只有"是不是本机可编辑的那一份"这一个用途 (前端据此显示删除按钮)。
		// 用前缀匹配而不是重新推导: 推导要再实现一遍 state/project/user 三来源的优先级,
		// 而那份优先级换了之后这里会静默错判"哪一份能删"。
		if strings.HasPrefix(a.SourcePath, s.agentAssetRoot()+string(filepath.Separator)) {
			d.SourcePath = a.SourcePath
		}
		out = append(out, d)
	}
	return out
}

// agentCatalog 返回本次请求应读的那份 agent 资产索引 (见 agentAssetList 的注释)。
//
// 抽成一个方法而不是在两处各写一遍: 列表与详情的口径必须一致 —— 分开写的话,
// 将来只改一处就会出现"列表里有它、点进去 503"这种自相矛盾的界面。
func (s *Server) agentCatalog() *asset.Set {
	if cat := agent.AgentCatalog(); cat != nil {
		return cat
	}
	return asset.LoadStandard(asset.DefaultRoots(s.projectCwd(), s.cfg.StateDir))
}

// agentCreateInput 新建/覆盖一份 agent 资产。
//
// 全部字段都是**可选但显式**的: 指针与 Has* 的存在, 是为了区分"没填"与"填了空"。
// 这个区分在这一层是有意义的 —— tools 写了空列表在资产语义里是 fail-closed (拒绝
// 一切工具), 与"没写 tools"完全相反 (见 pkg/asset/asset.go:143 MetaListSet 的注释)。
// 用一个零值去表示两件事, 会让"我要一个没有工具的 agent"这一诉求静默退化成
// "跟随基线, 什么工具都有"。
type agentCreateInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	Model       string   `json:"model,omitempty"`
	MaxTurns    *int     `json:"maxTurns,omitempty"`
	ToolProfile string   `json:"toolProfile,omitempty"`
	PermMode    string   `json:"permissionMode,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	Tools       *[]string `json:"tools,omitempty"`
	Disallowed  []string `json:"disallowedTools,omitempty"`
}

func (s *Server) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
	var in agentCreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !validAgentName(in.Name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name 必须为小写字母/数字/连字符 (它同时是文件名与 --agent 的取值)"))
		return
	}
	if strings.TrimSpace(in.Description) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("description 不能为空 (它进的是模型可见的 agent 清单, 模型据此决定要不要派它)"))
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("body 不能为空 (它就是该 agent 的系统提示词)"))
		return
	}
	if in.ToolProfile != "" && !builtin.ValidToolProfile(builtin.ToolProfile(in.ToolProfile)) {
		// ★ 这里必须**拒绝而不是归一**。NormalizeToolProfile 对拼错的档位静默返回 chat,
		//   且不留痕 —— 一个写成 `codign` 的声明会得到一个连 Read 都没有的子代理,
		//   而产物看起来完全正常。见 pkg/tool/builtin/profile.go:ValidToolProfile 的注释。
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"toolProfile %q 不是合法档位 (合法取值: chat | research | coding | team | analysis | admin)",
			in.ToolProfile))
		return
	}
	if in.PermMode != "" {
		switch in.PermMode {
		case "default", "auto", "plan", "bypass":
		default:
			writeError(w, http.StatusBadRequest, fmt.Errorf(
				"permissionMode %q 不是合法取值 (合法取值: default | auto | plan | bypass)", in.PermMode))
			return
		}
	}

	md := buildAgentMarkdown(in)
	root := s.agentAssetRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	dst := filepath.Join(root, in.Name+".md")
	// ★ 覆盖前先解析一遍新内容。写盘之后再发现写坏, 留下的是一份**已经覆盖掉旧定义**
	//   的坏文件 —— 用户既失去了老版本, 盘上那个还不能用。先验后写, 失败时盘上仍是
	//   上一份完好的定义。
	if _, err := asset.ParseContent(md, dst, "state", asset.KindAgent); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("生成的资产无法解析 (这是个缺陷, 请上报): %w", err))
		return
	}
	if err := os.WriteFile(dst, []byte(md), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.cfg.ReloadAgents != nil {
		s.cfg.ReloadAgents()
	}
	// 回读**刚写下去的那一份解析结果**, 不回显入参: 两者不一致时 (例如某个键的写法
	// 没被解析器认下来) 用户必须在响应里就看见, 而不是等到运行的时候发现某个声明
	// 没生效。这是 asset 层唯一能自证"写对了"的手段。
	back, err := asset.ParseFile(dst, "state", asset.KindAgent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("写入成功但回读失败: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "name": in.Name, "path": dst,
		"asset": dtoFromAsset(back, dst),
	})
}

// handleAgentDetail GET/DELETE /api/agents/{name}
func (s *Server) handleAgentDetail(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agents/"), "/")
	if name == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("agent name required"))
		return
	}
	if !validAgentName(name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("非法的 agent 名 %q", name))
		return
	}
	dst := filepath.Join(s.agentAssetRoot(), name+".md")

	if r.Method == http.MethodDelete {
		if _, err := os.Stat(dst); err != nil {
			// ★ 只删 state 根下的那一份, 且**只认文件在不在**。
			//   报 404 而不是"删成功": 同一个名字可能还有 user/project 根里的一份仍在
			//   生效 —— 那种情况下说"已删除"会让用户以为这个 agent 没了, 而它照样能
			//   被 --agent 解析到。所以响应里明说"只删了 state 这一份"。
			writeError(w, http.StatusNotFound, fmt.Errorf(
				"state 资产目录里没有 %q (user/project 根里的同名资产不在本接口的删除范围内)", name))
			return
		}
		if err := os.Remove(dst); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// ★ 删除**不检查"是否还有别的来源提供同名资产"** —— 因为 asset.Set.Add 是
		//   后加载覆盖先加载, state 来源**最后加载**, 所以 state 这份被删掉之后, 剩下的
		//   project/user 那两份会自然浮现出来 (如果它们存在)。这是覆盖语义的正常结果,
		//   不是残留 —— 响应里把这件事说清楚即可, 不必去猜"用户是不是想全删"。
		if s.cfg.ReloadAgents != nil {
			s.cfg.ReloadAgents()
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "deleted": name, "scope": "state"})
		return
	}

	cat := s.agentCatalog()
	a := cat.Get(asset.KindAgent, name)
	if a == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("agent %q not found", name))
		return
	}
	sp := ""
	if strings.HasPrefix(a.SourcePath, s.agentAssetRoot()+string(filepath.Separator)) {
		sp = a.SourcePath
	}
	writeJSON(w, http.StatusOK, dtoFromAsset(a, sp))
}

// buildAgentMarkdown 把入参渲染成一份资产文件。
//
// 输出形状与 pkg/asset 的解析器**逐键对应**: 键名用的是 agentbind 读的那几个
// (tool-profile / permission-mode / max-turns / model / skills / tools /
// disallowed-tools), 不是驼峰。多行列表用 `- item` 续行 (frontmatter.go 两种形态都认,
// 多行在这边可读性更好)。
//
// 只在**有值**时写那一行: 资产语义里"没写这个键"与"写了空"是两件事 (尤其 tools),
// 无条件写一行空值会把"未声明"变成"声明为空"。
func buildAgentMarkdown(in agentCreateInput) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("kind: agent\n")
	b.WriteString("name: " + in.Name + "\n")
	b.WriteString("description: " + oneLine(in.Description) + "\n")
	// status 显式写 active: IsActive() 对空值也是 true, 但显式写出来让这份文件在
	// 人读时能一眼看出"它是故意在这里的", 而不是"忘了写状态"。
	b.WriteString("status: active\n")
	if m := strings.TrimSpace(in.Model); m != "" {
		b.WriteString("model: " + oneLine(m) + "\n")
	}
	if in.MaxTurns != nil && *in.MaxTurns > 0 {
		fmt.Fprintf(&b, "max-turns: %d\n", *in.MaxTurns)
	}
	if p := strings.TrimSpace(in.ToolProfile); p != "" {
		b.WriteString("tool-profile: " + p + "\n")
	}
	if p := strings.TrimSpace(in.PermMode); p != "" {
		b.WriteString("permission-mode: " + p + "\n")
	}
	writeList := func(key string, items []string) {
		clean := make([]string, 0, len(items))
		for _, it := range items {
			if t := strings.TrimSpace(it); t != "" {
				clean = append(clean, t)
			}
		}
		if len(clean) == 0 {
			return
		}
		b.WriteString(key + ":\n")
		for _, it := range clean {
			b.WriteString("  - " + it + "\n")
		}
	}
	writeList("skills", in.Skills)
	writeList("disallowed-tools", in.Disallowed)
	// tools 单独走: *[]string 的"给了空列表"要落成一个**显式的空键**, 那是 fail-closed
	// 语义 (拒绝一切工具)。写成 `tools: []` 而解析器只认续行的话会读成"没写" ——
	// 所以这里写一个不带任何条目的键头, 让 MetaListSet 认出"写了, 且是空的"。
	if in.Tools != nil {
		clean := make([]string, 0, len(*in.Tools))
		for _, it := range *in.Tools {
			if t := strings.TrimSpace(it); t != "" {
				clean = append(clean, t)
			}
		}
		if len(clean) == 0 {
			b.WriteString("tools: []\n")
		} else {
			b.WriteString("tools:\n")
			for _, it := range clean {
				b.WriteString("  - " + it + "\n")
			}
		}
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(in.Body))
	b.WriteString("\n")
	return b.String()
}

// agentGenerateInput 需求驱动的草稿生成。
type agentGenerateInput struct {
	Objective string `json:"objective"`
	Name      string `json:"name,omitempty"`
}

// handleAgentGenerate POST /api/agents/generate {objective} → LLM 出草稿, **不落盘**。
//
// 与 handleSkillGenerate 同一形状, 且"不落盘"这一条同样是有意的: 生成出来的东西
// 是一份**会被写进全局资产目录、进而进入每一次请求的 agent 清单**的定义。让它在
// 用户看过、改过之前不落盘, 是这个流程里唯一的人工闸门。
func (s *Server) handleAgentGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	if s.cfg.LLMComplete == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("LLM 不可用"))
		return
	}
	var body agentGenerateInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Objective) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("objective 不能为空"))
		return
	}

	sys := "你是 claude-go 的 agent 定义专家。把一句需求变成一个可运行的单 agent 定义。只输出 JSON, 不要解释。"
	user := fmt.Sprintf(`需求: %s

输出一个 agent 资产的 JSON。字段含义:
- name: 小写字母/数字/连字符; 它同时是文件名与 --agent 的取值
- description: 第三人称, 一句话写清【做什么】+【何时该派它】。它进的是模型可见的 agent 清单, 模型据此决定要不要调用它
- body: 该 agent 的系统提示词 (Markdown)。写职责、边界、输出格式。不要复述通用编码规范
- model: 留空表示跟随调用方默认
- maxTurns: 整数, 留空表示不限制
- toolProfile: 只能是 chat | research | coding | team | analysis | admin 之一。
    · analysis = 只读分析/调研 (最精简工具集)
    · coding = 需要读写文件/执行命令
    · research = 需要联网检索
    · chat = 只要对话, 不要文件工具
- permissionMode: 只能是 default | auto | plan | bypass 之一, 留空即不声明
- skills: 该 agent 要挂载的技能名数组, 不确定就给空数组

{"name":"...","description":"...","body":"...","model":"","maxTurns":0,"toolProfile":"analysis","permissionMode":"","skills":[]}

要求: description 必须含触发条件; body 不要写空话; toolProfile 必须从上面六个里选一个。`, oneLine(body.Objective))
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
	var draft agentCreateInput
	if err := json.Unmarshal([]byte(js), &draft); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	// 草稿不落盘, 但**校验与创建走同一套** —— 否则会出现"生成说没问题、保存被拒"。
	// 这里只把问题作为 warn 回给前端让人修, 不当作 500: 草稿本来就是待审的。
	warn := ""
	switch {
	case !validAgentName(strings.TrimSpace(draft.Name)):
		warn = "name 不符合规范(应小写字母/数字/连字符), 请修正"
	case strings.TrimSpace(draft.Body) == "":
		warn = "body 为空, 请补上系统提示词"
	case draft.ToolProfile != "" && !builtin.ValidToolProfile(builtin.ToolProfile(draft.ToolProfile)):
		warn = fmt.Sprintf("toolProfile %q 不是合法档位, 请从 chat|research|coding|team|analysis|admin 里选", draft.ToolProfile)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"agent": draft, "warn": warn})
}
