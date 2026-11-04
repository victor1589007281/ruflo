// attachments.go — 附件引用与 blob 去重（Moqi R-H5）。
//
// # 要解决的问题
//
// 改造前 harness 对附件**只做模态校验**：`AttachmentRef` 从 TurnRequest 原样传进
// sink，谁也不保证"同一文件两次上传只有一份 blob"，也不保证"重启后那个引用还能
// 解析回内容"。业务层如果自己不做去重，两次上传同一张照片就在库里存了两份；
// 如果自己把 base64 内联进转录，转录体积会随对话轮数线性膨胀，且每次请求都要
// 重传整张图。
//
// 这里补三件事（对应 R-H5 的三条验收）：
//  1. **内容寻址**：BlobHash 由内容算出，同一文件两次上传得到同一个 hash；
//  2. **只存一份**：Put 按 hash 落盘，重复 Put 是幂等短路，不新增字节；
//  3. **可解析**：引用只带 hash 与元数据，内容在需要时按 hash 取回 —— 所以
//     重启后引用仍然有效，转录里也永远不出现完整 base64。
//
// # 边界
//
// 这里**不解析** PDF/OCR/ASR，只按字节存取。解析是业务层或工具层的事，
// 塞进 harness 会让它变成文档处理框架。
package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrBlobNotFound 表示按 hash 取不到 blob。
//
// 业务层要能用 errors.Is 区分"这个引用失效了"与"存储层坏了"：
// 前者该让用户重传，后者该重试。
var ErrBlobNotFound = errors.New("harness: 附件 blob 不存在")

// BlobStore 是附件的字节存储。按内容 hash 寻址，天然去重。
//
// 实现必须满足：Put 幂等（同 hash 重复写入不新增字节）、Get 对已 Put 过的
// hash 在**重启后**仍可解析。FSBlobStore 满足这两条。
type BlobStore interface {
	// Put 存入字节并返回内容 hash。重复内容返回同一个 hash 且不重复占用空间。
	Put(content []byte) (hash string, err error)
	// Get 按 hash 取回字节。不存在时返回 ErrBlobNotFound。
	Get(hash string) ([]byte, error)
	// Size 返回该 hash 的字节数；不存在时返回 (0, ErrBlobNotFound)。
	Size(hash string) (int64, error)
}

// ContentHash 计算内容 hash，与 FSBlobStore.Put 用的是同一套口径。
//
// 导出是因为业务层在**上传时**就要拿到 hash 来构造 AttachmentRef，
// 而那时内容可能还没经 harness。两处各写一套哈希算法会让引用永远对不上。
func ContentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// FSBlobStore 是文件系统实现：一 blob 一文件，按 hash 的前两位做子目录。
//
// 分两级目录的理由是避免单目录条目过多（十万级小文件时 ext4 的 lookup 会变慢），
// 而两级已经够用：256 个子目录 × 每个若干千文件。
type FSBlobStore struct {
	dir string

	// mu 保护 putCount 与 size。读取走文件系统不需要锁（内容寻址意味着
	// 同名文件内容必然相同，并发写同名文件是安全的幂等操作）。
	mu       sync.Mutex
	putCount int   // 真正落盘的次数（幂等短路不计）
	size     int64 // 已占用的字节总数
}

// NewFSBlobStore 在 dir 下建立 blob 存储。目录不存在会自动创建。
func NewFSBlobStore(dir string) (*FSBlobStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("harness: blob 存储目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("harness: 创建 blob 目录失败: %w", err)
	}
	return &FSBlobStore{dir: dir}, nil
}

// path 返回某 hash 的落盘路径。
//
// hash 只取前两位做子目录；hash 本身必须是十六进制，否则它来自外部输入（
// 例如一条被篡改的引用），拼进路径就成了路径穿越。这里显式校验而不是
// 依赖调用方 —— 引用可能从库里读回来，不能假设它是自己写的。
func (s *FSBlobStore) path(hash string) (string, error) {
	if !isHexHash(hash) {
		return "", fmt.Errorf("%w: 非法 hash %q", ErrBlobNotFound, hash)
	}
	return filepath.Join(s.dir, hash[:2], hash), nil
}

// isHexHash 校验 hash 是大小写十六进制且长度合理。
func isHexHash(h string) bool {
	if len(h) < 4 || len(h) > 128 {
		return false
	}
	for _, r := range h {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// Put 存入字节并返回 hash。
//
// 幂等短路的判据是"文件已存在且字节数相同"，而不是"文件已存在"：
// 内容寻址下 hash 相同即内容相同，但磁盘上可能有上一次写到一半留下的
// 截断文件。只看存在性会把损坏的 blob 当成已存好的，之后每次 Get 都拿到
// 半截内容而没有任何报错。
func (s *FSBlobStore) Put(content []byte) (string, error) {
	hash := ContentHash(content)
	p, err := s.path(hash)
	if err != nil {
		return "", err
	}
	if fi, statErr := os.Stat(p); statErr == nil && fi.Size() == int64(len(content)) {
		return hash, nil // 已有同样的字节，短路
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fmt.Errorf("harness: 创建 blob 子目录失败: %w", err)
	}
	// 先写临时文件再 rename：直接写目标路径时，中途崩溃会留下截断文件，
	// 而截断文件在内容寻址下会被下一次 Put 的"已存在"判断误认为完整。
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("harness: 创建临时 blob 失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里是空操作
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return "", fmt.Errorf("harness: 写 blob 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("harness: 关闭临时 blob 失败: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return "", fmt.Errorf("harness: 落盘 blob 失败: %w", err)
	}

	s.mu.Lock()
	s.putCount++
	s.size += int64(len(content))
	s.mu.Unlock()
	return hash, nil
}

// Get 按 hash 取回内容。
func (s *FSBlobStore) Get(hash string) ([]byte, error) {
	p, err := s.path(hash)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, hash)
		}
		return nil, fmt.Errorf("harness: 读取 blob 失败: %w", err)
	}
	return b, nil
}

// Size 返回 blob 字节数。
func (s *FSBlobStore) Size(hash string) (int64, error) {
	p, err := s.path(hash)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("%w: %s", ErrBlobNotFound, hash)
		}
		return 0, fmt.Errorf("harness: stat blob 失败: %w", err)
	}
	return fi.Size(), nil
}

// PutCount 返回真正落盘过多少次（幂等短路不计）。测试与观测用。
func (s *FSBlobStore) PutCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putCount
}

// Attach 把字节存入 blobStore 并产出 AttachmentRef。
//
// 这是业务层构造附件引用的**唯一推荐入口**：hash 由内容算、字节落盘、
// 引用里只有 hash 与元数据。手工拼 AttachmentRef 而不落盘是最容易犯的错 ——
// 引用看起来合法，直到要解析时才发现库里只有一条空引用。
func Attach(store BlobStore, kind, mime, name string, content []byte) (AttachmentRef, error) {
	if store == nil {
		return AttachmentRef{}, errors.New("harness: blob 存储为空")
	}
	hash, err := store.Put(content)
	if err != nil {
		return AttachmentRef{}, err
	}
	return AttachmentRef{
		// ID 与 hash 分开是因为同一 blob 可能在会话里被引用多次、每次有不同
		// 的 Anchors（比如同一份合同在不同 turn 里指向不同条款）。
		ID:       fmt.Sprintf("att-%s", hash[:12]),
		Kind:     kind,
		MIME:     mime,
		BlobHash: hash,
		Name:     name,
	}, nil
}
