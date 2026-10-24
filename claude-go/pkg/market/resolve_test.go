package market

import (
	"context"
	"testing"
)

// TestResolverCodes 覆盖任务要求的全部代码写法。
func TestResolverCodes(t *testing.T) {
	r := NewResolver()
	cases := []struct {
		in        string
		canonical string
		market    Market
		currency  string
		tz        string
	}{
		{"600519", "SH:600519", MarketCN, "CNY", "Asia/Shanghai"},
		{"SH600519", "SH:600519", MarketCN, "CNY", "Asia/Shanghai"},
		{"sh.600519", "SH:600519", MarketCN, "CNY", "Asia/Shanghai"},
		{"SH:600519", "SH:600519", MarketCN, "CNY", "Asia/Shanghai"},
		{"贵州茅台", "SH:600519", MarketCN, "CNY", "Asia/Shanghai"},
		{"茅台", "SH:600519", MarketCN, "CNY", "Asia/Shanghai"},
		{"000001", "SZ:000001", MarketCN, "CNY", "Asia/Shanghai"},
		{"SH:000001", "SH:000001", MarketCN, "CNY", "Asia/Shanghai"},
		{"300750", "SZ:300750", MarketCN, "CNY", "Asia/Shanghai"},
		{"688981", "SH:688981", MarketCN, "CNY", "Asia/Shanghai"},
		{"830799", "BJ:830799", MarketCN, "CNY", "Asia/Shanghai"},
		{"430047", "BJ:430047", MarketCN, "CNY", "Asia/Shanghai"},
		{"00700", "HK:00700", MarketHK, "HKD", "Asia/Hong_Kong"},
		{"HK00700", "HK:00700", MarketHK, "HKD", "Asia/Hong_Kong"},
		{"hk.00700", "HK:00700", MarketHK, "HKD", "Asia/Hong_Kong"},
		{"腾讯控股", "HK:00700", MarketHK, "HKD", "Asia/Hong_Kong"},
		{"AAPL", "US:AAPL", MarketUS, "USD", "America/New_York"},
		{"aapl", "US:AAPL", MarketUS, "USD", "America/New_York"},
		{"US:AAPL", "US:AAPL", MarketUS, "USD", "America/New_York"},
		{"苹果", "US:AAPL", MarketUS, "USD", "America/New_York"},
		{"BTC", "CRYPTO:BTC", MarketCrypto, "USD", "UTC"},
		{"CRYPTO:ETH", "CRYPTO:ETH", MarketCrypto, "USD", "UTC"},
	}
	for _, c := range cases {
		inst, err := r.Resolve(context.Background(), c.in)
		if err != nil {
			t.Errorf("Resolve(%q) error: %v", c.in, err)
			continue
		}
		if inst.Canonical != c.canonical || inst.Market != c.market ||
			inst.Currency != c.currency || inst.Timezone != c.tz {
			t.Errorf("Resolve(%q) = %+v, want %s/%s/%s/%s",
				c.in, inst, c.canonical, c.market, c.currency, c.tz)
		}
	}
}

// TestResolverAmbiguous 验证歧义名称不静默取第一个。
func TestResolverAmbiguous(t *testing.T) {
	r := NewResolver()
	cands := r.Candidates(context.Background(), "平安")
	if len(cands) < 2 {
		t.Fatalf("Candidates(平安) = %d, want >=2", len(cands))
	}
	if _, err := r.Resolve(context.Background(), "平安"); err == nil {
		t.Fatal("Resolve(平安) 应当返回歧义错误")
	} else if KindOf(err) != ErrUnresolved {
		t.Fatalf("KindOf(err) = %s, want %s", KindOf(err), ErrUnresolved)
	}
}

// TestResolverUnresolved 验证不可识别输入返回 ErrUnresolved。
func TestResolverUnresolved(t *testing.T) {
	r := NewResolver()
	for _, in := range []string{"", "!!!", "1234567", "ABCDEFG", "SZ:600519", "SH:300750"} {
		if _, err := r.Resolve(context.Background(), in); err == nil {
			t.Errorf("Resolve(%q) 应当失败", in)
		} else if KindOf(err) != ErrUnresolved {
			t.Errorf("Resolve(%q) kind = %s, want %s", in, KindOf(err), ErrUnresolved)
		}
	}
}

// TestResolverIndexCodes 验证显式前缀下的指数代码。
func TestResolverIndexCodes(t *testing.T) {
	r := NewResolver()
	for _, in := range []string{"SH:000001", "SH:000300", "SZ:399001"} {
		if _, err := r.Resolve(context.Background(), in); err != nil {
			t.Errorf("Resolve(%q) error: %v", in, err)
		}
	}
}

// TestResolverCandidatesSingle 验证确定性代码只产生一个候选。
func TestResolverCandidatesSingle(t *testing.T) {
	r := NewResolver()
	for _, in := range []string{"600519", "00700", "AAPL", "SH:000001"} {
		cands := r.Candidates(context.Background(), in)
		if len(cands) != 1 {
			t.Errorf("Candidates(%q) = %d, want 1", in, len(cands))
		}
	}
}

// TestResolverCanceledContext 验证上下文取消被归一化为超时错误。
func TestResolverCanceledContext(t *testing.T) {
	r := NewResolver()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Resolve(ctx, "600519"); err == nil {
		t.Fatal("已取消的 context 应当失败")
	} else if KindOf(err) != ErrTimeout {
		t.Fatalf("KindOf(err) = %s, want %s", KindOf(err), ErrTimeout)
	}
}
