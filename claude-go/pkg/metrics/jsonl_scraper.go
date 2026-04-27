package metrics

import (
	"bufio"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// JSONLScraper 按 scrape 节奏读取 JSONL 文件, 将新事件注入 Prometheus 注册表。
// 类似于 MySQL Exporter: 外部系统写 JSONL, 采集时读取最新数据。
// 通过 byte-offset 跟踪每个文件, 避免重复计数。
type JSONLScraper struct {
	mu          sync.Mutex
	jsonlDir    string
	fileOffsets map[string]int64 // module -> byte offset
}

// NewJSONLScraper 创建 scraper, 监控 stateDir/metrics/ 下的 JSONL 文件。
func NewJSONLScraper(stateDir string) *JSONLScraper {
	return &JSONLScraper{
		jsonlDir:    filepath.Join(stateDir, "metrics"),
		fileOffsets: make(map[string]int64),
	}
}

// Scrape 读取所有 JSONL 文件的新行, 注入到当前进程的 Prometheus 注册表。
func (s *JSONLScraper) Scrape() {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := filepath.Glob(filepath.Join(s.jsonlDir, "*.jsonl"))
	if err != nil {
		return
	}

	total := 0
	for _, path := range entries {
		module := moduleFromPath(path)
		off := s.fileOffsets[module]
		n, newOff, err := s.scrapeFile(path, off)
		if err != nil {
			continue
		}
		if newOff > off {
			s.fileOffsets[module] = newOff
			total += n
		}
	}

	if total > 0 {
		log.Printf("[metrics] JSONLScraper 采集了 %d 条新指标", total)
	}
}

// scrapeFile 从指定 offset 开始读取 JSONL 文件, 回放事件到 Prometheus。
func (s *JSONLScraper) scrapeFile(path string, offset int64) (int, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, offset, err
	}
	defer f.Close()

	// Seek 到上次读取的位置
	if offset > 0 {
		_, err = f.Seek(offset, 0)
		if err != nil {
			return 0, offset, err
		}
	}

	type jsonlEvent struct {
		Timestamp string            `json:"ts"`
		Module    string            `json:"module"`
		Name      string            `json:"name"`
		Value     float64           `json:"value"`
		Labels    map[string]string `json:"labels"`
	}

	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var evt jsonlEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		recordPromMetric(evt.Module, evt.Name, evt.Value, evt.Labels)
		count++
	}

	// 获取最终 offset
	finalOff, _ := f.Seek(0, 1)
	if finalOff < offset {
		return count, offset, nil
	}

	return count, finalOff, nil
}

// ScrapeAndServe 包装 http.Handler, 在请求处理前执行一次 Scrape。
// 用于包装 /metrics handler, 确保每次 Prometheus 采集都能拿到最新数据。
type ScrapeAndServe struct {
	scraper *JSONLScraper
	handler http.Handler
}

func (s *ScrapeAndServe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.scraper.Scrape()
	s.handler.ServeHTTP(w, r)
}

// WrapWithScrape 用 JSONLScraper 包装 http.Handler, 每次请求前自动 scrape。
func WrapWithScrape(scraper *JSONLScraper, handler http.Handler) http.Handler {
	return &ScrapeAndServe{scraper: scraper, handler: handler}
}

func moduleFromPath(path string) string {
	base := filepath.Base(path)
	return base[:len(base)-len(".jsonl")]
}
