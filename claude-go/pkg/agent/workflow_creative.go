package agent

func creativeWorkflow() *WorkflowDef {
	return &WorkflowDef{
		Name:        "creative",
		Description: "图片&视频创意团队: 创意策划→提示词工程→素材生成→视觉审查→后期合成",
		Mode:        "adversarial_dev",
		Rounds:      2,
		Stages: []StageDef{
			{
				Name: "creative-brief", Role: "creative-director",
				Prompt: `你是资深创意总监，擅长将模糊需求转化为精确的视觉创意方案。

创作需求: {objective}

请输出创意策划方案:

## 🎨 创意简报
1. **核心主题**: 一句话概括创作目标和核心表达
2. **目标受众**: 受众画像、审美偏好、使用场景
3. **视觉风格**: 风格定义 (如: 扁平插画/3D渲染/赛博朋克/国风水墨/写实摄影)
4. **色彩方案**: 主色调、辅助色、配色灵感
5. **构图规划**: 主体位置、视角、景深、空间关系
6. **参考基准**: 2-3个风格参考描述

## 📐 技术规格
- 输出格式: SVG / HTML+CSS / 组合图
- 分辨率/尺寸建议
- 是否需要动画/交互

## 🎬 视频规划 (如果需要)
- 分镜数量: N 帧
- 每帧时长: X 秒
- 运镜设计: 推/拉/平移/旋转/缩放
- 转场效果: 淡入淡出/硬切/形变
- 节奏控制: 快节奏/慢节奏/张弛有度`,
			},
			{
				Name: "prompt-engineer", Role: "prompt-engineer",
				DependsOn: []string{"creative-brief"},
				Prompt: `你是专业的 AI 视觉生成提示词工程师，精通 SVG/HTML 视觉创作指令。

创作需求: {objective}

创意简报:
{prev_result}

{adversarial_feedback}

请为每个需要生成的视觉素材编写详细的创作指令:

## 提示词设计
对于每个素材，输出:

### 素材 N: [名称]
**SVG/HTML 生成指令:**
- 精确的视觉描述 (颜色值、尺寸、位置、变换)
- 具体的 SVG 元素结构 (rect, circle, path, text, gradient, filter)
- CSS 动画指令 (如需要: @keyframes, transition, transform)
- 构图和层次关系

**质量控制要点:**
- 必须检查的视觉要素
- 常见生成错误的预防

## 视频分镜 (如果适用)
对于每一帧:
| 帧号 | 场景描述 | 运镜 | 主体动作 | 时长 | 转场 |
|------|---------|------|---------|------|------|
| 1 | ... | ... | ... | 2s | ... |

确保提示词足够精确，使 LLM 能生成高质量 SVG/HTML 代码。`,
			},
			{
				Name: "asset-generate", Role: "visual-artist",
				DependsOn: []string{"prompt-engineer"},
				Prompt: `你是专业的 SVG/HTML 视觉创作专家。根据提示词指令实际生成视觉素材代码。

创作需求: {objective}

创作指令:
{prev_result}

{adversarial_feedback}

## 生成要求 (必须实际输出代码, 禁止角色扮演!)
请按照创作指令，为每个素材生成完整的 SVG 或 HTML+CSS 代码:

1. **SVG 素材**: 输出完整的 <svg> 代码，包含所有图形元素、渐变、滤镜
2. **HTML 素材**: 输出完整的 HTML+CSS 代码块，可直接在浏览器中渲染
3. **动画素材**: 使用 CSS @keyframes 或 SVG SMIL 动画
4. **视频帧**: 如果是多帧场景，每帧一个独立 SVG/HTML 块

⚠️ 绝对禁止:
- 不允许只说"我是视觉艺术家，我已就位"然后标记完成
- 不允许只输出模板或说明文字而不生成实际代码
- 你的输出中必须包含至少一个完整的 <svg> 或 <html> 代码块
- 如果无法生成请求的内容，必须生成一个替代方案而非空手而归

## 质量标准
- 视觉美观、配色协调
- 代码语义清晰、结构合理
- 渐变和阴影适度使用提升质感
- 文字排版优美、字体选择恰当
- 响应式设计 (如 viewBox 正确设置)

## 输出验证
- 将生成的素材保存为文件 (使用 Write 工具)
- 输出每个素材的完整代码块和渲染说明`,
			},
			{
				Name: "visual-review", Role: "art-director",
				DependsOn: []string{"asset-generate"},
				Prompt: `你是资深艺术指导/视觉审查员(Evaluator 角色)。以挑剔的专业眼光审查生成的视觉素材。

创作需求: {objective}

生成的素材:
{prev_result}

请对每个素材进行严格审查:

## 审查维度 (每项 0-10 分)
Score each dimension. Output STRICTLY as JSON:
{"correctness": N, "completeness": N, "security": N, "code_quality": N, "pass": bool, "feedback": "..."}

映射:
- correctness → 视觉准确性 (是否符合创意简报)
- completeness → 完整性 (是否所有元素都呈现)
- security → 品牌安全性 (是否有不当内容、版权风险)
- code_quality → 代码质量 + 审美质量 (配色、构图、细节)

## 详细反馈
1. **视觉一致性**: 是否符合创意简报的风格定义
2. **色彩协调**: 配色是否和谐、对比是否合适
3. **构图平衡**: 元素布局是否美观、留白是否合理
4. **细节品质**: 渐变/阴影/边缘处理是否精细
5. **动画流畅度**: (如适用) 动画是否自然、节奏感是否良好
6. **技术规范**: SVG/CSS 代码是否规范、是否有冗余

Hard pass threshold: ALL ≥ 7 AND pass == true.
如不通过，给出具体、可操作的修改建议。`,
			},
			{
				Name: "post-production", Role: "post-producer",
				DependsOn: []string{"asset-generate"},
				Prompt: `你是后期制作专家。将通过审查的素材组装为最终交付物。

创作需求: {objective}

审查通过的素材:
{prev_result}

## 后期任务
1. **素材整合**: 将多个素材组合为完整的作品
2. **HTML 播放器**: (如有视频帧) 生成完整的 HTML 播放器
   - 帧间过渡动画 (CSS transitions)
   - 自动播放控制
   - 进度条和播放/暂停按钮
3. **格式导出**: 生成可直接使用的格式
   - SVG 图片: 完整独立的 SVG 文件代码
   - HTML 页面: 内联 CSS 的完整 HTML 文件
   - 视频 HTML: 含所有帧和动画的 HTML 播放页面

## 输出格式
对于每个最终作品:
- 完整的可运行代码
- 渲染预览说明
- 使用建议 (在哪些场景使用、如何嵌入)`,
				Parallel: true,
			},
		},
	}
}

