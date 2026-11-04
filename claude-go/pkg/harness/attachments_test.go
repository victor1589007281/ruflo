// attachments_test.go — R-H5 附件元数据的契约测试。
//
// 钉的是三件事，对应设计文档 §4 R-H5 的三条验收：
//  1. 同一文件两次上传只产生一个 blob 引用（内容寻址 + Put 幂等）；
//  2. 重启后附件引用仍可解析（引用只带 hash，字节在盘上）；
//  3. 转录里不出现完整 base64（引用是元数据，不是内容）。
package harness

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAttach_SameContentSameHash 是验收 1 的核心：两次上传同一份字节，
// hash 与 ID 必须完全相同 —— 这就是"只产生一个 blob 引用"。
func TestAttach_SameContentSameHash(t *testing.T) {
	store, err := NewFSBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("这是一份 PDF 的内容（测试用）")

	a1, err := Attach(store, AttachmentPDF, "application/pdf", "合同.pdf", content)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := Attach(store, AttachmentPDF, "application/pdf", "合同副本.pdf", content)
	if err != nil {
		t.Fatal(err)
	}

	if a1.BlobHash != a2.BlobHash {
		t.Errorf("同一内容应得到同一 BlobHash: %q vs %q", a1.BlobHash, a2.BlobHash)
	}
	if a1.ID != a2.ID {
		t.Errorf("同一内容应得到同一 ID: %q vs %q", a1.ID, a2.ID)
	}
	if a1.BlobHash != ContentHash(content) {
		t.Errorf("BlobHash 必须等于 ContentHash: %q vs %q", a1.BlobHash, ContentHash(content))
	}
}

// TestFSBlobStore_PutIsIdempotentOnDisk 钉住"只存一份"是**真的**只落盘一次，
// 不是"hash 相同但盘上写了两份"。
//
// 光比较 hash 不够：一个每次都写新文件的实现同样返回相同 hash，磁盘却在膨胀。
// 所以这里看 PutCount 与目录里的实际文件数。
func TestFSBlobStore_PutIsIdempotentOnDisk(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFSBlobStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("A"), 4096)

	for i := 0; i < 5; i++ {
		if _, err := store.Put(content); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.PutCount(); got != 1 {
		t.Errorf("同一内容 Put 5 次应只真正落盘 1 次, 实际 %d 次", got)
	}

	// 直接数盘上的文件（排除临时文件）。
	files := 0
	err = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() && !strings.HasPrefix(fi.Name(), ".tmp-") {
			files++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Errorf("盘上应只有 1 个 blob 文件, 实际 %d", files)
	}
}

// TestFSBlobStore_SurvivesRestart 是验收 2：新建一个 store 实例（等价于进程重启）
// 指向同一目录，旧引用必须仍能解析。
func TestFSBlobStore_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFSBlobStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("重启前写入的附件内容")
	ref, err := Attach(store, AttachmentImage, "image/png", "photo.png", content)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟重启：全新的 store 对象，只有目录是共享的。
	restarted, err := NewFSBlobStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Get(ref.BlobHash)
	if err != nil {
		t.Fatalf("重启后按引用解析失败: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("重启后取回的字节与写入不一致: %q", got)
	}
}

// TestAttachmentRef_TranscriptCarriesNoBase64 是验收 3：
// 引用里不得出现完整 base64 内容。
//
// 用一段能被 base64 明显识别的字节（重复的 "QUJD" 解码自 "ABC"），
// 检查引用序列化后既不含原文字节，也不含其 base64 形态。
func TestAttachmentRef_TranscriptCarriesNoBase64(t *testing.T) {
	store, err := NewFSBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("ABC"), 1000) // 3000 字节，够长以便识别
	ref, err := Attach(store, AttachmentImage, "image/png", "big.png", content)
	if err != nil {
		t.Fatal(err)
	}

	encoded := base64.StdEncoding.EncodeToString(content)
	// 取中段一小节做子串检查：整段太长，用中间 60 字符就足以判定"内联了内容"。
	probe := encoded[len(encoded)/2 : len(encoded)/2+60]

	rendered := ref.ID + ref.Kind + ref.MIME + ref.BlobHash + ref.Name
	if strings.Contains(rendered, probe) {
		t.Error("AttachmentRef 里出现了完整 base64 内容 —— 转录会随轮数线性膨胀")
	}
	if len(ref.BlobHash) > 128 {
		t.Errorf("BlobHash 不该是内容本身 (长度 %d)", len(ref.BlobHash))
	}
	// 反向确认：内容确实在 store 里能取回（否则"不含 base64"是靠丢内容实现的）。
	got, err := store.Get(ref.BlobHash)
	if err != nil || !bytes.Equal(got, content) {
		t.Errorf("blob 应可完整取回: err=%v", err)
	}
}

