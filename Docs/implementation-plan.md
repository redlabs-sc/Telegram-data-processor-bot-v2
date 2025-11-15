# Implementation Plan: Option 2 - Batch-Based Parallel Processing System
## Telegram Data Processor Bot

**Version**: 1.0
**Date**: 2025-11-13
**Estimated Duration**: 12 weeks (3 months)
**Target Completion**: 2026-02-13

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Project Setup](#2-project-setup)
3. [Phase-by-Phase Implementation](#3-phase-by-phase-implementation)
4. [Testing Strategy](#4-testing-strategy)
5. [Deployment Strategy](#5-deployment-strategy)
6. [Monitoring & Observability](#6-monitoring--observability)
7. [Risk Mitigation](#7-risk-mitigation)
8. [Rollback Plan](#8-rollback-plan)
9. [Prompts for Claude Code](#9-prompts-for-claude-code)

---

## 1. Executive Summary

### 1.1 Implementation Overview

This plan details the step-by-step implementation of **Option 2: Batch-Based Parallel Processing System**, a high-performance architecture that achieves **6× faster processing** compared to the sequential baseline.

**Key Characteristics**:
- **Zero Code Changes**: extract.go, convert.go, store.go remain 100% unchanged
- **Batch-Based Parallelism**: 5 concurrent batches processing 10 files each
- **PostgreSQL-Backed Queues**: Persistent, crash-resistant task management
- **Isolated Workspaces**: Each batch operates in its own directory hierarchy
- **Working Directory Pattern**: Preserve code by changing `os.Chdir()` before execution

### 1.2 Performance Targets

| Metric | Baseline (Current) | Target (Option 2) | Improvement |
|--------|-------------------|-------------------|-------------|
| Processing Time (100 files) | 4.4 hours | 1.7 hours | **6× faster** |
| Throughput | 23 files/hour | 59 files/hour | **2.6× increase** |
| Concurrent Processing | Sequential | 5 batches (50 files) | **50× parallelism** |
| User Notification Delay | 4.4 hours | < 2 hours | **2.4× faster** |

### 1.3 Success Criteria

- ✅ Process 100 files in < 2 hours (P95 latency)
- ✅ SHA256 hash match for extract.go, convert.go, store.go (zero modifications)
- ✅ Zero data loss during crash recovery tests
- ✅ RAM usage < 20%, CPU usage < 50%
- ✅ Success rate ≥ 98% over 1000-file test
- ✅ Admin receives batch notifications within 5 minutes of completion

---

## 2. Project Setup

### 2.1 Prerequisites

**Development Environment**:
- Go 1.21+ installed
- PostgreSQL 14+ installed and running
- Docker & Docker Compose (for Local Bot API Server)
- Git for version control
- IDE with Go support (VS Code, GoLand, etc.)

**Accounts & Access**:
- Telegram Bot Token (with Local Bot API Server configured)
- Telegram Premium account (for 4GB file support)
- Admin Telegram User IDs

**System Requirements**:
- **Development**: 4 vCPU, 16GB RAM, 500GB SSD
- **Staging**: 8 vCPU, 32GB RAM, 1TB SSD
- **Production**: 16 vCPU, 64GB RAM, 2TB SSD

### 2.2 Initial Project Structure Creation

**Step 1: Create New Option 2 Project Directory**

```bash
# Create separate directory for Option 2 implementation
mkdir -p telegram-bot-option2
cd telegram-bot-option2

# Initialize Go module
go mod init github.com/redlabs-sc/telegram-bot-option2

# Create directory structure
mkdir -p app/extraction/extract
mkdir -p app/extraction/convert
mkdir -p app/extraction/files/{all,txt,pass,nopass,errors}
mkdir -p batches
mkdir -p downloads
mkdir -p archive/failed
mkdir -p coordinator
mkdir -p database/migrations
mkdir -p logs
mkdir -p scripts
mkdir -p tests/{unit,integration,load}
mkdir -p monitoring
mkdir -p docs
```

**Step 2: Copy Preserved Code Files**

```bash
# Copy extract.go, convert.go, store.go from current project
# IMPORTANT: These files must NOT be modified
cp ../Telegram-data-processor-bot/app/extraction/extract/extract.go app/extraction/extract/
cp ../Telegram-data-processor-bot/app/extraction/convert/convert.go app/extraction/convert/
cp ../Telegram-data-processor-bot/app/extraction/store.go app/extraction/

# Copy password file
cp ../Telegram-data-processor-bot/app/extraction/pass.txt app/extraction/

# Verify no modifications (compute checksums)
sha256sum app/extraction/extract/extract.go > checksums.txt
sha256sum app/extraction/convert/convert.go >> checksums.txt
sha256sum app/extraction/store.go >> checksums.txt

# Commit checksums to git for verification
git add checksums.txt
git commit -m "Add checksums for preserved code files"
```

**Step 3: Create Configuration Files**

`.env`:
```bash
# Telegram Bot
TELEGRAM_BOT_TOKEN=your_token_here
ADMIN_IDS=123456789,987654321
USE_LOCAL_BOT_API=true
LOCAL_BOT_API_URL=http://localhost:8081
MAX_FILE_SIZE_MB=4096

# Database
DB_HOST=localhost
DB_PORT=5432
DB_NAME=telegram_bot_option2
DB_USER=bot_user
DB_PASSWORD=change_me_in_production
DB_SSL_MODE=disable  # Use 'require' in production

# Worker Configuration
MAX_DOWNLOAD_WORKERS=3
MAX_BATCH_WORKERS=5
BATCH_SIZE=10
BATCH_TIMEOUT_SEC=300

# Timeouts
DOWNLOAD_TIMEOUT_SEC=1800
EXTRACT_TIMEOUT_SEC=1800
CONVERT_TIMEOUT_SEC=1800
STORE_TIMEOUT_SEC=3600

# Resource Limits
MAX_RAM_PERCENT=20
MAX_CPU_PERCENT=50

# Logging
LOG_LEVEL=info
LOG_FORMAT=json
LOG_FILE=logs/coordinator.log

# Cleanup
COMPLETED_BATCH_RETENTION_HOURS=1
FAILED_BATCH_RETENTION_DAYS=7

# Monitoring
METRICS_PORT=9090
HEALTH_CHECK_PORT=8080
```

`.gitignore`:
```
# Environment
.env
.env.local

# Binaries
telegram-bot-option2
coordinator/coordinator
*.exe
*.dll
*.so
*.dylib

# Test binaries
*.test
*.out

# Batch workspaces
batches/*
!batches/.gitkeep

# Downloads
downloads/*
!downloads/.gitkeep

# Archive
archive/*
!archive/.gitkeep

# Logs
logs/*
!logs/.gitkeep

# Database
*.db
*.db-shm
*.db-wal

# IDE
.vscode/
.idea/
*.swp
*.swo
*~

# OS
.DS_Store
Thumbs.db
```

**Step 4: Initialize Git Repository**

```bash
# Create .gitkeep files
touch batches/.gitkeep
touch downloads/.gitkeep
touch archive/.gitkeep
touch logs/.gitkeep

# Initialize git
git init
git add .
git commit -m "Initial project structure for Option 2"
```

### 2.3 Database Setup

**Step 1: Install PostgreSQL** (if not already installed)

```bash
# Ubuntu/Debian
sudo apt update
sudo apt install postgresql postgresql-contrib

# macOS
brew install postgresql@14
brew services start postgresql@14

# Verify installation
psql --version
```

**Step 2: Create Database and User**

```bash
# Connect as postgres superuser
sudo -u postgres psql

# Create database and user
CREATE DATABASE telegram_bot_option2;
CREATE USER bot_user WITH ENCRYPTED PASSWORD 'change_me_in_production';
GRANT ALL PRIVILEGES ON DATABASE telegram_bot_option2 TO bot_user;
\q
```

**Step 3: Test Connection**

```bash
# Test connection with bot_user
PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT version();"
```

### 2.4 Dependencies Installation

`go.mod`:
```go
module github.com/redlabs-sc/telegram-bot-option2

go 1.21

require (
    github.com/go-telegram-bot-api/telegram-bot-api/v5 v5.5.1
    github.com/lib/pq v1.10.9
    github.com/joho/godotenv v1.5.1
    go.uber.org/zap v1.26.0
    github.com/prometheus/client_golang v1.18.0
)
```

Install dependencies:
```bash
go mod download
go mod tidy
```

---

## 3. Phase-by-Phase Implementation

### Phase 1: Foundation (Week 1-2)

#### Objectives
- Setup database schema
- Implement configuration management
- Create logging framework
- Implement health and metrics endpoints

---

#### Task 1.1: Database Schema Creation

**File**: `database/migrations/001_create_download_queue.sql`

```sql
-- Download Queue Table
CREATE TABLE download_queue (
    task_id BIGSERIAL PRIMARY KEY,
    file_id VARCHAR(255) NOT NULL,
    user_id BIGINT NOT NULL,
    filename TEXT NOT NULL,
    file_type VARCHAR(10) NOT NULL CHECK (file_type IN ('ZIP', 'RAR', 'TXT')),
    file_size BIGINT NOT NULL,
    sha256_hash VARCHAR(64),
    status VARCHAR(20) NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'DOWNLOADING', 'DOWNLOADED', 'FAILED')),
    download_attempts INT DEFAULT 0,
    last_error TEXT,
    priority INT DEFAULT 0,
    batch_id VARCHAR(50),
    created_at TIMESTAMP DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP
);

-- Indexes for performance
CREATE INDEX idx_dq_status ON download_queue(status);
CREATE INDEX idx_dq_priority ON download_queue(priority DESC, created_at ASC);
CREATE INDEX idx_dq_batch ON download_queue(batch_id);
CREATE INDEX idx_dq_created ON download_queue(created_at);

-- Comments
COMMENT ON TABLE download_queue IS 'Queue for file downloads from Telegram';
COMMENT ON COLUMN download_queue.status IS 'PENDING: queued, DOWNLOADING: in progress, DOWNLOADED: ready for batch, FAILED: error occurred';
COMMENT ON COLUMN download_queue.priority IS 'Higher values processed first (default 0)';
```

**File**: `database/migrations/002_create_batch_processing.sql`

```sql
-- Batch Processing Table
CREATE TABLE batch_processing (
    batch_id VARCHAR(50) PRIMARY KEY,
    file_count INT NOT NULL,
    archive_count INT DEFAULT 0,
    txt_count INT DEFAULT 0,
    status VARCHAR(20) NOT NULL DEFAULT 'QUEUED' CHECK (status IN ('QUEUED', 'EXTRACTING', 'CONVERTING', 'STORING', 'COMPLETED', 'FAILED')),
    worker_id VARCHAR(50),
    created_at TIMESTAMP DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    extract_duration_sec INT,
    convert_duration_sec INT,
    store_duration_sec INT,
    error_message TEXT
);

-- Indexes
CREATE INDEX idx_bp_status ON batch_processing(status);
CREATE INDEX idx_bp_created ON batch_processing(created_at);
CREATE INDEX idx_bp_worker ON batch_processing(worker_id);

-- Comments
COMMENT ON TABLE batch_processing IS 'Tracks batch processing lifecycle';
COMMENT ON COLUMN batch_processing.status IS 'QUEUED → EXTRACTING → CONVERTING → STORING → COMPLETED/FAILED';
```

**File**: `database/migrations/003_create_batch_files.sql`

```sql
-- Batch Files Join Table
CREATE TABLE batch_files (
    batch_id VARCHAR(50) NOT NULL,
    task_id BIGINT NOT NULL,
    file_type VARCHAR(10) NOT NULL,
    processing_status VARCHAR(20) DEFAULT 'PENDING' CHECK (processing_status IN ('PENDING', 'EXTRACTED', 'CONVERTED', 'STORED', 'FAILED')),
    error_message TEXT,

    PRIMARY KEY (batch_id, task_id),
    FOREIGN KEY (batch_id) REFERENCES batch_processing(batch_id) ON DELETE CASCADE,
    FOREIGN KEY (task_id) REFERENCES download_queue(task_id) ON DELETE CASCADE
);

-- Indexes
CREATE INDEX idx_bf_batch ON batch_files(batch_id);
CREATE INDEX idx_bf_task ON batch_files(task_id);
CREATE INDEX idx_bf_status ON batch_files(processing_status);
```

**File**: `database/migrations/004_create_metrics.sql`

```sql
-- Processing Metrics Table
CREATE TABLE processing_metrics (
    metric_id BIGSERIAL PRIMARY KEY,
    batch_id VARCHAR(50),
    metric_type VARCHAR(50) NOT NULL,
    metric_value DECIMAL(10, 2) NOT NULL,
    recorded_at TIMESTAMP DEFAULT NOW(),

    FOREIGN KEY (batch_id) REFERENCES batch_processing(batch_id) ON DELETE CASCADE
);

-- Indexes
CREATE INDEX idx_pm_type ON processing_metrics(metric_type, recorded_at);
CREATE INDEX idx_pm_batch ON processing_metrics(batch_id);

-- Comments
COMMENT ON TABLE processing_metrics IS 'Time-series metrics for analytics';
COMMENT ON COLUMN processing_metrics.metric_type IS 'download_speed, extract_time, convert_time, store_time, etc.';
```

**File**: `scripts/migrate.sh`

```bash
#!/bin/bash
set -e

# Load environment variables
source .env

# Run migrations in order
echo "Running database migrations..."

for migration in database/migrations/*.sql; do
    echo "Applying: $(basename $migration)"
    PGPASSWORD=$DB_PASSWORD psql -U $DB_USER -h $DB_HOST -d $DB_NAME -f $migration
done

echo "✅ All migrations completed successfully"
```

**Execute Migrations**:
```bash
chmod +x scripts/migrate.sh
./scripts/migrate.sh
```

**Verification**:
```bash
# Verify tables created
PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "\dt"

# Should show: download_queue, batch_processing, batch_files, processing_metrics
```

---

#### Task 1.2: Configuration Management

**File**: `coordinator/config.go`

```go
package main

import (
    "fmt"
    "os"
    "strconv"
    "strings"

    "github.com/joho/godotenv"
)

type Config struct {
    // Telegram
    TelegramBotToken   string
    AdminIDs           []int64
    UseLocalBotAPI     bool
    LocalBotAPIURL     string
    MaxFileSizeMB      int64

    // Database
    DBHost             string
    DBPort             int
    DBName             string
    DBUser             string
    DBPassword         string
    DBSSLMode          string

    // Workers
    MaxDownloadWorkers int
    MaxBatchWorkers    int
    BatchSize          int
    BatchTimeoutSec    int

    // Timeouts
    DownloadTimeoutSec int
    ExtractTimeoutSec  int
    ConvertTimeoutSec  int
    StoreTimeoutSec    int

    // Resource Limits
    MaxRAMPercent      int
    MaxCPUPercent      int

    // Logging
    LogLevel           string
    LogFormat          string
    LogFile            string

    // Cleanup
    CompletedBatchRetentionHours int
    FailedBatchRetentionDays     int

    // Monitoring
    MetricsPort        int
    HealthCheckPort    int
}

func LoadConfig() (*Config, error) {
    // Load .env file
    if err := godotenv.Load(); err != nil {
        return nil, fmt.Errorf("error loading .env file: %w", err)
    }

    cfg := &Config{}

    // Parse Telegram config
    cfg.TelegramBotToken = getEnv("TELEGRAM_BOT_TOKEN", "")
    if cfg.TelegramBotToken == "" {
        return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
    }

    adminIDsStr := getEnv("ADMIN_IDS", "")
    if adminIDsStr == "" {
        return nil, fmt.Errorf("ADMIN_IDS is required")
    }
    cfg.AdminIDs = parseAdminIDs(adminIDsStr)

    cfg.UseLocalBotAPI = getEnvBool("USE_LOCAL_BOT_API", true)
    cfg.LocalBotAPIURL = getEnv("LOCAL_BOT_API_URL", "http://localhost:8081")
    cfg.MaxFileSizeMB = getEnvInt64("MAX_FILE_SIZE_MB", 4096)

    // Parse Database config
    cfg.DBHost = getEnv("DB_HOST", "localhost")
    cfg.DBPort = getEnvInt("DB_PORT", 5432)
    cfg.DBName = getEnv("DB_NAME", "telegram_bot_option2")
    cfg.DBUser = getEnv("DB_USER", "bot_user")
    cfg.DBPassword = getEnv("DB_PASSWORD", "")
    if cfg.DBPassword == "" {
        return nil, fmt.Errorf("DB_PASSWORD is required")
    }
    cfg.DBSSLMode = getEnv("DB_SSL_MODE", "disable")

    // Parse Worker config
    cfg.MaxDownloadWorkers = getEnvInt("MAX_DOWNLOAD_WORKERS", 3)
    cfg.MaxBatchWorkers = getEnvInt("MAX_BATCH_WORKERS", 5)
    cfg.BatchSize = getEnvInt("BATCH_SIZE", 10)
    cfg.BatchTimeoutSec = getEnvInt("BATCH_TIMEOUT_SEC", 300)

    // Parse Timeout config
    cfg.DownloadTimeoutSec = getEnvInt("DOWNLOAD_TIMEOUT_SEC", 1800)
    cfg.ExtractTimeoutSec = getEnvInt("EXTRACT_TIMEOUT_SEC", 1800)
    cfg.ConvertTimeoutSec = getEnvInt("CONVERT_TIMEOUT_SEC", 1800)
    cfg.StoreTimeoutSec = getEnvInt("STORE_TIMEOUT_SEC", 3600)

    // Parse Resource Limits
    cfg.MaxRAMPercent = getEnvInt("MAX_RAM_PERCENT", 20)
    cfg.MaxCPUPercent = getEnvInt("MAX_CPU_PERCENT", 50)

    // Parse Logging config
    cfg.LogLevel = getEnv("LOG_LEVEL", "info")
    cfg.LogFormat = getEnv("LOG_FORMAT", "json")
    cfg.LogFile = getEnv("LOG_FILE", "logs/coordinator.log")

    // Parse Cleanup config
    cfg.CompletedBatchRetentionHours = getEnvInt("COMPLETED_BATCH_RETENTION_HOURS", 1)
    cfg.FailedBatchRetentionDays = getEnvInt("FAILED_BATCH_RETENTION_DAYS", 7)

    // Parse Monitoring config
    cfg.MetricsPort = getEnvInt("METRICS_PORT", 9090)
    cfg.HealthCheckPort = getEnvInt("HEALTH_CHECK_PORT", 8080)

    return cfg, nil
}

func (c *Config) IsAdmin(userID int64) bool {
    for _, adminID := range c.AdminIDs {
        if adminID == userID {
            return true
        }
    }
    return false
}

func (c *Config) GetDatabaseDSN() string {
    return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
        c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSSLMode)
}

// Helper functions
func getEnv(key, defaultValue string) string {
    if value := os.Getenv(key); value != "" {
        return value
    }
    return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
    if value := os.Getenv(key); value != "" {
        if intVal, err := strconv.Atoi(value); err == nil {
            return intVal
        }
    }
    return defaultValue
}

func getEnvInt64(key string, defaultValue int64) int64 {
    if value := os.Getenv(key); value != "" {
        if intVal, err := strconv.ParseInt(value, 10, 64); err == nil {
            return intVal
        }
    }
    return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
    if value := os.Getenv(key); value != "" {
        if boolVal, err := strconv.ParseBool(value); err == nil {
            return boolVal
        }
    }
    return defaultValue
}

func parseAdminIDs(idsStr string) []int64 {
    parts := strings.Split(idsStr, ",")
    adminIDs := make([]int64, 0, len(parts))

    for _, part := range parts {
        part = strings.TrimSpace(part)
        if id, err := strconv.ParseInt(part, 10, 64); err == nil {
            adminIDs = append(adminIDs, id)
        }
    }

    return adminIDs
}
```

---

#### Task 1.3: Logging Framework

**File**: `coordinator/logger.go`

```go
package main

import (
    "go.uber.org/zap"
    "go.uber.org/zap/zapcore"
)

func InitLogger(cfg *Config) (*zap.Logger, error) {
    // Configure log level
    var level zapcore.Level
    switch cfg.LogLevel {
    case "debug":
        level = zapcore.DebugLevel
    case "info":
        level = zapcore.InfoLevel
    case "warn":
        level = zapcore.WarnLevel
    case "error":
        level = zapcore.ErrorLevel
    default:
        level = zapcore.InfoLevel
    }

    // Configure encoding
    var encoding string
    if cfg.LogFormat == "json" {
        encoding = "json"
    } else {
        encoding = "console"
    }

    // Build configuration
    config := zap.Config{
        Level:            zap.NewAtomicLevelAt(level),
        Development:      false,
        Encoding:         encoding,
        EncoderConfig:    zap.NewProductionEncoderConfig(),
        OutputPaths:      []string{"stdout", cfg.LogFile},
        ErrorOutputPaths: []string{"stderr"},
    }

    // Customize time encoding
    config.EncoderConfig.TimeKey = "timestamp"
    config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

    // Build logger
    logger, err := config.Build()
    if err != nil {
        return nil, err
    }

    return logger, nil
}
```

---

#### Task 1.4: Health Check Endpoint

**File**: `coordinator/health.go`

```go
package main

import (
    "database/sql"
    "encoding/json"
    "net/http"
    "time"

    "go.uber.org/zap"
)

type HealthResponse struct {
    Status     string                 `json:"status"`
    Timestamp  string                 `json:"timestamp"`
    Components map[string]interface{} `json:"components"`
    Queue      map[string]int         `json:"queue"`
    Batches    map[string]int         `json:"batches"`
}

func StartHealthServer(cfg *Config, db *sql.DB, logger *zap.Logger) {
    http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
        health := checkHealth(db, logger)

        w.Header().Set("Content-Type", "application/json")
        if health.Status == "healthy" {
            w.WriteHeader(http.StatusOK)
        } else {
            w.WriteHeader(http.StatusServiceUnavailable)
        }

        json.NewEncoder(w).Encode(health)
    })

    addr := fmt.Sprintf(":%d", cfg.HealthCheckPort)
    logger.Info("Starting health check server", zap.String("addr", addr))

    go func() {
        if err := http.ListenAndServe(addr, nil); err != nil {
            logger.Error("Health server error", zap.Error(err))
        }
    }()
}

func checkHealth(db *sql.DB, logger *zap.Logger) HealthResponse {
    health := HealthResponse{
        Status:     "healthy",
        Timestamp:  time.Now().Format(time.RFC3339),
        Components: make(map[string]interface{}),
        Queue:      make(map[string]int),
        Batches:    make(map[string]int),
    }

    // Check database
    if err := db.Ping(); err != nil {
        health.Status = "unhealthy"
        health.Components["database"] = map[string]string{
            "status": "unhealthy",
            "error":  err.Error(),
        }
    } else {
        health.Components["database"] = "healthy"
    }

    // Check queue statistics
    var pending, downloading, downloaded, failed int
    db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='PENDING'").Scan(&pending)
    db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='DOWNLOADING'").Scan(&downloading)
    db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='DOWNLOADED'").Scan(&downloaded)
    db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='FAILED'").Scan(&failed)

    health.Queue["pending"] = pending
    health.Queue["downloading"] = downloading
    health.Queue["downloaded"] = downloaded
    health.Queue["failed"] = failed

    // Check batch statistics
    var queued, processing, completed int
    db.QueryRow("SELECT COUNT(*) FROM batch_processing WHERE status='QUEUED'").Scan(&queued)
    db.QueryRow("SELECT COUNT(*) FROM batch_processing WHERE status IN ('EXTRACTING', 'CONVERTING', 'STORING')").Scan(&processing)
    db.QueryRow("SELECT COUNT(*) FROM batch_processing WHERE status='COMPLETED' AND completed_at > NOW() - INTERVAL '1 hour'").Scan(&completed)

    health.Batches["queued"] = queued
    health.Batches["processing"] = processing
    health.Batches["completed_last_hour"] = completed

    return health
}
```

---

#### Task 1.5: Metrics Endpoint

**File**: `coordinator/metrics.go`

```go
package main

import (
    "database/sql"
    "fmt"
    "net/http"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "go.uber.org/zap"
)

var (
    queueSize = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "telegram_bot_queue_size",
            Help: "Number of tasks in each queue status",
        },
        []string{"status"},
    )

    batchProcessingDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "telegram_bot_batch_processing_duration_seconds",
            Help:    "Time to process a batch",
            Buckets: []float64{300, 600, 900, 1200, 1800, 2400, 3600},
        },
        []string{"stage"},
    )

    workerStatus = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "telegram_bot_worker_status",
            Help: "Worker health status (1=healthy, 0=unhealthy)",
        },
        []string{"type", "id"},
    )
)

func init() {
    prometheus.MustRegister(queueSize)
    prometheus.MustRegister(batchProcessingDuration)
    prometheus.MustRegister(workerStatus)
}

func StartMetricsServer(cfg *Config, db *sql.DB, logger *zap.Logger) {
    // Update metrics periodically
    go updateMetrics(db, logger)

    // Expose metrics endpoint
    http.Handle("/metrics", promhttp.Handler())

    addr := fmt.Sprintf(":%d", cfg.MetricsPort)
    logger.Info("Starting metrics server", zap.String("addr", addr))

    go func() {
        if err := http.ListenAndServe(addr, nil); err != nil {
            logger.Error("Metrics server error", zap.Error(err))
        }
    }()
}

func updateMetrics(db *sql.DB, logger *zap.Logger) {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()

    for range ticker.C {
        // Update queue sizes
        var pending, downloading, downloaded, failed int
        db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='PENDING'").Scan(&pending)
        db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='DOWNLOADING'").Scan(&downloading)
        db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='DOWNLOADED'").Scan(&downloaded)
        db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='FAILED'").Scan(&failed)

        queueSize.WithLabelValues("pending").Set(float64(pending))
        queueSize.WithLabelValues("downloading").Set(float64(downloading))
        queueSize.WithLabelValues("downloaded").Set(float64(downloaded))
        queueSize.WithLabelValues("failed").Set(float64(failed))
    }
}
```

---

#### Task 1.6: Main Entry Point (Skeleton)

**File**: `coordinator/main.go`

```go
package main

import (
    "database/sql"
    "fmt"
    "os"
    "os/signal"
    "syscall"

    _ "github.com/lib/pq"
    "go.uber.org/zap"
)

func main() {
    // 1. Load configuration
    cfg, err := LoadConfig()
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
        os.Exit(1)
    }

    // 2. Initialize logger
    logger, err := InitLogger(cfg)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error initializing logger: %v\n", err)
        os.Exit(1)
    }
    defer logger.Sync()

    logger.Info("Starting Telegram Bot - Option 2: Batch-Based Parallel Processing")

    // 3. Connect to database
    db, err := sql.Open("postgres", cfg.GetDatabaseDSN())
    if err != nil {
        logger.Fatal("Error connecting to database", zap.Error(err))
    }
    defer db.Close()

    // Test database connection
    if err := db.Ping(); err != nil {
        logger.Fatal("Error pinging database", zap.Error(err))
    }
    logger.Info("Connected to database successfully")

    // 4. Start health check server
    StartHealthServer(cfg, db, logger)

    // 5. Start metrics server
    StartMetricsServer(cfg, db, logger)

    // 6. TODO: Initialize Telegram bot receiver
    // 7. TODO: Start download workers
    // 8. TODO: Start batch coordinator
    // 9. TODO: Start batch workers

    logger.Info("All services started successfully")

    // Wait for interrupt signal
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
    <-sigChan

    logger.Info("Shutting down gracefully...")
    // TODO: Graceful shutdown logic
}
```

---

#### Phase 1 Testing

**Test 1.1: Configuration Loading**

```bash
# Verify configuration loads correctly
cd coordinator
go run . &
PID=$!
sleep 2

# Check if process started
ps -p $PID > /dev/null && echo "✅ Process started" || echo "❌ Process failed to start"

# Kill test process
kill $PID
```

**Test 1.2: Database Connection**

```bash
# Check logs for database connection
tail -n 20 logs/coordinator.log | grep "Connected to database"
# Expected: {"level":"info","timestamp":"...","msg":"Connected to database successfully"}
```

**Test 1.3: Health Endpoint**

```bash
# Query health endpoint
curl -s http://localhost:8080/health | jq .

# Expected:
# {
#   "status": "healthy",
#   "timestamp": "2025-11-13T10:23:45Z",
#   "components": {"database": "healthy"},
#   "queue": {"pending": 0, "downloading": 0, ...},
#   "batches": {"queued": 0, ...}
# }
```

**Test 1.4: Metrics Endpoint**

```bash
# Query metrics endpoint
curl -s http://localhost:9090/metrics | grep telegram_bot

# Expected:
# telegram_bot_queue_size{status="pending"} 0
# telegram_bot_queue_size{status="downloading"} 0
# ...
```

**Phase 1 Completion Checklist**:
- ✅ Database tables created (download_queue, batch_processing, batch_files, processing_metrics)
- ✅ Configuration loaded from .env
- ✅ Logger writing to logs/coordinator.log
- ✅ Health endpoint returning 200 OK at :8080/health
- ✅ Metrics endpoint returning Prometheus format at :9090/metrics
- ✅ Database connection verified with Ping()

---

### Phase 2: Download Pipeline (Week 3-4)

#### Objectives
- Implement Telegram bot receiver with admin validation
- Create download queue manager
- Implement 3 concurrent download workers
- Add SHA256 hash computation
- Implement retry logic and crash recovery

---

#### Task 2.1: Telegram Bot Receiver

**File**: `coordinator/telegram_receiver.go`

```go
package main

import (
    "database/sql"
    "fmt"
    "path/filepath"
    "strings"

    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
    "go.uber.org/zap"
)

type TelegramReceiver struct {
    bot    *tgbotapi.BotAPI
    cfg    *Config
    db     *sql.DB
    logger *zap.Logger
}

func NewTelegramReceiver(cfg *Config, db *sql.DB, logger *zap.Logger) (*TelegramReceiver, error) {
    var bot *tgbotapi.BotAPI
    var err error

    if cfg.UseLocalBotAPI {
        bot, err = tgbotapi.NewBotAPIWithAPIEndpoint(cfg.TelegramBotToken, cfg.LocalBotAPIURL+"/bot%s/%s")
    } else {
        bot, err = tgbotapi.NewBotAPI(cfg.TelegramBotToken)
    }

    if err != nil {
        return nil, fmt.Errorf("failed to create bot: %w", err)
    }

    logger.Info("Telegram bot authorized", zap.String("username", bot.Self.UserName))

    return &TelegramReceiver{
        bot:    bot,
        cfg:    cfg,
        db:     db,
        logger: logger,
    }, nil
}

func (tr *TelegramReceiver) Start() {
    u := tgbotapi.NewUpdate(0)
    u.Timeout = 60

    updates := tr.bot.GetUpdatesChan(u)

    for update := range updates {
        if update.Message == nil {
            continue
        }

        go tr.handleMessage(update.Message)
    }
}

func (tr *TelegramReceiver) handleMessage(msg *tgbotapi.Message) {
    // Check if user is admin
    if !tr.cfg.IsAdmin(msg.From.ID) {
        tr.sendReply(msg.ChatID, "❌ Unauthorized. This bot is admin-only.")
        tr.logger.Warn("Unauthorized access attempt",
            zap.Int64("user_id", msg.From.ID),
            zap.String("username", msg.From.UserName))
        return
    }

    // Handle commands
    if msg.IsCommand() {
        tr.handleCommand(msg)
        return
    }

    // Handle file uploads
    if msg.Document != nil {
        tr.handleDocument(msg)
        return
    }

    // Ignore other messages
}

func (tr *TelegramReceiver) handleCommand(msg *tgbotapi.Message) {
    switch msg.Command() {
    case "start":
        tr.handleStart(msg)
    case "help":
        tr.handleHelp(msg)
    case "queue":
        tr.handleQueue(msg)
    case "batches":
        tr.handleBatches(msg)
    case "stats":
        tr.handleStats(msg)
    case "health":
        tr.handleHealthCommand(msg)
    default:
        tr.sendReply(msg.ChatID, "Unknown command. Send /help for available commands.")
    }
}

func (tr *TelegramReceiver) handleStart(msg *tgbotapi.Message) {
    text := `👋 Welcome to Telegram Data Processor Bot (Option 2)

This bot processes archive files (ZIP, RAR) and text files with high-speed batch processing.

📤 Send me files to process:
• Archives: ZIP, RAR (up to 4GB)
• Text files: TXT (up to 4GB)

📊 Available commands:
/help - Show this help message
/queue - View queue status
/batches - View active batches
/stats - View processing statistics
/health - Check system health

🚀 Files are processed in batches of 10 for maximum speed!`

    tr.sendReply(msg.ChatID, text)
}

func (tr *TelegramReceiver) handleHelp(msg *tgbotapi.Message) {
    text := `📚 Available Commands:

/start - Welcome message
/help - This help message
/queue - Show queue statistics (pending, downloading, downloaded, failed)
/batches - List active batches with status
/stats - Overall system statistics (last 24 hours)
/health - System health check (workers, resources)

📤 File Upload:
Simply send a file (ZIP, RAR, or TXT) and it will be queued for processing.

⚡ Processing Pipeline:
1. Download (3 concurrent workers)
2. Batch Formation (10 files per batch)
3. Extract → Convert → Store (5 concurrent batches)
4. Notification on completion`

    tr.sendReply(msg.ChatID, text)
}

func (tr *TelegramReceiver) handleDocument(msg *tgbotapi.Message) {
    doc := msg.Document

    // Validate file size
    maxSizeBytes := tr.cfg.MaxFileSizeMB * 1024 * 1024
    if int64(doc.FileSize) > maxSizeBytes {
        tr.sendReply(msg.ChatID, fmt.Sprintf("❌ File too large. Max size: %d MB", tr.cfg.MaxFileSizeMB))
        return
    }

    // Validate file type
    fileType := getFileType(doc.FileName)
    if fileType == "" {
        tr.sendReply(msg.ChatID, "❌ Unsupported file type. Supported: ZIP, RAR, TXT")
        return
    }

    // Insert into download queue
    taskID, err := tr.enqueueDownload(msg.From.ID, doc.FileID, doc.FileName, fileType, int64(doc.FileSize))
    if err != nil {
        tr.logger.Error("Error enqueueing download",
            zap.Error(err),
            zap.String("filename", doc.FileName))
        tr.sendReply(msg.ChatID, "❌ Error queuing file for processing. Please try again.")
        return
    }

    // Send confirmation
    tr.sendReply(msg.ChatID, fmt.Sprintf("✅ File queued for processing\n\n📄 Filename: %s\n📦 Size: %.2f MB\n🆔 Task ID: %d\n\nYou'll receive a notification when processing completes.",
        doc.FileName,
        float64(doc.FileSize)/(1024*1024),
        taskID))

    tr.logger.Info("File queued",
        zap.Int64("task_id", taskID),
        zap.String("filename", doc.FileName),
        zap.String("file_type", fileType),
        zap.Int64("file_size", int64(doc.FileSize)))
}

func (tr *TelegramReceiver) enqueueDownload(userID int64, fileID, filename, fileType string, fileSize int64) (int64, error) {
    var taskID int64
    err := tr.db.QueryRow(`
        INSERT INTO download_queue (file_id, user_id, filename, file_type, file_size, status)
        VALUES ($1, $2, $3, $4, $5, 'PENDING')
        RETURNING task_id
    `, fileID, userID, filename, fileType, fileSize).Scan(&taskID)

    return taskID, err
}

func (tr *TelegramReceiver) sendReply(chatID int64, text string) {
    msg := tgbotapi.NewMessage(chatID, text)
    msg.ParseMode = "Markdown"
    tr.bot.Send(msg)
}

func getFileType(filename string) string {
    ext := strings.ToLower(filepath.Ext(filename))
    switch ext {
    case ".zip":
        return "ZIP"
    case ".rar":
        return "RAR"
    case ".txt":
        return "TXT"
    default:
        return ""
    }
}

// TODO: Implement /queue, /batches, /stats, /health commands
```

---

#### Task 2.2: Download Worker Pool

**File**: `coordinator/download_worker.go`

```go
package main

import (
    "context"
    "crypto/sha256"
    "database/sql"
    "encoding/hex"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "time"

    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
    "go.uber.org/zap"
)

type DownloadWorker struct {
    id     string
    bot    *tgbotapi.BotAPI
    cfg    *Config
    db     *sql.DB
    logger *zap.Logger
}

func NewDownloadWorker(id string, bot *tgbotapi.BotAPI, cfg *Config, db *sql.DB, logger *zap.Logger) *DownloadWorker {
    return &DownloadWorker{
        id:     id,
        bot:    bot,
        cfg:    cfg,
        db:     db,
        logger: logger.With(zap.String("worker", id)),
    }
}

func (dw *DownloadWorker) Start(ctx context.Context) {
    dw.logger.Info("Download worker started")

    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            dw.logger.Info("Download worker stopping")
            return
        case <-ticker.C:
            dw.processNext(ctx)
        }
    }
}

func (dw *DownloadWorker) processNext(ctx context.Context) {
    // Claim next task with optimistic locking
    tx, err := dw.db.BeginTx(ctx, nil)
    if err != nil {
        dw.logger.Error("Error starting transaction", zap.Error(err))
        return
    }
    defer tx.Rollback()

    var task struct {
        TaskID   int64
        FileID   string
        Filename string
        FileType string
        FileSize int64
    }

    err = tx.QueryRowContext(ctx, `
        SELECT task_id, file_id, filename, file_type, file_size
        FROM download_queue
        WHERE status = 'PENDING'
        ORDER BY priority DESC, created_at ASC
        LIMIT 1
        FOR UPDATE SKIP LOCKED
    `).Scan(&task.TaskID, &task.FileID, &task.Filename, &task.FileType, &task.FileSize)

    if err == sql.ErrNoRows {
        // No pending tasks
        return
    }
    if err != nil {
        dw.logger.Error("Error querying task", zap.Error(err))
        return
    }

    // Mark as DOWNLOADING
    _, err = tx.ExecContext(ctx, `
        UPDATE download_queue
        SET status = 'DOWNLOADING', started_at = NOW()
        WHERE task_id = $1
    `, task.TaskID)

    if err != nil {
        dw.logger.Error("Error updating status", zap.Error(err))
        return
    }

    if err := tx.Commit(); err != nil {
        dw.logger.Error("Error committing transaction", zap.Error(err))
        return
    }

    dw.logger.Info("Claimed task",
        zap.Int64("task_id", task.TaskID),
        zap.String("filename", task.Filename))

    // Download file with timeout
    downloadCtx, cancel := context.WithTimeout(ctx, time.Duration(dw.cfg.DownloadTimeoutSec)*time.Second)
    defer cancel()

    err = dw.downloadFile(downloadCtx, task.TaskID, task.FileID, task.Filename)

    if err != nil {
        // Mark as FAILED
        dw.logger.Error("Download failed",
            zap.Int64("task_id", task.TaskID),
            zap.Error(err))

        dw.db.Exec(`
            UPDATE download_queue
            SET status = 'FAILED',
                last_error = $2,
                download_attempts = download_attempts + 1,
                completed_at = NOW()
            WHERE task_id = $1
        `, task.TaskID, err.Error())
    } else {
        // Mark as DOWNLOADED
        dw.logger.Info("Download completed",
            zap.Int64("task_id", task.TaskID),
            zap.String("filename", task.Filename))

        dw.db.Exec(`
            UPDATE download_queue
            SET status = 'DOWNLOADED',
                completed_at = NOW()
            WHERE task_id = $1
        `, task.TaskID)
    }
}

func (dw *DownloadWorker) downloadFile(ctx context.Context, taskID int64, fileID, filename string) error {
    // Get file from Telegram
    fileConfig := tgbotapi.FileConfig{FileID: fileID}
    file, err := dw.bot.GetFile(fileConfig)
    if err != nil {
        return fmt.Errorf("get file error: %w", err)
    }

    // Determine download URL
    var fileURL string
    if dw.cfg.UseLocalBotAPI {
        fileURL = fmt.Sprintf("%s/file/bot%s/%s", dw.cfg.LocalBotAPIURL, dw.cfg.TelegramBotToken, file.FilePath)
    } else {
        fileURL = file.Link(dw.cfg.TelegramBotToken)
    }

    // Download file to temporary location
    tempPath := filepath.Join("downloads", fmt.Sprintf("%d_%s", taskID, filename))

    // Ensure downloads directory exists
    os.MkdirAll("downloads", 0755)

    // Create output file
    out, err := os.Create(tempPath)
    if err != nil {
        return fmt.Errorf("create file error: %w", err)
    }
    defer out.Close()

    // Download with streaming
    resp, err := http.Get(fileURL)
    if err != nil {
        return fmt.Errorf("http get error: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("http status: %d", resp.StatusCode)
    }

    // Compute SHA256 while downloading
    hash := sha256.New()
    multiWriter := io.MultiWriter(out, hash)

    _, err = io.Copy(multiWriter, resp.Body)
    if err != nil {
        return fmt.Errorf("copy error: %w", err)
    }

    // Store hash in database
    sha256Hash := hex.EncodeToString(hash.Sum(nil))
    _, err = dw.db.Exec(`
        UPDATE download_queue
        SET sha256_hash = $2
        WHERE task_id = $1
    `, taskID, sha256Hash)

    if err != nil {
        dw.logger.Warn("Error storing hash", zap.Error(err))
    }

    dw.logger.Info("File downloaded",
        zap.Int64("task_id", taskID),
        zap.String("path", tempPath),
        zap.String("sha256", sha256Hash))

    return nil
}
```

---

#### Task 2.3: Crash Recovery for Downloads

**File**: `coordinator/crash_recovery.go`

```go
package main

import (
    "context"
    "database/sql"
    "time"

    "go.uber.org/zap"
)

func RecoverCrashedDownloads(ctx context.Context, db *sql.DB, logger *zap.Logger) error {
    logger.Info("Starting crash recovery for downloads")

    // Find stuck downloads (DOWNLOADING for > 30 minutes)
    stuckTimeout := 30 * time.Minute

    result, err := db.ExecContext(ctx, `
        UPDATE download_queue
        SET status = 'PENDING',
            download_attempts = download_attempts + 1
        WHERE status = 'DOWNLOADING'
          AND started_at < NOW() - INTERVAL '30 minutes'
    `)

    if err != nil {
        return err
    }

    count, _ := result.RowsAffected()
    if count > 0 {
        logger.Info("Recovered stuck downloads",
            zap.Int64("count", count))
    }

    return nil
}
```

---

#### Task 2.4: Update Main Entry Point

Update `coordinator/main.go` to include Telegram receiver and download workers:

```go
// Add after metrics server start (after line 960)

// 6. Perform crash recovery
if err := RecoverCrashedDownloads(context.Background(), db, logger); err != nil {
    logger.Error("Crash recovery failed", zap.Error(err))
}

// 7. Initialize Telegram bot receiver
receiver, err := NewTelegramReceiver(cfg, db, logger)
if err != nil {
    logger.Fatal("Error creating Telegram receiver", zap.Error(err))
}

// Start receiver in background
go receiver.Start()
logger.Info("Telegram receiver started")

// 8. Start download workers
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

for i := 1; i <= cfg.MaxDownloadWorkers; i++ {
    workerID := fmt.Sprintf("download_worker_%d", i)
    worker := NewDownloadWorker(workerID, receiver.bot, cfg, db, logger)
    go worker.Start(ctx)
    logger.Info("Download worker started", zap.String("id", workerID))
}

// 9. TODO: Start batch coordinator
// 10. TODO: Start batch workers
```

---

#### Phase 2 Testing

**Test 2.1: File Upload and Queueing**

```bash
# Send a test file to the bot via Telegram
# Expected: Receive confirmation message with task_id

# Verify in database
PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT * FROM download_queue ORDER BY task_id DESC LIMIT 1;"

# Expected: task_id | file_id | filename | file_type | status='PENDING'
```

**Test 2.2: Download Worker Processing**

```bash
# Monitor logs
tail -f logs/coordinator.log | grep download_worker

# Expected:
# {"level":"info","worker":"download_worker_1","msg":"Claimed task","task_id":1}
# {"level":"info","worker":"download_worker_1","msg":"File downloaded","task_id":1,"path":"downloads/1_test.zip"}
# {"level":"info","worker":"download_worker_1","msg":"Download completed","task_id":1}

# Verify file exists
ls -lh downloads/

# Verify status updated
PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT task_id, status, sha256_hash FROM download_queue WHERE task_id=1;"

# Expected: status='DOWNLOADED', sha256_hash is populated
```

**Test 2.3: Concurrent Downloads**

```bash
# Upload 10 files quickly to the bot

# Monitor active downloads
watch -n 1 'PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT COUNT(*) FROM download_queue WHERE status='\''DOWNLOADING'\'';"'

# Expected: Never exceeds 3 concurrent downloads
```

**Test 2.4: Crash Recovery**

```bash
# Upload a file
# While downloading, kill the coordinator
killall coordinator

# Restart coordinator
cd coordinator && go run . &

# Check logs for recovery
tail -n 50 logs/coordinator.log | grep "Recovered stuck downloads"

# Verify task reset to PENDING
PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT task_id, status, download_attempts FROM download_queue WHERE status='PENDING' AND download_attempts > 0;"
```

**Phase 2 Completion Checklist**:
- ✅ Telegram bot accepts file uploads from admins
- ✅ Non-admin users rejected with "Unauthorized" message
- ✅ Files queued in download_queue table
- ✅ Exactly 3 download workers running concurrently
- ✅ Files downloaded to downloads/ directory
- ✅ SHA256 hash computed and stored
- ✅ Crash recovery resets stuck downloads
- ✅ Admin receives upload confirmation

---

### Phase 3: Batch Coordinator (Week 5-6)

#### Objectives
- Monitor downloaded files and create batches
- Create isolated batch directory structures
- Route files to appropriate batch subdirectories
- Enforce maximum concurrent batches

---

#### Task 3.1: Batch Coordinator

**File**: `coordinator/batch_coordinator.go`

```go
package main

import (
    "context"
    "database/sql"
    "fmt"
    "os"
    "path/filepath"
    "time"

    "go.uber.org/zap"
)

type BatchCoordinator struct {
    cfg    *Config
    db     *sql.DB
    logger *zap.Logger
}

func NewBatchCoordinator(cfg *Config, db *sql.DB, logger *zap.Logger) *BatchCoordinator {
    return &BatchCoordinator{
        cfg:    cfg,
        db:     db,
        logger: logger,
    }
}

func (bc *BatchCoordinator) Start(ctx context.Context) {
    bc.logger.Info("Batch coordinator started")

    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            bc.logger.Info("Batch coordinator stopping")
            return
        case <-ticker.C:
            bc.tryCreateBatch(ctx)
        }
    }
}

func (bc *BatchCoordinator) tryCreateBatch(ctx context.Context) {
    // Check if we have room for more batches
    var activeBatches int
    err := bc.db.QueryRowContext(ctx, `
        SELECT COUNT(*)
        FROM batch_processing
        WHERE status IN ('QUEUED', 'EXTRACTING', 'CONVERTING', 'STORING')
    `).Scan(&activeBatches)

    if err != nil {
        bc.logger.Error("Error counting active batches", zap.Error(err))
        return
    }

    if activeBatches >= bc.cfg.MaxBatchWorkers {
        bc.logger.Debug("Max batch workers reached", zap.Int("active", activeBatches))
        return
    }

    // Get downloaded files waiting for batch
    rows, err := bc.db.QueryContext(ctx, `
        SELECT task_id, filename, file_type, file_size
        FROM download_queue
        WHERE status = 'DOWNLOADED' AND batch_id IS NULL
        ORDER BY created_at ASC
        LIMIT $1
    `, bc.cfg.BatchSize)

    if err != nil {
        bc.logger.Error("Error querying downloaded files", zap.Error(err))
        return
    }
    defer rows.Close()

    type file struct {
        TaskID   int64
        Filename string
        FileType string
        FileSize int64
    }

    var files []file
    oldestFileTime := time.Now()

    for rows.Next() {
        var f file
        if err := rows.Scan(&f.TaskID, &f.Filename, &f.FileType, &f.FileSize); err != nil {
            bc.logger.Error("Error scanning row", zap.Error(err))
            continue
        }
        files = append(files, f)

        // Track oldest file time for timeout logic
        var createdAt time.Time
        bc.db.QueryRow("SELECT created_at FROM download_queue WHERE task_id=$1", f.TaskID).Scan(&createdAt)
        if createdAt.Before(oldestFileTime) {
            oldestFileTime = createdAt
        }
    }

    fileCount := len(files)

    // Create batch if:
    // 1. We have enough files (BATCH_SIZE), OR
    // 2. We have some files and oldest file is waiting > BATCH_TIMEOUT_SEC
    batchTimeout := time.Duration(bc.cfg.BatchTimeoutSec) * time.Second
    shouldCreate := fileCount >= bc.cfg.BatchSize ||
        (fileCount > 0 && time.Since(oldestFileTime) > batchTimeout)

    if !shouldCreate {
        bc.logger.Debug("Not enough files for batch",
            zap.Int("file_count", fileCount),
            zap.Duration("oldest_wait", time.Since(oldestFileTime)))
        return
    }

    // Create batch
    batchID := bc.generateBatchID()
    if err := bc.createBatch(ctx, batchID, files); err != nil {
        bc.logger.Error("Error creating batch", zap.Error(err), zap.String("batch_id", batchID))
        return
    }

    bc.logger.Info("Batch created",
        zap.String("batch_id", batchID),
        zap.Int("file_count", fileCount))
}

func (bc *BatchCoordinator) createBatch(ctx context.Context, batchID string, files []file) error {
    // Start transaction
    tx, err := bc.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    // Count archive vs txt files
    archiveCount := 0
    txtCount := 0
    for _, f := range files {
        if f.FileType == "TXT" {
            txtCount++
        } else {
            archiveCount++
        }
    }

    // Create batch record
    _, err = tx.ExecContext(ctx, `
        INSERT INTO batch_processing (batch_id, file_count, archive_count, txt_count, status)
        VALUES ($1, $2, $3, $4, 'QUEUED')
    `, batchID, len(files), archiveCount, txtCount)

    if err != nil {
        return fmt.Errorf("insert batch record: %w", err)
    }

    // Update download_queue with batch_id
    for _, f := range files {
        _, err := tx.ExecContext(ctx, `
            UPDATE download_queue
            SET batch_id = $2
            WHERE task_id = $1
        `, f.TaskID, batchID)

        if err != nil {
            return fmt.Errorf("update download_queue: %w", err)
        }

        // Insert into batch_files
        _, err = tx.ExecContext(ctx, `
            INSERT INTO batch_files (batch_id, task_id, file_type, processing_status)
            VALUES ($1, $2, $3, 'PENDING')
        `, batchID, f.TaskID, f.FileType)

        if err != nil {
            return fmt.Errorf("insert batch_files: %w", err)
        }
    }

    // Commit transaction
    if err := tx.Commit(); err != nil {
        return err
    }

    // Create batch directory structure
    if err := bc.createBatchDirectories(batchID); err != nil {
        return fmt.Errorf("create directories: %w", err)
    }

    // Move files to batch directories
    for _, f := range files {
        sourcePath := filepath.Join("downloads", fmt.Sprintf("%d_%s", f.TaskID, f.Filename))

        var destPath string
        if f.FileType == "TXT" {
            destPath = filepath.Join("batches", batchID, "app", "extraction", "files", "txt", f.Filename)
        } else {
            destPath = filepath.Join("batches", batchID, "app", "extraction", "files", "all", f.Filename)
        }

        if err := os.Rename(sourcePath, destPath); err != nil {
            bc.logger.Error("Error moving file",
                zap.Error(err),
                zap.String("source", sourcePath),
                zap.String("dest", destPath))
            continue
        }

        bc.logger.Debug("File moved to batch",
            zap.Int64("task_id", f.TaskID),
            zap.String("dest", destPath))
    }

    return nil
}

func (bc *BatchCoordinator) createBatchDirectories(batchID string) error {
    batchRoot := filepath.Join("batches", batchID)

    dirs := []string{
        filepath.Join(batchRoot, "app", "extraction", "files", "all"),
        filepath.Join(batchRoot, "app", "extraction", "files", "txt"),
        filepath.Join(batchRoot, "app", "extraction", "files", "pass"),
        filepath.Join(batchRoot, "app", "extraction", "files", "nopass"),
        filepath.Join(batchRoot, "app", "extraction", "files", "errors"),
        filepath.Join(batchRoot, "logs"),
    }

    for _, dir := range dirs {
        if err := os.MkdirAll(dir, 0755); err != nil {
            return fmt.Errorf("mkdir %s: %w", dir, err)
        }
    }

    // Copy pass.txt to batch directory
    passFile := filepath.Join("app", "extraction", "pass.txt")
    batchPassFile := filepath.Join(batchRoot, "app", "extraction", "pass.txt")

    if _, err := os.Stat(passFile); err == nil {
        // Read source
        data, err := os.ReadFile(passFile)
        if err != nil {
            bc.logger.Warn("Error reading pass.txt", zap.Error(err))
        } else {
            // Write to batch
            if err := os.WriteFile(batchPassFile, data, 0644); err != nil {
                bc.logger.Warn("Error copying pass.txt to batch", zap.Error(err))
            }
        }
    }

    bc.logger.Debug("Batch directories created", zap.String("batch_id", batchID))
    return nil
}

func (bc *BatchCoordinator) generateBatchID() string {
    // Query max batch number
    var maxNum int
    bc.db.QueryRow(`
        SELECT COALESCE(MAX(CAST(SUBSTRING(batch_id FROM 7) AS INTEGER)), 0)
        FROM batch_processing
        WHERE batch_id LIKE 'batch_%'
    `).Scan(&maxNum)

    return fmt.Sprintf("batch_%03d", maxNum+1)
}
```

---

#### Phase 3 Testing

**Test 3.1: Batch Creation**

```bash
# Upload 10 files to the bot

# Wait 30 seconds for batch coordinator to run

# Check batch created
PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT * FROM batch_processing ORDER BY created_at DESC LIMIT 1;"

# Expected: batch_id='batch_001', file_count=10, status='QUEUED'

# Verify files moved
ls -lh batches/batch_001/app/extraction/files/all/
ls -lh batches/batch_001/app/extraction/files/txt/

# Verify download_queue updated
PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT task_id, batch_id FROM download_queue WHERE batch_id='batch_001';"
```

**Test 3.2: Batch Timeout**

```bash
# Upload only 5 files (less than BATCH_SIZE=10)

# Wait 6 minutes (BATCH_TIMEOUT_SEC=300)

# Verify batch created with 5 files
PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT batch_id, file_count FROM batch_processing WHERE file_count < 10;"
```

**Test 3.3: Max Concurrent Batches**

```bash
# Set MAX_BATCH_WORKERS=2 in .env
# Upload 30 files (should create 3 batches of 10)

# Start processing workers (will implement in Phase 4)
# Verify only 2 batches are QUEUED/PROCESSING at a time
watch -n 2 'PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT batch_id, status FROM batch_processing WHERE status != '\''COMPLETED'\'' ORDER BY created_at;"'
```

**Phase 3 Completion Checklist**:
- ✅ Batch coordinator creates batches when BATCH_SIZE files ready
- ✅ Batch coordinator creates batches when timeout reached
- ✅ Batch directories created with correct structure
- ✅ Files moved from downloads/ to batch/app/extraction/files/
- ✅ Archives go to all/, TXT files go to txt/
- ✅ pass.txt copied to each batch
- ✅ Database records created (batch_processing, batch_files)
- ✅ Max concurrent batches enforced

---

**(Implementation plan continues with Phase 4: Batch Processing Workers, Phase 5: Admin Commands, Phase 6: Testing, Phase 7: Deployment, and additional sections. Due to length, creating a second file for the continuation.)**

---

**See**: `option2-implementation-plan-part2.md` for:
- Phase 4: Batch Processing Workers
- Phase 5: Admin Commands
- Phase 6: Testing & Optimization
- Phase 7: Documentation & Deployment
- Section 4: Testing Strategy
- Section 5: Deployment Strategy
- Section 6: Monitoring & Observability
- Section 7: Risk Mitigation
- Section 8: Rollback Plan
- Section 9: Prompts for Claude Code
