# Skill: HTTP Server Graceful Shutdown

## 场景 (When to use)
当需要启动一个 HTTP 服务，并希望在收到终止信号（SIGINT/SIGTERM）时优雅地关闭，确保正在处理的请求完成后再退出。

## 代码模板 (Template)
```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// TODO: implement health check logic
	})

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown error: %v\n", err)
	}
}
```

## 反模式警告 (Anti-patterns)
- 不要在业务代码中硬编码 `context.Background()`，应接受外部传入的 `context.Context`
- 不要直接调用 `server.Close()`，它会强制断开活跃连接
- 不要忽略 `ListenAndServe` 返回的错误

## 契约要求 (Contract Requirements)
- 无特殊接口要求，标准库 `net/http` 即可

## 测试模板 (Test Template)
```go
func TestGracefulShutdown(t *testing.T) {
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.NewServeMux()}
	go func() { _ = server.ListenAndServe() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}
```
