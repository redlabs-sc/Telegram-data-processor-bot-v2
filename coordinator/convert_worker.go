package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"

	// Import preserved convert package
	"github.com/redlabs-sc/telegram-bot-option2/app/extraction/convert"
)

// ConvertWorker is a singleton worker that processes rounds through the convert stage.
// It ensures only ONE round is converting at a time (required by convert.go).
func convertWorker(ctx context.Context, db *sql.DB, logger *zap.Logger) {
	workerID := "convert_1"
	logger.Info("Convert worker started", zap.String("worker_id", workerID))

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Try to claim a round for conversion
			round, err := claimRoundForConvert(ctx, db, workerID, logger)
			if err == sql.ErrNoRows {
				// No rounds ready - this is normal
				continue
			}
			if err != nil {
				logger.Error("Failed to claim round for conversion", zap.Error(err))
				continue
			}

			// Process the round
			logger.Info("Starting convert stage", zap.String("round_id", round.RoundID))

			startTime := time.Now()
			outputFiles, err := runConvertStage(round, logger)
			duration := int(time.Since(startTime).Seconds())

			// Update round status based on result
			if err != nil {
				logger.Error("Convert stage failed",
					zap.String("round_id", round.RoundID),
					zap.Error(err))

				updateConvertStatus(db, round.RoundID, "FAILED", duration, err.Error(), logger)
			} else {
				logger.Info("Convert stage completed",
					zap.String("round_id", round.RoundID),
					zap.Int("duration_sec", duration),
					zap.Int("output_files", len(outputFiles)))

				updateConvertStatus(db, round.RoundID, "COMPLETED", duration, "", logger)

				// Transition round to next stage (STORING)
				updateRoundStatus(db, round.RoundID, "STORING", logger)

				// Create store tasks for this round (2 files per task)
				err = createStoreTasks(db, round.RoundID, outputFiles, logger)
				if err != nil {
					logger.Error("Failed to create store tasks",
						zap.String("round_id", round.RoundID),
						zap.Error(err))
					// Mark round as failed if we can't create store tasks
					updateConvertStatus(db, round.RoundID, "FAILED", duration,
						fmt.Sprintf("Failed to create store tasks: %v", err), logger)
				}
			}

		case <-ctx.Done():
			logger.Info("Convert worker shutting down", zap.String("worker_id", workerID))
			return
		}
	}
}

// claimRoundForConvert atomically claims the next round ready for conversion.
// A round is ready if extract_status = 'COMPLETED' or 'SKIPPED' (no archives).
func claimRoundForConvert(ctx context.Context, db *sql.DB, workerID string, logger *zap.Logger) (*Round, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var round Round
	err = tx.QueryRowContext(ctx, `
		UPDATE processing_rounds
		SET convert_status = 'CONVERTING',
		    convert_worker_id = $1,
		    convert_started_at = NOW(),
		    round_status = 'CONVERTING'
		WHERE round_id = (
		    SELECT round_id FROM processing_rounds
		    WHERE convert_status = 'PENDING'
		      AND extract_status IN ('COMPLETED', 'SKIPPED')
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

// runConvertStage executes the preserved convert.go function.
// convert.ConvertTextFiles() processes ALL files in CONVERT_INPUT_DIR
// and writes consolidated output to CONVERT_OUTPUT_FILE.
func runConvertStage(round *Round, logger *zap.Logger) ([]string, error) {
	// Set environment variables required by convert.go
	inputDir := "app/extraction/files/pass"
	outputFile := "app/extraction/files/txt/converted.txt"

	os.Setenv("CONVERT_INPUT_DIR", inputDir)
	os.Setenv("CONVERT_OUTPUT_FILE", outputFile)

	logger.Info("Calling convert.ConvertTextFiles()",
		zap.String("input_dir", inputDir),
		zap.String("output_file", outputFile))

	// Call preserved convert function
	err := convert.ConvertTextFiles()
	if err != nil {
		return nil, fmt.Errorf("convert.ConvertTextFiles() failed: %w", err)
	}

	// Rename the converted output file with unique filename
	// Format: output_{roundID}_{timestamp}.txt
	if _, err := os.Stat(outputFile); err == nil {
		timestamp := time.Now().Format("20060102_150405")
		uniqueOutputFile := filepath.Join("app/extraction/files/txt",
			fmt.Sprintf("output_%s_%s.txt", round.RoundID, timestamp))

		err = os.Rename(outputFile, uniqueOutputFile)
		if err != nil {
			logger.Warn("Failed to rename converted file, continuing with original name",
				zap.Error(err),
				zap.String("original", outputFile),
				zap.String("new", uniqueOutputFile))
		} else {
			logger.Info("Renamed converted output file",
				zap.String("original", outputFile),
				zap.String("new", uniqueOutputFile))
		}
	}

	// List all files in output directory (txt/) for store stage
	// convert.go writes to a single output file, but also moves source files
	txtDir := "app/extraction/files/txt"
	files, err := os.ReadDir(txtDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read txt directory: %w", err)
	}

	var outputFiles []string
	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".txt" {
			outputFiles = append(outputFiles, filepath.Join(txtDir, file.Name()))
		}
	}

	logger.Info("Convert stage produced files",
		zap.String("round_id", round.RoundID),
		zap.Int("txt_files", len(outputFiles)))

	return outputFiles, nil
}

// updateConvertStatus updates the convert_status and related fields for a round
func updateConvertStatus(db *sql.DB, roundID string, status string, durationSec int, errorMsg string, logger *zap.Logger) {
	query := `
		UPDATE processing_rounds
		SET convert_status = $1,
		    convert_completed_at = NOW(),
		    convert_duration_sec = $2,
		    error_message = CASE WHEN $3 != '' THEN $3 ELSE error_message END
		WHERE round_id = $4
	`

	_, err := db.Exec(query, status, durationSec, errorMsg, roundID)
	if err != nil {
		logger.Error("Failed to update convert status",
			zap.String("round_id", roundID),
			zap.String("status", status),
			zap.Error(err))
	}
}

// createStoreTasks creates store tasks for the round.
// Each task processes exactly 2 files (matching store.go's behavior).
func createStoreTasks(db *sql.DB, roundID string, files []string, logger *zap.Logger) error {
	if len(files) == 0 {
		logger.Warn("No files to create store tasks for", zap.String("round_id", roundID))
		// Mark round as completed if there are no files to store
		_, err := db.Exec(`
			UPDATE processing_rounds
			SET store_status = 'COMPLETED',
			    round_status = 'COMPLETED',
			    store_completed_at = NOW()
			WHERE round_id = $1
		`, roundID)
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Group files into chunks of 2
	taskCount := 0
	for i := 0; i < len(files); i += 2 {
		end := i + 2
		if end > len(files) {
			end = len(files)
		}
		chunk := files[i:end]

		// Insert store task
		_, err := tx.Exec(`
			INSERT INTO store_tasks (round_id, file_paths, status)
			VALUES ($1, $2, 'PENDING')
		`, roundID, pq.Array(chunk))

		if err != nil {
			return fmt.Errorf("failed to insert store task: %w", err)
		}

		taskCount++
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	logger.Info("Created store tasks",
		zap.String("round_id", roundID),
		zap.Int("task_count", taskCount),
		zap.Int("file_count", len(files)))

	return nil
}
