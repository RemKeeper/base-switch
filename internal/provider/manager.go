package provider

import (
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

// ModelFetcher 模型获取器函数类型：根据 baseURL 和 apiKey 获取模型列表
type ModelFetcher func(baseURL, apiKey string) ([]string, error)

// AutoDiscoverModels 自动发现 models 为空的 Provider，调用 /v1/models 获取并持久化
// 返回成功发现的 Provider 数量
func (m *Manager) AutoDiscoverModels(fetcher ModelFetcher) (int, error) {
	providers, err := m.store.ListProvidersWithEmptyModels()
	if err != nil {
		return 0, err
	}

	discovered := 0
	for _, p := range providers {
		models, err := fetcher(p.BaseURL, p.APIKey)
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

// AddProvider 新增 Provider
func (m *Manager) AddProvider(name, baseURL, apiKey string, models []string, enabled bool) error {
	return m.store.InsertProvider(name, baseURL, apiKey, models, enabled)
}

// UpdateProvider 更新 Provider
func (m *Manager) UpdateProvider(name, baseURL, apiKey string, models []string, enabled bool) error {
	return m.store.UpdateProvider(name, baseURL, apiKey, models, enabled)
}

// DeleteProvider 删除 Provider
func (m *Manager) DeleteProvider(name string) error {
	return m.store.DeleteProvider(name)
}
