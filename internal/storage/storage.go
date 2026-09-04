package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"baseSwitch/internal/config"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Provider 存储的 Provider 记录
type Provider struct {
	ID        int64     `json:"id" gorm:"primaryKey;autoIncrement"`
	Name      string    `json:"name" gorm:"uniqueIndex;not null"`
	APIType   string    `json:"api_type" gorm:"not null;default:openai"`
	BaseURL   string    `json:"base_url" gorm:"not null"`
	APIKey    string    `json:"api_key" gorm:"not null;default:''"`
	ProxyURL  string    `json:"proxy_url" gorm:"not null;default:''"`
	Models    string    `json:"models" gorm:"not null;default:''"` // 逗号分隔
	Enabled   bool      `json:"enabled" gorm:"not null;default:true;index"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type RouteGroup struct {
	ID        int64     `json:"id" gorm:"primaryKey;autoIncrement"`
	Name      string    `json:"name" gorm:"uniqueIndex;not null"`
	Members   string    `json:"members" gorm:"not null;default:'[]'"`
	AutoRetry bool      `json:"auto_retry" gorm:"not null;default:false"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type RouteGroupMember struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// TableName 指定 Provider 表名，保持兼容已有 SQLite 表。
func (Provider) TableName() string {
	return "providers"
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
	db *gorm.DB
	mu sync.RWMutex
	// 模型缓存
	modelsCache []ModelEntry
	cacheTime   time.Time
	cacheTTL    time.Duration
}

// New 创建存储实例
func New(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}

	db, err := gorm.Open(sqlite.Open(dbPath+"?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取数据库连接失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(time.Hour)

	s := &Store{db: db, cacheTTL: 30 * time.Second}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}
	return s, nil
}

// migrate 创建表
func (s *Store) migrate() error {
	return s.db.AutoMigrate(&Provider{}, &RouteGroup{})
}

// Close 关闭数据库
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// SeedFromConfig 从配置文件初始化 Provider
func (s *Store) SeedFromConfig(providers []config.ProviderConfig) error {
	for _, p := range providers {
		provider := Provider{
			Name:     p.Name,
			APIType:  p.APIType,
			BaseURL:  p.BaseURL,
			APIKey:   p.APIKey,
			ProxyURL: p.ProxyURL,
			Models:   strings.Join(p.Models, ","),
			Enabled:  p.Enabled,
		}
		err := s.db.Where("name = ?", p.Name).Assign(map[string]any{
			"api_type":  p.APIType,
			"base_url":  p.BaseURL,
			"api_key":   p.APIKey,
			"proxy_url": p.ProxyURL,
			"models":    strings.Join(p.Models, ","),
			"enabled":   p.Enabled,
		}).FirstOrCreate(&provider).Error
		if err != nil {
			return fmt.Errorf("插入 provider %s 失败: %w", p.Name, err)
		}
	}
	s.invalidateCache()
	return nil
}

func (s *Store) SeedRouteGroups(groups []config.RouteGroupConfig) error {
	for _, g := range groups {
		members, err := json.Marshal(g.Members)
		if err != nil {
			return fmt.Errorf("编码路由分组 %s 失败: %w", g.Name, err)
		}
		row := RouteGroup{Name: g.Name, Members: string(members), AutoRetry: g.AutoRetry}
		if err := s.db.Where("name = ?", g.Name).Assign(map[string]any{"members": string(members), "auto_retry": g.AutoRetry}).FirstOrCreate(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListRouteGroups() ([]RouteGroup, error) {
	var rows []RouteGroup
	err := s.db.Order("name").Find(&rows).Error
	return rows, err
}
func (s *Store) GetRouteGroup(name string) (*RouteGroup, error) {
	var row RouteGroup
	err := s.db.Where("name = ?", name).First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &row, err
}
func (s *Store) SaveRouteGroup(name string, members []RouteGroupMember, autoRetry bool) error {
	data, err := json.Marshal(members)
	if err != nil {
		return err
	}
	row := RouteGroup{Name: name, Members: string(data), AutoRetry: autoRetry}
	if err := s.db.Create(&row).Error; err != nil {
		return fmt.Errorf("新增路由分组 %s 失败: %w", name, err)
	}
	return nil
}
func (s *Store) UpdateRouteGroup(name string, members []RouteGroupMember, autoRetry bool) error {
	data, err := json.Marshal(members)
	if err != nil {
		return err
	}
	result := s.db.Model(&RouteGroup{}).Where("name = ?", name).Updates(map[string]any{"members": string(data), "auto_retry": autoRetry})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("路由分组 '%s' 不存在", name)
	}
	return nil
}
func (s *Store) DeleteRouteGroup(name string) error {
	result := s.db.Where("name = ?", name).Delete(&RouteGroup{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("路由分组 '%s' 不存在", name)
	}
	return nil
}
func (s *Store) GroupMembers(row *RouteGroup) ([]RouteGroupMember, error) {
	var members []RouteGroupMember
	if err := json.Unmarshal([]byte(row.Members), &members); err != nil {
		return nil, err
	}
	return members, nil
}

// ListEnabledProviders 列出所有启用的 Provider
func (s *Store) ListEnabledProviders() ([]Provider, error) {
	var providers []Provider
	err := s.db.Where("enabled = ?", true).Order("name").Find(&providers).Error
	return providers, err
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

	s.mu.Lock()
	defer s.mu.Unlock()

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

// FindProvider 根据名称查找启用的 Provider
func (s *Store) FindProvider(name string) (*Provider, error) {
	var p Provider
	err := s.db.Where("name = ? AND enabled = ?", name, true).First(&p).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ResolveModel 解析 "Provider/model" 或 "model" 格式
func (s *Store) ResolveModel(modelID string) (*Provider, string, error) {
	parts := strings.SplitN(modelID, "/", 2)
	if len(parts) == 2 {
		p, err := s.FindProvider(parts[0])
		if err != nil {
			return nil, "", err
		}
		if p == nil {
			return nil, "", fmt.Errorf("provider '%s' 不存在或未启用", parts[0])
		}
		return p, parts[1], nil
	}

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
	var count int64
	err := s.db.Model(&Provider{}).Where("enabled = ?", true).Count(&count).Error
	return int(count), err
}

// ListProvidersWithEmptyModels 列出已启用但 models 为空的 Provider
func (s *Store) ListProvidersWithEmptyModels() ([]Provider, error) {
	var providers []Provider
	err := s.db.Where("enabled = ? AND (models IS NULL OR models = '')", true).Order("name").Find(&providers).Error
	return providers, err
}

// UpdateProviderModels 更新指定 Provider 的模型列表
func (s *Store) UpdateProviderModels(name string, models []string) error {
	result := s.db.Model(&Provider{}).Where("name = ?", name).Updates(map[string]any{"models": strings.Join(models, ",")})
	if result.Error != nil {
		return fmt.Errorf("更新 provider %s 模型列表失败: %w", name, result.Error)
	}
	s.invalidateCache()
	return nil
}

// ──────────────── Provider CRUD（管理 API 用）────────────────

// ListAllProviders 列出所有 Provider（包括禁用的）
func (s *Store) ListAllProviders() ([]Provider, error) {
	var providers []Provider
	err := s.db.Order("name").Find(&providers).Error
	return providers, err
}

// GetProviderByName 根据名称获取 Provider（包括禁用的）
func (s *Store) GetProviderByName(name string) (*Provider, error) {
	var p Provider
	err := s.db.Where("name = ?", name).First(&p).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// InsertProvider 新增 Provider
func (s *Store) InsertProvider(name, apiType, baseURL, apiKey, proxyURL string, models []string, enabled bool) error {
	p := Provider{Name: name, APIType: apiType, BaseURL: baseURL, APIKey: apiKey, ProxyURL: proxyURL, Models: strings.Join(models, ","), Enabled: enabled}
	if err := s.db.Create(&p).Error; err != nil {
		return fmt.Errorf("新增 provider %s 失败: %w", name, err)
	}
	s.invalidateCache()
	return nil
}

// UpdateProvider 更新 Provider（按 name 查找）
func (s *Store) UpdateProvider(name, apiType, baseURL, apiKey, proxyURL string, models []string, enabled bool) error {
	result := s.db.Model(&Provider{}).Where("name = ?", name).Updates(map[string]any{
		"api_type":  apiType,
		"base_url":  baseURL,
		"api_key":   apiKey,
		"proxy_url": proxyURL,
		"models":    strings.Join(models, ","),
		"enabled":   enabled,
	})
	if result.Error != nil {
		return fmt.Errorf("更新 provider %s 失败: %w", name, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("provider '%s' 不存在", name)
	}
	s.invalidateCache()
	return nil
}

// DeleteProvider 删除 Provider
func (s *Store) DeleteProvider(name string) error {
	result := s.db.Where("name = ?", name).Delete(&Provider{})
	if result.Error != nil {
		return fmt.Errorf("删除 provider %s 失败: %w", name, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("provider '%s' 不存在", name)
	}
	s.invalidateCache()
	return nil
}
