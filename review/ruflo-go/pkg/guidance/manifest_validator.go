package guidance

import (
	"fmt"
	"strings"
)

// 本文件：策略清单与 PolicyBundle 一致性校验。ValidateManifest 检查必填与计数非负；ValidateBundle 核对分片数、规则总数与 Manifest 声明。

// ValidationError 描述校验失败的字段与原因。
type ValidationError struct {
	Field   string `json:"field,omitempty"` // 字段路径，如 manifest.shard_count
	Message string `json:"message"`         // 错误说明
}

// Error 实现 error 接口。
func (e ValidationError) Error() string {
	if e.Field != "" {
		return e.Field + ": " + e.Message
	}
	return e.Message
}

// ValidateManifest 校验 BundleID、Version 非空，ShardCount/RuleCount 非负，ByRisk 计数和与 RuleCount 一致（在 RuleCount>0 且 sum>0 时）。
func ValidateManifest(manifest RuleManifest) []ValidationError {
	var errs []ValidationError
	if strings.TrimSpace(manifest.BundleID) == "" {
		errs = append(errs, ValidationError{Field: "bundle_id", Message: "required"})
	}
	if strings.TrimSpace(manifest.Version) == "" {
		errs = append(errs, ValidationError{Field: "version", Message: "required"})
	}
	if manifest.ShardCount < 0 {
		errs = append(errs, ValidationError{Field: "shard_count", Message: "must be non-negative"})
	}
	if manifest.RuleCount < 0 {
		errs = append(errs, ValidationError{Field: "rule_count", Message: "must be non-negative"})
	}
	if manifest.ByRisk != nil {
		sum := 0
		for _, n := range manifest.ByRisk {
			if n < 0 {
				errs = append(errs, ValidationError{Field: "by_risk", Message: "negative count"})
				break
			}
			sum += n
		}
		if sum != manifest.RuleCount && manifest.RuleCount > 0 && sum > 0 {
			errs = append(errs, ValidationError{Field: "by_risk", Message: fmt.Sprintf("sum %d != rule_count %d", sum, manifest.RuleCount)})
		}
	}
	return errs
}

// ValidateBundle 校验 bundle.ID/Version、每分片 ShardID、Manifest 中 ShardCount/RuleCount 与实际切片/规则数一致，并附加 ValidateManifest 结果。
func ValidateBundle(bundle PolicyBundle) []ValidationError {
	var errs []ValidationError
	if strings.TrimSpace(bundle.ID) == "" {
		errs = append(errs, ValidationError{Field: "id", Message: "required"})
	}
	if strings.TrimSpace(bundle.Version) == "" {
		errs = append(errs, ValidationError{Field: "version", Message: "required"})
	}
	shardCount := len(bundle.Shards)
	ruleCount := 0
	for _, sh := range bundle.Shards {
		ruleCount += len(sh.Rules)
		if strings.TrimSpace(sh.ShardID) == "" {
			errs = append(errs, ValidationError{Field: "shards.shard_id", Message: "required"})
		}
	}
	m := bundle.Manifest
	if m.ShardCount != 0 && m.ShardCount != shardCount {
		errs = append(errs, ValidationError{Field: "manifest.shard_count", Message: fmt.Sprintf("expected %d", shardCount)})
	}
	if m.RuleCount != 0 && m.RuleCount != ruleCount {
		errs = append(errs, ValidationError{Field: "manifest.rule_count", Message: fmt.Sprintf("expected %d", ruleCount)})
	}
	errs = append(errs, ValidateManifest(m)...)
	return errs
}
