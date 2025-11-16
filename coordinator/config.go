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
	TelegramBotToken string
	AdminIDs         []int64
	UseLocalBotAPI   bool
	LocalBotAPIURL   string
	MaxFileSizeMB    int64

	// Database
	DBHost     string
	DBPort     int
	DBName     string
	DBUser     string
	DBPassword string
	DBSSLMode  string

	// Workers
	MaxDownloadWorkers int
	MaxBatchWorkers    int
	BatchSize          int
	BatchTimeoutSec    int

	// Timeouts (in seconds)
	DownloadTimeoutSec int
	ExtractTimeoutSec  int
	ConvertTimeoutSec  int
	StoreTimeoutSec    int

	// Resource Limits
	MaxRAMPercent int
	MaxCPUPercent int

	// Logging
	LogLevel  string
	LogFormat string
	LogFile   string

	// Cleanup
	CompletedBatchRetentionHours int
	FailedBatchRetentionDays     int

	// Monitoring
	MetricsPort     int
	HealthCheckPort int

	// Internal
	ProjectRoot string
}

func LoadConfig() (*Config, error) {
	// Load .env file if it exists
	_ = godotenv.Load()

	cfg := &Config{
		// Telegram
		TelegramBotToken: getEnv("TELEGRAM_BOT_TOKEN", ""),
		AdminIDs:         parseAdminIDs(getEnv("ADMIN_IDS", "")),
		UseLocalBotAPI:   getEnvBool("USE_LOCAL_BOT_API", true),
		LocalBotAPIURL:   getEnv("LOCAL_BOT_API_URL", "http://localhost:8081"),
		MaxFileSizeMB:    getEnvInt64("MAX_FILE_SIZE_MB", 4096),

		// Database
		DBHost:     getEnv("DB_HOST", "localhost"),
		DBPort:     getEnvInt("DB_PORT", 5432),
		DBName:     getEnv("DB_NAME", "telegram_bot_option2"),
		DBUser:     getEnv("DB_USER", "bot_user"),
		DBPassword: getEnv("DB_PASSWORD", ""),
		DBSSLMode:  getEnv("DB_SSL_MODE", "disable"),

		// Workers
		MaxDownloadWorkers: getEnvInt("MAX_DOWNLOAD_WORKERS", 3),
		MaxBatchWorkers:    getEnvInt("MAX_BATCH_WORKERS", 5),
		BatchSize:          getEnvInt("BATCH_SIZE", 10),
		BatchTimeoutSec:    getEnvInt("BATCH_TIMEOUT_SEC", 300),

		// Timeouts
		DownloadTimeoutSec: getEnvInt("DOWNLOAD_TIMEOUT_SEC", 1800),
		ExtractTimeoutSec:  getEnvInt("EXTRACT_TIMEOUT_SEC", 1800),
		ConvertTimeoutSec:  getEnvInt("CONVERT_TIMEOUT_SEC", 1800),
		StoreTimeoutSec:    getEnvInt("STORE_TIMEOUT_SEC", 3600),

		// Resource Limits
		MaxRAMPercent: getEnvInt("MAX_RAM_PERCENT", 20),
		MaxCPUPercent: getEnvInt("MAX_CPU_PERCENT", 50),

		// Logging
		LogLevel:  getEnv("LOG_LEVEL", "info"),
		LogFormat: getEnv("LOG_FORMAT", "json"),
		LogFile:   getEnv("LOG_FILE", "logs/coordinator.log"),

		// Cleanup
		CompletedBatchRetentionHours: getEnvInt("COMPLETED_BATCH_RETENTION_HOURS", 1),
		FailedBatchRetentionDays:     getEnvInt("FAILED_BATCH_RETENTION_DAYS", 7),

		// Monitoring
		MetricsPort:     getEnvInt("METRICS_PORT", 9090),
		HealthCheckPort: getEnvInt("HEALTH_CHECK_PORT", 8080),
	}

	// Determine project root
	if wd, err := os.Getwd(); err == nil {
		cfg.ProjectRoot = wd
	} else {
		cfg.ProjectRoot = "."
	}

	// Validate required fields
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) Validate() error {
	if c.TelegramBotToken == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	if len(c.AdminIDs) == 0 {
		return fmt.Errorf("ADMIN_IDS is required (comma-separated user IDs)")
	}
	if c.DBPassword == "" {
		return fmt.Errorf("DB_PASSWORD is required")
	}
	if c.MaxDownloadWorkers != 3 {
		return fmt.Errorf("MAX_DOWNLOAD_WORKERS must be exactly 3 (Telegram API limit)")
	}
	if c.MaxBatchWorkers < 1 || c.MaxBatchWorkers > 20 {
		return fmt.Errorf("MAX_BATCH_WORKERS must be between 1 and 20")
	}
	return nil
}

func (c *Config) GetProjectRoot() string {
	return c.ProjectRoot
}

func (c *Config) GetDSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSSLMode,
	)
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

func parseAdminIDs(s string) []int64 {
	if s == "" {
		return []int64{}
	}

	parts := strings.Split(s, ",")
	ids := make([]int64, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if id, err := strconv.ParseInt(part, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}

	return ids
}
