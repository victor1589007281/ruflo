package embeddings

import "strings"

// ChunkConfig controls text splitting for embedding pipelines.
type ChunkConfig struct {
	MaxChunkSize int
	Overlap      int
	Separator    string
}

// Chunk is one slice of source text with byte offsets into the original UTF-8 string.
type Chunk struct {
	Text  string
	Start int
	End   int
	Index int
}

// DefaultChunkConfig returns MaxChunkSize=512, Overlap=64, Separator="\n".
func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{MaxChunkSize: 512, Overlap: 64, Separator: "\n"}
}

// ChunkText splits text into overlapping chunks. Indices are byte offsets.
// When a full MaxChunkSize window fits inside the string, the end may snap
// forward to the last Separator within the window (if any).
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
