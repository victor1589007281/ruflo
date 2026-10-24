package market

import (
	"time"
	_ "time/tzdata" // 内嵌 IANA 时区库: 容器内没有 /usr/share/zoneinfo 时仍能正确换算
)

// 本文件实现交易日历与交易时段判定。
//
// **维护约定**: 节假日表按交易所公告逐年维护。缺失的节假日会被误判为交易日,
// 因此新增年份时必须同步补表; 表中只列"工作日但休市"的日期, 周末由规则统一
// 判定, 不需要重复列出。临时休市 (台风/黑雨/技术故障) 不在表内, 由 provider
// 返回的 session/停牌状态表达。
//
// 已知边界:
//   - 港股半日市 (平安夜/除夕) 未建模, 按全天时段处理;
//   - 美股只做周末判定, 不内置美国联邦假日表;
//   - 加密资产 7x24 交易, 不使用交易日历。

// holidayTable 是 market -> "2006-01-02" 的休市日集合。
var holidayTable = map[Market]map[string]bool{
	MarketCN: setOf(
		// 2025 年 A 股休市日 (上交所/深交所公告)
		"2025-01-01",                                           // 元旦
		"2025-01-28", "2025-01-29", "2025-01-30", "2025-01-31", // 春节
		"2025-02-03", "2025-02-04", // 春节
		"2025-04-04",                             // 清明
		"2025-05-01", "2025-05-02", "2025-05-05", // 劳动节
		"2025-06-02", // 端午
		"2025-10-01", "2025-10-02", "2025-10-03", "2025-10-06",
		"2025-10-07", "2025-10-08", // 国庆 + 中秋
		// 2026 年 A 股休市日 (按国务院节假日安排预置, 交易所公告发布后复核)
		"2026-01-01", "2026-01-02", // 元旦
		"2026-02-16", "2026-02-17", "2026-02-18", "2026-02-19", "2026-02-20", // 春节
		"2026-04-06",                             // 清明 (4/5 为周日)
		"2026-05-01", "2026-05-04", "2026-05-05", // 劳动节
		"2026-06-19",                                                         // 端午
		"2026-09-25",                                                         // 中秋
		"2026-10-01", "2026-10-02", "2026-10-05", "2026-10-06", "2026-10-07", // 国庆
	),
	MarketHK: setOf(
		// 2025 年港股主要休市日
		"2025-01-01",                             // 元旦
		"2025-01-29", "2025-01-30", "2025-01-31", // 农历新年
		"2025-04-04",               // 清明
		"2025-04-18",               // 耶稣受难日
		"2025-04-21",               // 复活节星期一
		"2025-05-01",               // 劳动节
		"2025-05-05",               // 佛诞
		"2025-07-01",               // 香港特别行政区成立纪念日
		"2025-10-01",               // 国庆
		"2025-10-07",               // 中秋翌日
		"2025-10-29",               // 重阳
		"2025-12-25", "2025-12-26", // 圣诞 +  Boxing Day
		// 2026 年港股主要休市日 (按公开假期预置, 交易所公告发布后复核)
		"2026-01-01",                             // 元旦
		"2026-02-17", "2026-02-18", "2026-02-19", // 农历新年
		"2026-04-03", // 耶稣受难日
		"2026-04-06", // 复活节星期一 / 清明翌日
		"2026-05-01", // 劳动节
		"2026-05-25", // 佛诞翌日 (5/24 为周日)
		"2026-07-01", // 回归纪念日
		"2026-10-01", // 国庆
		"2026-10-19", // 重阳翌日 (10/18 为周日)
		"2026-12-25", // 圣诞
		"2026-12-28", // Boxing Day 翌日 (12/26 为周六)
	),
}

// setOf 把日期字符串列表转成集合。
func setOf(dates ...string) map[string]bool {
	m := make(map[string]bool, len(dates))
	for _, d := range dates {
		m[d] = true
	}
	return m
}

