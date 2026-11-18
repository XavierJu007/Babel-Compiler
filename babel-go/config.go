package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func configPath() string {
	localCfg := "babel.config.json"
	if _, err := os.Stat(localCfg); err == nil {
		return localCfg
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return localCfg
	}
	return filepath.Join(home, ".babel_config.json")
}

func defaultProfile() Profile {
	base := os.Getenv("BABEL_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := os.Getenv("BABEL_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	return Profile{
		Name:        "default",
		APIKey:      os.Getenv("BABEL_API_KEY"),
		BaseURL:     base,
		Model:       model,
		UpstreamURL: os.Getenv("BABEL_UPSTREAM_URL"),
	}
}

func loadConfig() Config {
	cfg := Config{
		ActiveProfile: "default",
		Profiles:      map[string]Profile{"default": defaultProfile()},
	}
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	var saved Config
	if json.Unmarshal(data, &saved) != nil {
		return cfg
	}
	if saved.Profiles == nil {
		saved.Profiles = make(map[string]Profile)
	}
	if len(saved.Profiles) == 0 && (saved.APIKey != "" || saved.BaseURL != "" || saved.Model != "") {
		p := defaultProfile()
		if saved.APIKey != "" {
			p.APIKey = saved.APIKey
		}
		if saved.BaseURL != "" {
			p.BaseURL = saved.BaseURL
		}
		if saved.Model != "" {
			p.Model = saved.Model
		}
		if saved.UpstreamURL != "" {
			p.UpstreamURL = saved.UpstreamURL
		}
		saved.Profiles["default"] = p
	}
	if len(saved.Profiles) > 0 {
		cfg.Profiles = saved.Profiles
	}
	if saved.ActiveProfile != "" {
		if _, exists := cfg.Profiles[saved.ActiveProfile]; exists {
			cfg.ActiveProfile = saved.ActiveProfile
		}
	}
	return cfg
}

func getActiveProfile(cfg Config, overrideName string) Profile {
	name := cfg.ActiveProfile
	if overrideName != "" {
		name = overrideName
	}
	if p, ok := cfg.Profiles[name]; ok {
		return p
	}
	for _, p := range cfg.Profiles {
		return p
	}
	return defaultProfile()
}

func writeConfigFile(cfg Config) error {
	active := getActiveProfile(cfg, "")
	cfg.APIKey = active.APIKey
	cfg.BaseURL = active.BaseURL
	cfg.Model = active.Model
	cfg.UpstreamURL = active.UpstreamURL
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}

func maskKey(k string) string {
	if len(k) <= 6 {
		if k == "" {
			return ""
		}
		return "***"
	}
	return k[:6] + "***"
}
