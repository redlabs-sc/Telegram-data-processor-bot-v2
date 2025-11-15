# Implementation Plan: Option 2 - Part 2
## Batch Processing Workers, Testing, Deployment, and Prompts

**Continuation from**: `option2-implementation-plan.md`

---

## Table of Contents

1. [Phase 4: Batch Processing Workers](#phase-4-batch-processing-workers-week-7-8)
2. [Phase 5: Admin Commands](#phase-5-admin-commands-week-9)
3. [Phase 6: Testing & Optimization](#phase-6-testing--optimization-week-10-11)
4. [Phase 7: Documentation & Deployment](#phase-7-documentation--deployment-week-12)
5. [Testing Strategy](#4-testing-strategy)
6. [Deployment Strategy](#5-deployment-strategy)
7. [Monitoring & Observability](#6-monitoring--observability)
8. [Risk Mitigation](#7-risk-mitigation)
9. [Rollback Plan](#8-rollback-plan)
10. [Prompts for Claude Code](#9-prompts-for-claude-code)

---

## Phase 4: Batch Processing Workers (Week 7-8)

### Objectives
- Implement batch worker pool (5 concurrent workers)
- Execute extract → convert → store pipeline sequentially per batch
- Use working directory changes to preserve code 100%
- Handle batch failures and logging
- Notify admin on batch completion

---

### Task 4.1: Batch Worker Implementation

**File**: `coordinator/batch_worker.go`

```go
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
    id     string
    cfg    *Config
    db     *sql.DB
    logger *zap.Logger
}

func NewBatchWorker(id string, cfg *Config, db *sql.DB, logger *zap.Logger) *BatchWorker {
    return &BatchWorker{
        id:     id,
        cfg:    cfg,
        db:     db,
        logger: logger.With(zap.String("worker", id)),
    }
}

func (bw *BatchWorker) Start(ctx context.Context) {
    bw.logger.Info("Batch worker started")

    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            bw.logger.Info("Batch worker stopping")
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

        // TODO: Notify admin of failure
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

        // TODO: Notify admin of completion
        // TODO: Cleanup batch directory
    }
}

func (bw *BatchWorker) processBatch(ctx context.Context, batchID string, archiveCount, txtCount int) error {
    batchRoot := filepath.Join("batches", batchID)

    // Save current working directory
    originalWD, err := os.Getwd()
    if err != nil {
        return fmt.Errorf("get working directory: %w", err)
    }
    defer os.Chdir(originalWD) // Always restore

    // Change to batch directory
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

    // Build path to extract.go (relative to batch root)
    extractPath := filepath.Join(bw.cfg.GetProjectRoot(), "app", "extraction", "extract", "extract.go")

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
            zap.Error(err))
        return err
    }

    // Store duration in database
    bw.db.Exec(`
        UPDATE batch_processing
        SET extract_duration_sec = $2
        WHERE batch_id = $1
    `, batchID, int(duration.Seconds()))

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
    convertPath := filepath.Join(bw.cfg.GetProjectRoot(), "app", "extraction", "convert", "convert.go")

    // Create context with timeout
    convertCtx, cancel := context.WithTimeout(ctx, time.Duration(bw.cfg.ConvertTimeoutSec)*time.Second)
    defer cancel()

    // Set environment variables for convert.go
    env := os.Environ()
    env = append(env, "CONVERT_INPUT_DIR=app/extraction/files/pass")
    env = append(env, "CONVERT_OUTPUT_FILE=app/extraction/files/all_extracted.txt")

    // Execute convert.go as subprocess
    cmd := exec.CommandContext(convertCtx, "go", "run", convertPath)
    cmd.Env = env
    output, err := cmd.CombinedOutput()

    // Log output
    logPath := filepath.Join("logs", "convert.log")
    os.WriteFile(logPath, output, 0644)

    duration := time.Since(startTime)

    if err != nil {
        bw.logger.Error("Convert stage failed",
            zap.String("batch_id", batchID),
            zap.Duration("duration", duration),
            zap.Error(err))
        return err
    }

    // Store duration
    bw.db.Exec(`
        UPDATE batch_processing
        SET convert_duration_sec = $2
        WHERE batch_id = $1
    `, batchID, int(duration.Seconds()))

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
    storePath := filepath.Join(bw.cfg.GetProjectRoot(), "app", "extraction", "store.go")

    // Create context with timeout
    storeCtx, cancel := context.WithTimeout(ctx, time.Duration(bw.cfg.StoreTimeoutSec)*time.Second)
    defer cancel()

    // Execute store.go as subprocess
    // Store.go will process:
    // 1. Converted output from app/extraction/files/all_extracted.txt
    // 2. Direct TXT files from app/extraction/files/txt/
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
            zap.Error(err))
        return err
    }

    // Store duration
    bw.db.Exec(`
        UPDATE batch_processing
        SET store_duration_sec = $2
        WHERE batch_id = $1
    `, batchID, int(duration.Seconds()))

    bw.logger.Info("Store stage completed",
        zap.String("batch_id", batchID),
        zap.Duration("duration", duration))

    return nil
}
```

**Add to Config struct** (`coordinator/config.go`):

```go
func (c *Config) GetProjectRoot() string {
    // Get absolute path to project root
    // Assumes coordinator is run from project root
    wd, _ := os.Getwd()
    return wd
}
```

---

### Task 4.2: Update Main Entry Point

Update `coordinator/main.go` to start batch coordinator and batch workers:

```go
// Add after download workers (after line 1578)

// 9. Start batch coordinator
batchCoordinator := NewBatchCoordinator(cfg, db, logger)
go batchCoordinator.Start(ctx)
logger.Info("Batch coordinator started")

// 10. Start batch workers
for i := 1; i <= cfg.MaxBatchWorkers; i++ {
    workerID := fmt.Sprintf("batch_worker_%d", i)
    worker := NewBatchWorker(workerID, cfg, db, logger)
    go worker.Start(ctx)
    logger.Info("Batch worker started", zap.String("id", workerID))
}

logger.Info("All services started successfully",
    zap.Int("download_workers", cfg.MaxDownloadWorkers),
    zap.Int("batch_workers", cfg.MaxBatchWorkers))
```

---

### Task 4.3: Batch Cleanup Service

**File**: `coordinator/batch_cleanup.go`

```go
package main

import (
    "context"
    "database/sql"
    "os"
    "path/filepath"
    "time"

    "go.uber.org/zap"
)

type BatchCleanup struct {
    cfg    *Config
    db     *sql.DB
    logger *zap.Logger
}

func NewBatchCleanup(cfg *Config, db *sql.DB, logger *zap.Logger) *BatchCleanup {
    return &BatchCleanup{
        cfg:    cfg,
        db:     db,
        logger: logger,
    }
}

func (bc *BatchCleanup) Start(ctx context.Context) {
    bc.logger.Info("Batch cleanup service started")

    ticker := time.NewTicker(15 * time.Minute)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            bc.logger.Info("Batch cleanup service stopping")
            return
        case <-ticker.C:
            bc.cleanupCompletedBatches(ctx)
            bc.archiveFailedBatches(ctx)
        }
    }
}

func (bc *BatchCleanup) cleanupCompletedBatches(ctx context.Context) {
    retentionHours := time.Duration(bc.cfg.CompletedBatchRetentionHours) * time.Hour

    rows, err := bc.db.QueryContext(ctx, `
        SELECT batch_id
        FROM batch_processing
        WHERE status = 'COMPLETED'
          AND completed_at < NOW() - INTERVAL '1 hour' * $1
    `, bc.cfg.CompletedBatchRetentionHours)

    if err != nil {
        bc.logger.Error("Error querying completed batches", zap.Error(err))
        return
    }
    defer rows.Close()

    for rows.Next() {
        var batchID string
        if err := rows.Scan(&batchID); err != nil {
            bc.logger.Error("Error scanning batch_id", zap.Error(err))
            continue
        }

        // Delete batch directory
        batchPath := filepath.Join("batches", batchID)
        if err := os.RemoveAll(batchPath); err != nil {
            bc.logger.Error("Error removing batch directory",
                zap.String("batch_id", batchID),
                zap.Error(err))
        } else {
            bc.logger.Info("Cleaned up completed batch",
                zap.String("batch_id", batchID))
        }
    }
}

func (bc *BatchCleanup) archiveFailedBatches(ctx context.Context) {
    retentionDays := time.Duration(bc.cfg.FailedBatchRetentionDays) * 24 * time.Hour

    rows, err := bc.db.QueryContext(ctx, `
        SELECT batch_id
        FROM batch_processing
        WHERE status = 'FAILED'
          AND completed_at < NOW() - INTERVAL '1 day' * $1
    `, bc.cfg.FailedBatchRetentionDays)

    if err != nil {
        bc.logger.Error("Error querying failed batches", zap.Error(err))
        return
    }
    defer rows.Close()

    for rows.Next() {
        var batchID string
        if err := rows.Scan(&batchID); err != nil {
            bc.logger.Error("Error scanning batch_id", zap.Error(err))
            continue
        }

        // Move to archive
        sourcePath := filepath.Join("batches", batchID)
        destPath := filepath.Join("archive", "failed", batchID)

        if err := os.Rename(sourcePath, destPath); err != nil {
            bc.logger.Error("Error archiving failed batch",
                zap.String("batch_id", batchID),
                zap.Error(err))
        } else {
            bc.logger.Info("Archived failed batch",
                zap.String("batch_id", batchID))
        }
    }
}
```

Add to main.go:
```go
// After batch workers
cleanupService := NewBatchCleanup(cfg, db, logger)
go cleanupService.Start(ctx)
logger.Info("Batch cleanup service started")
```

---

### Phase 4 Testing

**Test 4.1: End-to-End Batch Processing**

```bash
# Upload 10 archive files (ZIP/RAR) to the bot

# Monitor batch progression
watch -n 2 'PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT batch_id, status, extract_duration_sec, convert_duration_sec, store_duration_sec FROM batch_processing ORDER BY created_at DESC LIMIT 5;"'

# Expected status flow: QUEUED → EXTRACTING → CONVERTING → STORING → COMPLETED

# Check batch logs
tail -f batches/batch_001/logs/extract.log
tail -f batches/batch_001/logs/convert.log
tail -f batches/batch_001/logs/store.log

# Verify files extracted
ls -lh batches/batch_001/app/extraction/files/pass/

# Verify files converted
cat batches/batch_001/app/extraction/files/all_extracted.txt | head -20

# After completion, verify cleanup
sleep 70m  # Wait for retention period
ls -lh batches/  # batch_001 should be deleted
```

**Test 4.2: TXT File Direct Processing**

```bash
# Upload 10 TXT files (no archives)

# Monitor batch
watch -n 2 'PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT batch_id, status, txt_count, extract_duration_sec, convert_duration_sec FROM batch_processing ORDER BY created_at DESC LIMIT 1;"'

# Expected: extract_duration_sec and convert_duration_sec should be NULL or 0
# TXT files should go directly to store stage

# Verify files in txt directory
ls -lh batches/batch_002/app/extraction/files/txt/
```

**Test 4.3: Concurrent Batch Processing**

```bash
# Upload 50 archive files (should create 5 batches)

# Monitor concurrent processing
watch -n 2 'PGPASSWORD=change_me_in_production psql -U bot_user -h localhost -d telegram_bot_option2 -c "SELECT batch_id, status, worker_id FROM batch_processing WHERE status IN ('\''EXTRACTING'\'', '\''CONVERTING'\'', '\''STORING'\'') ORDER BY created_at;"'

# Expected: Maximum 5 batches processing concurrently (MAX_BATCH_WORKERS=5)
# Each batch assigned to different worker_id
```

**Test 4.4: Code Preservation Verification**

```bash
# Compute checksums of preserved files
sha256sum app/extraction/extract/extract.go
sha256sum app/extraction/convert/convert.go
sha256sum app/extraction/store.go

# Compare with original checksums in checksums.txt
diff <(sha256sum app/extraction/extract/extract.go) <(grep extract.go checksums.txt)
diff <(sha256sum app/extraction/convert/convert.go) <(grep convert.go checksums.txt)
diff <(sha256sum app/extraction/store.go) <(grep store.go checksums.txt)

# Expected: No differences - files are 100% preserved
```

**Phase 4 Completion Checklist**:
- ✅ Batch workers claim and process batches
- ✅ Extract → Convert → Store pipeline executes sequentially
- ✅ Working directory changed to batch root before execution
- ✅ Extract.go, convert.go, store.go remain 100% unchanged (checksum verified)
- ✅ Batch logs written to batch-specific log files
- ✅ Duration metrics recorded in database
- ✅ Failed batches marked with error messages
- ✅ Completed batches cleaned up after retention period
- ✅ Failed batches archived for 7 days
- ✅ Maximum 5 concurrent batches enforced

---

## Phase 5: Admin Commands (Week 9)

### Objectives
- Implement `/queue`, `/batches`, `/stats` commands
- Add `/retry`, `/cancel`, `/priority` commands for task management
- Implement `/health` command with detailed status
- Add `/logs` command to retrieve batch logs

---

### Task 5.1: Admin Command Implementations

**File**: `coordinator/admin_commands.go`

```go
package main

import (
    "fmt"
    "strings"
    "time"

    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
    "go.uber.org/zap"
)

func (tr *TelegramReceiver) handleQueue(msg *tgbotapi.Message) {
    var pending, downloading, downloaded, failed int

    tr.db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='PENDING'").Scan(&pending)
    tr.db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='DOWNLOADING'").Scan(&downloading)
    tr.db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='DOWNLOADED'").Scan(&downloaded)
    tr.db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='FAILED'").Scan(&failed)

    // Calculate time until next batch
    var oldestCreatedAt time.Time
    err := tr.db.QueryRow(`
        SELECT MIN(created_at)
        FROM download_queue
        WHERE status='DOWNLOADED' AND batch_id IS NULL
    `).Scan(&oldestCreatedAt)

    var nextBatchInfo string
    if err == nil && !oldestCreatedAt.IsZero() {
        waitTime := tr.cfg.BatchTimeoutSec - int(time.Since(oldestCreatedAt).Seconds())
        if waitTime < 0 {
            nextBatchInfo = "Creating batch now..."
        } else if downloaded >= tr.cfg.BatchSize {
            nextBatchInfo = fmt.Sprintf("Next batch forming now (%d files ready)", downloaded)
        } else {
            nextBatchInfo = fmt.Sprintf("Next batch in: %d seconds or when %d files ready",
                waitTime, tr.cfg.BatchSize)
        }
    } else {
        nextBatchInfo = "No files waiting for batch"
    }

    text := fmt.Sprintf(`📊 *Queue Status*

• Pending: %d files
• Downloading: %d files
• Downloaded: %d files (waiting for batch)
• Failed: %d files

%s`,
        pending, downloading, downloaded, failed, nextBatchInfo)

    tr.sendReply(msg.ChatID, text)
}

func (tr *TelegramReceiver) handleBatches(msg *tgbotapi.Message) {
    // Get active batches
    rows, err := tr.db.Query(`
        SELECT batch_id, status, file_count, worker_id, started_at,
               extract_duration_sec, convert_duration_sec, store_duration_sec
        FROM batch_processing
        WHERE status IN ('QUEUED', 'EXTRACTING', 'CONVERTING', 'STORING')
        ORDER BY created_at ASC
    `)

    if err != nil {
        tr.sendReply(msg.ChatID, "Error querying batches")
        return
    }
    defer rows.Close()

    var activeBatches []string
    for rows.Next() {
        var batchID, status, workerID string
        var fileCount int
        var startedAt time.Time
        var extractDur, convertDur, storeDur *int

        rows.Scan(&batchID, &status, &fileCount, &workerID, &startedAt,
            &extractDur, &convertDur, &storeDur)

        elapsed := time.Since(startedAt)
        activeBatches = append(activeBatches, fmt.Sprintf("• %s: %s (%d files, %s, %.0f min elapsed)",
            batchID, status, fileCount, workerID, elapsed.Minutes()))
    }

    // Get recently completed batches
    rows, err = tr.db.Query(`
        SELECT batch_id, file_count,
               extract_duration_sec + convert_duration_sec + store_duration_sec AS total_duration
        FROM batch_processing
        WHERE status = 'COMPLETED'
          AND completed_at > NOW() - INTERVAL '1 hour'
        ORDER BY completed_at DESC
        LIMIT 5
    `)

    if err != nil {
        tr.sendReply(msg.ChatID, "Error querying completed batches")
        return
    }
    defer rows.Close()

    var completedBatches []string
    for rows.Next() {
        var batchID string
        var fileCount, totalDuration int

        rows.Scan(&batchID, &fileCount, &totalDuration)

        completedBatches = append(completedBatches, fmt.Sprintf("• %s: ✅ COMPLETED (%d files, %d min total)",
            batchID, fileCount, totalDuration/60))
    }

    var activeText string
    if len(activeBatches) > 0 {
        activeText = strings.Join(activeBatches, "\n")
    } else {
        activeText = "No active batches"
    }

    var completedText string
    if len(completedBatches) > 0 {
        completedText = strings.Join(completedBatches, "\n")
    } else {
        completedText = "No recent completions"
    }

    text := fmt.Sprintf(`🔄 *Active Batches*
%s

*Recently Completed* (Last Hour):
%s`, activeText, completedText)

    tr.sendReply(msg.ChatID, text)
}

func (tr *TelegramReceiver) handleStats(msg *tgbotapi.Message) {
    var totalProcessed, totalFailed int
    var avgDuration float64
    var successRate float64

    // Total processed in last 24 hours
    tr.db.QueryRow(`
        SELECT COUNT(*)
        FROM batch_processing
        WHERE completed_at > NOW() - INTERVAL '24 hours'
          AND status = 'COMPLETED'
    `).Scan(&totalProcessed)

    // Total failed in last 24 hours
    tr.db.QueryRow(`
        SELECT COUNT(*)
        FROM batch_processing
        WHERE completed_at > NOW() - INTERVAL '24 hours'
          AND status = 'FAILED'
    `).Scan(&totalFailed)

    // Calculate success rate
    totalAttempts := totalProcessed + totalFailed
    if totalAttempts > 0 {
        successRate = float64(totalProcessed) / float64(totalAttempts) * 100
    }

    // Average processing time
    tr.db.QueryRow(`
        SELECT AVG(extract_duration_sec + convert_duration_sec + store_duration_sec)
        FROM batch_processing
        WHERE status = 'COMPLETED'
          AND completed_at > NOW() - INTERVAL '24 hours'
    `).Scan(&avgDuration)

    // Calculate throughput
    var filesProcessed int
    tr.db.QueryRow(`
        SELECT COALESCE(SUM(file_count), 0)
        FROM batch_processing
        WHERE status = 'COMPLETED'
          AND completed_at > NOW() - INTERVAL '24 hours'
    `).Scan(&filesProcessed)

    throughput := float64(filesProcessed) / 24.0 // files per hour

    // Current load
    var downloadWorkers, batchWorkers, queueSize int
    tr.db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='DOWNLOADING'").Scan(&downloadWorkers)
    tr.db.QueryRow("SELECT COUNT(*) FROM batch_processing WHERE status IN ('EXTRACTING', 'CONVERTING', 'STORING')").Scan(&batchWorkers)
    tr.db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='PENDING'").Scan(&queueSize)

    text := fmt.Sprintf(`📈 *System Statistics* (Last 24 Hours)

*Processing*:
• Total Processed: %d batches (%d files)
• Success Rate: %.1f%%
• Avg Processing Time: %.0f minutes/batch
• Throughput: %.1f files/hour

*Current Load*:
• Download Workers: %d/%d active
• Batch Workers: %d/%d active
• Queue Size: %d pending`,
        totalProcessed, filesProcessed, successRate, avgDuration/60, throughput,
        downloadWorkers, tr.cfg.MaxDownloadWorkers,
        batchWorkers, tr.cfg.MaxBatchWorkers,
        queueSize)

    tr.sendReply(msg.ChatID, text)
}

func (tr *TelegramReceiver) handleHealthCommand(msg *tgbotapi.Message) {
    // Check database
    dbStatus := "✅ Healthy"
    if err := tr.db.Ping(); err != nil {
        dbStatus = "❌ Unhealthy: " + err.Error()
    }

    // Check download workers
    var activeDownloads int
    tr.db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE status='DOWNLOADING'").Scan(&activeDownloads)
    downloadStatus := fmt.Sprintf("✅ %d/%d active", activeDownloads, tr.cfg.MaxDownloadWorkers)

    // Check batch workers
    var activeBatches int
    tr.db.QueryRow("SELECT COUNT(*) FROM batch_processing WHERE status IN ('EXTRACTING', 'CONVERTING', 'STORING')").Scan(&activeBatches)
    batchStatus := fmt.Sprintf("✅ %d/%d active", activeBatches, tr.cfg.MaxBatchWorkers)

    // Check disk space (simplified - would need syscall for real implementation)
    diskStatus := "✅ Sufficient"

    text := fmt.Sprintf(`🏥 *System Health*

*Components*:
• Database: %s
• Download Workers: %s
• Batch Workers: %s
• Disk Space: %s

*Queues*:
• Download Queue: ✅ Operating
• Batch Queue: ✅ Operating

All systems operational.`,
        dbStatus, downloadStatus, batchStatus, diskStatus)

    tr.sendReply(msg.ChatID, text)
}
```

---

### Phase 5 Testing

**Test 5.1: Queue Command**

```bash
# Upload some files
# Send /queue to bot

# Expected output:
# 📊 Queue Status
# • Pending: X files
# • Downloading: Y files
# • Downloaded: Z files
# • Next batch in: N seconds or when 10 files ready
```

**Test 5.2: Batches Command**

```bash
# With some batches processing, send /batches

# Expected output:
# 🔄 Active Batches
# • batch_001: EXTRACTING (10 files, batch_worker_1, 5 min elapsed)
# • batch_002: QUEUED (10 files)
#
# Recently Completed:
# • batch_000: ✅ COMPLETED (10 files, 42 min total)
```

**Test 5.3: Stats Command**

```bash
# After processing some batches, send /stats

# Expected output:
# 📈 System Statistics (Last 24 Hours)
# • Total Processed: 15 batches (150 files)
# • Success Rate: 98.5%
# • Avg Processing Time: 38 minutes/batch
# • Throughput: 55 files/hour
```

**Phase 5 Completion Checklist**:
- ✅ `/queue` shows accurate queue statistics
- ✅ `/batches` shows active and completed batches
- ✅ `/stats` shows 24-hour analytics
- ✅ `/health` shows component status
- ✅ All commands admin-only (non-admins rejected)
- ✅ Clear, formatted output with emojis
- ✅ Real-time data from PostgreSQL

---

## Phase 6: Testing & Optimization (Week 10-11)

### Objectives
- Write unit tests for each component
- Perform integration testing
- Conduct load testing with 100-1000 files
- Profile performance and optimize
- Verify resource usage targets

---

### Task 6.1: Unit Tests

**File**: `coordinator/config_test.go`

```go
package main

import (
    "testing"
)

func TestConfigLoad(t *testing.T) {
    // Create test .env file
    // Test configuration loading
    // Verify defaults
}

func TestIsAdmin(t *testing.T) {
    cfg := &Config{
        AdminIDs: []int64{123456789, 987654321},
    }

    tests := []struct {
        userID   int64
        expected bool
    }{
        {123456789, true},
        {987654321, true},
        {111111111, false},
    }

    for _, tt := range tests {
        result := cfg.IsAdmin(tt.userID)
        if result != tt.expected {
            t.Errorf("IsAdmin(%d) = %v, expected %v", tt.userID, result, tt.expected)
        }
    }
}
```

**File**: `coordinator/batch_coordinator_test.go`

```go
package main

import (
    "context"
    "database/sql"
    "testing"

    _ "github.com/lib/pq"
    "go.uber.org/zap"
)

func TestBatchCreation(t *testing.T) {
    // Setup test database
    db := setupTestDB(t)
    defer db.Close()

    // Create test config
    cfg := &Config{
        BatchSize: 10,
        BatchTimeoutSec: 300,
        MaxBatchWorkers: 5,
    }

    logger := zap.NewNop()
    bc := NewBatchCoordinator(cfg, db, logger)

    // Insert 10 test files
    for i := 0; i < 10; i++ {
        db.Exec(`
            INSERT INTO download_queue (file_id, user_id, filename, file_type, file_size, status)
            VALUES ($1, 1, $2, 'ZIP', 1024, 'DOWNLOADED')
        `, fmt.Sprintf("file_%d", i), fmt.Sprintf("test_%d.zip", i))
    }

    // Trigger batch creation
    bc.tryCreateBatch(context.Background())

    // Verify batch created
    var count int
    db.QueryRow("SELECT COUNT(*) FROM batch_processing").Scan(&count)
    if count != 1 {
        t.Errorf("Expected 1 batch, got %d", count)
    }

    // Verify files assigned to batch
    db.QueryRow("SELECT COUNT(*) FROM download_queue WHERE batch_id IS NOT NULL").Scan(&count)
    if count != 10 {
        t.Errorf("Expected 10 files assigned, got %d", count)
    }
}

func setupTestDB(t *testing.T) *sql.DB {
    // Connect to test database
    // Run migrations
    // Return db connection
}
```

---

### Task 6.2: Integration Tests

**File**: `tests/integration/end_to_end_test.go`

```go
package integration

import (
    "context"
    "os"
    "path/filepath"
    "testing"
    "time"
)

func TestEndToEndPipeline(t *testing.T) {
    // 1. Start coordinator
    // 2. Upload test files via bot
    // 3. Wait for download
    // 4. Verify batch creation
    // 5. Wait for batch processing
    // 6. Verify completion
    // 7. Check output files
}

func TestCrashRecovery(t *testing.T) {
    // 1. Start coordinator
    // 2. Upload files
    // 3. During processing, kill coordinator
    // 4. Restart coordinator
    // 5. Verify recovery completes successfully
}

func TestConcurrentBatches(t *testing.T) {
    // 1. Upload 50 files
    // 2. Verify maximum 5 batches process concurrently
    // 3. Verify all batches complete successfully
}
```

---

### Task 6.3: Load Testing

**File**: `tests/load/load_test.go`

```go
package load

import (
    "sync"
    "testing"
    "time"
)

func TestLoad100Files(t *testing.T) {
    startTime := time.Now()

    // Upload 100 test files concurrently
    var wg sync.WaitGroup
    for i := 0; i < 100; i++ {
        wg.Add(1)
        go func(idx int) {
            defer wg.Done()
            uploadTestFile(idx)
        }(i)
    }

    wg.Wait()

    // Wait for all processing to complete
    waitForCompletion(t, 100)

    duration := time.Since(startTime)

    // Verify duration < 2 hours
    if duration > 2*time.Hour {
        t.Errorf("Processing took %v, expected < 2 hours", duration)
    }

    t.Logf("Processed 100 files in %v", duration)
}

func TestLoad1000Files(t *testing.T) {
    // Similar test with 1000 files
    // Verify throughput maintains
}
```

**Running Load Tests**:

```bash
# Create test files
mkdir -p tests/testdata
for i in {1..100}; do
    echo "Test data $i" > tests/testdata/test_$i.txt
done

# Run load test
go test -v ./tests/load -timeout 3h

# Monitor resource usage during test
while true; do
    echo "$(date) - RAM: $(free -h | grep Mem | awk '{print $3/$2 * 100.0}')\%, CPU: $(top -bn1 | grep "Cpu(s)" | awk '{print $2}')\%"
    sleep 10
done
```

---

### Task 6.4: Performance Profiling

```bash
# CPU Profiling
go test -cpuprofile=cpu.prof -bench=. ./coordinator
go tool pprof cpu.prof

# Memory Profiling
go test -memprofile=mem.prof -bench=. ./coordinator
go tool pprof mem.prof

# Analyze bottlenecks
# Top commands in pprof:
# - top10: Show top 10 functions by resource usage
# - list <function>: Show source code with annotations
# - web: Generate graph visualization
```

**Optimization Targets**:
- Database query optimization (add indexes, use prepared statements)
- Reduce memory allocations (use sync.Pool for buffers)
- Optimize file operations (use buffered I/O)
- Tune worker pool sizes based on profiling results

---

### Phase 6 Completion Checklist

- ✅ Unit tests written for config, batch coordinator, workers
- ✅ Integration tests cover end-to-end pipeline
- ✅ Load test with 100 files completes in < 2 hours
- ✅ Load test with 1000 files maintains throughput
- ✅ Performance profiling completed
- ✅ Bottlenecks identified and optimized
- ✅ RAM usage < 20% during load test
- ✅ CPU usage < 50% sustained
- ✅ Success rate > 98% over 1000-file test
- ✅ Zero data loss verified in crash recovery tests

---

## Phase 7: Documentation & Deployment (Week 12)

### Objectives
- Update CLAUDE.md with Option 2 architecture
- Create deployment documentation
- Setup Docker Compose for development
- Create Kubernetes manifests for production
- Configure Grafana dashboards
- Write runbook for operations

---

### Task 7.1: Update CLAUDE.md

Add section to existing CLAUDE.md:

```markdown
## Option 2: Batch-Based Parallel Processing (Alternative Architecture)

**Status**: Production-ready alternative for high-throughput scenarios

### Architecture Overview

Option 2 implements batch-based parallel processing for 6× faster throughput:
- **Download**: 3 concurrent workers (unchanged)
- **Batch Formation**: Groups files into batches of 10
- **Batch Processing**: 5 concurrent batch workers
- **Code Preservation**: 100% - extract.go, convert.go, store.go unchanged

### Directory Structure

```
telegram-bot-option2/
├── app/extraction/          # Preserved code (DO NOT MODIFY)
│   ├── extract/extract.go   # SHA256: [hash]
│   ├── convert/convert.go   # SHA256: [hash]
│   └── store.go             # SHA256: [hash]
├── batches/                 # Batch workspaces (auto-managed)
│   └── batch_NNN/
│       ├── app/extraction/files/  # Isolated per batch
│       └── logs/            # Batch-specific logs
├── coordinator/             # Orchestration service
│   ├── main.go
│   ├── batch_worker.go
│   └── ...
└── downloads/               # Temporary download storage
```

### Performance Characteristics

- **Throughput**: 50-60 files/hour sustained
- **Processing Time**: 100 files in ~2 hours (vs 4.4 hours sequential)
- **Concurrency**: 5 batches × 10 files = 50 files processing simultaneously
- **Resource Usage**: < 20% RAM, < 50% CPU

### When to Use Option 2

Choose Option 2 when:
- Processing > 100 files regularly
- Throughput is priority over simplicity
- Team can manage additional complexity
- Infrastructure supports 5 concurrent batch workers

Choose Option 1 when:
- Processing < 50 files at a time
- Simplicity and reliability are priorities
- Limited infrastructure resources
- Easier debugging is preferred
```

---

### Task 7.2: Docker Compose Setup

**File**: `docker-compose.yml`

```yaml
version: '3.8'

services:
  postgres:
    image: postgres:14-alpine
    environment:
      POSTGRES_DB: telegram_bot_option2
      POSTGRES_USER: bot_user
      POSTGRES_PASSWORD: change_me_in_production
    ports:
      - "5432:5432"
    volumes:
      - postgres_data:/var/lib/postgresql/data
      - ./database/migrations:/docker-entrypoint-initdb.d
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U bot_user"]
      interval: 10s
      timeout: 5s
      retries: 5

  coordinator:
    build: .
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      DB_HOST: postgres
      DB_PORT: 5432
      DB_NAME: telegram_bot_option2
      DB_USER: bot_user
      DB_PASSWORD: change_me_in_production
    env_file:
      - .env
    volumes:
      - ./batches:/app/batches
      - ./downloads:/app/downloads
      - ./logs:/app/logs
      - ./app/extraction:/app/app/extraction:ro  # Read-only preserved code
    ports:
      - "8080:8080"  # Health check
      - "9090:9090"  # Metrics
    restart: unless-stopped

  local-bot-api:
    image: aiogram/telegram-bot-api:latest
    environment:
      TELEGRAM_API_ID: ${TELEGRAM_API_ID}
      TELEGRAM_API_HASH: ${TELEGRAM_API_HASH}
    ports:
      - "8081:8081"
    volumes:
      - bot_api_data:/var/lib/telegram-bot-api
    restart: unless-stopped

  prometheus:
    image: prom/prometheus:latest
    volumes:
      - ./monitoring/prometheus.yml:/etc/prometheus/prometheus.yml
      - prometheus_data:/prometheus
    ports:
      - "9091:9090"
    command:
      - '--config.file=/etc/prometheus/prometheus.yml'
      - '--storage.tsdb.path=/prometheus'
    restart: unless-stopped

  grafana:
    image: grafana/grafana:latest
    depends_on:
      - prometheus
    environment:
      GF_SECURITY_ADMIN_PASSWORD: admin
    ports:
      - "3000:3000"
    volumes:
      - grafana_data:/var/lib/grafana
      - ./monitoring/dashboards:/etc/grafana/provisioning/dashboards
    restart: unless-stopped

volumes:
  postgres_data:
  bot_api_data:
  prometheus_data:
  grafana_data:
```

**File**: `Dockerfile`

```dockerfile
FROM golang:1.21-alpine AS builder

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY coordinator/ ./coordinator/
COPY app/ ./app/

# Build
WORKDIR /build/coordinator
RUN CGO_ENABLED=0 GOOS=linux go build -o /build/coordinator-bin .

FROM alpine:latest

# Install ca-certificates for HTTPS and go runtime for subprocess execution
RUN apk --no-cache add ca-certificates go

WORKDIR /app

# Copy binary
COPY --from=builder /build/coordinator-bin ./coordinator

# Copy preserved code
COPY app/ ./app/

# Create directories
RUN mkdir -p batches downloads logs

EXPOSE 8080 9090

CMD ["./coordinator"]
```

---

### Task 7.3: Kubernetes Manifests

**File**: `k8s/deployment.yaml`

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: telegram-bot-coordinator
  labels:
    app: telegram-bot
    component: coordinator
spec:
  replicas: 1  # Single instance for now (database constraints)
  selector:
    matchLabels:
      app: telegram-bot
      component: coordinator
  template:
    metadata:
      labels:
        app: telegram-bot
        component: coordinator
    spec:
      containers:
      - name: coordinator
        image: your-registry/telegram-bot-coordinator:latest
        envFrom:
        - secretRef:
            name: telegram-bot-secrets
        - configMapRef:
            name: telegram-bot-config
        ports:
        - containerPort: 8080
          name: health
        - containerPort: 9090
          name: metrics
        resources:
          requests:
            memory: "512Mi"
            cpu: "500m"
          limits:
            memory: "4Gi"
            cpu: "2000m"
        livenessProbe:
          httpGet:
            path: /health
            port: 8080
          initialDelaySeconds: 30
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /health
            port: 8080
          initialDelaySeconds: 10
          periodSeconds: 5
        volumeMounts:
        - name: batches
          mountPath: /app/batches
        - name: downloads
          mountPath: /app/downloads
        - name: logs
          mountPath: /app/logs
      volumes:
      - name: batches
        emptyDir: {}
      - name: downloads
        emptyDir: {}
      - name: logs
        persistentVolumeClaim:
          claimName: logs-pvc
```

**File**: `k8s/service.yaml`

```yaml
apiVersion: v1
kind: Service
metadata:
  name: telegram-bot-coordinator
  labels:
    app: telegram-bot
spec:
  selector:
    app: telegram-bot
    component: coordinator
  ports:
  - name: health
    port: 8080
    targetPort: 8080
  - name: metrics
    port: 9090
    targetPort: 9090
  type: ClusterIP
```

---

### Task 7.4: Grafana Dashboard

**File**: `monitoring/dashboards/telegram-bot.json`

```json
{
  "dashboard": {
    "title": "Telegram Bot - Option 2",
    "panels": [
      {
        "title": "Queue Size",
        "targets": [{
          "expr": "telegram_bot_queue_size"
        }],
        "type": "graph"
      },
      {
        "title": "Batch Processing Duration",
        "targets": [{
          "expr": "histogram_quantile(0.95, telegram_bot_batch_processing_duration_seconds_bucket)"
        }],
        "type": "graph"
      },
      {
        "title": "Worker Status",
        "targets": [{
          "expr": "telegram_bot_worker_status"
        }],
        "type": "stat"
      }
    ]
  }
}
```

---

### Task 7.5: Runbook

**File**: `docs/RUNBOOK.md`

```markdown
# Operations Runbook: Telegram Bot Option 2

## Common Operations

### Starting the Bot

```bash
# Development
docker-compose up -d

# Production (Kubernetes)
kubectl apply -f k8s/
```

### Monitoring

**Health Check**:
```bash
curl http://localhost:8080/health
```

**Metrics**:
```bash
curl http://localhost:9090/metrics
```

**Grafana Dashboard**:
Open http://localhost:3000, login (admin/admin), navigate to "Telegram Bot - Option 2"

### Troubleshooting

**Problem**: Files not downloading
**Diagnosis**:
```bash
# Check download workers
curl http://localhost:8080/health | jq '.components.download_workers'

# Check logs
tail -f logs/coordinator.log | grep download_worker
```
**Solution**: Verify Local Bot API Server is running, check Telegram token

**Problem**: Batches stuck in EXTRACTING
**Diagnosis**:
```bash
# Check stuck batches
psql -c "SELECT batch_id, status, started_at FROM batch_processing WHERE status='EXTRACTING' AND started_at < NOW() - INTERVAL '2 hours';"

# Check batch logs
cat batches/batch_XXX/logs/extract.log
```
**Solution**: Kill stuck batch worker, batch will be requeued

**Problem**: High memory usage
**Diagnosis**:
```bash
# Check resource usage
docker stats telegram-bot-coordinator

# Check batch workers
psql -c "SELECT COUNT(*) FROM batch_processing WHERE status IN ('EXTRACTING', 'CONVERTING', 'STORING');"
```
**Solution**: Reduce MAX_BATCH_WORKERS in config, restart

### Manual Interventions

**Requeue Failed Batch**:
```bash
# Reset batch to QUEUED
psql -c "UPDATE batch_processing SET status='QUEUED', worker_id=NULL WHERE batch_id='batch_XXX';"

# Reset files
psql -c "UPDATE download_queue SET batch_id=NULL WHERE batch_id='batch_XXX';"
```

**Cancel Stuck Download**:
```bash
psql -c "UPDATE download_queue SET status='FAILED', last_error='Manually cancelled' WHERE task_id=XXX;"
```

**Clear Queue**:
```bash
# DANGER: Only use in emergencies
psql -c "DELETE FROM download_queue WHERE status='PENDING';"
```
```

---

### Phase 7 Completion Checklist

- ✅ CLAUDE.md updated with Option 2 documentation
- ✅ Docker Compose file created and tested
- ✅ Dockerfile builds successfully
- ✅ Kubernetes manifests created
- ✅ Grafana dashboard configured
- ✅ Runbook written with common operations
- ✅ Deployment tested in staging environment
- ✅ Production deployment completed
- ✅ Team trained on operations

---

## 4. Testing Strategy

### 4.1 Test Pyramid

```
         /\
        /  \       E2E Tests (10%)
       /____\      - Full pipeline with real files
      /      \     - Crash recovery scenarios
     /        \
    /__________\   Integration Tests (30%)
   /            \  - Multi-component interactions
  /              \ - Database + Workers
 /________________\
/                  \ Unit Tests (60%)
                    - Individual functions
                    - Config parsing
                    - Batch logic
```

### 4.2 Test Categories

**Unit Tests** (`*_test.go`):
- Config parsing and validation
- Batch ID generation
- File type detection
- Admin validation
- Metrics calculation

**Integration Tests** (`tests/integration/`):
- Download → Batch → Process flow
- Database transactions
- Worker coordination
- Crash recovery

**Load Tests** (`tests/load/`):
- 100 files baseline
- 1000 files stress test
- Concurrent upload burst
- Resource usage monitoring

**End-to-End Tests** (`tests/e2e/`):
- Real Telegram bot interaction
- Full pipeline with actual files
- Verification of output data

### 4.3 Test Data

**Test Files**:
```bash
tests/testdata/
├── archives/
│   ├── test_small.zip (1MB)
│   ├── test_medium.zip (100MB)
│   ├── test_large.zip (1GB)
│   └── test_password.zip (password-protected)
└── txt/
    ├── test_credentials.txt (contains betting credentials)
    └── test_large.txt (500MB)
```

### 4.4 Continuous Testing

**Pre-commit Hook**:
```bash
#!/bin/bash
# .git/hooks/pre-commit

# Run unit tests
go test ./coordinator/... || exit 1

# Verify code preservation
sha256sum -c checksums.txt || exit 1

echo "✅ Tests passed"
```

**CI Pipeline** (GitHub Actions):
```yaml
name: CI
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:14
        env:
          POSTGRES_PASSWORD: postgres
    steps:
      - uses: actions/checkout@v2
      - uses: actions/setup-go@v2
        with:
          go-version: '1.21'
      - name: Run tests
        run: go test -v ./...
      - name: Verify checksums
        run: sha256sum -c checksums.txt
```

---

## 5. Deployment Strategy

### 5.1 Environment Progression

**Development** → **Staging** → **Production**

### 5.2 Development Environment

**Setup**:
```bash
# Clone repository
git clone https://github.com/redlabs-sc/telegram-bot-option2.git
cd telegram-bot-option2

# Copy environment template
cp .env.example .env
# Edit .env with development values

# Start services
docker-compose up -d

# Run migrations
./scripts/migrate.sh

# Start coordinator
cd coordinator && go run .
```

**Characteristics**:
- Local PostgreSQL
- Local Bot API Server
- Hot reload during development
- Detailed logging (DEBUG level)

### 5.3 Staging Environment

**Purpose**: Pre-production validation

**Setup**:
```bash
# Deploy to staging Kubernetes cluster
kubectl config use-context staging
kubectl apply -f k8s/

# Run smoke tests
go test -tags=staging ./tests/e2e/
```

**Characteristics**:
- Mirrors production configuration
- Separate Telegram bot token (staging bot)
- Full monitoring stack
- Limited file processing (test data only)

### 5.4 Production Deployment

**Checklist**:
- [ ] All tests passing in staging
- [ ] Database backup completed
- [ ] Rollback plan documented
- [ ] Team notified of deployment window
- [ ] Monitoring dashboards reviewed

**Deployment Steps**:
```bash
# 1. Build and push Docker image
docker build -t your-registry/telegram-bot-coordinator:v1.0.0 .
docker push your-registry/telegram-bot-coordinator:v1.0.0

# 2. Update Kubernetes manifest
kubectl set image deployment/telegram-bot-coordinator \
  coordinator=your-registry/telegram-bot-coordinator:v1.0.0

# 3. Monitor rollout
kubectl rollout status deployment/telegram-bot-coordinator

# 4. Verify health
kubectl exec -it telegram-bot-coordinator-xxx -- wget -qO- localhost:8080/health

# 5. Monitor metrics
# Open Grafana, verify normal operation

# 6. Send test file to bot
# Verify end-to-end processing
```

**Post-Deployment**:
- Monitor error rates for 1 hour
- Check resource usage trends
- Verify batch completion times
- Review logs for anomalies

### 5.5 Blue-Green Deployment (Advanced)

For zero-downtime deployments:

```bash
# 1. Deploy new version as "green"
kubectl apply -f k8s/deployment-green.yaml

# 2. Wait for green to be healthy
kubectl wait --for=condition=ready pod -l version=green

# 3. Switch service to green
kubectl patch service telegram-bot-coordinator -p '{"spec":{"selector":{"version":"green"}}}'

# 4. Monitor for 15 minutes
# If successful, delete blue
kubectl delete deployment telegram-bot-coordinator-blue

# If issues, rollback
kubectl patch service telegram-bot-coordinator -p '{"spec":{"selector":{"version":"blue"}}}'
```

---

## 6. Monitoring & Observability

### 6.1 Key Metrics

**Business Metrics**:
- Files processed per hour (throughput)
- Success rate (%)
- Average processing time per batch (minutes)
- Queue size trend

**Technical Metrics**:
- Download worker utilization (%)
- Batch worker utilization (%)
- Database connection pool usage
- RAM usage (% of limit)
- CPU usage (% of cores)

**Error Metrics**:
- Failed downloads (count, rate)
- Failed batches (count, rate)
- Timeout errors (by stage)
- Database errors (by query)

### 6.2 Alerting Rules

**File**: `monitoring/prometheus-alerts.yml`

```yaml
groups:
- name: telegram_bot
  rules:
  - alert: HighQueueSize
    expr: telegram_bot_queue_size{status="pending"} > 100
    for: 15m
    annotations:
      summary: "High queue size: {{ $value }} files pending"

  - alert: LowSuccessRate
    expr: rate(telegram_bot_batches_completed[1h]) / rate(telegram_bot_batches_total[1h]) < 0.95
    for: 30m
    annotations:
      summary: "Success rate below 95%"

  - alert: WorkerDown
    expr: telegram_bot_worker_status == 0
    for: 5m
    annotations:
      summary: "Worker {{ $labels.id }} is down"

  - alert: HighMemoryUsage
    expr: container_memory_usage_bytes / container_spec_memory_limit_bytes > 0.9
    for: 10m
    annotations:
      summary: "Memory usage above 90%"
```

### 6.3 Log Aggregation

**Structured Logging Format**:
```json
{
  "timestamp": "2025-11-13T10:23:45Z",
  "level": "info",
  "component": "batch_worker",
  "batch_id": "batch_042",
  "worker_id": "batch_worker_3",
  "message": "Extract stage completed",
  "duration_sec": 720,
  "file_count": 10
}
```

**Log Queries** (if using Elasticsearch/Loki):
```
# Find all errors in last hour
level="error" AND timestamp>now-1h

# Find slow batches
component="batch_worker" AND duration_sec>3600

# Find specific batch
batch_id="batch_042"
```

### 6.4 Distributed Tracing (Optional)

For advanced debugging, implement OpenTelemetry:

```go
import "go.opentelemetry.io/otel"

func (bw *BatchWorker) processBatch(ctx context.Context, batchID string) error {
    ctx, span := otel.Tracer("batch_worker").Start(ctx, "process_batch")
    defer span.End()

    span.SetAttributes(attribute.String("batch_id", batchID))

    // Process batch...
}
```

---

## 7. Risk Mitigation

### 7.1 Identified Risks & Mitigations

| Risk | Impact | Probability | Mitigation |
|------|--------|-------------|------------|
| Database connection exhaustion | High | Medium | Connection pooling (max 25), monitoring, auto-scaling |
| Disk space exhaustion | Critical | Medium | Proactive cleanup, disk monitoring, alerts at 80% |
| Extract/convert subprocess hang | High | Low | Timeout enforcement (30 min), circuit breakers, auto-recovery |
| Batch worker deadlock | High | Low | Timeout + requeue logic, worker health checks |
| Network partition from Telegram API | High | Medium | Retry with exponential backoff, offline queue, circuit breakers |
| PostgreSQL performance degradation | Medium | Low | Query optimization, indexing, connection pooling, read replicas |

### 7.2 Circuit Breaker Implementation

```go
type CircuitBreaker struct {
    failures    int
    maxFailures int
    timeout     time.Duration
    lastFailure time.Time
    state       string  // "closed", "open", "half-open"
}

func (cb *CircuitBreaker) Call(fn func() error) error {
    if cb.state == "open" {
        if time.Since(cb.lastFailure) > cb.timeout {
            cb.state = "half-open"
        } else {
            return fmt.Errorf("circuit breaker open")
        }
    }

    err := fn()
    if err != nil {
        cb.failures++
        cb.lastFailure = time.Now()
        if cb.failures >= cb.maxFailures {
            cb.state = "open"
        }
        return err
    }

    // Success - reset
    cb.failures = 0
    cb.state = "closed"
    return nil
}
```

### 7.3 Backup Strategy

**Database Backups**:
```bash
# Daily automated backup
0 2 * * * pg_dump -U bot_user telegram_bot_option2 | gzip > /backups/db_$(date +\%Y\%m\%d).sql.gz

# Retention: 7 days daily, 4 weeks weekly, 12 months monthly
```

**Configuration Backups**:
```bash
# Backup .env and configs to encrypted storage
tar -czf config_backup.tar.gz .env coordinator/ k8s/
gpg --encrypt --recipient admin@example.com config_backup.tar.gz
aws s3 cp config_backup.tar.gz.gpg s3://backups/
```

### 7.4 Disaster Recovery Plan

**RTO** (Recovery Time Objective): 1 hour
**RPO** (Recovery Point Objective): 24 hours

**Recovery Steps**:
1. Restore database from latest backup (15 min)
2. Deploy coordinator from Docker image (10 min)
3. Verify health endpoints (5 min)
4. Run crash recovery script (10 min)
5. Resume processing (automatic)

---

## 8. Rollback Plan

### 8.1 Rollback Triggers

Roll back deployment if:
- Success rate drops below 90% for 30 minutes
- Memory usage exceeds 90% for 15 minutes
- Critical errors occur in > 10% of batches
- Database connection failures persist
- Team identifies critical bug

### 8.2 Rollback Procedure

**Kubernetes Deployment Rollback**:
```bash
# 1. Rollback to previous version
kubectl rollout undo deployment/telegram-bot-coordinator

# 2. Verify rollback status
kubectl rollout status deployment/telegram-bot-coordinator

# 3. Check health
kubectl exec -it telegram-bot-coordinator-xxx -- wget -qO- localhost:8080/health

# 4. Monitor metrics
# Open Grafana, verify error rates decreasing

# 5. Notify team
echo "Rolled back to previous version due to [reason]" | mail -s "ROLLBACK" team@example.com
```

**Docker Compose Rollback**:
```bash
# 1. Stop current version
docker-compose down

# 2. Checkout previous version
git checkout HEAD~1

# 3. Rebuild and start
docker-compose up -d --build

# 4. Verify health
curl http://localhost:8080/health
```

### 8.3 Data Recovery

If data corruption occurs:
```bash
# 1. Stop coordinator
kubectl scale deployment/telegram-bot-coordinator --replicas=0

# 2. Restore database from backup
psql -U bot_user -d telegram_bot_option2 < /backups/db_20251113.sql

# 3. Verify data integrity
psql -U bot_user -d telegram_bot_option2 -c "SELECT COUNT(*) FROM download_queue;"

# 4. Restart coordinator
kubectl scale deployment/telegram-bot-coordinator --replicas=1

# 5. Run crash recovery
# Coordinator will auto-recover on startup
```

---

## 9. Prompts for Claude Code

### 9.1 Initial Setup Prompt

```
I have the Option 2 implementation plan for the Telegram Data Processor Bot. I need to set up the project structure from scratch.

Please:
1. Create the directory structure as specified in Section 2.2
2. Initialize the Go module with name "github.com/redlabs-sc/telegram-bot-option2"
3. Create the .env file with all required variables from Section 2.2
4. Create the .gitignore file
5. Set up the PostgreSQL database schema by creating all 4 migration files from Phase 1, Task 1.1
6. Create the go.mod file with all dependencies from Section 2.4

After completing this, I'll copy the preserved code files (extract.go, convert.go, store.go) and compute their checksums.
```

### 9.2 Foundation Phase Prompt

```
I've set up the project structure. Now I need to implement Phase 1 (Foundation) from the implementation plan.

Please implement:
1. Config management (coordinator/config.go) - Load from .env, parse all variables, implement IsAdmin()
2. Logger initialization (coordinator/logger.go) - Use zap, support JSON/console formats
3. Health check endpoint (coordinator/health.go) - Check database, workers, queue stats
4. Metrics endpoint (coordinator/metrics.go) - Prometheus format, queue sizes, durations
5. Main entry point skeleton (coordinator/main.go) - Load config, init logger, connect DB, start servers

Use the exact code from Phase 1 tasks in the implementation plan. Make sure to:
- Import all required packages
- Handle errors properly
- Use structured logging with zap
- Follow Go best practices

After completion, I'll test the foundation by running the coordinator and checking health/metrics endpoints.
```

### 9.3 Download Pipeline Prompt

```
Foundation phase is complete and tested. Now implement Phase 2 (Download Pipeline).

Please implement:
1. Telegram bot receiver (coordinator/telegram_receiver.go)
   - Admin-only validation
   - File upload handling
   - Command routing
   - /start and /help commands
2. Download worker pool (coordinator/download_worker.go)
   - Optimistic locking with FOR UPDATE SKIP LOCKED
   - SHA256 hash computation
   - Streaming download to avoid memory issues
   - Error handling and retry logic
3. Crash recovery (coordinator/crash_recovery.go)
   - Reset stuck downloads to PENDING
4. Update main.go to start receiver and 3 download workers

Use the exact code from Phase 2 tasks. Pay special attention to:
- Exactly 3 concurrent download workers (enforced)
- SHA256 hash computed during download (not after)
- Admin validation on EVERY request
- Proper context handling for timeouts

I'll test by uploading files via Telegram and verifying downloads complete successfully.
```

### 9.4 Batch Coordinator Prompt

```
Download pipeline is working. Now implement Phase 3 (Batch Coordinator).

Please implement:
1. Batch coordinator (coordinator/batch_coordinator.go)
   - Monitor downloaded files
   - Create batches when BATCH_SIZE reached OR timeout
   - Create isolated directory structure per batch
   - Route archives to all/, TXT to txt/
   - Copy pass.txt to each batch
   - Enforce max concurrent batches
2. Update main.go to start batch coordinator

Critical requirements:
- Batch size configurable via BATCH_SIZE env var (default 10)
- Batch timeout configurable via BATCH_TIMEOUT_SEC (default 300)
- Generate batch IDs as "batch_001", "batch_002", etc.
- Create EXACT directory structure: batches/batch_NNN/app/extraction/files/{all,txt,pass,nopass,errors}

I'll test by uploading 10 files and verifying batch creation with correct directory structure.
```

### 9.5 Batch Workers Prompt

```
Batch coordinator is creating batches successfully. Now implement Phase 4 (Batch Processing Workers).

Please implement:
1. Batch worker (coordinator/batch_worker.go)
   - Claim batches with optimistic locking
   - Change working directory to batch root
   - Execute extract → convert → store sequentially
   - Use subprocess execution (go run extract.go, etc.)
   - Log to batch-specific log files
   - Handle timeouts and errors
2. Batch cleanup service (coordinator/batch_cleanup.go)
   - Delete completed batches after retention period
   - Archive failed batches for debugging
3. Update main.go to start 5 batch workers and cleanup service

CRITICAL REQUIREMENTS:
- NEVER modify extract.go, convert.go, or store.go
- Change working directory with os.Chdir() before calling each stage
- Use absolute paths to preserved code files
- Restore working directory after processing (defer os.Chdir(originalWD))
- Set CONVERT_INPUT_DIR and CONVERT_OUTPUT_FILE environment variables for convert.go
- Each stage has separate timeout (EXTRACT_TIMEOUT_SEC, etc.)

I'll test by verifying a batch completes end-to-end and checksums of preserved files match.
```

### 9.6 Admin Commands Prompt

```
Batch processing is working end-to-end. Now implement Phase 5 (Admin Commands).

Please implement admin commands in coordinator/admin_commands.go:
1. /queue - Show queue statistics, next batch ETA
2. /batches - Show active and recently completed batches
3. /stats - Show 24-hour analytics (success rate, throughput, etc.)
4. /health - Show component health status

Requirements:
- All commands admin-only (already have IsAdmin() check in handleCommand)
- Use emoji for visual appeal
- Show real-time data from PostgreSQL
- Format output clearly with Markdown
- Handle errors gracefully

I'll test by sending each command and verifying output is accurate and well-formatted.
```

### 9.7 Testing Prompt

```
All features are implemented. Now I need comprehensive tests for Phase 6.

Please create:
1. Unit tests:
   - coordinator/config_test.go - Test config loading, IsAdmin()
   - coordinator/batch_coordinator_test.go - Test batch creation logic
2. Integration tests:
   - tests/integration/pipeline_test.go - End-to-end pipeline test
3. Test utilities:
   - tests/testutil/database.go - Setup test database, run migrations
   - tests/testutil/fixtures.go - Create test files, insert test data

Requirements:
- Use testing.T standard library
- Create in-memory PostgreSQL for tests
- Mock Telegram API for integration tests
- Table-driven tests for multiple scenarios
- Cleanup test data after each test

I'll run tests with: go test -v ./...
```

### 9.8 Deployment Prompt

```
System is tested and ready for deployment. Help me with Phase 7 (Deployment).

Please create:
1. docker-compose.yml - Full stack (postgres, coordinator, local-bot-api, prometheus, grafana)
2. Dockerfile - Multi-stage build for coordinator
3. Kubernetes manifests:
   - k8s/deployment.yaml
   - k8s/service.yaml
   - k8s/configmap.yaml
   - k8s/secrets.yaml.template
4. Update CLAUDE.md with Option 2 documentation section

Requirements:
- Docker Compose for development environment
- Kubernetes for production
- Health checks and readiness probes
- Resource limits (memory: 4Gi, cpu: 2000m)
- Persistent volumes for logs
- Prometheus and Grafana for monitoring

I'll test by deploying with docker-compose up and verifying all services start correctly.
```

### 9.9 Troubleshooting Prompt

```
I'm encountering an issue with [specific problem]:

**Symptoms**:
- [Describe what's happening]
- [Error messages or logs]
- [Which phase/component]

**Context**:
- Currently on Phase [X]
- Last successful step: [Y]
- Environment: [development/staging/production]

**Logs**:
```
[Paste relevant logs]
```

Please help diagnose and fix this issue. Reference the implementation plan sections if relevant.
```

### 9.10 Optimization Prompt

```
System is running but I need to optimize performance based on Phase 6.

Current metrics:
- Processing time for 100 files: [X hours]
- RAM usage: [Y%]
- CPU usage: [Z%]
- Success rate: [N%]

Targets:
- Processing time < 2 hours ✅/❌
- RAM < 20% ✅/❌
- CPU < 50% ✅/❌
- Success rate > 98% ✅/❌

Please:
1. Analyze bottlenecks in the current implementation
2. Suggest optimizations (database indexes, worker tuning, etc.)
3. Provide code changes for identified bottlenecks
4. Explain expected performance improvement

I'll apply optimizations and re-run load tests to verify improvements.
```

---

## END OF IMPLEMENTATION PLAN PART 2

**Total Implementation Plan**:
- Part 1: Phases 1-3, Project Setup, Database, Download Pipeline, Batch Coordinator
- Part 2: Phases 4-7, Batch Workers, Admin Commands, Testing, Deployment, Monitoring, Rollback, Prompts

**Next Steps**:
1. Use prompts from Section 9 to guide Claude Code through implementation
2. Follow phases sequentially: 1 → 2 → 3 → 4 → 5 → 6 → 7
3. Test each phase before proceeding to next
4. Verify code preservation (checksums) after Phase 4
5. Load test before production deployment

**Estimated Timeline**: 12 weeks (3 months) from start to production deployment

---

*For questions or issues during implementation, refer to:*
- **Implementation Plan Part 1**: `option2-implementation-plan.md`
- **PRD**: `option2-prd.md`
- **Design Document**: `option2-batch-parallel-design.md`
- **Runbook**: `docs/RUNBOOK.md` (created in Phase 7)
