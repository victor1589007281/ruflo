// qh_crash_test.go — Q-H1：API 接受（用户消息落盘）后进程被 kill -9，
// 用户消息 0 丢失。100 次独立迭代。
//
// # 判据落在磁盘上，不落在返回值上
//
// 这一条的全部意义是"进程没有任何机会执行 defer/收尾"时磁盘上还剩什么。
// 在同一个进程里断言 RunTurn 的返回值、或断言 sink 的内存字段，都测不到
// kill -9 —— 那些内存状态随进程一起消失。所以这里用**子进程模式**：
//
//	父进程（本测试）  起子进程，等子进程把用户消息原子写到 users/<turnID>.json
//	                  并进入模型调用后，SIGKILL。
//	子进程（helper）  走生产构造路径 Runner.RunTurn；模型调用被 FakeModel 的
//	                  StepHang 永远挂住，只可能被父进程杀掉。
//
// 父进程随后**重开目录**，只读磁盘断言：该 TurnID 的用户记录恰好 1 条、
// 字节完整（sha256 与落盘时一致）、内容就是用户输入。
//
// # 为什么等待"模型已开始"而不是只等 ready
//
// ready 由 fs sink 在 EnsureUser 成功返回后写。若父进程看到 ready 立刻杀，
// 一个"先调模型、后落盘"的错误实现也可能碰巧通过（模型调用可能还没到）。
// 子进程在进入模型调用时先确认 ready 存在，不存在就写 order-violation，
// 再写 model-started。父进程等到 model-started 才杀，于是：
//   - 模型确实在跑（不是空转）；
//   - 模型开始之前 EnsureUser 已经完成（ready 已存在）。
//
// 100 次迭代各用独立临时目录，互不污染。
package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/api"
	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/harness/harnesstest"
	"github.com/anthropic/claude-go/pkg/hooks"
	"github.com/anthropic/claude-go/pkg/permissions"
	"github.com/anthropic/claude-go/pkg/prompt"
	"github.com/anthropic/claude-go/pkg/tool"
	"github.com/anthropic/claude-go/pkg/types"
)

// qh1Iterations 是需求写死的迭代次数（100 次 kill -9）。
const qh1Iterations = 100

// qh1TurnCanary 是用户消息正文。用一段独特且含中文的文本，既能证明"是我们
// 写的那条"，也能顺带验证 UTF-8 字节没有被截断在多字节边界上。
const qh1TurnCanary = "QH1-用户消息必须活过 kill -9：请记住这句话"

