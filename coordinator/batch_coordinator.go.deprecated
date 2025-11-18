package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

type BatchCoordinator struct {
	cfg                  *Config
	db                   *sql.DB
	logger               *zap.Logger
	metrics              *MetricsCollector
	batchCounter         int
	mu                   sync.Mutex
	activeBatchesCount   int
}

type FileInfo struct {
	TaskID   int64
	Filename string
	FileType string
	FileSize int64
}

func NewBatchCoordinator(cfg *Config, db *sql.DB, logger *zap.Logger, metrics *MetricsCollector) *BatchCoordinator {
	return &BatchCoordinator{
		cfg:              cfg,
		db:               db,
		logger:           logger,
		metrics:          metrics,
		batchCounter:     0,
	}
}

func (bc *BatchCoordinator) Start(ctx context.Context) {
	bc.logger.Info("Batch coordinator started",
		zap.Int("max_batch_workers", bc.cfg.MaxBatchWorkers),
		zap.Int("batch_size", bc.cfg.BatchSize))

	// Check for existing batches on startup to set counter
	bc.initializeBatchCounter()

	ticker := time.NewTicker(time.Duration(bc.cfg.BatchTimeoutSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			bc.logger.Info("Batch coordinator stopping")
			return
		case <-ticker.C:
			bc.createBatchIfNeeded(ctx)
		}
	}
}

func (bc *BatchCoordinator) initializeBatchCounter() {
	var maxBatchID string
	err := bc.db.QueryRow(`
		SELECT batch_id
		FROM batch_processing
		ORDER BY created_at DESC
		LIMIT 1
	`).Scan(&maxBatchID)

	if err == sql.ErrNoRows {
		bc.batchCounter = 0
		return
	}

	if err != nil {
		bc.logger.Error("Failed to get max batch ID", zap.Error(err))
		bc.batchCounter = 0
		return
	}

	// Extract number from batch_XXX format
	var num int
	fmt.Sscanf(maxBatchID, "batch_%d", &num)
	bc.batchCounter = num

	bc.logger.Info("Initialized batch counter", zap.Int("counter", bc.batchCounter))
}

