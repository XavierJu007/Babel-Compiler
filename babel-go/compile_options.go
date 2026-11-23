package main

import (
	"fmt"
	"net/http"
	"strings"
)

type TokenMeasurementError struct {
	Encoding string
	Err      error
}

func (e *TokenMeasurementError) Error() string {
	return fmt.Sprintf("Token 计数不可用（%s）: %v", e.Encoding, e.Err)
}

func (e *TokenMeasurementError) Unwrap() error { return e.Err }

func normalizeValidationMode(mode string) string {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return "compare"
	}
	return mode
}

func validateValidationMode(mode string) error {
	switch normalizeValidationMode(mode) {
	case "compare", "blind":
		return nil
	default:
		return fmt.Errorf("未知验证模式 %q；仅支持 compare 或 blind", mode)
	}
}

// normalizeReasoningEffort 收敛到受支持的低延迟取值；空串表示不注入该字段。
func normalizeReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "default":
		return ""
	case "none", "minimal", "low":
		return strings.ToLower(strings.TrimSpace(effort))
	default:
		return ""
	}
}

func validateReasoningEffort(effort string) error {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "default", "none", "minimal", "low":
		return nil
	default:
		return fmt.Errorf("未知 reasoning_effort %q；仅支持 none / minimal / low", effort)
	}
}

// Explicit names must never fall back to another account/model. Empty names
// retain the existing active-profile behavior for backwards compatibility.
func resolveCompileProfile(cfg Config, req CompileReq) (Profile, error) {
	name := strings.TrimSpace(req.Profile)
	if name != "" {
		if _, exists := cfg.Profiles[name]; !exists {
			return Profile{}, fmt.Errorf("配置方案不存在: %s", name)
		}
	}
	p := getActiveProfile(cfg, name)
	var err error
	p, err = applyCompilerCredentials(p, req.BaseURL, req.APIKey)
	if err != nil {
		return Profile{}, err
	}
	if req.Model != "" {
		p.Model = req.Model
	}
	if req.TokenEncoding != "" {
		p.TokenEncoding = req.TokenEncoding
	}
	if req.ValidationMode != "" {
		p.ValidationMode = req.ValidationMode
	}
	if req.ReaderProfile != nil {
		p.ReaderProfile = strings.TrimSpace(*req.ReaderProfile)
	}
	if req.AuditProfile != nil {
		p.AuditProfile = strings.TrimSpace(*req.AuditProfile)
	}
	p.TokenEncoding = normalizeTokenEncoding(p.TokenEncoding)
	p.ValidationMode = normalizeValidationMode(p.ValidationMode)
	p.ReasoningEffort = normalizeReasoningEffort(p.ReasoningEffort)
	if err := validateTokenEncoding(p.TokenEncoding); err != nil {
		return Profile{}, err
	}
	if err := validateValidationMode(p.ValidationMode); err != nil {
		return Profile{}, err
	}
	if err := validateReasoningEffort(p.ReasoningEffort); err != nil {
		return Profile{}, err
	}
	if p.ValidationMode == "blind" {
		if _, err := resolveReaderProfile(cfg, p); err != nil {
			return Profile{}, err
		}
	}
	if _, err := resolveAuditProfile(cfg, p, p.AuditProfile); err != nil {
		return Profile{}, err
	}
	return effectiveCompilerProfile(p), nil
}

func resolveReaderProfile(cfg Config, comparator Profile) (Profile, error) {
	name := strings.TrimSpace(comparator.ReaderProfile)
	if name == "" {
		return comparator, nil
	}
	reader, ok := cfg.Profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("阅读器配置方案不存在: %s", name)
	}
	return effectiveCompilerProfile(reader), nil
}

// resolveAuditProfile 解析校验/对照所用方案：显式名 > 编译方案上的 audit_profile
// > 编译方案本身。名字必须存在，绝不静默回退到别的账号。
func resolveAuditProfile(cfg Config, compilerProfile Profile, override string) (Profile, error) {
	name := strings.TrimSpace(override)
	if name == "" {
		name = strings.TrimSpace(compilerProfile.AuditProfile)
	}
	if name == "" {
		return compilerProfile, nil
	}
	auditor, ok := cfg.Profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("校验配置方案不存在: %s", name)
	}
	auditor.Name = name
	return effectiveCompilerProfile(auditor), nil
}

func resolveProxyProfile(r *http.Request, cfg Config) (Profile, error) {
	req := CompileReq{
		Profile:        r.Header.Get("X-Babel-Profile"),
		TokenEncoding:  r.Header.Get("X-Babel-Token-Encoding"),
		ValidationMode: r.Header.Get("X-Babel-Validation-Mode"),
	}
	if values, ok := r.Header[http.CanonicalHeaderKey("X-Babel-Reader-Profile")]; ok {
		value := strings.Join(values, ",")
		req.ReaderProfile = &value
	}
	if values, ok := r.Header[http.CanonicalHeaderKey("X-Babel-Audit-Profile")]; ok {
		value := strings.Join(values, ",")
		req.AuditProfile = &value
	}
	return resolveCompileProfile(cfg, req)
}
