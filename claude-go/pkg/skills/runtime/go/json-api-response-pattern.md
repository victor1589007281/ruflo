# Skill: JSON API Response Pattern

## 场景 (When to use)
当需要构建 RESTful JSON API，统一响应格式、错误处理和状态码，确保客户端能稳定解析。

## 代码模板 (Template)
```go
package api

import (
	"encoding/json"
	"net/http"
)

type Response struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   *APIError   `json:"error,omitempty"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func WriteJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := Response{Success: true, Data: data}
	_ = json.NewEncoder(w).Encode(resp)
}

func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := Response{Success: false, Error: &APIError{Code: code, Message: message}}
	_ = json.NewEncoder(w).Encode(resp)
}

func Handler(w http.ResponseWriter, r *http.Request) {
	// TODO: parse request parameters
	// TODO: call business logic
	result, err := doSomething(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, result)
}

func doSomething(ctx context.Context) (map[string]string, error) {
	// TODO: implement business logic
	return map[string]string{"status": "ok"}, nil
}
```

## 反模式警告 (Anti-patterns)
- 不要在响应中直接返回原始 `error.Error()` 给客户端，避免泄漏内部信息
- 不要忘记设置 `Content-Type: application/json`
- 不要在 handler 中直接调用 `panic`，应始终返回错误响应

## 契约要求 (Contract Requirements)
- Handler 签名必须匹配 `http.HandlerFunc`
- 业务逻辑函数应接受 `context.Context` 作为第一个参数

## 测试模板 (Test Template)
```go
func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, map[string]string{"key": "value"})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success {
		t.Fatal("expected success")
	}
}
```
