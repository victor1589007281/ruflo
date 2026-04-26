package dashboard

// prom_bridge.go — 历史快照代理已废弃。
// 现在 /metrics 端点直接使用 PrometheusHandler() 导出实时指标,
// 不再需要 JSONL 快照机制。