// qh1AtomicWrite 原子写：同目录临时文件 + rename。
//
// 进程被 kill -9 时，已 rename 的文件是完整的（不存在"半个 JSON"）；
// 未 rename 的临时文件对读者不可见（名字以 .tmp- 开头，且本测试只认
// <turnID>.json）。不依赖 fsync：kill -9 不会丢页缓存，掉电才需要 fsync，
// 而本验收的范围是进程级 kill。
func qh1AtomicWrite(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// qh1FSSink 是本次验收用的最小文件系统 TurnSink。
//
// 它刻意保持"每 turn 一个文件"：重复的 EnsureUser 若真的落到两次写，只会
// 覆盖而不会出现两条 —— 因此父进程的"恰好 1 条"断言配合 IdempotentSink
// （Runner 自动包装）一起读：包装层保证同键只转发一次，本 sink 保证每次
// 写入是原子的。
type qh1FSSink struct {
	root string
}

func (s *qh1FSSink) userPath(turnID string) string {
	return filepath.Join(s.root, "users", turnID+".json")
}

// EnsureUser 原子写用户记录，成功后写 ready 标记（turnID + 落盘字节的 sha256）。
//
// already=true 的路径不重写 ready：正常情况下一个 turn 只走一次这里；
// 若宿主此前已写过，ready 是那条既有记录的凭据，不该被后一次调用覆盖成
// "当前进程认为的字节"。
func (s *qh1FSSink) EnsureUser(_ context.Context, rec UserRecord) (bool, error) {
	p := s.userPath(rec.TurnID)
	if _, err := os.Stat(p); err == nil {
		return true, nil
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	if err := qh1AtomicWrite(p, b); err != nil {
		return false, err
	}
	sum := sha256.Sum256(b)
	ready := fmt.Sprintf("%s\n%x\n", rec.TurnID, sum)
	if err := qh1AtomicWrite(filepath.Join(s.root, "ready"), []byte(ready)); err != nil {
		return false, err
	}
	return false, nil
}

func (s *qh1FSSink) AppendAssistant(_ context.Context, rec AssistantRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return qh1AtomicWrite(filepath.Join(s.root, "assistants", rec.TurnID+".json"), b)
}

func (s *qh1FSSink) MarkTurn(_ context.Context, st TurnState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return qh1AtomicWrite(filepath.Join(s.root, "states", st.TurnID+".json"), b)
}

// TestQH1HelperProcess 是子进程入口。
//
// 只有父进程通过 QH1_HELPER=1 启动它时才做真正的工作；被外层 `go test -run`
// 顺带匹配到时立即返回（否则父进程自己的测试运行会卡住）。
func TestQH1HelperProcess(t *testing.T) {
	if os.Getenv("QH1_HELPER") != "1" {
		return
	}
	root := os.Getenv("QH1_ROOT")
	turnID := os.Getenv("QH1_TURN")
	if root == "" || turnID == "" {
		t.Fatalf("helper 缺少 QH1_ROOT/QH1_TURN")
	}

	// FakeModel 的 Hang 步骤会一直阻塞到请求被取消 —— 本进程只会被 SIGKILL，
	// 所以模型调用在这里"永远不返回"。外层包一层 handler，只为在请求真正
	// 到达时留下磁盘标记。
	fm := harnesstest.New(harnesstest.Hang())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ready := filepath.Join(root, "ready")
		if _, err := os.Stat(ready); err != nil {
			// 模型调用开始时用户消息还没落盘 —— 正是 Q-H1 要抓的顺序错误。
			_ = qh1AtomicWrite(filepath.Join(root, "order-violation"),
				[]byte("model request arrived before EnsureUser persisted the user record"))
		}
		_ = qh1AtomicWrite(filepath.Join(root, "model-started"), []byte("1"))
		fm.Handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	reg := tool.NewRegistry()
	client := &api.Client{
		BaseURL: srv.URL, APIKey: "qh1-key", Model: "qh1-model",
		Client: srv.Client(), RetryCount: 1,
	}
	pm := prompt.NewManager(filepath.Join(root, "prompt"))
	pm.OverridePrompt = "你是执行者"
	eng := engine.NewQueryEngine(
		&engine.Config{Model: "qh1-model", MaxTurns: 2, MaxRunTokens: -1},
		client, reg, hooks.NewRunner(nil, ""),
		permissions.NewChecker(types.PermissionModeAcceptEdits), nil, pm)

	r := NewRunner(RunnerOptions{Engine: eng, Sink: &qh1FSSink{root: root}})
	req := TurnRequest{
		SessionID: "qh1-session",
		TurnID:    turnID,
		Input:     []types.ContentBlock{{Type: types.ContentBlockText, Text: qh1TurnCanary}},
	}
	_, _ = r.RunTurn(context.Background(), req, nil)

	// 走到这里说明模型调用提前返回了（不该发生）。父进程随时会杀掉我们，
	// 阻塞住即可，不要 exit —— exit 会让父进程的 Kill 变成 "process already
	// finished"，掩盖"没进入阻塞模型调用"这一事实。
	select {}
}

// qh1WaitForFile 轮询等待文件出现。返回 false 表示超时。
func qh1WaitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// assertQH1UserRecord 重开目录，只读磁盘断言用户记录。
func assertQH1UserRecord(t *testing.T, root, turnID string) error {
	t.Helper()

	usersDir := filepath.Join(root, "users")
	entries, err := os.ReadDir(usersDir)
	if err != nil {
		return fmt.Errorf("读 users 目录失败: %w", err)
	}
	wantName := turnID + ".json"
	if len(entries) != 1 || entries[0].Name() != wantName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return fmt.Errorf("用户记录应恰好 1 条 (%s), 实际 %v", wantName, names)
	}

	raw, err := os.ReadFile(filepath.Join(usersDir, wantName))
	if err != nil {
		return fmt.Errorf("读用户记录失败: %w", err)
	}

	// ready 里记着落盘时的 sha256：用它证明"读到的是完整的那份字节"，
	// 而不是一个碰巧还能 parse 的前缀。
	readyRaw, err := os.ReadFile(filepath.Join(root, "ready"))
	if err != nil {
		return fmt.Errorf("读 ready 标记失败: %w", err)
	}
	var readyTurn, readySum string
	if _, err := fmt.Sscanf(string(readyRaw), "%s\n%s\n", &readyTurn, &readySum); err != nil {
		return fmt.Errorf("ready 标记格式错误 (%q): %w", string(readyRaw), err)
	}
	if readyTurn != turnID {
		return fmt.Errorf("ready 的 turnID=%q, 期望 %q", readyTurn, turnID)
	}
	sum := sha256.Sum256(raw)
	if got := fmt.Sprintf("%x", sum); got != readySum {
		return fmt.Errorf("用户记录字节不完整: sha256=%s, 落盘时=%s", got, readySum)
	}

	var rec UserRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("用户记录不是完整 JSON: %w (原始 %d 字节)", err, len(raw))
	}
	if rec.TurnID != turnID {
		return fmt.Errorf("TurnID=%q, 期望 %q", rec.TurnID, turnID)
	}
	if rec.SessionID != "qh1-session" {
		return fmt.Errorf("SessionID=%q, 期望 qh1-session", rec.SessionID)
	}
	if len(rec.Blocks) != 1 || rec.Blocks[0].Text != qh1TurnCanary {
		return fmt.Errorf("用户消息内容丢失: %+v", rec.Blocks)
	}
	return nil
}

