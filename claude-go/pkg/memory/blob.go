package memory

// blob.go —— 记忆的**未截断原文**存储。
//
// 背景: 团队产出的每个阶段在写进记忆之前就被截成几百字符 (摘要), 注入系统提示时
// 又按 4096 再截一刀。摘要足以让模型判断"这条记忆讲的是什么", 不足以让它照着做 ——
// 于是"记住了"和"用得上"之间裂开一道缝。这里把原文留在盘上, 摘要里只放一个句柄,
// 模型要用时再取 (渐进披露: 检索付摘要的钱, 使用才付全文的钱)。
//
// 三条设计约束, 都不是随便定的:
//
//  1. **内容寻址** — 文件名 = 内容哈希。同一条记忆写两次天然落在同一个文件上,
//     不需要去重表, 也不需要知道"以前有没有存过"。
//  2. **不可变** — 写好之后永不修改、永不覆盖成不同内容。这正是摘要可以放心引用
//     它的前提: 记忆条目会被重写 (AccessCount/Touch), 但它指向的原文不会变。
//  3. **不随记忆淘汰** — 删一条记忆不删它的 blob。代价是盘上留孤儿; 换来的是
//     **没有任何跨条目状态**: 不需要引用计数 (计数还要跟 episodic 的落盘时机对齐,
//     那是另一类会静默失配的 bug)。体积由 markdown 文本决定, 不是需要立刻处理的问题。
//
// 目录位置: 与 episodic_memory.json 同级的 blobs/ —— 谁的生命周期长,
// 就跟着谁走。persistPath 为空 (纯内存模式) 时整个机制关闭: 那时连记忆本身都不落盘,
// 留一份原文在盘上反而会制造"记忆没了、引用还在"的假象。

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// blobDirName 相对记忆持久化目录的原文快照目录名。
	blobDirName = "blobs"
	// blobExt 原文一律按 UTF-8 文本存 (记忆内容是模型产出的自然语言)。
	blobExt = ".txt"
	// blobMinRunes 原文至少要**比 Content 长这么多**才值得单独存一份。
	// 阈值定在 64: 短于此的超出通常是头部包装 (团队名/工作流/目标) 的差异,
	// 把这种"原文"存下来只会让模型多跑一次工具却读到几乎一样的东西。
	blobMinRunes = 64
	// blobRefLen 句柄长度 (sha256 十六进制前 32 位 = 128 bit)。
	blobRefLen = 32
	// blobMaxRunes 单份原文的硬上限。防止一次失控的团队产出 (模型循环输出)
	// 把整块盘吃掉。超限即**不存**, 而不是截断存 —— 半截原文比没有原文更坏:
	// 模型会以为拿到的是全文。
	blobMaxRunes = 1_000_000
)

// blobDir 返回快照目录; 未启用持久化时返回空串。
func (s *TieredStore) blobDir() string {
	if s.persistPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.persistPath), blobDirName)
}

// putOriginal 把原文存为快照并返回句柄。任何失败都返回空串 —— 记忆本身照常写入,
// 只是取不回全文。这是刻意的降级方向: 快照是**加分项**, 不能因为它失败而丢记忆。
func (s *TieredStore) putOriginal(original string) string {
	original = strings.TrimSpace(original)
	if original == "" {
		return ""
	}
	if utf8.RuneCountInString(original) > blobMaxRunes {
		return ""
	}
	dir := s.blobDir()
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(original))
	ref := hex.EncodeToString(sum[:])[:blobRefLen]
	path := filepath.Join(dir, ref+blobExt)

	// 已存在就到此为止: 内容寻址意味着同名即同内容, 重写一遍是纯浪费。
	if _, err := os.Stat(path); err == nil {
		return ref
	}
	// 先写临时文件再 rename: 并发 Add 相同内容时, 两个写者各自写各自的临时文件,
	// rename 是原子的 —— 读者永远看不到半截文件。(直接 WriteFile 会让读到写一半的
	// 文件成为可能, 而"原文不可变"这条保证会因此破掉。)
	tmp, err := os.CreateTemp(dir, ref+".tmp-*")
	if err != nil {
		return ""
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(original); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return ""
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return ""
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return ""
	}
	return ref
}

// ReadOriginal 按句柄取回原文。句柄非法、文件不存在或未启用持久化都返回 ok=false。
//
// ref 在校验后才拼路径: 它来自记忆条目 (可能经手过外部存储/文件), 直接拼进去等于
// 让一个 "%2e%2e%2f" 之类的值把读取带出 blobs/ 目录。合法句柄只可能是十六进制。
func (s *TieredStore) ReadOriginal(ref string) (string, bool) {
	if len(ref) != blobRefLen {
		return "", false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	dir := s.blobDir()
	if dir == "" {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(dir, ref+blobExt))
	if err != nil {
		return "", false
	}
	return string(data), true
}
