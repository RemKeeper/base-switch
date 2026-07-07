package provider

import (
	"fmt"
	"sync"

	"baseSwitch/internal/storage"
)

// Manager Provider 管理器
type Manager struct {
	store *storage.Store
	mu    sync.RWMutex
}

// NewManager 创建管理器
func NewManager(store *storage.Store) *Manager {
	return &Manager{store: store}
}

// GetAllModels 获取所有聚合模型
func (m *Manager) GetAllModels() ([]storage.ModelEntry, error) {
	return m.store.GetAllModels()
}

// ResolveModel 解析模型到具体 Provider
func (m *Manager) ResolveModel(modelID string) (*storage.Provider, string, error) {
	return m.store.ResolveModel(modelID)
}

// GetProviderCount 获取 Provider 数量
func (m *Manager) GetProviderCount() (int, error) {
	return m.store.GetProviderCount()
}

// ModelFetcher 模型获取器函数类型：根据 baseURL、apiKey 和代理地址获取模型列表
type ModelFetcher func(baseURL, apiKey, proxyURL string) ([]string, error)

// AutoDiscoverModels 自动发现 models 为空的 Provider，调用 /v1/models 获取并持久化
// 返回成功发现的 Provider 数量
func (m *Manager) AutoDiscoverModels(fetcher ModelFetcher) (int, error) {
	providers, err := m.store.ListProvidersWithEmptyModels()
	if err != nil {
		return 0, err
	}

	discovered := 0
	for _, p := range providers {
		models, err := fetcher(p.BaseURL, p.APIKey, p.ProxyURL)
		if err != nil {
			continue // 跳过失败的 provider，继续下一个
		}
		if len(models) == 0 {
			continue
		}
		if err := m.store.UpdateProviderModels(p.Name, models); err != nil {
			continue
		}
		discovered++
	}
	return discovered, nil
}

// RefreshProviderModels 刷新所有已启用 Provider 的模型列表
// 返回成功刷新数量、失败数量和失败详情
func (m *Manager) RefreshProviderModels(fetcher ModelFetcher) (int, int, []string, error) {
	providers, err := m.store.ListEnabledProviders()
	if err != nil {
		return 0, 0, nil, err
	}

	success := 0
	failed := 0
	errors := []string{}

	for _, p := range providers {
		models, err := fetcher(p.BaseURL, p.APIKey, p.ProxyURL)
		if err != nil {
			failed++
			errors = append(errors, fmt.Sprintf("%s: %v", p.Name, err))
			continue
		}
		if len(models) == 0 {
			failed++
			errors = append(errors, fmt.Sprintf("%s: 未发现任何模型", p.Name))
			continue
		}
		if err := m.store.UpdateProviderModels(p.Name, models); err != nil {
			failed++
			errors = append(errors, fmt.Sprintf("%s: %v", p.Name, err))
			continue
		}
		success++
	}

	return success, failed, errors, nil
}

// GetStore 暴露底层存储（供 proxy 层模型获取使用）
func (m *Manager) GetStore() *storage.Store {
	return m.store
}

// ──────────────── Provider 管理操作 ────────────────

// ListAllProviders 列出所有 Provider（包括禁用的）
func (m *Manager) ListAllProviders() ([]storage.Provider, error) {
	return m.store.ListAllProviders()
}

// GetProviderByName 根据名称获取 Provider
func (m *Manager) GetProviderByName(name string) (*storage.Provider, error) {
	return m.store.GetProviderByName(name)
}

// AddProvider 新增 Provider；models 为空时自动调用 Provider 的 /v1/models 发现模型
func (m *Manager) AddProvider(name, baseURL, apiKey, proxyURL string, models []string, enabled bool, fetcher ModelFetcher) error {
	if len(models) == 0 && fetcher != nil {
		discoveredModels, err := fetcher(baseURL, apiKey, proxyURL)
		if err != nil {
			return fmt.Errorf("自动发现 provider %s 模型失败: %w", name, err)
		}
		if len(discoveredModels) == 0 {
			return fmt.Errorf("自动发现 provider %s 模型失败: 未发现任何模型", name)
		}
		models = discoveredModels
	}

	return m.store.InsertProvider(name, baseURL, apiKey, proxyURL, models, enabled)
}

// UpdateProvider 更新 Provider
func (m *Manager) UpdateProvider(name, baseURL, apiKey, proxyURL string, models []string, enabled bool) error {
	return m.store.UpdateProvider(name, baseURL, apiKey, proxyURL, models, enabled)
}

// DeleteProvider 删除 Provider
func (m *Manager) DeleteProvider(name string) error {
	return m.store.DeleteProvider(name)
}
