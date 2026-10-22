// logger.go 把每次决策判定写入 agentDB 的 decision JSONLog，作为不可变流水。
//
// 落地形态（2026-10-21 与 agentDB serve 实测确认）：agentDB serve 已有通用端点
//   POST /v1/log/{bucket}/append   （追加一行，body 原样落 JSONL）
//   GET  /v1/log/{bucket}          （读回，base64 行数组）
// 因此 agentDB 侧零源码改动——决策流水直接复用该端点。
//
// 失败纪律（与 statestore/dual 的 fail-open 一致）：写流水失败只记日志，绝不阻断主路径。
// 这是"决策层挂掉 agent 照样跑"的关键——判断是增强，不是硬依赖。
package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Record 是写入 decision bucket 的一行流水。
// 字段刻意保持开放（map），因为 P0 召回判断与 P1 swarm 打分的 schema 不同，
// 强行统一会丢字段。共性字段（kind/run_id/at/model）固定，其余塞进 Extra。
type Record struct {
	// Kind 区分流水来源："recall"（RetrieveFor 精判）/ "quality_gate"（swarm 打分）/ ...
	Kind string `json:"kind"`
	// RunID 是 trace 四元组 episode id，用于和注入记录/run 级事实 join（可空）。
	RunID string `json:"run_id,omitempty"`
	// Model 是本次判定用的决策模型（如 snap:2b / jeb:4b）。
	Model string `json:"model,omitempty"`
	// At 是判定发生时间（UTC）。
	At time.Time `json:"at"`
	// LatencyMS 是决策调用的耗时（毫秒），用于观测无 GPU 机器上的真实延迟。
	LatencyMS int64 `json:"latency_ms,omitempty"`
	// FellBack 标记本次是否回退到了原策略（决策不可用）。
	FellBack bool `json:"fell_back,omitempty"`
	// Extra 是各 kind 自定义的负载（候选集、判断结果、双跑对照值等）。
	Extra map[string]any `json:"extra,omitempty"`
}

// Logger 把 Record 追加到 agentDB 的 decision bucket。
// 零值不可用——必须显式给 BaseURL，避免"忘了配就静默写进默认地址"。
type Logger struct {
	// BaseURL 是 agentDB serve 地址（如 http://agentdb:9090）。
	BaseURL string
	// Bucket 是 JSONLog 桶名，默认 "decision"。
	Bucket string
	// HTTPClient 可注入自定义 client（测试用）。
	HTTPClient *http.Client
	// Timeout 是单次 append 超时，默认 3s（写流水不该拖慢主路径）。
	Timeout time.Duration
	// Disabled 为 true 时 Log 直接返回 nil（配置关停决策流水，但保留调用点）。
	Disabled bool
}

// NewLogger 构造一个写 agentDB serve 的流水记录器。
func NewLogger(baseURL string) *Logger {
	return &Logger{BaseURL: strings.TrimRight(baseURL, "/"), Bucket: "decision", Timeout: 3 * time.Second}
}

func (l *Logger) bucket() string {
	if l.Bucket == "" {
		return "decision"
	}
	return l.Bucket
}

// Log 把一条 Record 追加到 decision bucket。任何失败都只记标准日志、返回 nil——
// 这是刻意的：流水是观测手段，不是业务正确性的前提（见包注释的 fail-open 纪律）。
// 返回 error 仅供测试断言"确实写进去了"。
//
// 为什么不复用上游 ctx：上游 ctx 可能已被决策调用消耗完超时预算（本 bug 在 2026-10-22
// k8s 实测中真撞到过——quality_jev.go 把 8s 决策 ctx 传给 Log，Decide 跑完后 Log 必超时）。
// 写流水独立给 Timeout 上限即可，ctx 参数仅用于取消语义（如调用方已 cancel 则不写）。
func (l *Logger) Log(ctx context.Context, rec Record) error {
	if l == nil || l.Disabled || l.BaseURL == "" {
		return nil
	}
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		log.Printf("[decision] 流水序列化失败 (fail-open): %v", err)
		return err
	}

	hc := l.HTTPClient
	to := l.Timeout
	if to <= 0 {
		to = 3 * time.Second
	}
	if hc == nil {
		hc = &http.Client{Timeout: to}
	}
	// 独立 ctx：上游 ctx 已 Done 则直接放弃写流水（尊重调用方的取消语义），
	// 否则给本次 append 一个独立的 Timeout 上限，不被上游残余预算拖死。
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	url := fmt.Sprintf("%s/v1/log/%s/append", l.BaseURL, l.bucket())
	req, err := http.NewRequestWithContext(writeCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		log.Printf("[decision] 流水构造请求失败 (fail-open): %v", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("[decision] 流水写入失败 (fail-open): %v", err)
		return err
	}
	defer resp.Body.Close()
	// 读空 body 以便连接复用；只在前 512 字节内提取错误信息。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("agentdb append 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		log.Printf("[decision] 流水写入被拒 (fail-open): %v", err)
		return err
	}
	return nil
}
