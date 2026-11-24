package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Validate an existing candidate without paying for generation or trusting its
// author's declaration of validity. A completed rejection is a normal result.
func candidateValidationHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	writeError := func(status int, err error) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"detail": err.Error()})
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(http.StatusMethodNotAllowed, fmt.Errorf("仅支持 POST"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*1024*1024)
	defer r.Body.Close()
	var req struct {
		CompileReq
		Candidate string `json:"candidate"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(http.StatusBadRequest, fmt.Errorf("无效验证请求: %w", err))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeError(http.StatusBadRequest, fmt.Errorf("请求必须只有一个 JSON 对象"))
		return
	}
	if strings.TrimSpace(req.Text) == "" || strings.TrimSpace(req.Candidate) == "" || !utf8.ValidString(req.Text) || !utf8.ValidString(req.Candidate) {
		writeError(http.StatusBadRequest, fmt.Errorf("原文和候选必须是非空 UTF-8 文本"))
		return
	}
	cfg := loadConfig()
	p, err := resolveCompileProfile(cfg, req.CompileReq)
	if err != nil {
		writeError(http.StatusBadRequest, err)
		return
	}
	auditor, err := resolveAuditProfile(cfg, p, "")
	if err != nil {
		writeError(http.StatusBadRequest, err)
		return
	}
	var result ValidationResult
	if p.ValidationMode == "blind" {
		reader, err := resolveReaderProfile(cfg, p)
		if err != nil {
			writeError(http.StatusBadRequest, err)
			return
		}
		result = runBlindValidationContext(r.Context(), reader, auditor, req.Text, req.Candidate, req.RequiredKeywords)
	} else {
		result = runValidationContext(r.Context(), auditor, req.Text, req.Candidate, req.RequiredKeywords)
	}
	if result.Status == "error" {
		w.WriteHeader(http.StatusBadGateway)
	}
	json.NewEncoder(w).Encode(map[string]any{"validation": result})
}
