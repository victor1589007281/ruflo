// protocol.go — MCP 2026-07-28 无状态协议共享层。
//
// 2026-07-28 (SEP-2575 / SEP-2567) 移除 initialize 握手与 Mcp-Session-Id 会话:
//   - 协议版本 / 客户端身份 / 能力声明改由 _meta 每次请求携带
//     (io.modelcontextprotocol/protocolVersion / clientInfo / clientCapabilities);
//   - HTTP 上协议版本固定到 MCP-Protocol-Version 请求头, 路由由 Mcp-Method /
//     Mcp-Name / Mcp-Param-* 请求头承载;
//   - 新增 server/discover RPC 与 UnsupportedProtocolVersionError (含 supported
//     列表, 客户端据此降级重试), 删除长连接 GET SSE (subcriptions/listen 取代)。
//
// 本文件同时承载"多后端会话粘性负载均衡"的一致性哈希环 —— 2026-07-28 虽无
// 协议层会话, 但应用层有进程内查询缓存/索引热态, 同一会话的请求落在同一实例
// 才能命中, 不同会话自然散开达到均衡。
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strconv"
)

// 协议版本常量。2026-07-28 为当前最新稳定版（正式发布于 2026-07-28）。
const (
	ProtocolVersion     = "2026-07-28"
	ProtocolVersion2025 = "2025-11-25"
	ProtocolVersion2024 = "2024-11-05"
)

// SupportedProtocolVersions 服务器支持的协议版本（优先级降序）。
var SupportedProtocolVersions = []string{ProtocolVersion, ProtocolVersion2025, ProtocolVersion2024}

// _meta 键名（SEP-2575 要求每次请求携带）。
const (
	MetaKeyProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaKeyClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
)

// HTTP 请求/响应头（2026-07-28 无状态路由）。
const (
	HeaderProtocolVersion = "MCP-Protocol-Version"
	HeaderMethod          = "Mcp-Method"
	HeaderName            = "Mcp-Name"
	HeaderRequestID       = "Mcp-Request-Id"
	HeaderMessageType     = "Mcp-Message-Type"
	HeaderParamPrefix     = "Mcp-Param-"
	HeaderBackend         = "X-Claude-Go-Backend"
)

// JSON-RPC 2.0 错误码。
const (
	ErrorParse                          = -32700
	ErrorInvalidRequest                 = -32600
	ErrorMethodNotFound                 = -32601
	ErrorInvalidParams                  = -32602
	ErrorInternal                       = -32603
	ErrorUnsupportedProtocolVersion     = -32002
	ErrorMissingRequiredClientCapability = -32003
	ErrorUnknownTool                    = -32004
)

// MetaInfo 从请求 _meta / 请求头提取的协议元数据。
type MetaInfo struct {
	ProtocolVersion    string                 `json:"protocolVersion,omitempty"`
	ClientName         string                 `json:"clientName,omitempty"`
	ClientVersion      string                 `json:"clientVersion,omitempty"`
	ClientCapabilities map[string]interface{} `json:"clientCapabilities,omitempty"`
}

// ProtocolVersionOr 返回 _meta 中声明的协议版本，为空时回退到 fallback。
func (m MetaInfo) ProtocolVersionOr(fallback string) string {
	if m.ProtocolVersion != "" {
		return m.ProtocolVersion
	}
	return fallback
}

// IsSupported 判断某协议版本是否在服务器支持列表内。
func IsSupported(v string) bool {
	for _, s := range SupportedProtocolVersions {
		if s == v {
			return true
		}
	}
	return false
}

// ExtractMetaFromParams 从 JSON-RPC params 的 _meta 字段提取协议元数据。
// params 形如 {"name": "...", "arguments": {...}, "_meta": {...}}。
func ExtractMetaFromParams(params map[string]interface{}) MetaInfo {
	var m MetaInfo
	raw, ok := params["_meta"]
	if !ok {
		return m
	}
	meta, ok := raw.(map[string]interface{})
	if !ok {
		return m
	}
	if v, ok := meta[MetaKeyProtocolVersion].(string); ok {
		m.ProtocolVersion = v
	}
	if ci, ok := meta[MetaKeyClientInfo].(map[string]interface{}); ok {
		if n, ok := ci["name"].(string); ok {
			m.ClientName = n
		}
		if v, ok := ci["version"].(string); ok {
			m.ClientVersion = v
		}
	}
	if cc, ok := meta[MetaKeyClientCapabilities].(map[string]interface{}); ok {
		m.ClientCapabilities = cc
	}
	return m
}

