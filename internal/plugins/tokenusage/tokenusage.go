package tokenusage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Usage 表示一次上游请求返回的 token 消耗。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// UnmarshalJSON 同时兼容 Chat Completions 与 Responses API 的 token 字段名。
func (u *Usage) UnmarshalJSON(data []byte) error {
	var raw struct {
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
		InputTokens      *int `json:"input_tokens"`
		OutputTokens     *int `json:"output_tokens"`
		TotalTokens      *int `json:"total_tokens"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	u.PromptTokens = firstTokenValue(raw.PromptTokens, raw.InputTokens)
	u.CompletionTokens = firstTokenValue(raw.CompletionTokens, raw.OutputTokens)
	if raw.TotalTokens != nil {
		u.TotalTokens = *raw.TotalTokens
	} else {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return nil
}

func firstTokenValue(primary, fallback *int) int {
	if primary != nil {
		return *primary
	}
	if fallback != nil {
		return *fallback
	}
	return 0
}

// Record 表示一条 token 消耗记录。
type Record struct {
	ID               int64     `json:"id"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	Endpoint         string    `json:"endpoint"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	CreatedAt        time.Time `json:"created_at"`
}

// TableName 指定 token 用量明细表名。
func (Record) TableName() string {
	return "token_usage_records"
}

// Summary 表示按 provider / model 聚合后的 token 消耗。
type Summary struct {
	Provider         string `json:"provider"`
	Model            string `json:"model,omitempty"`
	RequestCount     int64  `json:"request_count"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
}

// Query 查询条件。
type Query struct {
	Provider string
	Model    string
	GroupBy  string
	Limit    int
}

// Plugin token 用量统计插件。
type Plugin struct {
	db *gorm.DB
}

// New 创建 token 用量统计插件。
func New(dbPath string) (*Plugin, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建 token 统计数据库目录失败: %w", err)
	}

	db, err := gorm.Open(sqlite.Open(dbPath+"?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("打开 token 统计数据库失败: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取 token 统计数据库连接失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(time.Hour)

	p := &Plugin{db: db}
	if err := p.migrate(); err != nil {
		return nil, fmt.Errorf("token 统计数据库迁移失败: %w", err)
	}
	return p, nil
}

func (p *Plugin) migrate() error {
	if err := p.db.AutoMigrate(&Record{}); err != nil {
		return err
	}
	if err := p.db.Exec(`CREATE INDEX IF NOT EXISTS idx_token_usage_provider ON token_usage_records(provider)`).Error; err != nil {
		return err
	}
	if err := p.db.Exec(`CREATE INDEX IF NOT EXISTS idx_token_usage_model ON token_usage_records(model)`).Error; err != nil {
		return err
	}
	return p.db.Exec(`CREATE INDEX IF NOT EXISTS idx_token_usage_created_at ON token_usage_records(created_at)`).Error
}

// Close 关闭插件。
func (p *Plugin) Close() error {
	sqlDB, err := p.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// RecordUsage 写入一次 token 消耗；usage 为 nil 时写入 0 token 记录，用于统计请求次数。
func (p *Plugin) RecordUsage(provider, model, endpoint string, usage *Usage) error {
	if p == nil {
		return nil
	}
	if usage == nil {
		usage = &Usage{}
	}
	return p.db.Create(&Record{
		Provider:         provider,
		Model:            model,
		Endpoint:         endpoint,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
	}).Error
}

// Summary 查询聚合统计。groupBy 支持 provider / model，默认 model。
func (p *Plugin) Summary(q Query) ([]Summary, error) {
	if p == nil {
		return []Summary{}, nil
	}
	groupByProviderOnly := q.GroupBy == "provider"
	selectModel := "model"
	groupBy := "provider, model"
	if groupByProviderOnly {
		selectModel = "'' AS model"
		groupBy = "provider"
	}

	result := []Summary{}
	db := applyFilters(p.db.Model(&Record{}), q)
	err := db.Select(fmt.Sprintf(`provider, %s, COUNT(*) AS request_count, COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens, COALESCE(SUM(completion_tokens), 0) AS completion_tokens, COALESCE(SUM(total_tokens), 0) AS total_tokens`, selectModel)).
		Group(groupBy).
		Order("total_tokens DESC").
		Order("request_count DESC").
		Scan(&result).Error
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Records 查询明细记录。
func (p *Plugin) Records(q Query) ([]Record, error) {
	if p == nil {
		return []Record{}, nil
	}
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	result := []Record{}
	if err := applyFilters(p.db.Model(&Record{}), q).Order("id DESC").Limit(limit).Find(&result).Error; err != nil {
		return nil, err
	}
	return result, nil
}

func applyFilters(db *gorm.DB, q Query) *gorm.DB {
	if q.Provider != "" {
		db = db.Where("provider = ?", q.Provider)
	}
	if q.Model != "" {
		db = db.Where("model = ?", q.Model)
	}
	return db
}

// ExtractUsageFromJSON 从非流式 OpenAI 兼容 JSON 响应中提取 usage。
func ExtractUsageFromJSON(body []byte) *Usage {
	var raw struct {
		Usage    *Usage `json:"usage"`
		Response *struct {
			Usage *Usage `json:"usage"`
		} `json:"response"`
		Result *struct {
			Usage *Usage `json:"usage"`
		} `json:"result"`
		Data *struct {
			Usage *Usage `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	if raw.Usage != nil {
		return normalizeUsage(raw.Usage)
	}
	if raw.Response != nil && raw.Response.Usage != nil {
		return normalizeUsage(raw.Response.Usage)
	}
	if raw.Result != nil && raw.Result.Usage != nil {
		return normalizeUsage(raw.Result.Usage)
	}
	if raw.Data != nil && raw.Data.Usage != nil {
		return normalizeUsage(raw.Data.Usage)
	}
	return nil
}

// ExtractUsageFromSSE 从 SSE 文本中提取 usage，支持 OpenAI stream_options.include_usage 返回的 chunk。
func ExtractUsageFromSSE(body []byte) *Usage {
	var found *Usage
	var eventData []byte
	flushEvent := func() {
		if len(eventData) == 0 {
			return
		}
		payload := bytes.TrimSpace(eventData)
		if !bytes.Equal(payload, []byte("[DONE]")) {
			if usage := ExtractUsageFromJSON(payload); usage != nil {
				found = usage
			}
		}
		eventData = eventData[:0]
	}

	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			flushEvent()
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data:"))
		if len(payload) > 0 && payload[0] == ' ' {
			payload = payload[1:]
		}
		if len(eventData) > 0 {
			eventData = append(eventData, '\n')
		}
		eventData = append(eventData, payload...)
	}
	flushEvent()
	return found
}

func normalizeUsage(usage *Usage) *Usage {
	if usage == nil {
		return nil
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage
}
