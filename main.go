package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"baseSwitch/internal/config"
	"baseSwitch/internal/plugins/tokenusage"
	"baseSwitch/internal/provider"
	"baseSwitch/internal/proxy"
	"baseSwitch/internal/storage"
)

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	printBanner()
	log.Println("🚀 AI 反向代理服务启动中...")

	// 1. 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("❌ 加载配置失败: %v", err)
	}
	log.Printf("📋 配置加载成功，监听地址: %s", cfg.ListenAddr())

	// 2. 初始化存储
	store, err := storage.New(cfg.Database.Path)
	if err != nil {
		log.Fatalf("❌ 初始化存储失败: %v", err)
	}
	defer store.Close()
	log.Printf("🗄️  数据库已连接: %s", cfg.Database.Path)

	// 3. 从配置文件初始化 Provider
	if err := store.SeedFromConfig(cfg.Providers); err != nil {
		log.Fatalf("❌ 初始化 Provider 失败: %v", err)
	}

	providerCount, _ := store.GetProviderCount()
	log.Printf("🔌 已加载 %d 个 Provider", providerCount)

	// 4. 打印模型列表
	models, _ := store.GetAllModels()
	log.Printf("🤖 可用模型 (%d 个):", len(models))
	for _, m := range models {
		log.Printf("   • %s", m.ID)
	}

	// 5. 创建 Provider 管理器
	mgr := provider.NewManager(store)

	// 初始化 token 用量统计插件
	usagePlugin, err := tokenusage.New("./data/token_usage.db")
	if err != nil {
		log.Fatalf("❌ 初始化 token 用量统计插件失败: %v", err)
	}
	defer usagePlugin.Close()
	log.Println("📊 Token 用量统计插件已启用")

	// 6. 创建代理处理器
	handler := proxy.NewHandler(mgr, &cfg.Auth, &cfg.Management, usagePlugin)

	// 鉴权状态提示
	if cfg.Auth.IsAuthEnabled() {
		log.Printf("🔐 前置 API Key 鉴权已启用 (%d 个密钥)", len(cfg.Auth.Keys))
	} else {
		log.Println("⚠️  未启用前置鉴权，服务对外开放")
	}

	// 管理 API 状态提示
	if cfg.Management.IsManagementEnabled() {
		log.Printf("🛡️  管理 API 已启用 (%d 个管理密钥)", len(cfg.Management.Keys))
	} else {
		log.Println("⚠️  管理 API 未启用，无法通过 API 管理 Provider")
	}

	// 7. 自动发现 models 为空的 Provider
	log.Println("🔍 检查是否需要自动发现模型...")
	discovered, err := mgr.AutoDiscoverModels(handler.FetchProviderModels)
	if err != nil {
		log.Printf("⚠️  自动发现模型失败: %v", err)
	} else if discovered > 0 {
		log.Printf("✅ 自动发现完成，已为 %d 个 Provider 更新模型列表", discovered)
		// 重新打印更新后的模型列表
		models, _ := store.GetAllModels()
		log.Printf("🤖 更新后可用模型 (%d 个):", len(models))
		for _, m := range models {
			log.Printf("   • %s", m.ID)
		}
	} else {
		log.Println("   📋 所有 Provider 均已配置模型，跳过自动发现")
	}

	// 启动 Provider 模型列表自动刷新任务
	stopRefresh := make(chan struct{})
	if cfg.ModelRefresh.IsModelRefreshEnabled() {
		interval := time.Duration(cfg.ModelRefresh.IntervalSeconds) * time.Second
		log.Printf("🔄 Provider 模型列表自动刷新已启用，间隔: %s", interval)
		go startModelRefreshLoop(mgr, handler.FetchProviderModels, interval, stopRefresh)
	} else {
		log.Println("ℹ️  Provider 模型列表自动刷新未启用")
	}

	// 8. 启动 HTTP 服务
	server := &http.Server{
		Addr:    cfg.ListenAddr(),
		Handler: handler.Router(),
	}

	// 优雅关闭
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("🛑 收到信号 %v，正在关闭服务...", sig)
		close(stopRefresh)
		server.Close()
	}()

	log.Printf("✅ 服务已启动: http://%s", cfg.ListenAddr())
	log.Println("   📌 代理端点:")
	log.Println("      GET  /health              - 健康检查")
	log.Println("      GET  /v1/models           - 获取所有模型列表")
	log.Println("      POST /v1/chat/completions - Chat Completions (OpenAI 兼容)")
	log.Println("   📌 模型格式: Provider/model (如 openai/gpt-4o) 或直接 model 名")
	log.Println("   📌 管理端点 (需管理 API Key):")
	log.Println("      GET    /admin/providers        - 列出所有 Provider")
	log.Println("      POST   /admin/providers        - 新增 Provider")
	log.Println("      GET    /admin/providers/:name  - 获取指定 Provider")
	log.Println("      PUT    /admin/providers/:name  - 更新指定 Provider")
	log.Println("      DELETE /admin/providers/:name  - 删除指定 Provider")

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("❌ 服务启动失败: %v", err)
	}

	log.Println("👋 服务已关闭")
}

// printBanner 打印横幅
func printBanner() {
	fmt.Println(`
╔══════════════════════════════════════════╗
║        🤖 AI Reverse Proxy 🤖           ║
║    多 Provider 聚合反向代理服务          ║
╚══════════════════════════════════════════╝`)
}

// startModelRefreshLoop 按固定间隔刷新所有已启用 Provider 的模型列表
func startModelRefreshLoop(mgr *provider.Manager, fetcher provider.ModelFetcher, interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Println("🔄 开始自动刷新 Provider 模型列表...")
			success, failed, errors, err := mgr.RefreshProviderModels(fetcher)
			if err != nil {
				log.Printf("⚠️  自动刷新 Provider 模型列表失败: %v", err)
				continue
			}
			log.Printf("✅ 自动刷新完成，成功: %d，失败: %d", success, failed)
			for _, item := range errors {
				log.Printf("   ⚠️  %s", item)
			}
		case <-stop:
			log.Println("🛑 Provider 模型列表自动刷新任务已停止")
			return
		}
	}
}
