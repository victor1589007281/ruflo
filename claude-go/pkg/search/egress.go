package search

// egress.go —— 按 provider 钉死的出口 (直连 / 代理 / 自动)。
//
// 每个 provider 在构造时声明自己的出口模式; 本文件预建两个 http.Client,
// 由 Client(pin) 返回对应实例:
//   - direct: 永不走代理;
//   - proxy: 配了代理且探测健康时走代理, 否则回落到 direct 并把健康位暴露
//     给上层记 degraded;
//   - auto: 按代理健康位在两者之间切换。
//
// 代理健康由后台探测维护: 连续失败 failThreshold 次后摘除, 持续 recoverAfter
// 后再探; 不配代理时 auto 直接退化为直连 (这是正常配置, 不是降级)。
import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Mode 是出口模式。
type Mode string

const (
	ModeDirect Mode = "direct"
	ModeProxy  Mode = "proxy"
	ModeAuto   Mode = "auto"
)

// Egress 持有直连与代理两个预建客户端, 并按 provider 的选择返回其一。
//
// 关键约束:
//   - 不使用 http.ProxyFromEnvironment: 它读取进程环境且有 sync.Once 缓存,
//     与"按 provider 钉死出口"冲突;
//   - 显式设置 Transport.Proxy 后 NO_PROXY 不再生效, 因此 bypass 由本包自己判定;
//   - 不设置 http.Client.Timeout, 超时由每次请求的 context 控制。
type Egress struct {
	direct        *http.Client
	proxy         *http.Client
	mode          Mode
	noProxy       []string
	probeURL      string
	probeInterval time.Duration
	probeTimeout  time.Duration
	failThreshold int
	recoverAfter  time.Duration

	healthy atomic.Bool
	fails   atomic.Int32
	lastOK  atomic.Int64 // unix nano

	stopOnce sync.Once
	stop     chan struct{}
}

// NewEgress 按配置构造出口。代理 URL 非法时返回错误 (调用方可降级为纯直连)。
func NewEgress(cfg EgressConfig) (*Egress, error) {
	e := &Egress{
		mode:          cfg.Mode,
		noProxy:       append([]string(nil), cfg.NoProxy...),
		probeURL:      cfg.ProbeURL,
		probeInterval: cfg.Interval,
		probeTimeout:  cfg.Timeout,
		failThreshold: cfg.FailThreshold,
		recoverAfter:  cfg.RecoverAfter,
		stop:          make(chan struct{}),
	}
	if e.mode == "" {
		e.mode = ModeAuto
	}
	if e.probeURL == "" {
		e.probeURL = "https://example.com"
	}
	if e.probeInterval <= 0 {
		e.probeInterval = 30 * time.Second
	}
	if e.probeTimeout <= 0 {
		e.probeTimeout = 2 * time.Second
	}
	if e.failThreshold <= 0 {
		e.failThreshold = 3
	}
	if e.recoverAfter <= 0 {
		e.recoverAfter = 60 * time.Second
	}

	e.direct = newDirectClient()

	if cfg.ProxyURL == "" {
		// 没配代理: 显式 proxy 的 provider 将不可用, auto 退化为直连。
		e.healthy.Store(false)
		return e, nil
	}
	c, err := newProxyClient(cfg.ProxyURL, e.noProxy)
	if err != nil {
		return nil, err
	}
	e.proxy = c
	e.healthy.Store(true) // 初始乐观, 探测失败后再摘除

	return e, nil
}

// Start 启动代理健康探测。未配置代理时为空操作。
func (e *Egress) Start() {
	if e.proxy == nil {
		return
	}
	go e.probeLoop()
}

// Close 停止探测并释放空闲连接。
func (e *Egress) Close() error {
	e.stopOnce.Do(func() { close(e.stop) })
	e.direct.CloseIdleConnections()
	if e.proxy != nil {
		e.proxy.CloseIdleConnections()
	}
	return nil
}

// Client 返回按 provider 钉死的客户端。
//
// direct 恒直连; proxy 在代理未配置或已摘除时返回 direct (调用方应据此记 degraded);
// auto 按健康状态选择。返回值永不为 nil。
func (e *Egress) Client(pin Mode) *http.Client {
	switch pin {
	case ModeDirect:
		return e.direct
	case ModeProxy:
		if e.proxy == nil || !e.healthy.Load() {
			return e.direct
		}
		return e.proxy
	case ModeAuto:
		if e.proxy != nil && e.healthy.Load() {
			return e.proxy
		}
		return e.direct
	default:
		return e.direct
	}
}

