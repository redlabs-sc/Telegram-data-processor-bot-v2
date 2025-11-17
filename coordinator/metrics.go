package main

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

type MetricsCollector struct {
	db     *sql.DB
	logger *zap.Logger

	// Queue metrics
	queueSize *prometheus.GaugeVec

	// Round metrics
	roundProcessingDuration *prometheus.HistogramVec
	roundsActive            *prometheus.GaugeVec // By stage: extracting, converting, storing
	roundsTotal             *prometheus.CounterVec
	storeTasksActive        prometheus.Gauge
	storeTasksTotal         *prometheus.CounterVec

	// Worker metrics
	workerStatus *prometheus.GaugeVec

	// File metrics
	filesProcessed *prometheus.CounterVec
	filesSize      *prometheus.HistogramVec
}

func NewMetricsCollector(db *sql.DB, logger *zap.Logger) *MetricsCollector {
	mc := &MetricsCollector{
		db:     db,
		logger: logger,

		queueSize: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "telegram_bot_queue_size",
				Help: "Number of tasks in each queue status",
			},
			[]string{"status"},
		),

		roundProcessingDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "telegram_bot_round_processing_duration_seconds",
				Help:    "Time to process a round through each stage",
				Buckets: []float64{60, 300, 600, 1200, 1800, 3600, 7200}, // 1m, 5m, 10m, 20m, 30m, 1h, 2h
			},
			[]string{"stage"}, // extract, convert, store
		),

		roundsActive: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "telegram_bot_rounds_active",
				Help: "Number of rounds in each processing stage",
			},
			[]string{"stage"}, // extracting, converting, storing
		),

		roundsTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telegram_bot_rounds_total",
				Help: "Total number of rounds processed",
			},
			[]string{"status"}, // completed, failed
		),

		storeTasksActive: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "telegram_bot_store_tasks_active",
				Help: "Number of store tasks currently processing",
			},
		),

		storeTasksTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telegram_bot_store_tasks_total",
				Help: "Total number of store tasks processed",
			},
			[]string{"status"}, // completed, failed
		),

		workerStatus: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "telegram_bot_worker_status",
				Help: "Worker health status (1=healthy, 0=unhealthy)",
			},
			[]string{"type", "id"},
		),

		filesProcessed: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telegram_bot_files_processed_total",
				Help: "Total number of files processed",
			},
			[]string{"type", "status"},
		),

		filesSize: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "telegram_bot_file_size_bytes",
				Help:    "Size of processed files",
				Buckets: prometheus.ExponentialBuckets(1024*1024, 2, 12), // 1MB to 4GB
			},
			[]string{"type"},
		),
	}

	return mc
}

// UpdateQueueMetrics updates queue size metrics from database
func (mc *MetricsCollector) UpdateQueueMetrics() {
	var pending, downloading, downloaded, failed int

	query := `
		SELECT
			COUNT(CASE WHEN status = 'PENDING' THEN 1 END) as pending,
			COUNT(CASE WHEN status = 'DOWNLOADING' THEN 1 END) as downloading,
			COUNT(CASE WHEN status = 'DOWNLOADED' THEN 1 END) as downloaded,
			COUNT(CASE WHEN status = 'FAILED' THEN 1 END) as failed
		FROM download_queue
	`

	err := mc.db.QueryRow(query).Scan(&pending, &downloading, &downloaded, &failed)
	if err != nil {
		mc.logger.Error("Failed to update queue metrics", zap.Error(err))
		return
	}

	mc.queueSize.WithLabelValues("pending").Set(float64(pending))
	mc.queueSize.WithLabelValues("downloading").Set(float64(downloading))
	mc.queueSize.WithLabelValues("downloaded").Set(float64(downloaded))
	mc.queueSize.WithLabelValues("failed").Set(float64(failed))
}

// UpdateRoundMetrics updates round-related metrics from database
func (mc *MetricsCollector) UpdateRoundMetrics() {
	// Count rounds in each processing stage
	var extracting, converting, storing int

	query := `
		SELECT
			COUNT(CASE WHEN round_status = 'EXTRACTING' THEN 1 END) as extracting,
			COUNT(CASE WHEN round_status = 'CONVERTING' THEN 1 END) as converting,
			COUNT(CASE WHEN round_status = 'STORING' THEN 1 END) as storing
		FROM processing_rounds
		WHERE round_status IN ('EXTRACTING', 'CONVERTING', 'STORING')
	`

	err := mc.db.QueryRow(query).Scan(&extracting, &converting, &storing)
	if err != nil {
		mc.logger.Error("Failed to update round stage metrics", zap.Error(err))
	} else {
		mc.roundsActive.WithLabelValues("extracting").Set(float64(extracting))
		mc.roundsActive.WithLabelValues("converting").Set(float64(converting))
		mc.roundsActive.WithLabelValues("storing").Set(float64(storing))
	}

	// Count active store tasks
	var storeTasksProcessing int
	err = mc.db.QueryRow(`
		SELECT COUNT(*) FROM store_tasks WHERE status = 'STORING'
	`).Scan(&storeTasksProcessing)

	if err != nil {
		mc.logger.Error("Failed to update store task metrics", zap.Error(err))
	} else {
		mc.storeTasksActive.Set(float64(storeTasksProcessing))
	}
}

// RecordRoundDuration records the duration of a round processing stage
func (mc *MetricsCollector) RecordRoundDuration(stage string, durationSec int) {
	mc.roundProcessingDuration.WithLabelValues(stage).Observe(float64(durationSec))
}

// RecordRoundCompleted increments the round completion counter
func (mc *MetricsCollector) RecordRoundCompleted(status string) {
	mc.roundsTotal.WithLabelValues(status).Inc()
}

// RecordStoreTaskCompleted increments the store task completion counter
func (mc *MetricsCollector) RecordStoreTaskCompleted(status string) {
	mc.storeTasksTotal.WithLabelValues(status).Inc()
}

// SetWorkerStatus sets the health status of a worker
func (mc *MetricsCollector) SetWorkerStatus(workerType, workerID string, healthy bool) {
	value := 0.0
	if healthy {
		value = 1.0
	}
	mc.workerStatus.WithLabelValues(workerType, workerID).Set(value)
}

// RecordFileProcessed increments the file processing counter
func (mc *MetricsCollector) RecordFileProcessed(fileType, status string) {
	mc.filesProcessed.WithLabelValues(fileType, status).Inc()
}

// RecordFileSize records the size of a processed file
func (mc *MetricsCollector) RecordFileSize(fileType string, sizeBytes int64) {
	mc.filesSize.WithLabelValues(fileType).Observe(float64(sizeBytes))
}
