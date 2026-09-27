package feishu

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropic/claude-go/pkg/engine"
	"github.com/anthropic/claude-go/pkg/sessionapi"
)

// session_spec_test.go — 会话身份声明的两处安全边界。
//
// 这一组只验两件事, 但两件都是"错了不报错"的那一类:
//  1. 白名单的三态经**落盘-读回**之后不被放宽 (全拒 → 不限 是最危险的方向)
//  2. 会话 id 不能逃出 <state>/sessions/ (id 来自 HTTP 请求体, 直接参与文件路径)

func TestValidSessionIDRejectsPathEscapes(t *testing.T) {
	bad := []string{
		"../teams/x",
		"..",
		".",
		"a/b",
		`a\b`,
		".hidden",
		"",
		"a b",
		"会话一", // 非 ASCII: 会被不同文件系统归一化成不同名字, 不做标识
		"-leading",
		"a\x00b",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 65 字符
	}
	for _, id := range bad {
		if ValidSessionID(id) {
			t.Errorf("id %q 应被拒绝 (它能逃出 sessions 目录, 或在不同文件系统上不可辨)", id)
		}
	}
	good := []string{"dba-1", "ops.lead", "s_1", "Ab9", "a-b_c.d"}
	for _, id := range good {
		if !ValidSessionID(id) {
			t.Errorf("id %q 应被接受", id)
		}
	}
}

// TestSpecStoreRoundTripsToolThreeState 是这一组的核心: 落盘-读回**不得放宽**。
//
// 判别力: 把 Spec.AllowedTools 从 *[]string 改回 []string (带 omitempty),
// "全拒" 那一例会红 —— 空切片被序列化掉, 读回变成 nil = 不限。
func TestSpecStoreRoundTripsToolThreeState(t *testing.T) {
	dir := t.TempDir()
	store := newSpecStore(dir, "")

	in := sessionapi.Spec{ID: "dba-1", AllowedTools: sessionapi.ToolsDenyAll()}
	if err := store.put(in); err != nil {
		t.Fatalf("put: %v", err)
	}

	// 关键: **换一个 store 实例**读 —— 同一个实例会命中内存缓存, 那样验的是
	// "缓存里对不对", 而不是"盘上写对了没有"。重启后走的正是新实例这条路径。
	fresh := newSpecStore(dir, "")
	got, ok := fresh.get("dba-1")
	if !ok {
		t.Fatal("新实例读不回会话 —— spec 根本没落盘")
	}
	if got.AllowedTools == nil {
		t.Fatalf("「全拒」经一次落盘-读回变成了「不限」(AllowedTools=%v)\n"+
			"★ 这是这个字段最危险的一种退化: 声明越严, 重启后越松, 且无提示", *got.AllowedTools)
	}
	if len(*got.AllowedTools) != 0 {
		t.Errorf("全拒应保持为空, 得到 %v", *got.AllowedTools)
	}

	// 反向对照: "不限" 不能被读成 "全拒" —— 那会把一个正常会话变成什么都不能做,
	// 症状 (工具全部拒绝) 与"配置写错了"一样, 同样难归因。
	store2 := newSpecStore(dir, "")
	_ = store2.put(sessionapi.Spec{ID: "dba-2"})
	fresh2 := newSpecStore(dir, "")
	got2, _ := fresh2.get("dba-2")
	if got2.AllowedTools != nil {
		t.Errorf("「不限」经往返变成了白名单: %v", *got2.AllowedTools)
	}
}

