// Package wechat 实现微信公众号自动排版与草稿发布 (Tier 2)。
//
// 流水线: Markdown → (mermaid 渲染成 PNG 并上传) → 公众号安全内联 HTML → 上传封面 → 建草稿。
//
// 注意: 公众号 API 要求服务器出口 IP 在「IP 白名单」内, 否则所有调用返回 errcode 40164。
package wechat

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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
//
// 强制 HTTP/1.1: Go 默认走 HTTP/2, 其指纹会被 Tencent WAF 拦成 501 page; curl(HTTP/1.1) 正常。
func NewClient(cfg Config) *Client {
	tr := &http.Transport{
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{}, // 禁用 HTTP/2
	}
	return &Client{cfg: cfg, httpc: &http.Client{Timeout: 30 * time.Second, Transport: tr}}
}

const apiBase = "https://api.weixin.qq.com"

// userAgent 用真实浏览器 UA, 规避 Tencent WAF 把 Go 默认 UA 当机器人拦截 (501 page)。
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

func (c *Client) get(u string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

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
	body, err := c.get(u)
	if err != nil {
		return "", fmt.Errorf("请求 access_token 失败: %w", err)
	}
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
	body, err := c.postJSON(u, payload)
	if err != nil {
		return "", fmt.Errorf("请求 draft/add 失败: %w", err)
	}
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

// UpdateDraft 更新已有草稿的第 index 篇文章。对应 cgi-bin/draft/update。
func (c *Client) UpdateDraft(mediaID string, index int, a DraftArticle) error {
	token, err := c.AccessToken()
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/cgi-bin/draft/update?access_token=%s", apiBase, url.QueryEscape(token))
	payload, _ := json.Marshal(map[string]any{"media_id": mediaID, "index": index, "articles": a})
	body, err := c.postJSON(u, payload)
	if err != nil {
		return fmt.Errorf("请求 draft/update 失败: %w", err)
	}
	var out wxErr
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("解析 draft/update 响应失败: %w (%s)", err, string(body))
	}
	return out.err()
}

// looksLikeHTML 判断响应是否是 WAF/网关拦截页 (非 JSON)。
func looksLikeHTML(body []byte) bool {
	b := bytes.TrimSpace(body)
	return len(b) > 0 && b[0] == '<'
}

// postJSON POST application/json。
//
// 实测: Go 的 net/http (HTTP/1.1 或 2 + 浏览器 UA 都试过) 在 draft/* 端点会被 Tencent WAF
// 按 TLS 指纹拦成 501 page, 而 curl(HTTP/1.1) 正常。故 draft 的 JSON 请求改走 curl 子进程。
func (c *Client) postJSON(u string, payload []byte) ([]byte, error) {
	tmp, err := os.CreateTemp("", "wxpost-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()

	var last []byte
	waits := []time.Duration{0, 5 * time.Second, 15 * time.Second}
	for attempt := 0; attempt < len(waits); attempt++ {
		if waits[attempt] > 0 {
			time.Sleep(waits[attempt])
		}
		cmd := exec.Command("curl", "-s", "--max-time", "40", "-A", userAgent,
			"-X", "POST", u, "-H", "Content-Type: application/json",
			"--data-binary", "@"+tmp.Name())
		out, cerr := cmd.Output()
		if cerr != nil {
			last = out
			continue
		}
		if looksLikeHTML(out) {
			last = out
			continue
		}
		return out, nil
	}
	return nil, fmt.Errorf("响应被网关/WAF 拦截(HTML), 重试后仍失败: %s", truncate(string(last), 120))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// DeleteDraft 删除草稿。对应 cgi-bin/draft/delete。
func (c *Client) DeleteDraft(mediaID string) error {
	token, err := c.AccessToken()
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/cgi-bin/draft/delete?access_token=%s", apiBase, url.QueryEscape(token))
	payload, _ := json.Marshal(map[string]any{"media_id": mediaID})
	body, err := c.postJSON(u, payload)
	if err != nil {
		return fmt.Errorf("请求 draft/delete 失败: %w", err)
	}
	var out wxErr
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("解析 draft/delete 响应失败: %w (%s)", err, string(body))
	}
	return out.err()
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
	ct := w.FormDataContentType()
	raw := buf.Bytes()
	var last []byte
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*3) * time.Second)
		}
		req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", ct)
		req.Header.Set("User-Agent", userAgent)
		resp, err := c.httpc.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if looksLikeHTML(body) {
			last = body
			continue
		}
		return body, nil
	}
	return nil, fmt.Errorf("上传响应被网关/WAF 拦截(HTML), 重试后仍失败: %s", truncate(string(last), 120))
}
