// digest.go —— 13.6 F1「skill 渐进披露」的指纹与已注入记账。
//
// 问题: Skill 工具每次成功调用都把技能全文注入对话。自训练技能量上来后
// (数十~数百个), 同一 run 内反复加载同一技能、或多 run 会话里技能内容未变
// 仍重复注入, token 随技能数线性增长。
//
// 方案: Register 时为每个技能算内容指纹 (Digest), Skill 工具成功加载时记账
// (injected: name → 已注入时的 digest)。持久注入方 (13.7 池化的 pool_load、
// 角色技能注入) 拿 WasInjectedUnchanged(name) 判断"已注入且内容未变", 可跳过
// 重复注入只补一句"已加载"; 技能被改进 (digest 变化) 后自动失效, 下次重新
// 注入。记账随进程会话存活, Reload 不清 (技能内容未变时重载不应导致重复注入)。
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ComputeDigest 技能内容指纹: 对影响行为的字段做 sha256。
// 不含 SourcePath/SkillDir/LoadedFrom/Rank —— 同内容换位置不算变化, 不应触发重注入。
func ComputeDigest(s *Skill) string {
	h := sha256.New()
	fmt.Fprintf(h, "name:%s\ndesc:%s\nwhen:%s\nver:%s\nmodel:%s\nstatus:%s\n",
		s.Name, s.Description, s.WhenToUse, s.Version, s.Model, s.Status)
	for _, p := range s.Paths {
		fmt.Fprintf(h, "path:%s\n", p)
	}
	for _, t := range s.AllowedTools {
		fmt.Fprintf(h, "tool:%s\n", t)
	}
	if s.ModelInvocable != nil {
		fmt.Fprintf(h, "mi:%v\n", *s.ModelInvocable)
	}
	if s.UserInvocable != nil {
		fmt.Fprintf(h, "ui:%v\n", *s.UserInvocable)
	}
	h.Write([]byte("body:"))
	h.Write([]byte(s.Body))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// MarkInjected 记账: 技能已注入到当前会话上下文。
// 记录注入时刻的内容指纹; 技能此后被改进 (digest 变化) 会自动失效。
// 技能不存在时静默忽略 (记账是优化路径, fail-open)。
func (r *Registry) MarkInjected(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.skills[name]; ok {
		if r.injected == nil {
			r.injected = make(map[string]string)
		}
		r.injected[name] = s.Digest
	}
}

// WasInjectedUnchanged 技能是否"已注入且内容未变"。
// 未注入 / 已被卸载 / 注入后又被改进 (digest 变化) → false, 调用方应重新注入。
func (r *Registry) WasInjectedUnchanged(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.injected[name]
	if !ok {
		return false
	}
	s, ok := r.skills[name]
	return ok && s.Digest == d
}
