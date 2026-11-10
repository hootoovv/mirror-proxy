// mirror-proxy：多源镜像缓存代理服务器。
//
// 用法：
//
//	mirror-proxy [-config config.yaml]
//
// 环境变量覆盖：MIRROR_PROXY_CONFIG / MIRROR_PROXY_ADDR / MIRROR_PROXY_CACHE_DIR。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"mirror-proxy/internal/cache"
	"mirror-proxy/internal/config"
	"mirror-proxy/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()
	if v := os.Getenv("MIRROR_PROXY_CONFIG"); v != "" {
		*configPath = v
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	store, err := cache.New(cfg.Server.CacheDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化缓存目录失败: %v\n", err)
		os.Exit(1)
	}

	// 启动时扫描缓存目录，加载到内存索引。
	scanStart := time.Now()
	scanStats, err := store.Scan()
	if err != nil {
		fmt.Fprintf(os.Stderr, "扫描缓存目录失败: %v\n", err)
		os.Exit(1)
	}
	var totalFiles, totalBytes int64
	for _, st := range scanStats {
		totalFiles += st.Files
		totalBytes += st.Bytes
	}
	for typeName, st := range scanStats {
		logger.Info("缓存扫描完成", "source", typeName, "files", st.Files, "bytes", st.Bytes)
	}
	logger.Info("缓存索引就绪",
		"entries", store.Count(), "files", totalFiles, "bytes", totalBytes, "duration", time.Since(scanStart).String())

	prox, err := proxy.New(cfg, store, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化代理失败: %v\n", err)
		os.Exit(1)
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery(), gin.Logger())
	prox.Register(engine)

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           engine,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Info("mirror-proxy 已启动", "addr", cfg.Server.Addr, "cache_dir", cfg.Server.CacheDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "HTTP 服务异常退出: %v\n", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("收到退出信号，正在优雅停机（最长等待 10s）…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Warn("优雅停机超时", "error", err.Error())
	}
	logger.Info("已退出")
}
