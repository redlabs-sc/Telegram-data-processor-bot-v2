package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"

	// Import preserved extraction package (contains store functionality)
	"github.com/redlabs-sc/telegram-bot-option2/app/extraction"
)

// StoreTask represents a store task (2 files)
type StoreTask struct {
	TaskID    int64
	RoundID   string
	FilePaths []string
}

// StoreWorker processes store tasks in parallel.
// Multiple store workers (5 by default) can run simultaneously.
func storeWorker(ctx context.Context, workerID string, db *sql.DB, logger *zap.Logger) {
	logger.Info("Store worker started", zap.String("worker_id", workerID))

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Try to claim a store task
			task, err := claimStoreTask(ctx, db, workerID, logger)
			if err == sql.ErrNoRows {
				// No tasks ready - this is normal
				continue
			}
			if err != nil {
				logger.Error("Failed to claim store task", zap.Error(err))
				continue
			}

			// Process the task
			logger.Info("Starting store task",
				zap.String("worker_id", workerID),
				zap.Int64("task_id", task.TaskID),
				zap.String("round_id", task.RoundID),
				zap.Int("file_count", len(task.FilePaths)))

			startTime := time.Now()
			err = runStoreTask(task, logger)
			duration := int(time.Since(startTime).Seconds())

			// Update task status based on result
			if err != nil {
				logger.Error("Store task failed",
					zap.Int64("task_id", task.TaskID),
					zap.String("round_id", task.RoundID),
					zap.Error(err))

				updateStoreTaskStatus(db, task.TaskID, "FAILED", duration, err.Error(), logger)
			} else {
				logger.Info("Store task completed",
					zap.Int64("task_id", task.TaskID),
					zap.String("round_id", task.RoundID),
					zap.Int("duration_sec", duration))

				updateStoreTaskStatus(db, task.TaskID, "COMPLETED", duration, "", logger)
			}

			// Check if all tasks for this round are complete
			checkRoundStoreCompletion(db, task.RoundID, logger)

		case <-ctx.Done():
			logger.Info("Store worker shutting down", zap.String("worker_id", workerID))
			return
		}
	}
}

// claimStoreTask atomically claims the next store task.
// Uses FOR UPDATE SKIP LOCKED for optimistic concurrency control.
func claimStoreTask(ctx context.Context, db *sql.DB, workerID string, logger *zap.Logger) (*StoreTask, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var task StoreTask
	err = tx.QueryRowContext(ctx, `
		UPDATE store_tasks
		SET status = 'STORING',
		    worker_id = $1,
		    started_at = NOW()
		WHERE task_id = (
		    SELECT task_id FROM store_tasks
		    WHERE status = 'PENDING'
		    ORDER BY created_at ASC
		    LIMIT 1
		    FOR UPDATE SKIP LOCKED
		)
		RETURNING task_id, round_id, file_paths
	`, workerID).Scan(&task.TaskID, &task.RoundID, pq.Array(&task.FilePaths))

	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	return &task, nil
}

// runStoreTask executes the preserved store.go function in an isolated directory.
// This prevents conflicts when multiple store workers run simultaneously.
func runStoreTask(task *StoreTask, logger *zap.Logger) error {
	// Create isolated task directory
	taskDir := fmt.Sprintf("store_tasks/%d", task.TaskID)
	defer func() {
		// Cleanup task directory after processing
		if err := os.RemoveAll(taskDir); err != nil {
			logger.Warn("Failed to cleanup task directory",
				zap.String("task_dir", taskDir),
				zap.Error(err))
		}
	}()

	// Setup directory structure matching store.go expectations
	err := setupStoreTaskDirectory(taskDir, task.FilePaths, logger)
	if err != nil {
		return fmt.Errorf("failed to setup task directory: %w", err)
	}

	// Save current working directory
	originalDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	// Change to task directory
	err = os.Chdir(taskDir)
	if err != nil {
		return fmt.Errorf("failed to change to task directory: %w", err)
	}

	// Always restore original directory
	defer func() {
		if err := os.Chdir(originalDir); err != nil {
			logger.Error("Failed to restore working directory",
				zap.String("original_dir", originalDir),
				zap.Error(err))
		}
	}()

	// Call preserved store function
	// store.RunPipeline() processes files in app/extraction/files/txt/
	// and moves them through: move → merge → valuable → filter → database
	logger.Info("Calling extraction.RunPipeline()",
		zap.Int64("task_id", task.TaskID),
		zap.String("working_dir", taskDir))

	storeCtx := context.Background()

	// Create a logger function compatible with store.go's expected signature
	logFunc := func(format string, args ...interface{}) {
		logger.Info(fmt.Sprintf(format, args...))
	}

	storeService := extraction.NewStoreService(logFunc)

	err = storeService.RunPipeline(storeCtx)
	if err != nil {
		return fmt.Errorf("store.RunPipeline() failed: %w", err)
	}

	return nil
}

