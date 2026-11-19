package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"
)

func domainAPIHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	respondError := func(status int, err error) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"detail": err.Error()})
	}
	respondDomains := func(status string) {
		count, err := reloadDomains()
		body := map[string]any{
			"status": status, "count": count, "domains": listSortedDomains(),
			"public_symbols": listPublicSymbols(),
			"global_dir":     globalDomainsDir(), "workspace_dir": workspaceDomainsDir(),
		}
		if err != nil {
			body["warning"] = err.Error()
		}
		json.NewEncoder(w).Encode(body)
	}
	switch r.Method {
	case http.MethodGet:
		respondDomains("ok")
		return
	case http.MethodPost:
		// The envelope adds a little overhead to the domain file's own limit.
		r.Body = http.MaxBytesReader(w, r.Body, maxDomainFileBytes+2048)
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			respondError(http.StatusBadRequest, fmt.Errorf("领域请求无效: %w", err))
			return
		}
		if !utf8.Valid(raw) {
			respondError(http.StatusBadRequest, fmt.Errorf("领域请求必须为有效 UTF-8，未保存或替换非法字节"))
			return
		}
		var req struct {
			Action string     `json:"action"`
			Scope  string     `json:"scope"`
			Name   string     `json:"name"`
			Domain DomainPack `json:"domain"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			respondError(http.StatusBadRequest, fmt.Errorf("领域请求无效: %w", err))
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			respondError(http.StatusBadRequest, fmt.Errorf("请求必须只有一个 JSON 对象"))
			return
		}
		if req.Action == "reload" {
			respondDomains("reloaded")
			return
		}
		if _, err := domainScopeDir(req.Scope); err != nil {
			respondError(http.StatusBadRequest, err)
			return
		}
		switch req.Action {
		case "save":
			if err := validateDomainPack(req.Domain); err != nil {
				respondError(http.StatusBadRequest, err)
				return
			}
			if err := saveDomain(req.Domain, req.Scope); err != nil {
				respondError(http.StatusInternalServerError, fmt.Errorf("保存领域失败: %w", err))
				return
			}
			respondDomains("saved")
		case "delete":
			if !validDomainName(req.Name) {
				respondError(http.StatusBadRequest, fmt.Errorf("无效的领域名称"))
				return
			}
			if err := deleteDomain(req.Name, req.Scope); err != nil {
				respondError(http.StatusInternalServerError, fmt.Errorf("删除领域覆盖失败: %w", err))
				return
			}
			respondDomains("deleted")
		default:
			respondError(http.StatusBadRequest, fmt.Errorf("未知领域操作: %s", req.Action))
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		respondError(http.StatusMethodNotAllowed, fmt.Errorf("仅支持 GET 或 POST"))
	}
}
