// Package app 组合配置、日志存储、DNS 内核与内部 API，管理进程启动与退出。
package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/CedarHuang/Namesplit/internal/api"
	"github.com/CedarHuang/Namesplit/internal/config"
	"github.com/CedarHuang/Namesplit/internal/dnskernel"
	"github.com/CedarHuang/Namesplit/internal/logstore"
	"github.com/CedarHuang/Namesplit/internal/version"
)

// Options 是进程启动参数。
type Options struct {
	// Token 是 NAMESPLIT_TOKEN 的值。
	Token string
	// DataDir 是数据目录；为空时使用 %LOCALAPPDATA%\Namesplit。
	DataDir string
	// Stdout 用于 READY 行；默认 os.Stdout。
	Stdout io.Writer
	// Stderr 用于日志与错误；默认 os.Stderr。
	Stderr io.Writer
	// Targets 覆盖 DNS 监听目标；仅供测试注入，产品路径留空。
	Targets []dnskernel.Target
}

// App 是 sidecar 进程的组合根。
type App struct {
	options Options
	logger  *slog.Logger

	dataDir    string
	configPath string

	cfg   *config.Manager
	store *runtimeStore
	logs  *logstore.Store

	kernel     *dnskernel.Server
	api        *api.Server
	httpServer *http.Server
	listener   net.Listener

	startedAt time.Time
	done      chan struct{}
	doneOnce  sync.Once
}

// Run 运行 sidecar，返回进程退出码。
func Run(ctx context.Context, opts Options) int {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	if err := validateToken(opts.Token); err != nil {
		fmt.Fprintf(stderr, "NAMESPLIT_TOKEN 非法: %v\n", err)

		return 2
	}

	dataDir, err := resolveDataDir(opts.DataDir)
	if err != nil {
		fmt.Fprintf(stderr, "无法确定数据目录: %v\n", err)

		return 1
	}

	logger := newLogger(stderr, os.Getenv("NAMESPLIT_LOG_LEVEL"))

	app := &App{
		options:    opts,
		logger:     logger,
		dataDir:    dataDir,
		configPath: filepath.Join(dataDir, "config.toml"),
		store:      &runtimeStore{},
		startedAt:  time.Now(),
		done:       make(chan struct{}),
	}

	if err := app.loadConfig(); err != nil {
		payload, _ := json.Marshal(err)
		fmt.Fprintf(stderr, "NAMESPLIT_CONFIG_ERROR %s\n", payload)

		return 3
	}

	cfg, _, _, _ := app.cfg.Current()

	logs, err := logstore.Open(dataDir, logstore.Options{
		RingSize: cfg.Settings.LogRingSize,
		Persist:  cfg.Settings.LogPersist,
	}, logger)
	if err != nil {
		logger.Error("打开日志存储失败", "error", err)

		return 1
	}
	app.logs = logs

	kernel, err := dnskernel.New(dnskernel.Config{
		Store:   app.store,
		Sink:    app.logs.Ingest,
		Logger:  logger,
		Targets: opts.Targets,
	})
	if err != nil {
		logger.Error("构造 DNS 内核失败", "error", err)

		return 1
	}
	app.kernel = kernel

	server, err := api.NewServer(api.Deps{Token: opts.Token, Backend: app, Logger: logger})
	if err != nil {
		logger.Error("构造内部 API 失败", "error", err)

		return 1
	}
	app.api = server

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logger.Error("监听内部 API 失败", "error", err)

		return 1
	}
	app.listener = listener
	app.httpServer = &http.Server{Handler: server, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		if err := app.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("内部 API 退出", "error", err)
			app.triggerShutdown()
		}
	}()

	server.SetReady(true)
	fmt.Fprintf(stdout, "NAMESPLIT_READY {\"port\":%d,\"pid\":%d}\n", listener.Addr().(*net.TCPAddr).Port, os.Getpid())

	if err := kernel.Start(ctx); err != nil {
		logger.Error("启动 DNS 内核失败", "error", err)
		app.triggerShutdown()
	}

	select {
	case <-ctx.Done():
	case <-app.done:
	}

	app.shutdown()
	logger.Info("sidecar 已退出")

	return 0
}

// loadConfig 打开配置并构建初始运行时。
func (a *App) loadConfig() *config.Error {
	manager, loadErr := config.Open(a.configPath, a.buildConfigRuntime, a.logger)
	if loadErr != nil {
		return loadErr
	}
	a.cfg = manager

	return nil
}

// buildConfigRuntime 是配置管理器使用的运行时构建函数。
func (a *App) buildConfigRuntime(cfg *config.Config) (config.Runtime, error) {
	snap, reuse, err := a.buildSnapshot(cfg)
	if err != nil {
		return nil, err
	}

	return &pendingRuntime{app: a, snap: snap, reuse: reuse}, nil
}

// triggerShutdown 请求退出；可重复调用。
func (a *App) triggerShutdown() {
	a.doneOnce.Do(func() { close(a.done) })
}

// shutdown 依次停止 DNS、日志存储与内部 API。
func (a *App) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a.api.BeginShutdown()

	if a.kernel != nil {
		if err := a.kernel.Shutdown(ctx); err != nil {
			a.logger.Error("停止 DNS 内核失败", "error", err)
		}
	}

	if a.logs != nil {
		if err := a.logs.Close(ctx); err != nil {
			a.logger.Error("关闭日志存储失败", "error", err)
		}
	}

	if a.httpServer != nil {
		if err := a.httpServer.Shutdown(ctx); err != nil {
			_ = a.httpServer.Close()
		}
	}
}

// validateToken 校验 64 位十六进制 token。
func validateToken(token string) error {
	if len(token) != 64 {
		return fmt.Errorf("长度必须是 64 位十六进制")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return fmt.Errorf("不是十六进制: %w", err)
	}

	return nil
}

// resolveDataDir 返回数据目录。
func resolveDataDir(configured string) (string, error) {
	if configured != "" {
		if err := os.MkdirAll(configured, 0o755); err != nil {
			return "", err
		}

		return configured, nil
	}

	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return "", errors.New("LOCALAPPDATA 未设置")
	}

	dataDir := filepath.Join(base, "Namesplit")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}

	return dataDir, nil
}

// newLogger 根据环境变量构造日志器。
func newLogger(output io.Writer, levelText string) *slog.Logger {
	level := slog.LevelInfo
	switch levelText {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	handler := slog.NewTextHandler(output, &slog.HandlerOptions{Level: level})

	return slog.New(handler).With("version", version.String())
}