// setupStoreTaskDirectory creates the isolated directory structure for a store task.
// Copies only the 2 assigned files and necessary configuration.
func setupStoreTaskDirectory(taskDir string, filePaths []string, logger *zap.Logger) error {
	// Create required directories
	dirs := []string{
		filepath.Join(taskDir, "app/extraction/files/txt"),
		filepath.Join(taskDir, "app/extraction/files/nonsorted"),
		filepath.Join(taskDir, "app/extraction/files/done"),
		filepath.Join(taskDir, "app/extraction/files/errors"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Copy the assigned files to task's txt directory
	for _, srcPath := range filePaths {
		// Verify source file exists
		if _, err := os.Stat(srcPath); os.IsNotExist(err) {
			logger.Warn("Source file does not exist", zap.String("file", srcPath))
			continue
		}

		dstPath := filepath.Join(taskDir, "app/extraction/files/txt", filepath.Base(srcPath))
		if err := copyFile(srcPath, dstPath); err != nil {
			return fmt.Errorf("failed to copy file %s: %w", srcPath, err)
		}

		logger.Debug("Copied file to task directory",
			zap.String("src", srcPath),
			zap.String("dst", dstPath))
	}

	// Copy .env file for database configuration
	// store.go needs DB credentials to connect to MySQL/PostgreSQL
	envSrc := ".env"
	envDst := filepath.Join(taskDir, ".env")
	if _, err := os.Stat(envSrc); err == nil {
		if err := copyFile(envSrc, envDst); err != nil {
			return fmt.Errorf("failed to copy .env file: %w", err)
		}
	} else {
		logger.Warn(".env file not found, store.go may fail if it needs DB credentials")
	}

	return nil
}

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	// Open source file
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer srcFile.Close()

	// Create destination file
	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	defer dstFile.Close()

	// Copy contents
	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return fmt.Errorf("copy contents: %w", err)
	}

	// Sync to disk
	err = dstFile.Sync()
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}

	return nil
}

// updateStoreTaskStatus updates the status of a store task
func updateStoreTaskStatus(db *sql.DB, taskID int64, status string, durationSec int, errorMsg string, logger *zap.Logger) {
	query := `
		UPDATE store_tasks
		SET status = $1,
		    completed_at = NOW(),
		    duration_sec = $2,
		    error_message = $3
		WHERE task_id = $4
	`

	_, err := db.Exec(query, status, durationSec, errorMsg, taskID)
	if err != nil {
		logger.Error("Failed to update store task status",
			zap.Int64("task_id", taskID),
			zap.String("status", status),
			zap.Error(err))
	}
}

// checkRoundStoreCompletion checks if all store tasks for a round are complete.
// If so, updates the round's store_status and round_status.
func checkRoundStoreCompletion(db *sql.DB, roundID string, logger *zap.Logger) {
	tx, err := db.Begin()
	if err != nil {
		logger.Error("Failed to begin transaction for round completion check",
			zap.String("round_id", roundID),
			zap.Error(err))
		return
	}
	defer tx.Rollback()

	// Count pending/storing tasks for this round
	var pendingCount int
	err = tx.QueryRow(`
		SELECT COUNT(*) FROM store_tasks
		WHERE round_id = $1 AND status IN ('PENDING', 'STORING')
	`, roundID).Scan(&pendingCount)

	if err != nil {
		logger.Error("Failed to count pending tasks",
			zap.String("round_id", roundID),
			zap.Error(err))
		return
	}

	// If there are still pending tasks, round is not complete
	if pendingCount > 0 {
		return
	}

	// All tasks are complete (either COMPLETED or FAILED)
	// Check if any failed
	var failedCount int
	err = tx.QueryRow(`
		SELECT COUNT(*) FROM store_tasks
		WHERE round_id = $1 AND status = 'FAILED'
	`, roundID).Scan(&failedCount)

	if err != nil {
		logger.Error("Failed to count failed tasks",
			zap.String("round_id", roundID),
			zap.Error(err))
		return
	}

	// Update round status
	var roundStatus, storeStatus string
	if failedCount > 0 {
		roundStatus = "FAILED"
		storeStatus = "FAILED"
		logger.Warn("Round has failed store tasks",
			zap.String("round_id", roundID),
			zap.Int("failed_count", failedCount))
	} else {
		roundStatus = "COMPLETED"
		storeStatus = "COMPLETED"
		logger.Info("Round store stage completed successfully",
			zap.String("round_id", roundID))
	}

	// Calculate total store duration
	var totalDuration int
	tx.QueryRow(`
		SELECT COALESCE(SUM(duration_sec), 0)
		FROM store_tasks
		WHERE round_id = $1
	`, roundID).Scan(&totalDuration)

	_, err = tx.Exec(`
		UPDATE processing_rounds
		SET store_status = $1,
		    round_status = $2,
		    store_completed_at = NOW(),
		    store_duration_sec = $3
		WHERE round_id = $4
	`, storeStatus, roundStatus, totalDuration, roundID)

	if err != nil {
		logger.Error("Failed to update round status",
			zap.String("round_id", roundID),
			zap.Error(err))
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Error("Failed to commit round completion",
			zap.String("round_id", roundID),
			zap.Error(err))
		return
	}

	logger.Info("Round completed",
		zap.String("round_id", roundID),
		zap.String("final_status", roundStatus),
		zap.Int("store_duration_sec", totalDuration))
}
