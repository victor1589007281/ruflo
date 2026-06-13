// 共享浏览器池: 进程内复用一个常驻 Chrome 分配器, 避免每次渲染都新起 Chrome。
//
// 旧实现 newBrowserCtx 每次渲染都 NewExecAllocator → 启动一个 Chrome 进程 (冷启动数百 ms)。
// 改为进程级懒初始化一个 allocator, 每次渲染只在其上新开一个 tab (chromedp.NewContext),
// 渲染延迟大降。对长驻服务 (dashboard/feishu) 收益最大; CLI 一次性运行时 Chrome 随进程退出。
package media

import (
	"context"
	"sync"

	"github.com/chromedp/chromedp"
)

var (
	sharedAllocOnce   sync.Once
	sharedAllocCtx    context.Context
	sharedAllocCancel context.CancelFunc
)

// sharedAllocator 返回进程级常驻 Chrome 分配器上下文 (懒初始化)。
func sharedAllocator() context.Context {
	sharedAllocOnce.Do(func() {
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.WindowSize(1920, 1080),
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-web-security", true),
			chromedp.Flag("allow-file-access-from-files", true),
		)
		sharedAllocCtx, sharedAllocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
	})
	return sharedAllocCtx
}

// ShutdownBrowserPool 关闭常驻 Chrome (供服务优雅退出时调用, 可选)。
func ShutdownBrowserPool() {
	if sharedAllocCancel != nil {
		sharedAllocCancel()
	}
}
