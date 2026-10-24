package builtin

import (
	"net/http"
	"os"
	"sync"

	"github.com/anthropic/claude-go/pkg/market"
	marketcn "github.com/anthropic/claude-go/pkg/market/providers/cn"
	marketglobal "github.com/anthropic/claude-go/pkg/market/providers/global"
	"github.com/anthropic/claude-go/pkg/search"
	searchproviders "github.com/anthropic/claude-go/pkg/search/providers"
)

// DataStack 是进程内共享的搜索与行情数据面。
//
// 搜索源与行情源都基于同一份出口状态 (代理健康位), 因此复用同一个 Egress;
// 构建本身不发起网络请求, 首次真正检索/取数时才建连。
//
// 装配点放在 builtin 而不是 cmd: 所有注册路径 (主 agent、nested agent、
// tools 子命令、dashboard 能力清单、profile 裁剪集) 都经过这里, 漏掉任何一条
// 都会出现"有的入口能搜、有的入口说没配置"的分叉。
type DataStack struct {
	Search *search.LegacySearcher
	Market *market.Registry
	egress *search.Egress
}

var (
	dataOnce  sync.Once
	dataStack *DataStack
)

// DefaultDataStack 返回进程级数据面; 环境变量 CLAUDE_GO_DATA_DISABLED=1 时返回 nil。
//
// 返回 nil 表示"本进程不装配数据面", 调用方必须区分 nil 与"已装配但无结果":
// 前者不应注册工具, 后者必须把错误如实抛给模型。
func DefaultDataStack() *DataStack {
	dataOnce.Do(func() { dataStack = buildDataStack() })
	return dataStack
}

// buildDataStack 按环境变量装配搜索与行情。
func buildDataStack() *DataStack {
	if os.Getenv("CLAUDE_GO_DATA_DISABLED") == "1" {
		return nil
	}

	scfg := search.LoadConfig()
	eg, err := search.NewEgress(scfg.Egress)
	if err != nil {
		// 代理地址非法时不能静默退化成直连: 那会让"配了代理但仍被墙"
		// 表现成上游 000/超时。这里直接不装配, 由工具层报告配置错误。
		return nil
	}
	eg.Start()

	manager := search.NewManager(scfg, eg, searchproviders.Default(scfg, eg))

	mcfg := market.LoadConfig()
	client := func(mode string) *http.Client { return eg.Client(search.Mode(mode)) }
	providers := append(marketcn.Providers(mcfg, client), marketglobal.Providers(mcfg, client)...)

	return &DataStack{
		Search: search.NewLegacySearcher(manager),
		Market: market.NewRegistry(mcfg, providers...),
		egress: eg,
	}
}

// EgressHealthy 报告代理出口是否可用 (未配置代理时为 false)。
func (d *DataStack) EgressHealthy() bool { return d != nil && d.egress != nil && d.egress.Healthy() }
