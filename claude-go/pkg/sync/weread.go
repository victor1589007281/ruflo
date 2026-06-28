package sync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const wereadGateway = "https://i.weread.qq.com/api/agent/gateway"

// WeReadAdapter 调用微信读书 agent gateway。
type WeReadAdapter struct {
	APIKey string
	client *http.Client
}

// NewWeReadAdapter 创建微信读书适配器。
func NewWeReadAdapter(apiKey string) *WeReadAdapter {
	return &WeReadAdapter{
		APIKey: apiKey,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

// Source 返回数据源名称。
func (a *WeReadAdapter) Source() string { return "weread" }

// List 列出所有有笔记的书籍。
func (a *WeReadAdapter) List() ([]ExternalItem, error) {
	var items []ExternalItem
	var lastSort int64
	for {
		body := map[string]interface{}{
			"api_name":      "/user/notebooks",
			"count":         100,
			"skill_version": "1.0.3",
		}
		if lastSort != 0 {
			body["lastSort"] = lastSort
		}
		var resp struct {
			TotalBookCount int `json:"totalBookCount"`
			TotalNoteCount int `json:"totalNoteCount"`
			HasMore        int `json:"hasMore"`
			Books          []struct {
				BookID    string `json:"bookId"`
				Sort      int64  `json:"sort"`
				ReviewCnt int    `json:"reviewCount"`
				NoteCnt   int    `json:"noteCount"`
				Book      struct {
					Title  string `json:"title"`
					Author string `json:"author"`
				} `json:"book"`
			} `json:"books"`
		}
		if err := a.postGateway(body, &resp); err != nil {
			return nil, err
		}
		for _, b := range resp.Books {
			updated := time.Now().UTC()
			items = append(items, ExternalItem{
				ExternalID: b.BookID,
				Type:       "book",
				Title:      fmt.Sprintf("%s (%s)", b.Book.Title, b.Book.Author),
				URL:        "https://weread.qq.com/web/reader/" + b.BookID,
				UpdatedAt:  updated,
			})
		}
		if resp.HasMore != 1 || len(resp.Books) == 0 {
			break
		}
		lastSort = resp.Books[len(resp.Books)-1].Sort
	}
	return items, nil
}

// Fetch 拉取单本书的笔记内容。
func (a *WeReadAdapter) Fetch(item ExternalItem) (ExternalItem, error) {
	var parts []string

	// 1. 划线内容
	var marks struct {
		Updated []struct {
			ChapterUID int    `json:"chapterUid"`
			MarkText   string `json:"markText"`
			CreateTime int64  `json:"createTime"`
		} `json:"updated"`
		Chapters []struct {
			ChapterUID int    `json:"chapterUid"`
			Title      string `json:"title"`
		} `json:"chapters"`
	}
	if err := a.postGateway(map[string]interface{}{
		"api_name": "/book/bookmarklist",
		"bookId":   item.ExternalID,
	}, &marks); err == nil {
		chapMap := make(map[int]string)
		for _, c := range marks.Chapters {
			chapMap[c.ChapterUID] = c.Title
		}
		for _, m := range marks.Updated {
			chap := chapMap[m.ChapterUID]
			if chap != "" {
				parts = append(parts, "## "+chap)
				chapMap[m.ChapterUID] = "" // 只输出一次章节标题
			}
			parts = append(parts, "> "+m.MarkText)
		}
	}

	// 2. 想法/点评
	var reviews struct {
		Reviews []struct {
			Review struct {
				Content     string `json:"content"`
				ChapterName string `json:"chapterName"`
				CreateTime  int64  `json:"createTime"`
				Abstract    string `json:"abstract"`
			} `json:"review"`
		} `json:"reviews"`
		HasMore int   `json:"hasMore"`
		SyncKey int64 `json:"synckey"`
	}
	syncKey := int64(0)
	for {
		if err := a.postGateway(map[string]interface{}{
			"api_name": "/review/list/mine",
			"bookid":   item.ExternalID,
			"synckey":  syncKey,
			"count":    100,
		}, &reviews); err != nil {
			break
		}
		for _, r := range reviews.Reviews {
			if r.Review.ChapterName != "" {
				parts = append(parts, "## "+r.Review.ChapterName)
			}
			if r.Review.Abstract != "" {
				parts = append(parts, "> "+r.Review.Abstract)
			}
			parts = append(parts, r.Review.Content)
		}
		if reviews.HasMore != 1 {
			break
		}
		syncKey = reviews.SyncKey
	}

	item.Body = strings.Join(parts, "\n\n")
	if item.Body == "" {
		item.Body = "(暂无笔记)"
	}
	return item, nil
}

func (a *WeReadAdapter) postGateway(body interface{}, dst interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, wereadGateway, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.APIKey)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求微信读书 gateway 失败: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("微信读书 gateway 返回 %d: %s", resp.StatusCode, string(respBody))
	}
	if err := json.Unmarshal(respBody, dst); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	return nil
}
