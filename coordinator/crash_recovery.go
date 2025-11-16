package main

import (
	"context"
	"database/sql"
	"time"

	"go.uber.org/zap"
)

type CrashRecovery struct {
	db     *sql.DB
	logger *zap.Logger
}

func NewCrashRecovery(db *sql.DB, logger *zap.Logger) *CrashRecovery {
	return &CrashRecovery{
		db:     db,
		logger: logger,
	}
}

// RecoverOnStartup performs crash recovery when the coordinator starts
func (cr *CrashRecovery) RecoverOnStartup(ctx context.Context) error {
	cr.logger.Info("Starting crash recovery")

	if err := cr.recoverStuckDownloads(ctx); err != nil {
		return err
	}

	if err := cr.recoverStuckBatches(ctx); err != nil {
		return err
	}

	cr.logger.Info("Crash recovery completed")
	return nil
}

// recoverStuckDownloads resets downloads that were interrupted
func (cr *CrashRecovery) recoverStuckDownloads(ctx context.Context) error {
	// Find downloads stuck in DOWNLOADING state for > 30 minutes
	query := `
		UPDATE download_queue
		SET status = 'PENDING',
			started_at = NULL
		WHERE status = 'DOWNLOADING'
		  AND started_at < NOW() - INTERVAL '30 minutes'
		RETURNING task_id, filename
	`

	rows, err := cr.db.QueryContext(ctx, query)
	if err != nil {
		cr.logger.Error("Failed to recover stuck downloads", zap.Error(err))
		return err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var taskID int64
		var filename string
		if err := rows.Scan(&taskID, &filename); err != nil {
			cr.logger.Error("Failed to scan recovered download", zap.Error(err))
			continue
		}

		cr.logger.Info("Recovered stuck download",
			zap.Int64("task_id", taskID),
			zap.String("filename", filename))
		count++
	}

	if count > 0 {
		cr.logger.Info("Recovered stuck downloads", zap.Int("count", count))
	}

	return nil
}

// recoverStuckBatches resets batches that were interrupted
func (cr *CrashRecovery) recoverStuckBatches(ctx context.Context) error {
	// Find batches stuck in processing states for > 2 hours
	query := `
		UPDATE batch_processing
		SET status = 'QUEUED',
			worker_id = NULL,
			started_at = NULL
		WHERE status IN ('EXTRACTING', 'CONVERTING', 'STORING')
		  AND started_at < NOW() - INTERVAL '2 hours'
		RETURNING batch_id
	`

	rows, err := cr.db.QueryContext(ctx, query)
	if err != nil {
		cr.logger.Error("Failed to recover stuck batches", zap.Error(err))
		return err
	}
	defer rows.Close()

	count := 0
	var batchIDs []string
	for rows.Next() {
		var batchID string
		if err := rows.Scan(&batchID); err != nil {
			cr.logger.Error("Failed to scan recovered batch", zap.Error(err))
			continue
		}
		batchIDs = append(batchIDs, batchID)
		count++
	}

	if count > 0 {
		cr.logger.Info("Recovered stuck batches", zap.Int("count", count))

		// Reset files in recovered batches to DOWNLOADED state
		for _, batchID := range batchIDs {
			_, err := cr.db.ExecContext(ctx, `
				UPDATE download_queue
				SET batch_id = NULL
				WHERE batch_id = $1
			`, batchID)

			if err != nil {
				cr.logger.Error("Failed to reset batch files",
					zap.String("batch_id", batchID),
					zap.Error(err))
			} else {
				cr.logger.Info("Reset files from recovered batch",
					zap.String("batch_id", batchID))
			}
		}
	}

	return nil
}

// PeriodicHealthCheck runs periodic checks to detect and recover from issues
func (cr *CrashRecovery) PeriodicHealthCheck(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cr.checkForStuckTasks(ctx)
		}
	}
}

func (cr *CrashRecovery) checkForStuckTasks(ctx context.Context) {
	// Check for downloads stuck for > 30 minutes
	var stuckDownloads int
	err := cr.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM download_queue
		WHERE status = 'DOWNLOADING'
		  AND started_at < NOW() - INTERVAL '30 minutes'
	`).Scan(&stuckDownloads)

	if err == nil && stuckDownloads > 0 {
		cr.logger.Warn("Detected stuck downloads", zap.Int("count", stuckDownloads))
		cr.recoverStuckDownloads(ctx)
	}

	// Check for batches stuck for > 2 hours
	var stuckBatches int
	err = cr.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM batch_processing
		WHERE status IN ('EXTRACTING', 'CONVERTING', 'STORING')
		  AND started_at < NOW() - INTERVAL '2 hours'
	`).Scan(&stuckBatches)

	if err == nil && stuckBatches > 0 {
		cr.logger.Warn("Detected stuck batches", zap.Int("count", stuckBatches))
		cr.recoverStuckBatches(ctx)
	}
}
