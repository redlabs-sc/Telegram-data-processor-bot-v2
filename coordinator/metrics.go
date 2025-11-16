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

	// Batch metrics
	batchProcessingDuration *prometheus.HistogramVec
	batchesActive           prometheus.Gauge
	batchesTotal            *prometheus.CounterVec

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

		batchProcessingDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "telegram_bot_batch_processing_duration_seconds",
				Help:    "Time to process a batch",
				Buckets: []float64{60, 300, 600, 1200, 1800, 3600, 7200}, // 1m, 5m, 10m, 20m, 30m, 1h, 2h
			},
			[]string{"stage"},
		),

		batchesActive: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "telegram_bot_batches_active",
				Help: "Number of currently active batches",
			},
		),

		batchesTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "telegram_bot_batches_total",
				Help: "Total number of batches processed",
			},
			[]string{"status"},
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

// UpdateBatchMetrics updates batch-related metrics from database
func (mc *MetricsCollector) UpdateBatchMetrics() {
	var processing int

	query := `
		SELECT COUNT(*)
		FROM batch_processing
		WHERE status IN ('EXTRACTING', 'CONVERTING', 'STORING')
	`

	err := mc.db.QueryRow(query).Scan(&processing)
	if err != nil {
		mc.logger.Error("Failed to update batch metrics", zap.Error(err))
		return
	}

	mc.batchesActive.Set(float64(processing))
}

// RecordBatchDuration records the duration of a batch processing stage
func (mc *MetricsCollector) RecordBatchDuration(stage string, durationSec int) {
	mc.batchProcessingDuration.WithLabelValues(stage).Observe(float64(durationSec))
}

// RecordBatchCompleted increments the batch completion counter
func (mc *MetricsCollector) RecordBatchCompleted(status string) {
	mc.batchesTotal.WithLabelValues(status).Inc()
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
