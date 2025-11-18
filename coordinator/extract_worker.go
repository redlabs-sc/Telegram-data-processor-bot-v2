package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	// Import preserved extract package
	"github.com/redlabs-sc/telegram-bot-option2/app/extraction/extract"
)

// ExtractWorker is a singleton worker that processes rounds through the extract stage.
// It ensures only ONE round is extracting at a time (required by extract.go).
func extractWorker(ctx context.Context, db *sql.DB, logger *zap.Logger) {
	workerID := "extract_1"
	logger.Info("Extract worker started", zap.String("worker_id", workerID))

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Try to claim a round for extraction
			round, err := claimRoundForExtract(ctx, db, workerID, logger)
			if err == sql.ErrNoRows {
				// No rounds ready - this is normal
				continue
			}
			if err != nil {
				logger.Error("Failed to claim round for extraction", zap.Error(err))
				continue
			}

			// Process the round
			logger.Info("Starting extract stage",
				zap.String("round_id", round.RoundID),
				zap.Int("archive_count", round.ArchiveCount))

			startTime := time.Now()
			err = runExtractStage(round, logger)
			duration := int(time.Since(startTime).Seconds())

			// Update round status based on result
			if err != nil {
				logger.Error("Extract stage failed",
					zap.String("round_id", round.RoundID),
					zap.Error(err))

				updateExtractStatus(db, round.RoundID, "FAILED", duration, err.Error(), logger)
			} else {
				logger.Info("Extract stage completed",
					zap.String("round_id", round.RoundID),
					zap.Int("duration_sec", duration))

				updateExtractStatus(db, round.RoundID, "COMPLETED", duration, "", logger)

				// Transition round to next stage (CONVERTING)
				updateRoundStatus(db, round.RoundID, "CONVERTING", logger)
			}

		case <-ctx.Done():
			logger.Info("Extract worker shutting down", zap.String("worker_id", workerID))
			return
		}
	}
}

// Round represents a processing round
type Round struct {
	RoundID      string
	FileCount    int
	ArchiveCount int
	TxtCount     int
}

// claimRoundForExtract atomically claims the next round ready for extraction.
// Uses FOR UPDATE SKIP LOCKED for optimistic concurrency control.
func claimRoundForExtract(ctx context.Context, db *sql.DB, workerID string, logger *zap.Logger) (*Round, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var round Round
	err = tx.QueryRowContext(ctx, `
		UPDATE processing_rounds
		SET extract_status = 'EXTRACTING',
		    extract_worker_id = $1,
		    extract_started_at = NOW(),
		    round_status = 'EXTRACTING'
		WHERE round_id = (
		    SELECT round_id FROM processing_rounds
		    WHERE extract_status = 'PENDING'
		      AND round_status = 'CREATED'
		    ORDER BY created_at ASC
		    LIMIT 1
		    FOR UPDATE SKIP LOCKED
		)
		RETURNING round_id, file_count, archive_count, txt_count
	`, workerID).Scan(&round.RoundID, &round.FileCount, &round.ArchiveCount, &round.TxtCount)

	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	return &round, nil
}

// runExtractStage executes the preserved extract.go function.
// extract.ExtractArchives() processes ALL archives in app/extraction/files/all/
func runExtractStage(round *Round, logger *zap.Logger) error {
	// Check if this round has any archives to extract
	if round.ArchiveCount == 0 {
		logger.Info("Round has no archives, skipping extract stage",
			zap.String("round_id", round.RoundID))
		return nil
	}

	// Verify expected directories exist
	allDir := "app/extraction/files/all"
	passDir := "app/extraction/files/pass"
	nopassDir := "app/extraction/files/nopass"

	for _, dir := range []string{allDir, passDir, nopassDir} {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return fmt.Errorf("required directory %s does not exist", dir)
		}
	}

	// Call preserved extract function
	// This function processes ALL files in app/extraction/files/all/
	// and outputs to app/extraction/files/pass/
	logger.Info("Calling extract.ExtractArchives()",
		zap.String("input_dir", allDir),
		zap.String("output_dir", passDir))

	// extract.ExtractArchives() has no return value - it handles errors internally
	// by moving problematic files to nopass/ or errors/ directories
	extract.ExtractArchives()

	// Verify extraction produced some output
	// If extract fails completely, pass/ directory should be empty
	passFiles, err := os.ReadDir(passDir)
	if err != nil {
		return fmt.Errorf("failed to read pass directory: %w", err)
	}

	logger.Info("Extract stage produced files",
		zap.String("round_id", round.RoundID),
		zap.Int("extracted_files", len(passFiles)))

	return nil
}

// updateExtractStatus updates the extract_status and related fields for a round
func updateExtractStatus(db *sql.DB, roundID string, status string, durationSec int, errorMsg string, logger *zap.Logger) {
	query := `
		UPDATE processing_rounds
		SET extract_status = $1,
		    extract_completed_at = NOW(),
		    extract_duration_sec = $2,
		    error_message = CASE WHEN $3 != '' THEN $3 ELSE error_message END
		WHERE round_id = $4
	`

	_, err := db.Exec(query, status, durationSec, errorMsg, roundID)
	if err != nil {
		logger.Error("Failed to update extract status",
			zap.String("round_id", roundID),
			zap.String("status", status),
			zap.Error(err))
	}
}

// updateRoundStatus updates the overall round_status field
func updateRoundStatus(db *sql.DB, roundID string, status string, logger *zap.Logger) {
	_, err := db.Exec(`
		UPDATE processing_rounds
		SET round_status = $1
		WHERE round_id = $2
	`, status, roundID)

	if err != nil {
		logger.Error("Failed to update round status",
			zap.String("round_id", roundID),
			zap.String("status", status),
			zap.Error(err))
	}
}
