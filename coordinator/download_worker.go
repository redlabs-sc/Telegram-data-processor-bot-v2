package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.uber.org/zap"
)

type DownloadWorker struct {
	id      string
	cfg     *Config
	db      *sql.DB
	bot     *tgbotapi.BotAPI
	logger  *zap.Logger
	metrics *MetricsCollector
}

func NewDownloadWorker(id string, cfg *Config, db *sql.DB, bot *tgbotapi.BotAPI, logger *zap.Logger, metrics *MetricsCollector) *DownloadWorker {
	return &DownloadWorker{
		id:      id,
		cfg:     cfg,
		db:      db,
		bot:     bot,
		logger:  logger.With(zap.String("worker", id)),
		metrics: metrics,
	}
}

func (dw *DownloadWorker) Start(ctx context.Context) {
	dw.logger.Info("Download worker started")
	dw.metrics.SetWorkerStatus("download", dw.id, true)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			dw.logger.Info("Download worker stopping")
			dw.metrics.SetWorkerStatus("download", dw.id, false)
			return
		case <-ticker.C:
			dw.processNext(ctx)
		}
	}
}

func (dw *DownloadWorker) processNext(ctx context.Context) {
	// Claim next pending task with optimistic locking
	tx, err := dw.db.BeginTx(ctx, nil)
	if err != nil {
		dw.logger.Error("Failed to begin transaction", zap.Error(err))
		return
	}
	defer tx.Rollback()

	var taskID int64
	var fileID, filename, fileType string
	var fileSize int64
	var downloadAttempts int

	query := `
		SELECT task_id, file_id, filename, file_type, file_size, download_attempts
		FROM download_queue
		WHERE status = 'PENDING'
		ORDER BY priority DESC, created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`

	err = tx.QueryRowContext(ctx, query).Scan(&taskID, &fileID, &filename, &fileType, &fileSize, &downloadAttempts)
	if err == sql.ErrNoRows {
		// No pending tasks
		return
	}
	if err != nil {
		dw.logger.Error("Failed to query pending task", zap.Error(err))
		return
	}

	// Check max attempts
	if downloadAttempts >= 3 {
		dw.logger.Warn("Task exceeded max download attempts",
			zap.Int64("task_id", taskID),
			zap.Int("attempts", downloadAttempts))

		_, err = tx.ExecContext(ctx, `
			UPDATE download_queue
			SET status = 'FAILED',
				last_error = 'Exceeded maximum download attempts',
				completed_at = NOW()
			WHERE task_id = $1
		`, taskID)

		if err != nil {
			dw.logger.Error("Failed to mark task as failed", zap.Error(err))
			return
		}

		tx.Commit()
		return
	}

	// Mark as downloading
	_, err = tx.ExecContext(ctx, `
		UPDATE download_queue
		SET status = 'DOWNLOADING',
			started_at = NOW(),
			download_attempts = download_attempts + 1
		WHERE task_id = $1
	`, taskID)

	if err != nil {
		dw.logger.Error("Failed to update task status", zap.Error(err))
		return
	}

	if err := tx.Commit(); err != nil {
		dw.logger.Error("Failed to commit transaction", zap.Error(err))
		return
	}

	dw.logger.Info("Claimed download task",
		zap.Int64("task_id", taskID),
		zap.String("filename", filename),
		zap.String("type", fileType),
		zap.Int64("size", fileSize))

	// Download the file
	if err := dw.downloadFile(ctx, taskID, fileID, filename, fileType, fileSize); err != nil {
		dw.logger.Error("Download failed",
			zap.Int64("task_id", taskID),
			zap.String("filename", filename),
			zap.Error(err))

		// Mark as PENDING for retry (or FAILED if max attempts exceeded)
		_, dbErr := dw.db.ExecContext(ctx, `
			UPDATE download_queue
			SET status = CASE
				WHEN download_attempts >= 3 THEN 'FAILED'
				ELSE 'PENDING'
			END,
			last_error = $2,
			completed_at = CASE
				WHEN download_attempts >= 3 THEN NOW()
				ELSE NULL
			END
			WHERE task_id = $1
		`, taskID, err.Error())

		if dbErr != nil {
			dw.logger.Error("Failed to update error status", zap.Error(dbErr))
		}

		return
	}

	// Mark as DOWNLOADED
	_, err = dw.db.ExecContext(ctx, `
		UPDATE download_queue
		SET status = 'DOWNLOADED',
			completed_at = NOW()
		WHERE task_id = $1
	`, taskID)

	if err != nil {
		dw.logger.Error("Failed to mark as downloaded", zap.Error(err))
		return
	}

	dw.logger.Info("Download completed successfully",
		zap.Int64("task_id", taskID),
		zap.String("filename", filename))

	dw.metrics.RecordFileProcessed(fileType, "downloaded")
}

func (dw *DownloadWorker) downloadFile(ctx context.Context, taskID int64, fileID, filename, fileType string, fileSize int64) error {
	// Create download directory if not exists
	if err := os.MkdirAll("downloads", 0755); err != nil {
		return fmt.Errorf("create downloads directory: %w", err)
	}

	// Create context with timeout
	downloadCtx, cancel := context.WithTimeout(ctx, time.Duration(dw.cfg.DownloadTimeoutSec)*time.Second)
	defer cancel()

	// Get file from Telegram
	fileConfig := tgbotapi.FileConfig{FileID: fileID}
	file, err := dw.bot.GetFile(fileConfig)
	if err != nil {
		return fmt.Errorf("get file from Telegram: %w", err)
	}

	// Download file with timeout
	downloadURL := file.Link(dw.bot.Token)
	if dw.cfg.UseLocalBotAPI {
		// Use local bot API URL
		downloadURL = fmt.Sprintf("%s/file/bot%s/%s", dw.cfg.LocalBotAPIURL, dw.bot.Token, file.FilePath)
	}

	httpClient := &http.Client{Timeout: time.Duration(dw.cfg.DownloadTimeoutSec) * time.Second}
	resp, err := httpClient.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	// Create temporary file
	destPath := filepath.Join("downloads", fmt.Sprintf("%d_%s", taskID, filename))
	tempPath := destPath + ".tmp"

	outFile, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer outFile.Close()

	// Copy and compute SHA256 hash simultaneously
	hash := sha256.New()
	writer := io.MultiWriter(outFile, hash)

	startTime := time.Now()

	written, err := io.Copy(writer, resp.Body)
	if err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("write file: %w", err)
	}

	duration := time.Since(startTime)
	speedMBs := float64(written) / (1024 * 1024) / duration.Seconds()

	dw.logger.Info("File downloaded",
		zap.Int64("task_id", taskID),
		zap.String("filename", filename),
		zap.Int64("bytes", written),
		zap.Duration("duration", duration),
		zap.Float64("speed_mbps", speedMBs))

	// Close file before rename
	outFile.Close()

	// Atomic rename
	if err := os.Rename(tempPath, destPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("rename file: %w", err)
	}

	// Store SHA256 hash in database
	hashString := fmt.Sprintf("%x", hash.Sum(nil))
	_, err = dw.db.ExecContext(downloadCtx, `
		UPDATE download_queue
		SET sha256_hash = $2
		WHERE task_id = $1
	`, taskID, hashString)

	if err != nil {
		dw.logger.Warn("Failed to store hash", zap.Error(err))
	}

	// Record download duration metric
	dw.metrics.RecordRoundDuration("download", int(duration.Seconds()))

	return nil
}