func TestSpecStoreDeleteIsDurable(t *testing.T) {
	dir := t.TempDir()
	store := newSpecStore(dir, "")
	_ = store.put(sessionapi.Spec{ID: "dba-1"})

	if _, err := os.Stat(filepath.Join(dir, "sessions", "dba-1.json")); err != nil {
		t.Fatalf("spec 文件应落在 <state>/sessions/ 下: %v", err)
	}
	if err := store.delete("dba-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// 反向对照: 新实例也读不到 (证明删的是盘, 不只是内存缓存)。
	if _, ok := newSpecStore(dir, "").get("dba-1"); ok {
		t.Error("删掉的会话在新实例里又回来了 —— 只删了内存缓存")
	}
	// 删不存在的**不得报错**: Delete 是幂等的, 上层可能重试。
	if err := store.delete("从没存在过"); err != nil {
		t.Errorf("删不存在的会话应幂等成功, 得到 %v", err)
	}
}

// TestApplySpecToWhitelistIsFailClosed —
//
// 守的是"spec 的白名单真的落进了 engine.Config, 且空数组是**全拒**"。
// 引擎侧那条规则 (engine.go:335) 已经是对的, 这里验的是我们**喂对了**。
func TestApplySpecToWhitelistIsFailClosed(t *testing.T) {
	// 具名白名单
	cfg := &engine.Config{}
	applySpecTo(cfg, sessionapi.Spec{AllowedTools: sessionapi.ToolsAllow("Bash")})
	if cfg.AllowedTools == nil || !cfg.AllowedTools["Bash"] || len(cfg.AllowedTools) != 1 {
		t.Fatalf("白名单没落进 cfg: %v", cfg.AllowedTools)
	}

	// 空数组 = 全拒 (非 nil 的空 map)
	cfg2 := &engine.Config{}
	applySpecTo(cfg2, sessionapi.Spec{AllowedTools: sessionapi.ToolsDenyAll()})
	if cfg2.AllowedTools == nil {
		t.Fatal("「全拒」被写成了 nil —— 引擎侧 nil 的含义是**不限**, 方向正好相反")
	}
	if len(cfg2.AllowedTools) != 0 {
		t.Errorf("「全拒」应得到一个空 map, 得到 %v", cfg2.AllowedTools)
	}

	// 字段缺席 = 不动 (沿用引擎默认)
	cfg3 := &engine.Config{AllowedTools: map[string]bool{"Existing": true}}
	applySpecTo(cfg3, sessionapi.Spec{})
	if !cfg3.AllowedTools["Existing"] {
		t.Error("spec 没声明白名单时不该改动引擎已有的设置")
	}
}

// TestApplySpecToExpandsHome 守 ~ 展开。
//
// 为什么这条值得一测: Cwd 来自 HTTP 请求体, 而 `~` 是 shell 的语法 —— 一个不经
// shell 的进程会把它当字面量, 于是工具在一个叫 "~" 的不存在目录里跑。报出来的错
// 与权限、与配置都无关, 极难归因。
func TestApplySpecToExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("环境没有可用的 home 目录")
	}
	cfg := &engine.Config{}
	applySpecTo(cfg, sessionapi.Spec{Cwd: "~/work"})
	want := filepath.Join(home, "work")
	if cfg.Cwd != want {
		t.Errorf("~/work 应展开成 %q, 得到 %q", want, cfg.Cwd)
	}

	// 反向对照: 本来就是绝对路径的不该被动过。
	cfg2 := &engine.Config{}
	applySpecTo(cfg2, sessionapi.Spec{Cwd: "/opt/data"})
	if cfg2.Cwd != "/opt/data" {
		t.Errorf("绝对路径不该被改写, 得到 %q", cfg2.Cwd)
	}
}

// TestSpecStringDistinguishesDenyAllFromUnlimited —
//
// 日志里 "全拒" 与 "不限" 必须印得不一样。写成同一个 (比如空字符串), 一次配错的
// 白名单在日志上与正常配置完全相同 —— 而这两个的后果差着一个数量级。
func TestSpecStringDistinguishesDenyAllFromUnlimited(t *testing.T) {
	unlimited := specString(sessionapi.Spec{ID: "a"})
	denyAll := specString(sessionapi.Spec{ID: "a", AllowedTools: sessionapi.ToolsDenyAll()})
	whitelist := specString(sessionapi.Spec{ID: "a", AllowedTools: sessionapi.ToolsAllow("Bash")})

	if unlimited == denyAll {
		t.Fatalf("「不限」与「全拒」在日志里长得一样: %q", unlimited)
	}
	if !contains(denyAll, "全拒") {
		t.Errorf("「全拒」应在日志里明说, 得到 %q", denyAll)
	}
	if !contains(whitelist, "Bash") {
		t.Errorf("白名单应列出工具名, 得到 %q", whitelist)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// TestSessionIDInSpecIsWhatGetsPersisted —
//
// 挡的是"HTTP 传的 id"与"盘上的文件名"分叉。若落盘时对 id 做了转义, List 回来的
// id 就与请求时的不同, 而调用方会拿那个 id 去发消息 → 404, 且看不出为什么。
func TestSessionIDInSpecIsWhatGetsPersisted(t *testing.T) {
	dir := t.TempDir()
	store := newSpecStore(dir, "")
	const id = "ops.lead-1"
	_ = store.put(sessionapi.Spec{ID: id, Name: "运维组长"})

	raw, err := os.ReadFile(filepath.Join(dir, "sessions", id+".json"))
	if err != nil {
		t.Fatalf("文件名应就是 id 本身: %v", err)
	}
	var back sessionapi.Spec
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if back.ID != id {
		t.Errorf("盘上的 id (%q) 与请求的 id (%q) 不一致", back.ID, id)
	}
}
