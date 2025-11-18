package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"syscall"
	"time"

	"go.uber.org/zap"
)

type HealthChecker struct {
	cfg    *Config
	db     *sql.DB
	logger *zap.Logger
}

type HealthResponse struct {
	Status     string                 `json:"status"`
	Timestamp  string                 `json:"timestamp"`
	Components map[string]interface{} `json:"components"`
	Queue      QueueStats             `json:"queue"`
	Batches    BatchStats             `json:"batches"`
}

type QueueStats struct {
	Pending     int `json:"pending"`
	Downloading int `json:"downloading"`
	Downloaded  int `json:"downloaded"`
	Failed      int `json:"failed"`
}

type BatchStats struct {
	Queued             int `json:"queued"`
	Processing         int `json:"processing"`
	CompletedLastHour  int `json:"completed_last_hour"`
}

func NewHealthChecker(cfg *Config, db *sql.DB, logger *zap.Logger) *HealthChecker {
	return &HealthChecker{
		cfg:    cfg,
		db:     db,
		logger: logger,
	}
}

func (h *HealthChecker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	response := HealthResponse{
		Status:     "healthy",
		Timestamp:  time.Now().Format(time.RFC3339),
		Components: make(map[string]interface{}),
	}

	// Check database
	dbStatus := h.checkDatabase()
	response.Components["database"] = dbStatus

	// Check filesystem
	fsStatus := h.checkFilesystem()
	response.Components["filesystem"] = fsStatus

	// Get queue statistics
	queueStats, err := h.getQueueStats()
	if err != nil {
		h.logger.Error("Failed to get queue stats", zap.Error(err))
		queueStats = QueueStats{}
	}
	response.Queue = queueStats

	// Get batch statistics
	batchStats, err := h.getBatchStats()
	if err != nil {
		h.logger.Error("Failed to get batch stats", zap.Error(err))
		batchStats = BatchStats{}
	}
	response.Batches = batchStats

	// Determine overall status
	if dbStatus != "healthy" || fsStatus != "healthy" {
		response.Status = "unhealthy"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (h *HealthChecker) checkDatabase() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := h.db.PingContext(ctx); err != nil {
		h.logger.Error("Database health check failed", zap.Error(err))
		return "unhealthy"
	}

	return "healthy"
}

func (h *HealthChecker) checkFilesystem() string {
	// Check if we can write to critical directories
	dirs := []string{"downloads", "store_tasks", "logs"}

	for _, dir := range dirs {
		testFile := dir + "/.health_check"
		if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
			h.logger.Error("Filesystem health check failed", zap.String("dir", dir), zap.Error(err))
			return "unhealthy"
		}
		os.Remove(testFile)
	}

	// Check disk space
	var stat syscall.Statfs_t
	if err := syscall.Statfs(".", &stat); err != nil {
		h.logger.Error("Failed to get disk stats", zap.Error(err))
		return "unhealthy"
	}

	// Calculate available space in GB
	availableGB := stat.Bavail * uint64(stat.Bsize) / (1024 * 1024 * 1024)
	if availableGB < 50 {
		h.logger.Warn("Low disk space", zap.Uint64("available_gb", availableGB))
		return "degraded"
	}

	return "healthy"
}

func (h *HealthChecker) getQueueStats() (QueueStats, error) {
	var stats QueueStats

	query := `
		SELECT
			COUNT(CASE WHEN status = 'PENDING' THEN 1 END) as pending,
			COUNT(CASE WHEN status = 'DOWNLOADING' THEN 1 END) as downloading,
			COUNT(CASE WHEN status = 'DOWNLOADED' THEN 1 END) as downloaded,
			COUNT(CASE WHEN status = 'FAILED' THEN 1 END) as failed
		FROM download_queue
	`

	err := h.db.QueryRow(query).Scan(
		&stats.Pending,
		&stats.Downloading,
		&stats.Downloaded,
		&stats.Failed,
	)

	return stats, err
}

func (h *HealthChecker) getBatchStats() (BatchStats, error) {
	var stats BatchStats

	query := `
		SELECT
			COUNT(CASE WHEN status = 'QUEUED' THEN 1 END) as queued,
			COUNT(CASE WHEN status IN ('EXTRACTING', 'CONVERTING', 'STORING') THEN 1 END) as processing,
			COUNT(CASE WHEN status = 'COMPLETED' AND completed_at > NOW() - INTERVAL '1 hour' THEN 1 END) as completed_last_hour
		FROM batch_processing
	`

	err := h.db.QueryRow(query).Scan(
		&stats.Queued,
		&stats.Processing,
		&stats.CompletedLastHour,
	)

	return stats, err
}
