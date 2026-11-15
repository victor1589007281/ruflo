package techdocs

// match.go —— 把一条查询映射到目录里的若干站点域名。
//
// 匹配分两级, 都要求查询里出现**明确信号**, 不做模糊语义猜测:
//
//  1. 产品别名命中: 例如 "mysql 主从延迟" 命中 MySQL, "k8s network policy"
//     命中 Kubernetes。取别名最长 (最具体) 的至多 6 个站, 域名去重。
//  2. 组信号命中: 没有产品名, 但出现 "数据库事务" / "内核调度" / "负载均衡"
//     这类组内通用词, 用该组前几个核心站兜底。
//
// 两级都不命中时返回空 —— 调用方据此把这次检索让给通用源, 避免每一种查询
// 都被塞进技术文档站。别名匹配用 ASCII 词边界, 中文按子串; 边界规则写在
// matchesAlias 里。

import (
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// MaxDomains 是单次查询最多带上的 site: 域名数。
	//
	// Exa 的 site: OR 查询在 6 个域名内仍然稳定 (2026-11 实测); 再多会把
	// 关键词权重摊薄, 长 query 容易返回 0 条。
	MaxDomains = 6
	// MaxFallbackSites 是组兜底时使用的核心站数量。
	MaxFallbackSites = 4
)

// Match 是一次 query → 目录的匹配结果。
type Match struct {
	// Sites 是命中的站点 (按具体程度排序), 仅供诊断/展示。
	Sites []Site
	// Domains 是去重后的 site: 域名, 按优先级排序。
	Domains []string
	// Fallback 表示没有命中产品名, 走的是组信号兜底。
	Fallback bool
	// Group 是兜底时使用的分组 (Fallback=true 时有效)。
	Group Group
}

// Matched 表示这次查询是否适合交给技术文档源。
func (m Match) Matched() bool { return len(m.Domains) > 0 }

// 组信号词。只收在该组内基本无歧义的词; 通用编程词 (function/class/loop)
// 刻意不收 —— 它们更可能指向语言文档或 Stack Overflow, 而不是这些站。
var groupSignals = map[Group][]string{
	GroupDatabase: {
		"数据库", "database", "sql", "事务", "transaction", "索引", "indexing",
		"查询优化", "query optimization", "执行计划", "query plan", "存储引擎",
		"storage engine", "mvcc", "主从", "replication", "分库分表", "oltp", "olap",
		"deadlock", "死锁",
	},
	GroupCloud: {
		"云原生", "cloud native", "云平台", "cloud platform", "容器", "container",
		"serverless", "无服务器", "虚拟机", "virtual machine", "对象存储",
		"object storage", "弹性伸缩", "autoscaling", "云函数",
	},
	GroupOS: {
		"操作系统", "operating system", "内核", "kernel", "进程调度", "线程调度",
		"内存管理", "memory management", "文件系统", "filesystem", "system call",
		"cgroup", "namespace", "grub", "systemd", "启动引导", "bootloader",
		"内核模块", "kernel module", "页表", "page table",
	},
	GroupStorage: {
		"存储", "storage", "磁盘", "disk", "raid", "lvm", "块存储", "block storage",
		"分布式存储", "distributed storage", "快照", "snapshot", "纠删码",
		"erasure coding", "i/o 调度", "io scheduler", "缓存淘汰", "cache eviction",
		"命名空间", "inode", "fsck",
	},
	GroupNetwork: {
		"网络", "network", "tcp", "udp", "http/2", "http/3", "dns", "网关",
		"gateway", "负载均衡", "load balanc", "反向代理", "reverse proxy",
		"路由", "routing", "bgp", "vlan", "iptables", "防火墙", "firewall",
		"三次握手", "handshake", "service mesh", "服务网格", "流量治理",
		"tls", "quic", "cdn", "mtu",
	},
	GroupMiddleware: {
		"消息队列", "message queue", "中间件", "middleware", "发布订阅", "pub/sub",
		"rpc", "序列化", "serialization", "流处理", "stream processing",
		"事件驱动", "event-driven", "服务发现", "service discovery",
	},
	GroupCommunity: {
		"工程实践", "engineering blog", "架构设计", "architecture", "性能优化",
		"performance tuning", "踩坑", "实战", "最佳实践", "best practice",
		"技术博客", "tech blog", "故障复盘", "postmortem",
	},
}

