package browser

// stealthArgs 返回 Chrome headless 反检测启动参数集。
// 参考: puppeteer-extra-plugin-stealth + undetected-chromedriver。
func stealthArgs() []string {
	return []string{
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--disable-blink-features=AutomationControlled",
		"--disable-features=IsolateOrigins,site-per-process",
		"--disable-infobars",
		"--disable-background-networking",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-hang-monitor",
		"--disable-prompt-on-repost",
		"--disable-sync",
		"--disable-translate",
		"--disable-domain-reliability",
		"--disable-client-side-phishing-detection",
		"--no-first-run",
		"--no-default-browser-check",
		"--metrics-recording-only",
		"--mute-audio",
		"--window-size=1920,1080",
		"--lang=zh-CN,zh,en-US,en",
	}
}

// stealthUserAgents 返回常见真实浏览器 UA (轮换使用)。
var stealthUserAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.2 Safari/605.1.15",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:134.0) Gecko/20100101 Firefox/134.0",
}
