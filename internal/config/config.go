package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// ProviderConfig 单个 AI Provider 配置
type ProviderConfig struct {
	Name     string   `json:"name"`
	APIType  string   `json:"api_type"`
	BaseURL  string   `json:"base_url"`
	APIKey   string   `json:"api_key"`
	ProxyURL string   `json:"proxy_url"`
	Models   []string `json:"models"`
	Enabled  bool     `json:"enabled"`
}

type RouteGroupMemberConfig struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type RouteGroupConfig struct {
	Name      string                   `json:"name"`
	Members   []RouteGroupMemberConfig `json:"members"`
	AutoRetry bool                     `json:"auto_retry"`
}

// ListenConfig 监听配置
type ListenConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	Path string `json:"path"`
}

// AuthConfig 前置鉴权配置
type AuthConfig struct {
	Enabled bool     `json:"enabled"`
	Keys    []string `json:"keys"`
}

// ManagementConfig 管理 API 鉴权配置
type ManagementConfig struct {
	Enabled bool     `json:"enabled"`
	Keys    []string `json:"keys"`
}

// ModelRefreshConfig Provider 模型列表自动刷新配置
type ModelRefreshConfig struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
}

// AppConfig 应用总配置
type AppConfig struct {
	Listen       ListenConfig       `json:"listen"`
	Database     DatabaseConfig     `json:"database"`
	Auth         AuthConfig         `json:"auth"`
	Management   ManagementConfig   `json:"management"`
	ModelRefresh ModelRefreshConfig `json:"model_refresh"`
	Providers    []ProviderConfig   `json:"providers"`
	RouteGroups  []RouteGroupConfig `json:"route_groups"`
}

// Load 从文件加载配置
func Load(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 设置默认值
	if cfg.Listen.Host == "" {
		cfg.Listen.Host = "0.0.0.0"
	}
	if cfg.Listen.Port == 0 {
		cfg.Listen.Port = 8080
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path = "./data/providers.db"
	}
	if cfg.ModelRefresh.IntervalSeconds <= 0 {
		cfg.ModelRefresh.IntervalSeconds = 3600
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].APIType == "" {
			cfg.Providers[i].APIType = "openai"
		}
	}

	return &cfg, nil
}

// ListenAddr 返回监听地址
func (c *AppConfig) ListenAddr() string {
	return fmt.Sprintf("%s:%d", c.Listen.Host, c.Listen.Port)
}

// IsAuthEnabled 判断鉴权是否启用（enabled 且至少有一个 key）
func (a *AuthConfig) IsAuthEnabled() bool {
	return a.Enabled && len(a.Keys) > 0
}

// IsValidKey 校验 API Key 是否有效
func (a *AuthConfig) IsValidKey(key string) bool {
	for _, k := range a.Keys {
		if k == key {
			return true
		}
	}
	return false
}

// IsManagementEnabled 判断管理 API 鉴权是否启用
func (m *ManagementConfig) IsManagementEnabled() bool {
	return m.Enabled && len(m.Keys) > 0
}

// IsValidManagementKey 校验管理 API Key 是否有效
func (m *ManagementConfig) IsValidManagementKey(key string) bool {
	for _, k := range m.Keys {
		if k == key {
			return true
		}
	}
	return false
}

// IsModelRefreshEnabled 判断自动刷新是否启用
func (m *ModelRefreshConfig) IsModelRefreshEnabled() bool {
	return m.Enabled && m.IntervalSeconds > 0
}
