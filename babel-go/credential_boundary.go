package main

import (
	"fmt"
	"net/url"
	"strings"
)

func explicitAPIKey(key string) bool {
	return strings.TrimSpace(key) != "" && !strings.Contains(key, "***")
}

// Credentials belong to an origin, not to an arbitrary URL supplied by a
// compile/probe request. Changing only a path on that origin remains supported.
func compilerCredentialOrigin(raw string) (string, bool) {
	u, err := url.Parse(compilerRequestURL(raw))
	if err != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return u.Scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port, true
}

func sameCompilerCredentialOrigin(a, b string) bool {
	x, xOK := compilerCredentialOrigin(a)
	y, yOK := compilerCredentialOrigin(b)
	return xOK && yOK && x == y
}

func applyCompilerCredentials(p Profile, baseURL, apiKey string) (Profile, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL != "" {
		if p.APIKey != "" && !explicitAPIKey(apiKey) && !sameCompilerCredentialOrigin(p.BaseURL, baseURL) {
			return Profile{}, fmt.Errorf("更换服务地址的协议、主机或端口时，必须显式填写该服务的 API Key；不会向新地址发送已保存的密钥")
		}
		p.BaseURL = baseURL
	}
	if explicitAPIKey(apiKey) {
		p.APIKey = strings.TrimSpace(apiKey)
	}
	return p, nil
}