// TestQH1_CrashKill9NoUserMessageLoss 是 Q-H1 的主验收。
//
// 100 次迭代中每一次：起子进程 → 等它落盘并进入模型调用 → SIGKILL →
// 重开目录断言用户记录恰好 1 条且字节完整。所有迭代都跑完后再汇总，
// 这样报告里给出的是**实际通过数**而不是"第一条失败就停了"。
func TestQH1_CrashKill9NoUserMessageLoss(t *testing.T) {
	parentEnv := os.Environ()
	passed := 0
	failed := 0

	for i := 0; i < qh1Iterations; i++ {
		root := t.TempDir()
		turnID := fmt.Sprintf("qh1-turn-%03d", i)

		cmd := exec.Command(os.Args[0], "-test.run=^TestQH1HelperProcess$")
		cmd.Env = append(append([]string(nil), parentEnv...),
			"QH1_HELPER=1",
			"QH1_ROOT="+root,
			"QH1_TURN="+turnID,
		)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out

		if err := cmd.Start(); err != nil {
			t.Errorf("迭代 %d: 启动 helper 失败: %v", i, err)
			failed++
			continue
		}

		// 等模型调用真正开始（见文件头：这同时钉住了 EnsureUser 先于模型）。
		if !qh1WaitForFile(filepath.Join(root, "model-started"), 20*time.Second) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Errorf("迭代 %d: helper 未进入模型调用: %s", i, out.String())
			failed++
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "order-violation")); err == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Errorf("迭代 %d: 模型调用先于用户消息落盘", i)
			failed++
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "ready")); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Errorf("迭代 %d: 进入模型调用时 ready 不存在 —— EnsureUser 未先落盘", i)
			failed++
			continue
		}

		// SIGKILL：进程拿不到任何收尾机会。
		if err := cmd.Process.Kill(); err != nil {
			t.Errorf("迭代 %d: SIGKILL 失败: %v", i, err)
			_ = cmd.Wait()
			failed++
			continue
		}
		_ = cmd.Wait() // 被信号杀死时返回 error，属于预期

		if err := assertQH1UserRecord(t, root, turnID); err != nil {
			t.Errorf("迭代 %d: %v", i, err)
			failed++
			continue
		}
		passed++
	}

	t.Logf("Q-H1 kill -9 迭代: %d/%d 通过, %d 失败", passed, qh1Iterations, failed)
	if failed > 0 {
		t.Fatalf("Q-H1 失败: %d/%d 通过（用户消息 0 丢失未成立）", passed, qh1Iterations)
	}
}
