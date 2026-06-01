package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"baseSwitch/internal/config"

	_ "modernc.org/sqlite"
)

// Provider 存储的 Provider 记录
type Provider struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	BaseURL   string    `json:"base_url"`
	APIKey    string    `json:"api_key"`
	Models    string    `json:"models"` // 逗号分隔
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ModelEntry 聚合后的模型条目
type ModelEntry struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Object   string `json:"object"`
	Created  int64  `json:"created"`
	OwnedBy  string `json:"owned_by"`
}

// Store SQLite 存储
type Store struct {
	db *sql.DB
	mu sync.RWMutex
	// 模型缓存
	modelsCache []ModelEntry
	cacheTime   time.Time
	cacheTTL    time.Duration
}

// New 创建存储实例
func New(dbPath string) (*Store, error) {
	// 确保目录存在
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 连接池配置
	db.SetMaxOpenConns(1) // SQLite 单写
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Hour)

	s := &Store{
		db:       db,
		cacheTTL: 30 * time.Second,
	}

	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	return s, nil
}

// migrate 创建表
func (s *Store) migrate() error {
	query := `
	CREATE TABLE IF NOT EXISTS providers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		base_url TEXT NOT NULL,
		api_key TEXT NOT NULL DEFAULT '',
		models TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_providers_name ON providers(name);
	CREATE INDEX IF NOT EXISTS idx_providers_enabled ON providers(enabled);
	`
	_, err := s.db.Exec(query)
	return err
}

// Close 关闭数据库
func (s *Store) Close() error {
	return s.db.Close()
}