func (bc *BatchCoordinator) createBatchIfNeeded(ctx context.Context) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	// Check how many batches are currently processing
	var processingCount int
	err := bc.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM batch_processing
		WHERE status IN ('QUEUED', 'EXTRACTING', 'CONVERTING', 'STORING')
	`).Scan(&processingCount)

	if err != nil {
		bc.logger.Error("Failed to count processing batches", zap.Error(err))
		return
	}

	// Don't create new batch if we're at max capacity
	if processingCount >= bc.cfg.MaxBatchWorkers {
		bc.logger.Debug("Max concurrent batches reached, waiting",
			zap.Int("processing", processingCount),
			zap.Int("max", bc.cfg.MaxBatchWorkers))
		return
	}

	// Get downloaded files ready for batching
	rows, err := bc.db.QueryContext(ctx, `
		SELECT task_id, filename, file_type, file_size
		FROM download_queue
		WHERE status = 'DOWNLOADED' AND batch_id IS NULL
		ORDER BY created_at ASC
		LIMIT $1
	`, bc.cfg.BatchSize)

	if err != nil {
		bc.logger.Error("Failed to query downloaded files", zap.Error(err))
		return
	}
	defer rows.Close()

	var files []FileInfo
	for rows.Next() {
		var f FileInfo
		if err := rows.Scan(&f.TaskID, &f.Filename, &f.FileType, &f.FileSize); err != nil {
			bc.logger.Error("Failed to scan file info", zap.Error(err))
			continue
		}
		files = append(files, f)
	}

	if len(files) == 0 {
		bc.logger.Debug("No files ready for batching")
		return
	}

	// Check if we should create a batch
	shouldCreate := false

	// Create batch if we have enough files
	if len(files) >= bc.cfg.BatchSize {
		shouldCreate = true
		bc.logger.Info("Creating batch: enough files", zap.Int("file_count", len(files)))
	} else {
		// Check if oldest file has been waiting too long
		var oldestCreatedAt time.Time
		err := bc.db.QueryRowContext(ctx, `
			SELECT created_at
			FROM download_queue
			WHERE status = 'DOWNLOADED' AND batch_id IS NULL
			ORDER BY created_at ASC
			LIMIT 1
		`).Scan(&oldestCreatedAt)

		if err == nil {
			waitTime := time.Since(oldestCreatedAt)
			batchTimeout := time.Duration(bc.cfg.BatchTimeoutSec) * time.Second

			if waitTime > batchTimeout {
				shouldCreate = true
				bc.logger.Info("Creating batch: timeout reached",
					zap.Int("file_count", len(files)),
					zap.Duration("wait_time", waitTime))
			}
		}
	}

	if !shouldCreate {
		bc.logger.Debug("Not creating batch yet",
			zap.Int("file_count", len(files)),
			zap.Int("batch_size", bc.cfg.BatchSize))
		return
	}

	// Create the batch
	bc.batchCounter++
	batchID := fmt.Sprintf("batch_%03d", bc.batchCounter)

	if err := bc.createBatch(ctx, batchID, files); err != nil {
		bc.logger.Error("Failed to create batch",
			zap.String("batch_id", batchID),
			zap.Error(err))
		bc.batchCounter-- // Rollback counter
		return
	}

	bc.logger.Info("Batch created successfully",
		zap.String("batch_id", batchID),
		zap.Int("file_count", len(files)))
}

func (bc *BatchCoordinator) createBatch(ctx context.Context, batchID string, files []FileInfo) error {
	// Create batch directory structure
	batchRoot := filepath.Join("batches", batchID)

	if err := bc.createBatchDirectories(batchRoot); err != nil {
		return fmt.Errorf("create batch directories: %w", err)
	}

	// Copy password file
	if err := bc.copyPasswordFile(batchRoot); err != nil {
		bc.logger.Warn("Failed to copy password file", zap.Error(err))
	}

	// Count archives vs txt files
	archiveCount := 0
	txtCount := 0
	for _, f := range files {
		if f.FileType == "ZIP" || f.FileType == "RAR" {
			archiveCount++
		} else if f.FileType == "TXT" {
			txtCount++
		}
	}

	// Create batch record in database
	tx, err := bc.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Insert batch record
	_, err = tx.ExecContext(ctx, `
		INSERT INTO batch_processing (batch_id, file_count, archive_count, txt_count, status, created_at)
		VALUES ($1, $2, $3, $4, 'QUEUED', NOW())
	`, batchID, len(files), archiveCount, txtCount)

	if err != nil {
		return fmt.Errorf("insert batch record: %w", err)
	}

	// Move files to batch directories and update database
	for _, f := range files {
		// Determine source and destination paths
		srcPath := filepath.Join("downloads", fmt.Sprintf("%d_%s", f.TaskID, f.Filename))

		var destDir string
		if f.FileType == "ZIP" || f.FileType == "RAR" {
			destDir = filepath.Join(batchRoot, "app/extraction/files/all")
		} else {
			destDir = filepath.Join(batchRoot, "app/extraction/files/txt")
		}

		destPath := filepath.Join(destDir, f.Filename)

		// Move file
		if err := os.Rename(srcPath, destPath); err != nil {
			bc.logger.Error("Failed to move file to batch",
				zap.String("src", srcPath),
				zap.String("dest", destPath),
				zap.Error(err))
			return fmt.Errorf("move file %s: %w", f.Filename, err)
		}

		// Update download_queue to link to batch
		_, err = tx.ExecContext(ctx, `
			UPDATE download_queue
			SET batch_id = $1
			WHERE task_id = $2
		`, batchID, f.TaskID)

		if err != nil {
			return fmt.Errorf("update task %d: %w", f.TaskID, err)
		}

		// Insert batch_files record
		_, err = tx.ExecContext(ctx, `
			INSERT INTO batch_files (batch_id, task_id, file_type, processing_status)
			VALUES ($1, $2, $3, 'PENDING')
		`, batchID, f.TaskID, f.FileType)

		if err != nil {
			return fmt.Errorf("insert batch_file record: %w", err)
		}

		bc.logger.Debug("File added to batch",
			zap.String("batch_id", batchID),
			zap.String("filename", f.Filename),
			zap.String("type", f.FileType))
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	bc.logger.Info("Batch files routed",
		zap.String("batch_id", batchID),
		zap.Int("archives", archiveCount),
		zap.Int("txt_files", txtCount))

	return nil
}

func (bc *BatchCoordinator) createBatchDirectories(batchRoot string) error {
	// Create full directory structure matching what extract.go, convert.go, store.go expect
	dirs := []string{
		filepath.Join(batchRoot, "app/extraction/files/all"),
		filepath.Join(batchRoot, "app/extraction/files/txt"),
		filepath.Join(batchRoot, "app/extraction/files/pass"),
		filepath.Join(batchRoot, "app/extraction/files/nopass"),
		filepath.Join(batchRoot, "app/extraction/files/errors"),
		filepath.Join(batchRoot, "logs"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	bc.logger.Debug("Batch directories created", zap.String("batch_root", batchRoot))
	return nil
}

func (bc *BatchCoordinator) copyPasswordFile(batchRoot string) error {
	srcPath := "app/extraction/pass.txt"
	destPath := filepath.Join(batchRoot, "app/extraction/pass.txt")

	// Check if source exists
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		bc.logger.Warn("Password file not found, skipping", zap.String("path", srcPath))
		return nil
	}

	// Read source
	content, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read password file: %w", err)
	}

	// Write to destination
	if err := os.WriteFile(destPath, content, 0644); err != nil {
		return fmt.Errorf("write password file: %w", err)
	}

	bc.logger.Debug("Password file copied", zap.String("dest", destPath))
	return nil
}
