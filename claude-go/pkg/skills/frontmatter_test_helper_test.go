package skills

import "github.com/anthropic/claude-go/pkg/asset"

// parseFM 供本包测试使用: 把**裸 frontmatter 文本**包成完整的块再交给生产解析器。
//
// 为什么需要它: parseFrontmatter 的入参现在是 asset.Frontmatter (已经是结构化的),
// 而本包的历史测试直接喂裸文本。生产路径上这个包装永远不必做 —— 真实文件自带
// `---` 定界符, asset.SplitFrontmatter 一次就拆好了。所以它只存在于测试里,
// 不放进生产代码当"顺手多给一个入口"。
func parseFM(fm string, sk *Skill) {
	parseFrontmatter(asset.SplitFrontmatter("---\n"+fm+"\n---\n"), sk)
}
