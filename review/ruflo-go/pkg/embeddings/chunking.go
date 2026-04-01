// 文本分块（chunking.go）
//
// 设计思路：嵌入模型与向量库通常有上下文长度上限；长文档需切分为重叠窗口，使跨块边界的关键词仍可能在相邻块同现，缓解语义断裂。
// 在硬截断前优先沿 Separator（如换行）回退切分点，使块边界尽量落在自然分隔符之后，提升短语完整性。
//
// 整体架构：ChunkConfig 描述窗口与重叠；ChunkText 以字节偏移（UTF-8 字节序列）滑动，输出带 Start/End/Index 的 Chunk 切片。
package embeddings

import "strings"

// ChunkConfig 控制分块尺寸、相邻块重叠与优先断句分隔符。
type ChunkConfig struct {
	MaxChunkSize int    // 单块最大字节长度（UTF-8 字节偏移）
	Overlap      int    // 下一块起点相对当前块末尾的回退重叠，利于跨块语义连续
	Separator    string // 在窗口内从后向前找最后一次出现，用于软截断
}

// Chunk 表示原文的一段连续子串及在 UTF-8 字节序列中的起止与序号。
type Chunk struct {
	Text  string // 子串内容
	Start int    // 起始字节下标（含）
	End   int    // 结束字节下标（不含）
	Index int    // 块序号，从 0 递增
}

// DefaultChunkConfig 返回常用默认：512 字节块、64 重叠、按换行优先切分。
func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{MaxChunkSize: 512, Overlap: 64, Separator: "\n"}
}

// ChunkText 从 start=0 起循环：end=min(start+MaxChunkSize,len)；若未达文末则在 [start,end) 内找 Separator 最后一次出现并将 end 对齐到分隔符之后；
// 下一起点 next=end-Overlap（至少前进 1），形成滑动窗口。空文本返回 nil。
func ChunkText(text string, cfg ChunkConfig) []Chunk {
	if cfg.MaxChunkSize <= 0 {
		cfg.MaxChunkSize = 512
	}
	if cfg.Overlap < 0 {
		cfg.Overlap = 0
	}
	if cfg.Overlap >= cfg.MaxChunkSize {
		cfg.Overlap = cfg.MaxChunkSize - 1
		if cfg.Overlap < 0 {
			cfg.Overlap = 0
		}
	}
	if cfg.Separator == "" {
		cfg.Separator = "\n"
	}
	if text == "" {
		return nil
	}
	maxSize := cfg.MaxChunkSize
	overlap := cfg.Overlap
	var chunks []Chunk
	start := 0
	for start < len(text) {
		end := start + maxSize
		if end > len(text) {
			end = len(text)
		} else if sep := cfg.Separator; sep != "" {
			sub := text[start:end]
			if li := strings.LastIndex(sub, sep); li >= 0 {
				ne := start + li + len(sep)
				if ne > start {
					end = ne
				}
			}
		}
		chunks = append(chunks, Chunk{
			Text:  text[start:end],
			Start: start,
			End:   end,
			Index: len(chunks),
		})
		if end >= len(text) {
			break
		}
		next := end - overlap
		if next <= start {
			next = start + 1
		}
		start = next
	}
	return chunks
}
