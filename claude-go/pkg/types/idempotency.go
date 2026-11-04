// idempotency.go — 幂等键的**唯一**实现（Moqi R-H9）。
//
// # 为什么放在 types 而不是 harness
//
// 幂等键有两个消费方：业务契约层（`harness.IdempotencyKey`，用于"这一轮重试是不是
// 同一件事"）与执行层（`pkg/tool`，用于"同一 (工具, 参数) 的第二次调用直接返回首次
// 结果"）。依赖方向是
//
//	harness → engine → tool → types
//
// `pkg/tool` 不能 import `pkg/harness`（反向依赖成环）。若两边各写一份哈希，最危险
// 的失效形态不是报错而是"两套键都算得出来、但彼此不等"：执行层按自己的键去重，
// 业务层按自己的键判重放，于是同一件事被记成两件，而没有任何日志异常。这与
// owner 载体（见 owner.go 文件头）是同一个陷阱，解法也一样：只此一份实现，上层引用。
//
// # 三条必须成立的性质
//
//  1. **跨进程稳定**：只用 sha256(标准库)，不用 maphash（每个进程种子不同），
//     也不用 fmt 的 map 格式化（Go 1.12 起按键排序，但那不是 API 承诺）。
//  2. **键序无关**：先把输入重新序列化一遍。json.Marshal 对 map 按键排序，
//     所以 `{"a":1,"b":2}` 与 `{"b":2,"a":1}` 得到同一个键；直接哈希原始字节
//     则会把同一次调用算成两个，重放时去重失效。
//  3. **数值不丢精度**：解码用 UseNumber，保留数字字面量。用默认的 float64
//     会把超过 2^53 的整数（数据库 id、时间戳纳秒）压成同一个浮点值，导致
//     不同的 id 撞成同一个幂等键——那时去重会把两个不同操作合并成一个。
package types

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// IdempotencyKey 返回工具名 + 规范化输入参数的稳定哈希。
//
// 空输入与 `null` 视为同一件事（"没有参数"）：无参工具以 nil 还是 `null`
// 传进来取决于调用方构造方式，二者语义相同，不该产生两个键。
//
// 非法 JSON 不会 panic，也不会返回空串：以 `raw:` 前缀区分，对原始字节做哈希。
// 走到这里说明上游校验已经失效，此时给一个确定且可区分的键，比给一个所有
// 坏输入共用的空键安全——后者会让所有解析失败调用被去重成一个。
func IdempotencyKey(toolName string, input json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(toolName))
	h.Write([]byte{0}) // 分隔符：防止 "ab"+"c" 与 "a"+"bc" 撞键
	if canon, err := CanonicalJSON(input); err == nil {
		h.Write(canon)
	} else {
		h.Write([]byte("raw:"))
		h.Write(input)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CanonicalJSON 把 JSON 重新序列化成语义等价、键有序、无冗余空白的形态。
//
// 用 decoded → Marshal 的往返而不是手写排序器：排序、转义、数字格式这三件事
// 交给标准库，才不会出现"我们以为等价的两个输入序列化出来仍不同"。
func CanonicalJSON(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []byte("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber() // 见 IdempotencyKey 第 3 条
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// Decode 只吃第一个值；`{}{"x":1}` 这种尾部垃圾不算等价于 `{}`。
	if dec.More() {
		return nil, errors.New("JSON 之后还有多余内容")
	}
	return json.Marshal(v)
}