// WithMetaParams 向 params 注入 _meta（协议版本 + 客户端身份 + 能力）。
// params 为 nil 时创建新 map。
func WithMetaParams(params map[string]interface{}, version, clientName, clientVersion string, caps map[string]interface{}) map[string]interface{} {
	if params == nil {
		params = map[string]interface{}{}
	}
	meta := map[string]interface{}{}
	if prev, ok := params["_meta"].(map[string]interface{}); ok {
		meta = prev
	}
	meta[MetaKeyProtocolVersion] = version
	ci := map[string]interface{}{"name": clientName, "version": clientVersion}
	if old, ok := meta[MetaKeyClientInfo].(map[string]interface{}); ok {
		for k, v := range old {
			ci[k] = v
		}
	}
	meta[MetaKeyClientInfo] = ci
	if caps != nil {
		meta[MetaKeyClientCapabilities] = caps
	}
	params["_meta"] = meta
	return params
}

// ============================================================================
// 会话粘性负载均衡 —— 一致性哈希环
// ============================================================================

// ConsistentHash 一致性哈希环：同一会话键恒映射到同一后端，不同会话键均匀散开。
// 用虚拟节点稀释热区；后端增减时仅影响相邻节点的映射，具备最小迁移。
type ConsistentHash struct {
	ring    []int            // 排序后的哈希值列表
	lookup  map[int]string   // 哈希值 → 后端
	virtual int              // 每个物理后端虚拟节点数
}

// NewConsistentHash 创建一致性哈希环。virtual<=0 时用默认 128。
func NewConsistentHash(nodes []string, virtual int) *ConsistentHash {
	if virtual <= 0 {
		virtual = 128
	}
	ch := &ConsistentHash{
		lookup:  make(map[int]string),
		virtual: virtual,
	}
	ch.Add(nodes)
	return ch
}

// Add 添加（或替换）后端并重建环。
func (ch *ConsistentHash) Add(nodes []string) {
	ch.lookup = make(map[int]string, len(nodes)*ch.virtual)
	for _, n := range nodes {
		for i := 0; i < ch.virtual; i++ {
			key := hashKey(n + "#" + strconv.Itoa(i))
			ch.lookup[key] = n
		}
	}
	ch.ring = make([]int, 0, len(ch.lookup))
	for h := range ch.lookup {
		ch.ring = append(ch.ring, h)
	}
	sort.Ints(ch.ring)
}

// Get 返回 stickyKey 命中的后端。环为空时返回空串。
func (ch *ConsistentHash) Get(stickyKey string) string {
	if len(ch.ring) == 0 {
		return ""
	}
	h := hashKey(stickyKey)
	// 顺时针找第一个 ≥ h 的虚拟节点；绕环回卷则取 0。
	i := sort.SearchInts(ch.ring, h)
	if i == len(ch.ring) {
		i = 0
	}
	return ch.lookup[ch.ring[i]]
}

// hashKey 对会话键/虚拟节点键做一致性哈希。
//
// 曾用 FNV-1a（32/64 位）：对"共享长前缀、仅尾部递增"的键（如后端基址 + "#N"）
// 聚簇严重 —— 实测 3 后端下某后端占到 56%~58% 环空间、最大相邻空隙达 30%~52%，
// 会让负载明显偏斜。改 SHA-256（截 31 位），3 后端实测约 31/33/36、最大空隙 1.17%。
func hashKey(s string) int {
	sum := sha256.Sum256([]byte(s))
	// 取前 4 字节转 uint32，再掩掉符号位得到 31 位正数，避免 int 溢出影响排序一致性。
	return int(binary.BigEndian.Uint32(sum[:4]) & 0x7fffffff)
}

// ============================================================================
// 会话粘性键的上下文传递
// ============================================================================

type stickyKeyCtxKey struct{}

// WithStickyKey 把会话粘性键写入 context，供一致性哈希选路。
func WithStickyKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, stickyKeyCtxKey{}, key)
}

// StickyKeyFromContext 读取会话粘性键；无则返回空串（调用方回退到默认键）。
func StickyKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(stickyKeyCtxKey{}).(string); ok {
		return v
	}
	return ""
}