// marketLocation 返回市场所在时区。优先使用 IANA 数据库; 加载失败时退回固定偏移
// (美股 DST 无法在固定偏移下精确表达, 但至少不会 panic)。
func marketLocation(m Market) *time.Location {
	name := timezoneByMarket[m]
	if name == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	switch m {
	case MarketCN:
		return time.FixedZone("CST", 8*60*60)
	case MarketHK:
		return time.FixedZone("HKT", 8*60*60)
	case MarketUS:
		return time.FixedZone("EST", -5*60*60)
	}
	return time.UTC
}

// IsTradingDay 判断 d 所在的市场本地日期是否为交易日。
// 加密资产 7x24 交易, 恒为 true; 宏观序列没有交易日概念, 恒为 false。
func IsTradingDay(m Market, d time.Time) bool {
	switch m {
	case MarketCrypto:
		return true
	case MarketMacro:
		return false
	}
	local := d.In(marketLocation(m))
	if local.Weekday() == time.Saturday || local.Weekday() == time.Sunday {
		return false
	}
	if set := holidayTable[m]; set != nil {
		if set[local.Format("2006-01-02")] {
			return false
		}
	}
	return true
}

// LastTradingDay 返回 d 当日或之前最近的一个交易日 (市场本地时区的 00:00)。
// 日历表缺口会导致这里返回错误的"交易日", 见文件头维护约定。
func LastTradingDay(m Market, d time.Time) time.Time {
	loc := marketLocation(m)
	local := d.In(loc)
	if m == MarketCrypto {
		return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	}
	// 最多回看 60 天: 覆盖春节/国庆长假; 超出说明输入年份没有日历数据。
	for i := 0; i < 60; i++ {
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		if IsTradingDay(m, day) {
			return day
		}
		local = local.AddDate(0, 0, -1)
	}
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

// SessionOf 按市场本地时间判定交易时段。
//
// 时段定义:
//   - A 股: pre 09:15–09:30, regular 09:30–11:30 / 13:00–15:00, post 15:05–15:30
//     (科创板盘后固定价格交易窗口), 其余 closed;
//   - 港股: pre 09:00–09:30, regular 09:30–12:00 / 13:00–16:00, post 16:00–16:10
//     (收市竞价), 其余 closed;
//   - 美股: pre 04:00–09:30, regular 09:30–16:00, post 16:00–20:00, 其余 closed;
//   - 加密: 恒为 regular; 宏观: closed。
//
// 非交易日一律返回 closed。停牌 (halted) 不由日历判定, 由 provider 依据成交量
// 与停牌信息设置。
func SessionOf(m Market, now time.Time) Session {
	switch m {
	case MarketCrypto:
		return SessionRegular
	case MarketMacro:
		return SessionClosed
	}
	local := now.In(marketLocation(m))
	if !IsTradingDay(m, local) {
		return SessionClosed
	}
	minutes := local.Hour()*60 + local.Minute()
	switch m {
	case MarketCN:
		switch {
		case minutes >= 9*60+15 && minutes < 9*60+30:
			return SessionPre
		case minutes >= 9*60+30 && minutes < 11*60+30:
			return SessionRegular
		case minutes >= 13*60 && minutes < 15*60:
			return SessionRegular
		case minutes >= 15*60+5 && minutes < 15*60+30:
			return SessionPost
		}
	case MarketHK:
		switch {
		case minutes >= 9*60 && minutes < 9*60+30:
			return SessionPre
		case minutes >= 9*60+30 && minutes < 12*60:
			return SessionRegular
		case minutes >= 13*60 && minutes < 16*60:
			return SessionRegular
		case minutes >= 16*60 && minutes < 16*60+10:
			return SessionPost
		}
	case MarketUS:
		switch {
		case minutes >= 4*60 && minutes < 9*60+30:
			return SessionPre
		case minutes >= 9*60+30 && minutes < 16*60:
			return SessionRegular
		case minutes >= 16*60 && minutes < 20*60:
			return SessionPost
		}
	}
	return SessionClosed
}