// TestFSBlobStore_MissingBlobIsTyped 钉住"取不到"是 typed error。
//
// 业务层要靠 errors.Is 分流：引用失效 -> 让用户重传；存储故障 -> 重试。
// 一个裸字符串错误让这两种情形无法区分。
func TestFSBlobStore_MissingBlobIsTyped(t *testing.T) {
	store, err := NewFSBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 一个合法形状但从未写入的 hash。
	_, err = store.Get(strings.Repeat("ab", 32))
	if !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("未命中应返回 ErrBlobNotFound, 实际 %v", err)
	}
	if _, err := store.Size(strings.Repeat("ab", 32)); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("Size 未命中应返回 ErrBlobNotFound, 实际 %v", err)
	}
}

// TestFSBlobStore_RejectsPathTraversalHash 钉住 hash 校验。
//
// AttachmentRef 可能从库里读回来（业务层存的），不能假设它是本进程写的。
// 一个 "../.." 形态的 hash 若不校验就会变成路径穿越读取。
//
// 判据不能只是"返回了 error" —— 路径拼错时读一个不存在的文件同样返回 error,
// 那样的断言在**删掉校验之后依然通过**（已实测：去掉 isHexHash 后本测试仍绿）。
// 真正要钉的是"解析出的路径仍在存储目录内", 所以这里直接看 path(), 并在
// 存储目录外**放一个真的可读文件**, 确认 store 既读不到它、也解析不到它。
func TestFSBlobStore_RejectsPathTraversalHash(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "blobs") // 存储目录故意放深一层, 便于向外穿越
	store, err := NewFSBlobStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 在存储目录外放一个真实可读的文件。若发生穿越, 它会被读到。
	outside := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	// pathNoPanic 把 subject 的 panic 变成"(路径, 错误)"。
	//
	// 空 hash 在无校验的实现里会 [ :2] 越界 panic。panic 本身也是缺陷, 但如果
	// 让它逃出去, 循环就在第一个坏输入处中断, 后面的用例一个都跑不到 —— 诊断
	// 信息会从"7 个坏输入"缩成"1 个坏输入"。
	pathNoPanic := func(hash string) (p string, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return store.path(hash)
	}

	for _, bad := range []string{
		"../secret.txt", "../../etc/passwd", "..", "ab/cd", "", "zzzz",
		filepath.Join("..", "secret.txt"),
	} {
		p, perr := pathNoPanic(bad)
		if perr != nil {
			// 报错是预期结果。但"报错"必须来自校验, 不是来自空 hash 的越界 panic。
			if strings.HasPrefix(perr.Error(), "panic:") {
				t.Errorf("非法 hash %q 让 subject panic 了, 应当在校验处返回错误: %v", bad, perr)
			}
		}
		if perr == nil {
			// 没报错就必须证明路径还在存储目录内。
			rel, rerr := filepath.Rel(dir, p)
			if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Errorf("非法 hash %q 解析出目录外路径 %q —— 这是路径穿越", bad, p)
			}
		}
		// 两条取数路径都必须拒绝。同样把 panic 收成错误, 否则一个坏输入就
		// 中断整个循环。
		got, gerr := func() (b []byte, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			return store.Get(bad)
		}()
		if gerr == nil {
			t.Errorf("非法 hash %q 应被拒绝, 却返回了 %d 字节", bad, len(got))
		} else if strings.HasPrefix(gerr.Error(), "panic:") {
			t.Errorf("非法 hash %q 让 Get panic 了, 应当在校验处返回错误: %v", bad, gerr)
		}
	}
}

// TestFSBlobStore_TruncatedBlobIsRewritten 钉住 Put 的短路判据是"字节数相同"
// 而不只是"文件存在"。
//
// 场景: 上一次写到一半崩溃留下截断文件。若只判存在性，这个损坏 blob 会被
// 永久当作完好的 —— 之后每次 Get 都拿到半截内容，且没有任何报错。
func TestFSBlobStore_TruncatedBlobIsRewritten(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFSBlobStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("X"), 1000)
	hash := ContentHash(content)

	// 手工放一个截断的文件（模拟写到一半崩溃）。
	p, err := store.path(hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content[:100], 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Put(content); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(hash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("截断的 blob 应被完整重写, 实际长度 %d 期望 %d", len(got), len(content))
	}
}
