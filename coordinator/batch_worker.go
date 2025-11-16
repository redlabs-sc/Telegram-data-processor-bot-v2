package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

type BatchWorker struct {
	id      string
	cfg     *Config
	db      *sql.DB
	logger  *zap.Logger
	metrics *MetricsCollector
}

func NewBatchWorker(id string, cfg *Config, db *sql.DB, logger *zap.Logger, metrics *MetricsCollector) *BatchWorker {
	return &BatchWorker{
		id:      id,
		cfg:     cfg,
		db:      db,
		logger:  logger.With(zap.String("worker", id)),
		metrics: metrics,
	}
}

func (bw *BatchWorker) Start(ctx context.Context) {
	bw.logger.Info("Batch worker started")
	bw.metrics.SetWorkerStatus("batch", bw.id, true)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			bw.logger.Info("Batch worker stopping")
			bw.metrics.SetWorkerStatus("batch", bw.id, false)
			return
		case <-ticker.C:
			bw.processNext(ctx)
		}
	}
}

func (bw *BatchWorker) processNext(ctx context.Context) {
	// Claim next queued batch with optimistic locking
	tx, err := bw.db.BeginTx(ctx, nil)
	if err != nil {
		bw.logger.Error("Error starting transaction", zap.Error(err))
		return
	}
	defer tx.Rollback()

	var batchID string
	var fileCount, archiveCount, txtCount int

	err = tx.QueryRowContext(ctx, `
		SELECT batch_id, file_count, archive_count, txt_count
		FROM batch_processing
		WHERE status = 'QUEUED'
		ORDER BY created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(&batchID, &fileCount, &archiveCount, &txtCount)

	if err == sql.ErrNoRows {
		// No queued batches
		return
	}
	if err != nil {
		bw.logger.Error("Error querying batch", zap.Error(err))
		return
	}

	// Mark as EXTRACTING
	_, err = tx.ExecContext(ctx, `
		UPDATE batch_processing
		SET status = 'EXTRACTING',
			worker_id = $2,
			started_at = NOW()
		WHERE batch_id = $1
	`, batchID, bw.id)

	if err != nil {
		bw.logger.Error("Error updating batch status", zap.Error(err))
		return
	}

	if err := tx.Commit(); err != nil {
		bw.logger.Error("Error committing transaction", zap.Error(err))
		return
	}

	bw.logger.Info("Claimed batch",
		zap.String("batch_id", batchID),
		zap.Int("file_count", fileCount),
		zap.Int("archive_count", archiveCount),
		zap.Int("txt_count", txtCount))

	// Process batch through pipeline
	if err := bw.processBatch(ctx, batchID, archiveCount, txtCount); err != nil {
		bw.logger.Error("Batch processing failed",
			zap.String("batch_id", batchID),
			zap.Error(err))

		// Mark as FAILED
		bw.db.Exec(`
			UPDATE batch_processing
			SET status = 'FAILED',
				error_message = $2,
				completed_at = NOW()
			WHERE batch_id = $1
		`, batchID, err.Error())

		bw.metrics.RecordBatchCompleted("failed")
	} else {
		bw.logger.Info("Batch completed successfully",
			zap.String("batch_id", batchID))

		// Mark as COMPLETED
		bw.db.Exec(`
			UPDATE batch_processing
			SET status = 'COMPLETED',
				completed_at = NOW()
			WHERE batch_id = $1
		`, batchID)

		bw.metrics.RecordBatchCompleted("completed")

		// TODO: Notify admin of completion (Phase 5)

		// Cleanup batch directory after a delay
		go bw.cleanupBatchLater(batchID)
	}
}

func (bw *BatchWorker) processBatch(ctx context.Context, batchID string, archiveCount, txtCount int) error {
	batchRoot := filepath.Join("batches", batchID)

	// Verify batch directory exists
	if _, err := os.Stat(batchRoot); os.IsNotExist(err) {
		return fmt.Errorf("batch directory does not exist: %s", batchRoot)
	}

	// Save current working directory
	originalWD, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	defer os.Chdir(originalWD) // Always restore

	// Change to batch directory
	// CRITICAL: This allows extract.go, convert.go, store.go to work unchanged
	if err := os.Chdir(batchRoot); err != nil {
		return fmt.Errorf("change to batch directory: %w", err)
	}

	bw.logger.Info("Changed working directory",
		zap.String("batch_id", batchID),
		zap.String("wd", batchRoot))

	// STAGE 1: Extract (only if archives exist)
	if archiveCount > 0 {
		if err := bw.runExtractStage(ctx, batchID); err != nil {
			return fmt.Errorf("extract stage: %w", err)
		}
	} else {
		bw.logger.Info("Skipping extract stage (no archives)",
			zap.String("batch_id", batchID))
	}

	// STAGE 2: Convert (only if archives were extracted)
	if archiveCount > 0 {
		if err := bw.runConvertStage(ctx, batchID); err != nil {
			return fmt.Errorf("convert stage: %w", err)
		}
	} else {
		bw.logger.Info("Skipping convert stage (no archives)",
			zap.String("batch_id", batchID))
	}

	// STAGE 3: Store (processes both converted output and direct TXT files)
	if err := bw.runStoreStage(ctx, batchID); err != nil {
		return fmt.Errorf("store stage: %w", err)
	}

	return nil
}

func (bw *BatchWorker) runExtractStage(ctx context.Context, batchID string) error {
	bw.logger.Info("Starting extract stage", zap.String("batch_id", batchID))

	// Update status
	bw.db.Exec(`
		UPDATE batch_processing
		SET status = 'EXTRACTING'
		WHERE batch_id = $1
	`, batchID)

	startTime := time.Now()

	// Build path to extract.go (relative from project root)
	extractPath := filepath.Join(bw.cfg.ProjectRoot, "app", "extraction", "extract", "extract.go")

	// Create context with timeout
	extractCtx, cancel := context.WithTimeout(ctx, time.Duration(bw.cfg.ExtractTimeoutSec)*time.Second)
	defer cancel()

	// Execute extract.go as subprocess
	// CRITICAL: Working directory is already batch root, so extract.go will process
	// the files in batches/{batch_id}/app/extraction/files/all/
	cmd := exec.CommandContext(extractCtx, "go", "run", extractPath)
	output, err := cmd.CombinedOutput()

	// Log output to batch-specific log file
	logPath := filepath.Join("logs", "extract.log")
	os.WriteFile(logPath, output, 0644)

	duration := time.Since(startTime)

	if err != nil {
		bw.logger.Error("Extract stage failed",
			zap.String("batch_id", batchID),
			zap.Duration("duration", duration),
			zap.Error(err),
			zap.String("output", string(output)))
		return err
	}

	// Store duration in database
	bw.db.Exec(`
		UPDATE batch_processing
		SET extract_duration_sec = $2
		WHERE batch_id = $1
	`, batchID, int(duration.Seconds()))

	bw.metrics.RecordBatchDuration("extract", int(duration.Seconds()))

	bw.logger.Info("Extract stage completed",
		zap.String("batch_id", batchID),
		zap.Duration("duration", duration))

	return nil
}

func (bw *BatchWorker) runConvertStage(ctx context.Context, batchID string) error {
	bw.logger.Info("Starting convert stage", zap.String("batch_id", batchID))

	// Update status
	bw.db.Exec(`
		UPDATE batch_processing
		SET status = 'CONVERTING'
		WHERE batch_id = $1
	`, batchID)

	startTime := time.Now()

	// Build path to convert.go
	convertPath := filepath.Join(bw.cfg.ProjectRoot, "app", "extraction", "convert", "convert.go")

	// Create context with timeout
	convertCtx, cancel := context.WithTimeout(ctx, time.Duration(bw.cfg.ConvertTimeoutSec)*time.Second)
	defer cancel()

	// Execute convert.go as subprocess
	// Working directory is batch root, so convert.go will process
	// batches/{batch_id}/app/extraction/files/pass/
	cmd := exec.CommandContext(convertCtx, "go", "run", convertPath)
	output, err := cmd.CombinedOutput()

	// Log output
	logPath := filepath.Join("logs", "convert.log")
	os.WriteFile(logPath, output, 0644)

	duration := time.Since(startTime)

	if err != nil {
		bw.logger.Error("Convert stage failed",
			zap.String("batch_id", batchID),
			zap.Duration("duration", duration),
			zap.Error(err),
			zap.String("output", string(output)))
		return err
	}

	// Store duration
	bw.db.Exec(`
		UPDATE batch_processing
		SET convert_duration_sec = $2
		WHERE batch_id = $1
	`, batchID, int(duration.Seconds()))

	bw.metrics.RecordBatchDuration("convert", int(duration.Seconds()))

	bw.logger.Info("Convert stage completed",
		zap.String("batch_id", batchID),
		zap.Duration("duration", duration))

	return nil
}

func (bw *BatchWorker) runStoreStage(ctx context.Context, batchID string) error {
	bw.logger.Info("Starting store stage", zap.String("batch_id", batchID))

	// Update status
	bw.db.Exec(`
		UPDATE batch_processing
		SET status = 'STORING'
		WHERE batch_id = $1
	`, batchID)

	startTime := time.Now()

	// Build path to store.go
	storePath := filepath.Join(bw.cfg.ProjectRoot, "app", "extraction", "store.go")

	// Create context with timeout
	storeCtx, cancel := context.WithTimeout(ctx, time.Duration(bw.cfg.StoreTimeoutSec)*time.Second)
	defer cancel()

	// Execute store.go as subprocess
	// Working directory is batch root, so store.go will process
	// batches/{batch_id}/app/extraction/files/txt/ and converted files
	cmd := exec.CommandContext(storeCtx, "go", "run", storePath)
	output, err := cmd.CombinedOutput()

	// Log output
	logPath := filepath.Join("logs", "store.log")
	os.WriteFile(logPath, output, 0644)

	duration := time.Since(startTime)

	if err != nil {
		bw.logger.Error("Store stage failed",
			zap.String("batch_id", batchID),
			zap.Duration("duration", duration),
			zap.Error(err),
			zap.String("output", string(output)))
		return err
	}

	// Store duration
	bw.db.Exec(`
		UPDATE batch_processing
		SET store_duration_sec = $2
		WHERE batch_id = $1
	`, batchID, int(duration.Seconds()))

	bw.metrics.RecordBatchDuration("store", int(duration.Seconds()))

	bw.logger.Info("Store stage completed",
		zap.String("batch_id", batchID),
		zap.Duration("duration", duration))

	return nil
}

func (bw *BatchWorker) cleanupBatchLater(batchID string) {
	// Wait for retention period before cleanup
	retentionDuration := time.Duration(bw.cfg.CompletedBatchRetentionHours) * time.Hour
	time.Sleep(retentionDuration)

	batchRoot := filepath.Join("batches", batchID)

	if err := os.RemoveAll(batchRoot); err != nil {
		bw.logger.Error("Failed to cleanup batch directory",
			zap.String("batch_id", batchID),
			zap.Error(err))
		return
	}

	bw.logger.Info("Batch directory cleaned up",
		zap.String("batch_id", batchID),
		zap.Duration("after", retentionDuration))
}
