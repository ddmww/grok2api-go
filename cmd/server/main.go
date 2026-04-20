package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ddmww/grok2api-go/internal/app"
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/proxy"
	"github.com/ddmww/grok2api-go/internal/control/refresh"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/config"
	"github.com/ddmww/grok2api-go/internal/platform/logging"
	"github.com/ddmww/grok2api-go/internal/platform/paths"
	"github.com/ddmww/grok2api-go/internal/platform/tasks"
	"github.com/ddmww/grok2api-go/internal/products/admin"
	"github.com/ddmww/grok2api-go/internal/products/anthropic"
	"github.com/ddmww/grok2api-go/internal/products/openai"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	if err := paths.EnsureRuntimeDirs(); err != nil {
		panic(err)
	}
	fileLogging := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FILE_ENABLED")))
	logger := logging.Setup(os.Getenv("LOG_LEVEL"), fileLogging == "1" || fileLogging == "true" || fileLogging == "yes" || fileLogging == "on")
	repo, err := account.NewRepositoryFromEnv()
	if err != nil {
		logger.Error("repository init failed", "error", err)
		panic(err)
	}
	defer repo.Close()
	if err := repo.Initialize(context.Background()); err != nil {
		logger.Error("repository schema init failed", "error", err)
		panic(err)
	}
	var backend config.Backend = config.NewLocalBackend(paths.LocalConfigPath())
	if repo.StorageType() == "mysql" {
		backend = config.NewMySQLBackend(repo.DB())
	}
	cfg := config.New(backend)
	if err := cfg.Load(context.Background()); err != nil {
		logger.Error("config load failed", "error", err)
		panic(err)
	}
	config.LogLoaded(cfg)
	runtime := account.NewRuntime(repo)
	if err := runtime.Sync(context.Background()); err != nil {
		logger.Error("runtime bootstrap failed", "error", err)
		panic(err)
	}
	proxyRuntime := proxy.NewRuntime(cfg)
	xaiClient := xai.NewClient(cfg, proxyRuntime)
	refreshService := refresh.New(repo, runtime, cfg, xaiClient)
	refreshService.Start()
	defer refreshService.Stop()

	state := &app.State{
		Config:  cfg,
		Repo:    repo,
		Runtime: runtime,
		Refresh: refreshService,
		Proxy:   proxyRuntime,
		XAI:     xaiClient,
		Tasks:   tasks.NewStore(),
	}

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	admin.Mount(router, state)
	openai.Mount(router, state)
	anthropic.Mount(router, state)

	host := os.Getenv("SERVER_HOST")
	if host == "" {
		host = "0.0.0.0"
	}
	port := os.Getenv("SERVER_PORT")
	if port == "" {
		port = "8000"
	}
	server := &http.Server{
		Addr:              host + ":" + port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("server starting", "addr", server.Addr, "storage", repo.StorageType(), "runtime_size", runtime.Size())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server exited", "error", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
