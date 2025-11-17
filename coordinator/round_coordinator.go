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

// RoundCoordinator periodically checks for downloaded files and creates processing rounds.
// Rounds are created when:
// 1. >= ROUND_SIZE files are ready (e.g., 50 files)
// 2. >= 10 files AND oldest file has been waiting > ROUND_TIMEOUT_SEC
func roundCoordinator(ctx context.Context, db *sql.DB, cfg *Config, logger *zap.Logger) {
	logger.Info("Round coordinator started",
		zap.Int("round_size", cfg.RoundSize),
		zap.Int("timeout_sec", cfg.RoundTimeoutSec))

	// Check every 30 seconds for files ready to be batched
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := createRoundIfReady(ctx, db, cfg, logger)
			if err != nil {
				logger.Error("Failed to create round", zap.Error(err))
			}

		case <-ctx.Done():
			logger.Info("Round coordinator shutting down")
			return
		}
	}
}

// FileInfo represents a downloaded file ready for processing
type FileInfo struct {
	TaskID   int64
	Filename string
	FileType string
}

// createRoundIfReady checks if conditions are met to create a new round.
// Returns nil if no round was created (normal case).
func createRoundIfReady(ctx context.Context, db *sql.DB, cfg *Config, logger *zap.Logger) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Count files ready for processing (DOWNLOADED status, not assigned to round)
	var readyCount int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM download_queue
		WHERE status = 'DOWNLOADED' AND round_id IS NULL
	`).Scan(&readyCount)

	if err != nil {
		return fmt.Errorf("failed to count ready files: %w", err)
	}

	if readyCount == 0 {
		return nil // No files ready
	}

	// Check timeout condition: oldest downloaded file waiting > ROUND_TIMEOUT_SEC
	var oldestAgeSec float64
	var hasWaiting bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXTRACT(EPOCH FROM (NOW() - MIN(completed_at)))
		FROM download_queue
		WHERE status = 'DOWNLOADED' AND round_id IS NULL
	`).Scan(&oldestAgeSec)

	hasWaiting = (err == nil && oldestAgeSec > float64(cfg.RoundTimeoutSec))

	// Determine minimum files for round creation
	minFiles := 10 // Minimum to create a round with timeout
	if readyCount >= cfg.RoundSize {
		minFiles = cfg.RoundSize // Full round
	}

	// Create round if:
	// 1. >= ROUND_SIZE files ready (full round), OR
	// 2. >= 10 files AND oldest file waiting > timeout
	shouldCreate := (readyCount >= cfg.RoundSize) || (readyCount >= minFiles && hasWaiting)

	if !shouldCreate {
		logger.Debug("Not enough files or timeout not reached",
			zap.Int("ready_count", readyCount),
			zap.Float64("oldest_age_sec", oldestAgeSec),
			zap.Int("round_size", cfg.RoundSize),
			zap.Int("timeout_sec", cfg.RoundTimeoutSec))
		return nil
	}

	// Generate round ID
	var maxRoundNum int
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(CAST(SUBSTRING(round_id FROM 7) AS INT)), 0)
		FROM processing_rounds
		WHERE round_id ~ '^round_[0-9]+$'
	`).Scan(&maxRoundNum)

	if err != nil {
		return fmt.Errorf("failed to get max round number: %w", err)
	}

	roundID := fmt.Sprintf("round_%03d", maxRoundNum+1)

	// Select files for this round
	rows, err := tx.QueryContext(ctx, `
		SELECT task_id, filename, file_type
		FROM download_queue
		WHERE status = 'DOWNLOADED' AND round_id IS NULL
		ORDER BY priority DESC, completed_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, cfg.RoundSize)

	if err != nil {
		return fmt.Errorf("failed to select files for round: %w", err)
	}
	defer rows.Close()

	var files []FileInfo
	var archiveCount, txtCount int

	for rows.Next() {
		var file FileInfo
		err := rows.Scan(&file.TaskID, &file.Filename, &file.FileType)
		if err != nil {
			return fmt.Errorf("failed to scan file: %w", err)
		}

		files = append(files, file)

		// Count file types
		if file.FileType == "ZIP" || file.FileType == "RAR" {
			archiveCount++
		} else if file.FileType == "TXT" {
			txtCount++
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating files: %w", err)
	}

	if len(files) == 0 {
		logger.Debug("No files selected for round (likely locked by another process)")
		return nil
	}

	logger.Info("Creating new round",
		zap.String("round_id", roundID),
		zap.Int("file_count", len(files)),
		zap.Int("archive_count", archiveCount),
		zap.Int("txt_count", txtCount))

	// Determine initial extract status
	// If round has no archives, skip extract stage
	extractStatus := "PENDING"
	if archiveCount == 0 {
		extractStatus = "SKIPPED"
	}

	// Create round record
	_, err = tx.ExecContext(ctx, `
		INSERT INTO processing_rounds (
			round_id, file_count, archive_count, txt_count,
			round_status, extract_status, convert_status, store_status
		) VALUES ($1, $2, $3, $4, 'CREATED', $5, 'PENDING', 'PENDING')
	`, roundID, len(files), archiveCount, txtCount, extractStatus)

	if err != nil {
		return fmt.Errorf("failed to insert round: %w", err)
	}

	// Move files to staging directories and assign to round
	for _, file := range files {
		// Update database first
		_, err := tx.ExecContext(ctx, `
			UPDATE download_queue
			SET round_id = $1
			WHERE task_id = $2
		`, roundID, file.TaskID)

		if err != nil {
			return fmt.Errorf("failed to assign file %d to round: %w", file.TaskID, err)
		}

		// Move file to appropriate staging directory
		srcPath := filepath.Join("downloads", file.Filename)

		var dstPath string
		if file.FileType == "ZIP" || file.FileType == "RAR" {
			// Archives go to extract input directory
			dstPath = filepath.Join("app/extraction/files/all", file.Filename)
		} else if file.FileType == "TXT" {
			// Text files go directly to convert/store input directory
			dstPath = filepath.Join("app/extraction/files/txt", file.Filename)
		} else {
			logger.Warn("Unknown file type, skipping",
				zap.String("filename", file.Filename),
				zap.String("file_type", file.FileType))
			continue
		}

		// Atomic move: copy + verify + delete source
		err = atomicMoveFile(srcPath, dstPath, logger)
		if err != nil {
			return fmt.Errorf("failed to move file %s: %w", file.Filename, err)
		}

		logger.Debug("Moved file to staging",
			zap.String("filename", file.Filename),
			zap.String("src", srcPath),
			zap.String("dst", dstPath))
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	logger.Info("Round created successfully",
		zap.String("round_id", roundID),
		zap.Int("total_files", len(files)),
		zap.Int("archives", archiveCount),
		zap.Int("txt_files", txtCount),
		zap.String("extract_status", extractStatus))

	return nil
}

// atomicMoveFile performs an atomic move operation.
// Uses copy-verify-delete pattern to ensure data integrity.
func atomicMoveFile(src, dst string, logger *zap.Logger) error {
	// Verify source exists
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("source file does not exist: %w", err)
	}

	// Ensure destination directory exists
	dstDir := filepath.Dir(dst)
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	// Try direct rename first (fast, works if same filesystem)
	err = os.Rename(src, dst)
	if err == nil {
		return nil
	}

	// Rename failed (likely different filesystem), use copy-delete pattern
	logger.Debug("Rename failed, using copy-delete pattern",
		zap.String("src", src),
		zap.String("dst", dst),
		zap.Error(err))

	// Copy file
	err = copyFile(src, dst)
	if err != nil {
		return fmt.Errorf("copy failed: %w", err)
	}

	// Verify destination file size matches source
	dstInfo, err := os.Stat(dst)
	if err != nil {
		return fmt.Errorf("failed to verify destination: %w", err)
	}

	if dstInfo.Size() != srcInfo.Size() {
		// Size mismatch - delete partial copy and fail
		os.Remove(dst)
		return fmt.Errorf("size mismatch: src=%d, dst=%d", srcInfo.Size(), dstInfo.Size())
	}

	// Delete source only after successful copy and verification
	err = os.Remove(src)
	if err != nil {
		logger.Warn("Failed to delete source file after copy (destination file is valid)",
			zap.String("src", src),
			zap.Error(err))
		// Not a critical error - destination file is valid
	}

	return nil
}