// MatchQuery 把 query 映射到目录域名。
func MatchQuery(query string) Match {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return Match{}
	}

	var hits []siteHit
	for i, s := range Sites {
		best := 0
		for _, alias := range s.Aliases {
			if matchesAlias(q, strings.ToLower(alias)) {
				if n := utf8.RuneCountInString(alias); n > best {
					best = n
				}
			}
		}
		if best > 0 {
			hits = append(hits, siteHit{site: s, score: best, order: i})
		}
	}

	if len(hits) > 0 {
		// 别名越长越具体 ("阿里云 polardb" 应先于 "阿里云"); 同分时保持目录顺序。
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].score != hits[j].score {
				return hits[i].score > hits[j].score
			}
			return hits[i].order < hits[j].order
		})
		sites := make([]Site, 0, len(hits))
		for _, h := range hits {
			sites = append(sites, h.site)
		}
		return buildMatch(sites, false, "")
	}

	group, ok := inferGroup(q)
	if !ok {
		return Match{}
	}
	return buildMatch(fallbackSites(group), true, group)
}

// siteHit 是产品别名匹配的中间结果。
type siteHit struct {
	site  Site
	score int
	order int
}

// buildMatch 把站点列表压成去重后的域名列表。
func buildMatch(sites []Site, fallback bool, group Group) Match {
	m := Match{Sites: sites, Fallback: fallback, Group: group}
	seen := make(map[string]bool, MaxDomains)
	for _, s := range sites {
		for _, d := range s.Domains {
			d = strings.TrimSpace(strings.ToLower(d))
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			m.Domains = append(m.Domains, d)
			if len(m.Domains) >= MaxDomains {
				return m
			}
		}
	}
	return m
}

// fallbackSites 返回某组的前 N 个站点 (组内顺序即优先级)。
func fallbackSites(g Group) []Site {
	out := make([]Site, 0, MaxFallbackSites)
	for _, s := range Sites {
		if s.Group != g {
			continue
		}
		out = append(out, s)
		if len(out) >= MaxFallbackSites {
			break
		}
	}
	return out
}

// inferGroup 按组信号词判断查询属于哪一组。
//
// 命中数相同时按 GroupOrder 取靠前的组: 例如 "文件系统 存储" 同时命中 OS 与
// storage, 文件系统首先是内核子系统, 交给 OS 文档更准。
func inferGroup(q string) (Group, bool) {
	best := Group("")
	bestScore := 0
	for _, g := range GroupOrder {
		score := 0
		for _, sig := range groupSignals[g] {
			if matchesAlias(q, sig) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = g, score
		}
	}
	return best, bestScore > 0
}

// matchesAlias 判断 query 是否包含 alias。
//
// 含非 ASCII 字符的别名 (中文) 按子串匹配; 纯 ASCII 别名要求两侧不是 ASCII
// 字母/数字/下划线 —— "rds" 不应命中 "records", 但 "使用redis" 里的 "redis"
// 两侧是汉字, 属于合法边界。
func matchesAlias(q, alias string) bool {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return false
	}
	if !isASCII(alias) {
		return strings.Contains(q, alias)
	}
	for from := 0; from <= len(q)-len(alias); {
		i := strings.Index(q[from:], alias)
		if i < 0 {
			return false
		}
		i += from
		before, after := byte(0), byte(0)
		if i > 0 {
			before = q[i-1]
		}
		if j := i + len(alias); j < len(q) {
			after = q[j]
		}
		if !isASCIIWord(before) && !isASCIIWord(after) {
			return true
		}
		from = i + 1
	}
	return false
}

// isASCII 判断字符串是否只含 ASCII 字符。
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// isASCIIWord 判断字节是否是 ASCII 词字符 (字母/数字/下划线)。
func isASCIIWord(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}
