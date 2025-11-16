package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

func main() {
	// Load configuration
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger
	logger, err := InitLogger(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	logger.Info("Starting Telegram Bot Coordinator",
		zap.String("version", "2.0.0"),
		zap.String("log_level", cfg.LogLevel))

	// Connect to database
	db, err := sql.Open("postgres", cfg.GetDSN())
	if err != nil {
		logger.Fatal("Failed to connect to database", zap.Error(err))
	}
	defer db.Close()

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Test database connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		logger.Fatal("Database connection failed", zap.Error(err))
	}

	logger.Info("Database connection established",
		zap.String("host", cfg.DBHost),
		zap.String("database", cfg.DBName))

	// Initialize metrics collector
	metrics := NewMetricsCollector(db, logger)

	// Start metrics update loop
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			metrics.UpdateQueueMetrics()
			metrics.UpdateBatchMetrics()
		}
	}()

	// Initialize health checker
	healthChecker := NewHealthChecker(cfg, db, logger)

	// Start health check server
	healthMux := http.NewServeMux()
	healthMux.Handle("/health", healthChecker)

	healthServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HealthCheckPort),
		Handler: healthMux,
	}

	go func() {
		logger.Info("Health check server starting", zap.Int("port", cfg.HealthCheckPort))
		if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Health check server failed", zap.Error(err))
		}
	}()

	// Start metrics server
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())

	metricsServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.MetricsPort),
		Handler: metricsMux,
	}

	go func() {
		logger.Info("Metrics server starting", zap.Int("port", cfg.MetricsPort))
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Metrics server failed", zap.Error(err))
		}
	}()

	// Create necessary directories
	dirs := []string{"batches", "downloads", "logs", "archive/failed"}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			logger.Fatal("Failed to create directory", zap.String("dir", dir), zap.Error(err))
		}
	}

	logger.Info("Coordinator initialized successfully")

	// Perform crash recovery
	crashRecovery := NewCrashRecovery(db, logger)
	if err := crashRecovery.RecoverOnStartup(context.Background()); err != nil {
		logger.Error("Crash recovery failed", zap.Error(err))
	}

	// Start periodic health checks
	go crashRecovery.PeriodicHealthCheck(context.Background())

	// Initialize Telegram bot
	telegramReceiver, err := NewTelegramReceiver(cfg, db, logger, metrics)
	if err != nil {
		logger.Fatal("Failed to initialize Telegram receiver", zap.Error(err))
	}

	// Start Telegram receiver
	go telegramReceiver.Start(context.Background())

	// Start download workers (3 concurrent)
	for i := 1; i <= cfg.MaxDownloadWorkers; i++ {
		workerID := fmt.Sprintf("download_worker_%d", i)
		worker := NewDownloadWorker(workerID, cfg, db, telegramReceiver.bot, logger, metrics)
		go worker.Start(context.Background())
	}

	// Start batch coordinator
	batchCoordinator := NewBatchCoordinator(cfg, db, logger, metrics)
	go batchCoordinator.Start(context.Background())

	// Start batch workers (up to 5 concurrent)
	for i := 1; i <= cfg.MaxBatchWorkers; i++ {
		workerID := fmt.Sprintf("batch_worker_%d", i)
		worker := NewBatchWorker(workerID, cfg, db, logger, metrics)
		go worker.Start(context.Background())
	}

	logger.Info("All workers started",
		zap.Int("download_workers", cfg.MaxDownloadWorkers),
		zap.Int("batch_workers", cfg.MaxBatchWorkers))

	logger.Info("🚀 Telegram Bot Coordinator is fully operational")

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	logger.Info("Shutdown signal received", zap.String("signal", sig.String()))

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	logger.Info("Shutting down servers...")

	if err := healthServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("Health server shutdown error", zap.Error(err))
	}

	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("Metrics server shutdown error", zap.Error(err))
	}

	logger.Info("Coordinator shutdown complete")
}
