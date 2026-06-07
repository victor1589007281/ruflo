// Package wechat 实现微信公众号自动排版与草稿发布 (Tier 2)。
//
// 流水线: Markdown → (mermaid 渲染成 PNG 并上传) → 公众号安全内联 HTML → 上传封面 → 建草稿。
//
// 注意: 公众号 API 要求服务器出口 IP 在「IP 白名单」内, 否则所有调用返回 errcode 40164。
package wechat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Config 公众号凭证。
type Config struct {
	AppID     string `json:"appId"`
	AppSecret string `json:"appSecret"`
	Author    string `json:"author,omitempty"`
}

// Client 公众号 API 客户端 (带 access_token 缓存)。
type Client struct {
	cfg   Config
	httpc *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewClient 构造客户端。
func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg, httpc: &http.Client{Timeout: 30 * time.Second}}
}

const apiBase = "https://api.weixin.qq.com"

type wxErr struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func (e wxErr) err() error {
	if e.ErrCode == 0 {
		return nil
	}
	hint := ""
	if e.ErrCode == 40164 {
		hint = " (服务器出口 IP 不在公众号 IP 白名单, 请在 设置与开发→基本配置→IP白名单 添加)"
	}
	return fmt.Errorf("公众号 API 错误 errcode=%d errmsg=%s%s", e.ErrCode, e.ErrMsg, hint)
}

// AccessToken 返回有效的 access_token (缓存, 过期自动刷新)。
func (c *Client) AccessToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}
	u := fmt.Sprintf("%s/cgi-bin/token?grant_type=client_credential&appid=%s&secret=%s",
		apiBase, url.QueryEscape(c.cfg.AppID), url.QueryEscape(c.cfg.AppSecret))
	resp, err := c.httpc.Get(u)
	if err != nil {
		return "", fmt.Errorf("请求 access_token 失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		wxErr
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("解析 access_token 响应失败: %w (%s)", err, string(body))
	}
	if e := out.wxErr.err(); e != nil {
		return "", e
	}
	c.token = out.AccessToken
	c.tokenExp = time.Now().Add(time.Duration(out.ExpiresIn-300) * time.Second) // 提前 5min 过期
	return c.token, nil
}

// UploadContentImage 上传正文图片, 返回可直接用于图文 content 的 URL。
// 对应 cgi-bin/media/uploadimg, 不占用永久素材配额。
func (c *Client) UploadContentImage(img []byte, filename string) (string, error) {
	token, err := c.AccessToken()
	if err != nil {
		return "", err
	}
	u := fmt.Sprintf("%s/cgi-bin/media/uploadimg?access_token=%s", apiBase, url.QueryEscape(token))
	body, err := c.postMultipart(u, "media", filename, img)
	if err != nil {
		return "", err
	}
	var out struct {
		wxErr
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("解析 uploadimg 响应失败: %w (%s)", err, string(body))
	}
	if e := out.wxErr.err(); e != nil {
		return "", e
	}
	return out.URL, nil
}

// AddImageThumb 上传图片为永久素材, 返回 media_id (用作草稿封面 thumb_media_id)。
// 对应 cgi-bin/material/add_material?type=image。
func (c *Client) AddImageThumb(img []byte, filename string) (string, error) {
	token, err := c.AccessToken()
	if err != nil {
		return "", err
	}
	u := fmt.Sprintf("%s/cgi-bin/material/add_material?access_token=%s&type=image", apiBase, url.QueryEscape(token))
	body, err := c.postMultipart(u, "media", filename, img)
	if err != nil {
		return "", err
	}
	var out struct {
		wxErr
		MediaID string `json:"media_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("解析 add_material 响应失败: %w (%s)", err, string(body))
	}
	if e := out.wxErr.err(); e != nil {
		return "", e
	}
	return out.MediaID, nil
}

// DraftArticle 草稿图文。
type DraftArticle struct {
	Title            string `json:"title"`
	Author           string `json:"author,omitempty"`
	Digest           string `json:"digest,omitempty"`
	Content          string `json:"content"` // 公众号安全内联 HTML
	ContentSourceURL string `json:"content_source_url,omitempty"`
	ThumbMediaID     string `json:"thumb_media_id"`
}

// AddDraft 新建草稿, 返回草稿 media_id。对应 cgi-bin/draft/add。
func (c *Client) AddDraft(a DraftArticle) (string, error) {
	token, err := c.AccessToken()
	if err != nil {
		return "", err
	}
	u := fmt.Sprintf("%s/cgi-bin/draft/add?access_token=%s", apiBase, url.QueryEscape(token))
	payload, _ := json.Marshal(map[string]any{"articles": []DraftArticle{a}})
	resp, err := c.httpc.Post(u, "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("请求 draft/add 失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		wxErr
		MediaID string `json:"media_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("解析 draft/add 响应失败: %w (%s)", err, string(body))
	}
	if e := out.wxErr.err(); e != nil {
		return "", e
	}
	return out.MediaID, nil
}

func (c *Client) postMultipart(u, field, filename string, data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(data); err != nil {
		return nil, err
	}
	w.Close()
	req, err := http.NewRequest(http.MethodPost, u, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