// Healthy 返回代理是否可用。未配置代理时恒为 false。
func (e *Egress) Healthy() bool { return e.proxy != nil && e.healthy.Load() }

// HasProxy 返回是否配置了代理 (与健康状态无关)。
func (e *Egress) HasProxy() bool { return e.proxy != nil }

// Bypass 判断 host 是否应绕过代理 (环回、localhost、代理自身、内网后缀)。
func (e *Egress) Bypass(host string) bool {
	return bypass(host, e.noProxy)
}

// Mode 返回配置的默认出口模式。
func (e *Egress) Mode() Mode { return e.mode }

// probeLoop 周期性探测代理; 连续失败达到阈值即摘除, 恢复后重新放量。
func (e *Egress) probeLoop() {
	t := time.NewTicker(e.probeInterval)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			e.probeOnce()
		}
	}
}

// probeOnce 执行一次探测并按结果更新健康位。
func (e *Egress) probeOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), e.probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.probeURL, nil)
	if err != nil {
		e.markFail()
		return
	}
	req.Header.Set("User-Agent", "claude-go-search-probe/1.0")
	resp, err := e.proxy.Do(req)
	if err != nil {
		e.markFail()
		return
	}
	_ = resp.Body.Close()
	// 任何 HTTP 响应都说明隧道通; 目标站的 4xx/5xx 不代表代理故障。
	e.markOK()
}

// markFail 记录一次探测失败, 达到阈值后摘除代理。
func (e *Egress) markFail() {
	n := e.fails.Add(1)
	if int(n) >= e.failThreshold {
		e.healthy.Store(false)
	}
}

// markOK 记录一次探测成功并恢复健康位。
func (e *Egress) markOK() {
	e.fails.Store(0)
	e.lastOK.Store(time.Now().UnixNano())
	e.healthy.Store(true)
}

// newDirectClient 构造直连客户端。
func newDirectClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 3 * time.Second
	tr.ResponseHeaderTimeout = 8 * time.Second
	tr.MaxIdleConnsPerHost = 4
	tr.ForceAttemptHTTP2 = true
	return &http.Client{Transport: tr}
}

// newProxyClient 构造走指定代理的客户端。
//
// 与 pkg/api.Client.SetProxy 的 clone-transport 手法同构, 区别是不改写全局 Transport,
// 且 bypass 在本函数内实现 (显式 Proxy 函数会让 NO_PROXY 失效)。
func newProxyClient(raw string, noProxy []string) (*http.Client, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, &url.Error{Op: "parse-proxy", URL: raw, Err: errInvalidProxy}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		if bypass(req.URL.Hostname(), noProxy) {
			return nil, nil
		}
		return u, nil
	}
	tr.DialContext = (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 3 * time.Second
	tr.ResponseHeaderTimeout = 8 * time.Second
	tr.MaxIdleConnsPerHost = 4
	tr.ForceAttemptHTTP2 = true
	return &http.Client{Transport: tr}, nil
}

// bypass 判断 host 是否命中 no_proxy 列表。支持精确匹配、后缀匹配与 CIDR。
func bypass(host string, noProxy []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, raw := range noProxy {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" {
			continue
		}
		if strings.Contains(p, "/") {
			if _, ipnet, err := net.ParseCIDR(p); err == nil {
				if ip := net.ParseIP(host); ip != nil && ipnet.Contains(ip) {
					return true
				}
			}
			continue
		}
		p = strings.TrimPrefix(p, ".")
		if host == p || strings.HasSuffix(host, "."+p) {
			return true
		}
		// 环回别名: localhost 也应覆盖 127.0.0.1 与 ::1。
		if p == "localhost" && (host == "127.0.0.1" || host == "::1") {
			return true
		}
	}
	return false
}

// hostOf 从 URL 中取 host (不含端口)。解析失败返回空串。
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// errInvalidProxy 表示代理 URL 缺少 scheme 或 host。
var errInvalidProxy = errors.New("proxy URL must include scheme and host")
