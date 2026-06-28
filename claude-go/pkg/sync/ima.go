package sync

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// IMAAdapter 通过 node ima_api.cjs 调用 IMA OpenAPI。
type IMAAdapter struct {
	ClientID string
	APIKey   string
	Script   string
}

// NewIMAAdapter 创建 IMA 适配器。
func NewIMAAdapter(clientID, apiKey string) *IMAAdapter {
	script := "/home/victor/.claude/skills/ima-skill/ima_api.cjs"
	return &IMAAdapter{ClientID: clientID, APIKey: apiKey, Script: script}
}

// Source 返回数据源名称。
func (a *IMAAdapter) Source() string { return "ima" }

// List 列出所有笔记摘要。
func (a *IMAAdapter) List() ([]ExternalItem, error) {
	body := map[string]interface{}{
		"folder_id": "",
		"sort_type": 1,
		"cursor":    "",
		"limit":     20,
	}
	optsJSON := a.optionsJSON()

	var all []ExternalItem
	cursor := ""
	for {
		body["cursor"] = cursor
		bodyJSON, _ := json.Marshal(body)
		out, err := a.call("openapi/note/v1/list_note", string(bodyJSON), optsJSON)
		if err != nil {
			return nil, err
		}

		var resp struct {
			Data struct {
				NoteBookList []struct {
					NoteID     string `json:"note_id"`
					Title      string `json:"title"`
					ModifyTime string `json:"modify_time"`
				} `json:"note_book_list"`
				Notes []struct {
					NoteID     string `json:"note_id"`
					Title      string `json:"title"`
					ModifyTime string `json:"modify_time"`
				} `json:"notes"`
				IsEnd  bool   `json:"is_end"`
				Cursor string `json:"cursor"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			return nil, fmt.Errorf("解析笔记列表失败: %w", err)
		}

		list := resp.Data.NoteBookList
		if len(list) == 0 {
			list = resp.Data.Notes
		}

		for _, n := range list {
			updated := time.Now().UTC()
			if ms, err := strconv.ParseInt(n.ModifyTime, 10, 64); err == nil && ms > 0 {
				updated = time.Unix(ms/1000, 0).UTC()
			}
			all = append(all, ExternalItem{
				ExternalID: n.NoteID,
				Type:       "note",
				Title:      n.Title,
				URL:        "https://ima.qq.com/note/" + n.NoteID,
				UpdatedAt:  updated,
			})
		}

		if resp.Data.IsEnd || resp.Data.Cursor == "" {
			break
		}
		cursor = resp.Data.Cursor
	}
	return all, nil
}

// Fetch 拉取单条笔记正文。
func (a *IMAAdapter) Fetch(item ExternalItem) (ExternalItem, error) {
	body := map[string]interface{}{
		"note_id":               item.ExternalID,
		"target_content_format": 1,
	}
	bodyJSON, _ := json.Marshal(body)
	out, err := a.call("openapi/note/v1/get_doc_content", string(bodyJSON), a.optionsJSON())
	if err != nil {
		return item, err
	}

	var resp struct {
		Data struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return item, fmt.Errorf("解析笔记内容失败: %w", err)
	}
	if resp.Data.Title != "" {
		item.Title = resp.Data.Title
	}
	item.Body = resp.Data.Content
	if item.Body == "" {
		item.Body = "(空笔记)"
	}
	return item, nil
}

func (a *IMAAdapter) optionsJSON() string {
	opts := map[string]string{}
	if a.ClientID != "" {
		opts["clientId"] = a.ClientID
	}
	if a.APIKey != "" {
		opts["apiKey"] = a.APIKey
	}
	data, _ := json.Marshal(opts)
	return string(data)
}

func (a *IMAAdapter) call(apiPath, bodyJSON, optsJSON string) ([]byte, error) {
	args := []string{a.Script, apiPath, bodyJSON, optsJSON}
	cmd := exec.Command("node", args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return nil, fmt.Errorf("调用 ima_api.cjs %s 失败: %w (stderr: %s)", apiPath, err, stderr)
	}
	out = []byte(strings.TrimSpace(string(out)))
	// 检查业务错误码
	var base struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(out, &base)
	if base.Code != 0 {
		return nil, fmt.Errorf("IMA API %s 返回错误: code=%d msg=%s", apiPath, base.Code, base.Msg)
	}
	return out, nil
}