// SeedFromConfig 从配置文件初始化 Provider
func (s *Store) SeedFromConfig(providers []config.ProviderConfig) error {
	for _, p := range providers {
		models := strings.Join(p.Models, ",")
		enabled := 0
		if p.Enabled {
			enabled = 1
		}
		_, err := s.db.Exec(
			`INSERT OR REPLACE INTO providers (name, base_url, api_key, models, enabled, updated_at)
			 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
			p.Name, p.BaseURL, p.APIKey, models, enabled,
		)
		if err != nil {
			return fmt.Errorf("插入 provider %s 失败: %w", p.Name, err)
		}
	}
	s.invalidateCache()
	return nil
}

// ListEnabledProviders 列出所有启用的 Provider
func (s *Store) ListEnabledProviders() ([]Provider, error) {
	rows, err := s.db.Query(
		`SELECT id, name, base_url, api_key, models, enabled, created_at, updated_at
		 FROM providers WHERE enabled = 1 ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var providers []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.Models, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	return providers, rows.Err()
}

// GetAllModels 获取所有启用 Provider 的聚合模型列表
func (s *Store) GetAllModels() ([]ModelEntry, error) {
	s.mu.RLock()
	if time.Since(s.cacheTime) < s.cacheTTL && len(s.modelsCache) > 0 {
		cache := s.modelsCache
		s.mu.RUnlock()
		return cache, nil
	}
	s.mu.RUnlock()

	// 重建缓存
	s.mu.Lock()
	defer s.mu.Unlock()

	// double check
	if time.Since(s.cacheTime) < s.cacheTTL && len(s.modelsCache) > 0 {
		return s.modelsCache, nil
	}

	providers, err := s.ListEnabledProviders()
	if err != nil {
		return nil, err
	}

	var models []ModelEntry
	for _, p := range providers {
		if p.Models == "" {
			continue
		}
		for _, model := range strings.Split(p.Models, ",") {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			models = append(models, ModelEntry{
				ID:       fmt.Sprintf("%s/%s", p.Name, model),
				Provider: p.Name,
				Object:   "model",
				Created:  p.CreatedAt.Unix(),
				OwnedBy:  p.Name,
			})
		}
	}

	s.modelsCache = models
	s.cacheTime = time.Now()
	return models, nil
}

// FindProvider 根据名称查找 Provider
func (s *Store) FindProvider(name string) (*Provider, error) {
	var p Provider
	err := s.db.QueryRow(
		`SELECT id, name, base_url, api_key, models, enabled, created_at, updated_at
		 FROM providers WHERE name = ? AND enabled = 1`,
		name,
	).Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.Models, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ResolveModel 解析 "Provider/model" 或 "model" 格式
// 返回 provider 和实际 model 名
func (s *Store) ResolveModel(modelID string) (*Provider, string, error) {
	parts := strings.SplitN(modelID, "/", 2)
	if len(parts) == 2 {
		// 明确指定了 Provider/model
		p, err := s.FindProvider(parts[0])
		if err != nil {
			return nil, "", err
		}
		if p == nil {
			return nil, "", fmt.Errorf("provider '%s' 不存在或未启用", parts[0])
		}
		return p, parts[1], nil
	}

	// 只有 model 名，搜索所有 provider
	providers, err := s.ListEnabledProviders()
	if err != nil {
		return nil, "", err
	}

	for _, p := range providers {
		if p.Models == "" {
			continue
		}
		for _, m := range strings.Split(p.Models, ",") {
			if strings.TrimSpace(m) == modelID {
				return &p, modelID, nil
			}
		}
	}

	return nil, "", fmt.Errorf("model '%s' 在所有 provider 中未找到", modelID)
}

func (s *Store) invalidateCache() {
	s.mu.Lock()
	s.modelsCache = nil
	s.cacheTime = time.Time{}
	s.mu.Unlock()
}

// GetProviderCount 获取 Provider 数量
func (s *Store) GetProviderCount() (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM providers WHERE enabled = 1").Scan(&count)
	return count, err
}

// ListProvidersWithEmptyModels 列出已启用但 models 为空的 Provider
func (s *Store) ListProvidersWithEmptyModels() ([]Provider, error) {
	rows, err := s.db.Query(
		`SELECT id, name, base_url, api_key, models, enabled, created_at, updated_at
		 FROM providers WHERE enabled = 1 AND (models IS NULL OR models = '') ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var providers []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.Models, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	return providers, rows.Err()
}

// UpdateProviderModels 更新指定 Provider 的模型列表
func (s *Store) UpdateProviderModels(name string, models []string) error {
	modelsStr := strings.Join(models, ",")
	_, err := s.db.Exec(
		`UPDATE providers SET models = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`,
		modelsStr, name,
	)
	if err != nil {
		return fmt.Errorf("更新 provider %s 模型列表失败: %w", name, err)
	}
	s.invalidateCache()
	return nil
}

// ──────────────── Provider CRUD（管理 API 用）────────────────

// ListAllProviders 列出所有 Provider（包括禁用的）
func (s *Store) ListAllProviders() ([]Provider, error) {
	rows, err := s.db.Query(
		`SELECT id, name, base_url, api_key, models, enabled, created_at, updated_at
		 FROM providers ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var providers []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.Models, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	return providers, rows.Err()
}

// GetProviderByName 根据名称获取 Provider（包括禁用的）
func (s *Store) GetProviderByName(name string) (*Provider, error) {
	var p Provider
	err := s.db.QueryRow(
		`SELECT id, name, base_url, api_key, models, enabled, created_at, updated_at
		 FROM providers WHERE name = ?`,
		name,
	).Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.Models, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// InsertProvider 新增 Provider
func (s *Store) InsertProvider(name, baseURL, apiKey string, models []string, enabled bool) error {
	modelsStr := strings.Join(models, ",")
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO providers (name, base_url, api_key, models, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		name, baseURL, apiKey, modelsStr, enabledInt,
	)
	if err != nil {
		return fmt.Errorf("新增 provider %s 失败: %w", name, err)
	}
	s.invalidateCache()
	return nil
}

// UpdateProvider 更新 Provider（按 name 查找）
func (s *Store) UpdateProvider(name, baseURL, apiKey string, models []string, enabled bool) error {
	modelsStr := strings.Join(models, ",")
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	result, err := s.db.Exec(
		`UPDATE providers SET base_url = ?, api_key = ?, models = ?, enabled = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE name = ?`,
		baseURL, apiKey, modelsStr, enabledInt, name,
	)
	if err != nil {
		return fmt.Errorf("更新 provider %s 失败: %w", name, err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("provider '%s' 不存在", name)
	}
	s.invalidateCache()
	return nil
}

// DeleteProvider 删除 Provider
func (s *Store) DeleteProvider(name string) error {
	result, err := s.db.Exec(`DELETE FROM providers WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("删除 provider %s 失败: %w", name, err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("provider '%s' 不存在", name)
	}
	s.invalidateCache()
	return nil
}
