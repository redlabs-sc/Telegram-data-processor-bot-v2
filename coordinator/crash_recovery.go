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

	if err := cr.recoverStuckRounds(ctx); err != nil {
		return err
	}

	if err := cr.recoverStuckStoreTasks(ctx); err != nil {
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

// recoverStuckRounds resets rounds that were interrupted during extract or convert stages
func (cr *CrashRecovery) recoverStuckRounds(ctx context.Context) error {
	// Find rounds stuck in EXTRACTING state for > 2 hours
	extractQuery := `
		UPDATE processing_rounds
		SET extract_status = 'PENDING',
		    extract_worker_id = NULL,
		    extract_started_at = NULL,
		    round_status = 'CREATED'
		WHERE extract_status = 'EXTRACTING'
		  AND extract_started_at < NOW() - INTERVAL '2 hours'
		RETURNING round_id
	`

	rows, err := cr.db.QueryContext(ctx, extractQuery)
	if err != nil {
		cr.logger.Error("Failed to recover stuck extract rounds", zap.Error(err))
		return err
	}

	extractCount := 0
	for rows.Next() {
		var roundID string
		if err := rows.Scan(&roundID); err != nil {
			cr.logger.Error("Failed to scan recovered extract round", zap.Error(err))
			continue
		}
		cr.logger.Info("Recovered stuck extract round", zap.String("round_id", roundID))
		extractCount++
	}
	rows.Close()

	if extractCount > 0 {
		cr.logger.Info("Recovered stuck extract rounds", zap.Int("count", extractCount))
	}

	// Find rounds stuck in CONVERTING state for > 2 hours
	convertQuery := `
		UPDATE processing_rounds
		SET convert_status = 'PENDING',
		    convert_worker_id = NULL,
		    convert_started_at = NULL,
		    round_status = 'EXTRACTING'
		WHERE convert_status = 'CONVERTING'
		  AND convert_started_at < NOW() - INTERVAL '2 hours'
		RETURNING round_id
	`

	rows, err = cr.db.QueryContext(ctx, convertQuery)
	if err != nil {
		cr.logger.Error("Failed to recover stuck convert rounds", zap.Error(err))
		return err
	}

	convertCount := 0
	for rows.Next() {
		var roundID string
		if err := rows.Scan(&roundID); err != nil {
			cr.logger.Error("Failed to scan recovered convert round", zap.Error(err))
			continue
		}
		cr.logger.Info("Recovered stuck convert round", zap.String("round_id", roundID))
		convertCount++
	}
	rows.Close()

	if convertCount > 0 {
		cr.logger.Info("Recovered stuck convert rounds", zap.Int("count", convertCount))
	}

	return nil
}

// recoverStuckStoreTasks resets store tasks that were interrupted
func (cr *CrashRecovery) recoverStuckStoreTasks(ctx context.Context) error {
	// Find store tasks stuck in STORING state for > 2 hours
	query := `
		UPDATE store_tasks
		SET status = 'PENDING',
		    worker_id = NULL,
		    started_at = NULL
		WHERE status = 'STORING'
		  AND started_at < NOW() - INTERVAL '2 hours'
		RETURNING task_id, round_id
	`

	rows, err := cr.db.QueryContext(ctx, query)
	if err != nil {
		cr.logger.Error("Failed to recover stuck store tasks", zap.Error(err))
		return err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var taskID int64
		var roundID string
		if err := rows.Scan(&taskID, &roundID); err != nil {
			cr.logger.Error("Failed to scan recovered store task", zap.Error(err))
			continue
		}

		cr.logger.Info("Recovered stuck store task",
			zap.Int64("task_id", taskID),
			zap.String("round_id", roundID))
		count++
	}

	if count > 0 {
		cr.logger.Info("Recovered stuck store tasks", zap.Int("count", count))
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

	// Check for rounds stuck in extract stage for > 2 hours
	var stuckExtractRounds int
	err = cr.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM processing_rounds
		WHERE extract_status = 'EXTRACTING'
		  AND extract_started_at < NOW() - INTERVAL '2 hours'
	`).Scan(&stuckExtractRounds)

	if err == nil && stuckExtractRounds > 0 {
		cr.logger.Warn("Detected stuck extract rounds", zap.Int("count", stuckExtractRounds))
		cr.recoverStuckRounds(ctx)
	}

	// Check for rounds stuck in convert stage for > 2 hours
	var stuckConvertRounds int
	err = cr.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM processing_rounds
		WHERE convert_status = 'CONVERTING'
		  AND convert_started_at < NOW() - INTERVAL '2 hours'
	`).Scan(&stuckConvertRounds)

	if err == nil && stuckConvertRounds > 0 {
		cr.logger.Warn("Detected stuck convert rounds", zap.Int("count", stuckConvertRounds))
		cr.recoverStuckRounds(ctx)
	}

	// Check for store tasks stuck for > 2 hours
	var stuckStoreTasks int
	err = cr.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM store_tasks
		WHERE status = 'STORING'
		  AND started_at < NOW() - INTERVAL '2 hours'
	`).Scan(&stuckStoreTasks)

	if err == nil && stuckStoreTasks > 0 {
		cr.logger.Warn("Detected stuck store tasks", zap.Int("count", stuckStoreTasks))
		cr.recoverStuckStoreTasks(ctx)
	}
}
