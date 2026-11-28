// args_hash.go — 工具入参的归一化短哈希 (P1: ndjson progress 事件的 args_hash 字段)。
//
// # 为什么不是 internal_hook.NormalizeHash
//
// NormalizeHash 是对**原始字节**直接 sha256 (注释说去首尾空白, 实现没有): 模型两次
// 发出语义相同但键序/空白不同的 JSON 时, 它给出两个不同的哈希。args_hash 的契约是
// **等值判据** —— "两次调用入参相同" 必须能从哈希相等推出来, 所以先做 canonical JSON
// (unmarshal → marshal, Go 的 map 序列化键序稳定) 再哈希。
//
// # 为什么哈希而不是原文
//
// ndjson progress 事件的纪律是绝不携带完整工具参数 (参数里可能有密钥)。哈希只回答
// "两次调用是不是同一个", 不回答 "参数是什么" —— 消费方 (评测的循环检测) 需要的
// 恰好只是前者。
//
// # 验收口径
//
// P1 验收要求 "args_hash 与用原始 args 重算一致率 100%": 重算算法就是本函数 ——
// canonical JSON (json.Unmarshal 到 any 再 json.Marshal) → sha256 → hex 前 8 字符;
// 入参不是合法 JSON 时退化为原始字节哈希 (同一二进制内口径唯一)。
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// canonicalArgsHash8 返回工具入参的归一化短哈希 (8 hex 字符)。
// 空输入返回空串 (与 StreamEvent 零值一致, 消费方按"无哈希"处理)。
func canonicalArgsHash8(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	canon := raw
	var v any
	// 只有合法 JSON 才做 canonical 化; 非法输入 (截断的流式残片等) 直接哈希原文,
	// 此时等值判据退化为逐字节相等 —— 比报错好, 调用方不该因为哈希失败丢事件。
	if err := json.Unmarshal(raw, &v); err == nil {
		if b, err := json.Marshal(v); err == nil {
			canon = b
		}
	}
	h := sha256.Sum256(canon)
	return hex.EncodeToString(h[:])[:8]
}
