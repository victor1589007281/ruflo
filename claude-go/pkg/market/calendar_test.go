package market

import (
	"testing"
	"time"
)

// mustLoc 加载测试时区, 失败直接终止。
func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%s): %v", name, err)
	}
	return loc
}

// TestIsTradingDayCN 覆盖 2025–2026 A 股节假日与周末。
func TestIsTradingDayCN(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	holidays := []time.Time{
		time.Date(2025, 1, 1, 10, 0, 0, 0, sh),  // 元旦
		time.Date(2025, 1, 28, 10, 0, 0, 0, sh), // 春节
		time.Date(2025, 2, 4, 10, 0, 0, 0, sh),  // 春节最后一天
		time.Date(2025, 4, 4, 10, 0, 0, 0, sh),  // 清明
		time.Date(2025, 5, 5, 10, 0, 0, 0, sh),  // 劳动节
		time.Date(2025, 6, 2, 10, 0, 0, 0, sh),  // 端午
		time.Date(2025, 10, 8, 10, 0, 0, 0, sh), // 国庆
		time.Date(2026, 2, 17, 10, 0, 0, 0, sh), // 2026 春节
		time.Date(2026, 10, 7, 10, 0, 0, 0, sh), // 2026 国庆
		time.Date(2025, 6, 7, 10, 0, 0, 0, sh),  // 周六
	}
	for _, d := range holidays {
		if IsTradingDay(MarketCN, d) {
			t.Errorf("%s 应为 A 股非交易日", d.Format("2006-01-02 Mon"))
		}
	}
	trading := []time.Time{
		time.Date(2025, 6, 3, 10, 0, 0, 0, sh),
		time.Date(2025, 9, 30, 10, 0, 0, 0, sh),
		time.Date(2026, 9, 30, 10, 0, 0, 0, sh),
	}
	for _, d := range trading {
		if !IsTradingDay(MarketCN, d) {
			t.Errorf("%s 应为 A 股交易日", d.Format("2006-01-02 Mon"))
		}
	}
}

// TestIsTradingDayHK 覆盖港股主要假日。
func TestIsTradingDayHK(t *testing.T) {
	hk := mustLoc(t, "Asia/Hong_Kong")
	for _, d := range []time.Time{
		time.Date(2025, 1, 29, 10, 0, 0, 0, hk), // 农历新年
		time.Date(2025, 4, 18, 10, 0, 0, 0, hk), // 耶稣受难日
		time.Date(2025, 12, 25, 10, 0, 0, 0, hk),
		time.Date(2026, 2, 18, 10, 0, 0, 0, hk),
	} {
		if IsTradingDay(MarketHK, d) {
			t.Errorf("%s 应为港股非交易日", d.Format("2006-01-02"))
		}
	}
	if !IsTradingDay(MarketHK, time.Date(2025, 6, 3, 10, 0, 0, 0, hk)) {
		t.Error("2025-06-03 应为港股交易日")
	}
}

// TestLastTradingDayFallback 验证非交易日回退与跨长假。
func TestLastTradingDayFallback(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	got := LastTradingDay(MarketCN, time.Date(2026, 10, 4, 12, 0, 0, 0, sh))
	want := time.Date(2026, 9, 30, 0, 0, 0, 0, sh)
	if !got.Equal(want) {
		t.Errorf("LastTradingDay = %s, want %s", got.Format("2006-01-02"), want.Format("2006-01-02"))
	}
	// 周六回退到周五
	got = LastTradingDay(MarketCN, time.Date(2025, 6, 28, 12, 0, 0, 0, sh))
	want = time.Date(2025, 6, 27, 0, 0, 0, 0, sh)
	if !got.Equal(want) {
		t.Errorf("周六回退 = %s, want %s", got.Format("2006-01-02"), want.Format("2006-01-02"))
	}
}

// TestSessionOf 验证各市场本地时段判定与时区换算。
func TestSessionOf(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	hk := mustLoc(t, "Asia/Hong_Kong")
	ny := mustLoc(t, "America/New_York")

	cases := []struct {
		name string
		m    Market
		at   time.Time
		want Session
	}{
		{"CN 盘前", MarketCN, time.Date(2025, 6, 3, 9, 20, 0, 0, sh), SessionPre},
		{"CN 上午", MarketCN, time.Date(2025, 6, 3, 10, 0, 0, 0, sh), SessionRegular},
		{"CN 午休", MarketCN, time.Date(2025, 6, 3, 12, 0, 0, 0, sh), SessionClosed},
		{"CN 下午", MarketCN, time.Date(2025, 6, 3, 14, 0, 0, 0, sh), SessionRegular},
		{"CN 盘后", MarketCN, time.Date(2025, 6, 3, 15, 10, 0, 0, sh), SessionPost},
		{"CN 收盘瞬间", MarketCN, time.Date(2025, 6, 3, 15, 0, 0, 0, sh), SessionClosed},
		{"CN 夜间", MarketCN, time.Date(2025, 6, 3, 20, 0, 0, 0, sh), SessionClosed},
		{"CN 周末", MarketCN, time.Date(2025, 6, 7, 10, 0, 0, 0, sh), SessionClosed},
		{"CN UTC 换算", MarketCN, time.Date(2025, 6, 3, 2, 0, 0, 0, time.UTC), SessionRegular},
		{"HK 上午", MarketHK, time.Date(2025, 6, 3, 10, 0, 0, 0, hk), SessionRegular},
		{"HK 午休", MarketHK, time.Date(2025, 6, 3, 12, 30, 0, 0, hk), SessionClosed},
		{"HK 下午", MarketHK, time.Date(2025, 6, 3, 14, 0, 0, 0, hk), SessionRegular},
		{"HK 收市竞价", MarketHK, time.Date(2025, 6, 3, 16, 5, 0, 0, hk), SessionPost},
		{"US 盘前", MarketUS, time.Date(2025, 6, 3, 8, 0, 0, 0, ny), SessionPre},
		{"US 盘中", MarketUS, time.Date(2025, 6, 3, 10, 0, 0, 0, ny), SessionRegular},
		{"US 盘后", MarketUS, time.Date(2025, 6, 3, 17, 0, 0, 0, ny), SessionPost},
		{"加密", MarketCrypto, time.Date(2025, 6, 7, 3, 0, 0, 0, time.UTC), SessionRegular},
		{"宏观", MarketMacro, time.Date(2025, 6, 3, 10, 0, 0, 0, time.UTC), SessionClosed},
	}
	for _, c := range cases {
		if got := SessionOf(c.m, c.at); got != c.want {
			t.Errorf("%s: SessionOf = %s, want %s", c.name, got, c.want)
		}
	}
}
