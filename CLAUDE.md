# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a **Telegram Data Processor Bot** implementing **Option 2: Batch-Based Parallel Processing System**. The architecture achieves 6× faster processing compared to sequential processing by distributing files into isolated batch directories and running multiple extract/convert/store pipelines in parallel.

**Implementation Status**: Phases 1-4 complete (Foundation, Download Pipeline, Batch Coordinator, Batch Workers). Phases 5-6 (Admin Commands, Testing) are optional enhancements.

**Critical Design Principle**: Three core processing files (`extract.go`, `convert.go`, `store.go`) are **100% preserved** - never modify them. All parallelism is achieved through batch directory isolation and working directory changes.

## Architecture

### Core Components

1. **Telegram Receiver** (`telegram_receiver.go`): Handles bot commands and file uploads, validates admin users
2. **Download Workers** (`download_worker.go`): 3 concurrent workers download files from Telegram to `downloads/`
3. **Batch Coordinator** (`batch_coordinator.go`): Groups downloaded files into batches of 10, creates isolated directories
4. **Batch Workers** (`batch_worker.go`): 5 concurrent workers process batches through extract → convert → store pipeline
5. **Crash Recovery** (`crash_recovery.go`): Detects and recovers stuck downloads/batches on startup
6. **Health/Metrics** (`health.go`, `metrics.go`): HTTP endpoints for monitoring

### Batch Processing Flow

```
Telegram → Download Queue → downloads/ → Batch Creation → batches/batch_XXX/ → Process → Database
```

Each batch gets an isolated directory structure:
```
batches/batch_001/
├── app/extraction/
│   ├── files/
│   │   ├── all/          # Input archives
│   │   ├── txt/          # Direct TXT files + converted output
│   │   ├── pass/         # Extracted files
│   │   ├── nopass/       # Password-protected archives
│   │   └── errors/       # Failed extractions
│   └── pass.txt          # Password list (copied from root)
└── logs/
    ├── extract.log
    ├── convert.log
    └── store.log
```

### Code Preservation Strategy

The existing extraction code processes files in fixed relative paths (`app/extraction/files/all/`, etc.). Instead of modifying this code, we:

1. Create the full `app/extraction/files/` directory structure inside each batch directory
2. Change the working directory to the batch root before executing processing code
3. The unchanged code operates on its expected relative paths, which now resolve to batch-specific directories

**Critical Implementation** (from `batch_worker.go:215-220`):
```go
// Save current working directory
originalWD, _ := os.Getwd()
defer os.Chdir(originalWD) // Always restore

// Change to batch directory - this is the key to code preservation
os.Chdir(batchRoot) // e.g., "batches/batch_001/"

// Now when extract.go references "app/extraction/files/all/",
// it resolves to "batches/batch_001/app/extraction/files/all/"
```

## Development Commands

### Initial Setup

```bash
# Install dependencies
go mod download
go mod tidy

# Create .env from example
cp .env.example .env
# Edit .env with your values (TELEGRAM_BOT_TOKEN, DB credentials, etc.)

# Verify preserved code checksums
sha256sum -c checksums.txt
```

### Database Setup

```bash
# Create PostgreSQL database
createdb telegram_bot_option2
createuser bot_user
psql telegram_bot_option2 -c "GRANT ALL PRIVILEGES ON DATABASE telegram_bot_option2 TO bot_user;"

# Run all migrations
./scripts/migrate.sh

# Verify tables created
psql -U bot_user -d telegram_bot_option2 -c "\dt"
# Should show: download_queue, batch_processing, batch_files, processing_metrics
```

### Building and Running

```bash
# Build the coordinator
cd coordinator
go build -o telegram-bot-coordinator

# Run in development mode
go run .

# Run with custom log level
LOG_LEVEL=debug go run .

# Run compiled binary
./telegram-bot-coordinator
```

### Monitoring

```bash
# Check health endpoint
curl http://localhost:8080/health | jq .

# View Prometheus metrics
curl http://localhost:9090/metrics

# View coordinator logs (JSON format by default)
tail -f logs/coordinator.log

# View batch-specific logs
tail -f batches/batch_001/logs/extract.log
tail -f batches/batch_001/logs/convert.log
tail -f batches/batch_001/logs/store.log

# Monitor queue in database
psql -U bot_user -d telegram_bot_option2 -c \
  "SELECT status, COUNT(*) FROM download_queue GROUP BY status;"

# Monitor active batches
psql -U bot_user -d telegram_bot_option2 -c \
  "SELECT batch_id, status, worker_id, started_at FROM batch_processing WHERE status != 'COMPLETED' ORDER BY created_at;"
```

### Code Verification

```bash
# CRITICAL: Verify preserved files haven't been modified
sha256sum -c checksums.txt

# Expected output:
# app/extraction/extract/extract.go: OK
# app/extraction/convert/convert.go: OK
# app/extraction/store.go: OK

# If checksums don't match, IMMEDIATELY revert changes
git checkout app/extraction/extract/extract.go app/extraction/convert/convert.go app/extraction/store.go
```

## Configuration

All configuration is managed via environment variables in `.env`. See `.env.example` for template.

**Critical Settings**:
- `MAX_DOWNLOAD_WORKERS=3` - **MUST be exactly 3** (Telegram API rate limit)
- `MAX_BATCH_WORKERS=5` - Maximum concurrent batches (5 is tested, can increase to 10-20 for scaling)
- `BATCH_SIZE=10` - Files per batch
- `BATCH_TIMEOUT_SEC=300` - Create batch if files wait > 5 minutes (even if not full)

**Database** (PostgreSQL required):
- `DB_HOST=localhost`, `DB_PORT=5432`, `DB_NAME=telegram_bot_option2`
- `DB_USER=bot_user`, `DB_PASSWORD=...`
- `DB_SSL_MODE=disable` for local development, `require` for production

**Telegram Bot**:
- `TELEGRAM_BOT_TOKEN` - Get from @BotFather
- `ADMIN_IDS` - Comma-separated Telegram user IDs (only these users can upload files)
- `USE_LOCAL_BOT_API=true` - Required for files > 20MB (Telegram limitation)
- `LOCAL_BOT_API_URL=http://localhost:8081` - Local Bot API Server endpoint

**Timeouts** (in seconds):
- `DOWNLOAD_TIMEOUT_SEC=1800` (30 min)
- `EXTRACT_TIMEOUT_SEC=1800` (30 min)
- `CONVERT_TIMEOUT_SEC=1800` (30 min)
- `STORE_TIMEOUT_SEC=3600` (60 min)

**Cleanup**:
- `COMPLETED_BATCH_RETENTION_HOURS=1` - Delete successful batches after 1 hour
- `FAILED_BATCH_RETENTION_DAYS=7` - Keep failed batches for debugging

## Critical Implementation Rules

### 1. Code Preservation (ABSOLUTE REQUIREMENT)

**NEVER modify these files**:
- `app/extraction/extract/extract.go`
- `app/extraction/convert/convert.go`
- `app/extraction/store.go`

**Verification**: Run `sha256sum -c checksums.txt` after ANY changes. If checksums don't match, revert immediately.

**Why**: The entire batch-based architecture is designed around preserving these files. Any modification breaks the working directory pattern and defeats the purpose of this implementation.

### 2. Worker Concurrency Limits

**Fixed limits**:
- Download workers: **Exactly 3** (Telegram API constraint, enforced in `config.go` validation)
- Batch workers: **Maximum 5** (tested configuration, configurable via `MAX_BATCH_WORKERS`)

**Reason**: Extract and convert process entire directories. Multiple workers on the same directory cause race conditions and data corruption. Batch isolation solves this by giving each worker its own directory.

### 3. Database Locking Pattern

All workers use `FOR UPDATE SKIP LOCKED` for optimistic concurrency control:

```sql
-- Example from download_worker.go
SELECT task_id, file_id, filename, file_type, file_size
FROM download_queue
WHERE status = 'PENDING'
ORDER BY priority DESC, created_at ASC
LIMIT 1
FOR UPDATE SKIP LOCKED
```

This pattern:
- Prevents duplicate processing (multiple workers won't claim same task)
- Avoids blocking (workers skip locked rows instead of waiting)
- Ensures fairness (oldest tasks processed first)

### 4. Working Directory Pattern

**Always restore the original working directory**:

```go
originalDir, _ := os.Getwd()
defer os.Chdir(originalDir)  // Use defer to ensure restoration even on error

os.Chdir(batchDir)
// ... execute batch processing
```

**Why `defer` is critical**: If processing fails and panics, defer ensures we restore the working directory. Without this, subsequent batches might process in the wrong directory.

### 5. Batch Cleanup

Implemented in `batch_worker.go:380`:
- Completed batches: Deleted after `COMPLETED_BATCH_RETENTION_HOURS` (default 1 hour) to free disk space
- Failed batches: Should be moved to `archive/failed/` and retained for `FAILED_BATCH_RETENTION_DAYS` (default 7 days)
- Monitor disk space: Health endpoint warns if < 50GB free

## Admin Telegram Commands

**Currently Implemented** (basic stubs in `telegram_receiver.go`):
- `/start` - Welcome message with bot information
- `/help` - List available commands

**Stub Implementations** (placeholders for Phase 5):
- `/queue` - Show queue statistics (pending, downloading, downloaded, failed)
- `/batches` - List active batches with status and progress
- `/stats` - Overall system statistics (throughput, success rate, avg time)
- `/health` - System health check (workers alive, disk space, memory)

**Not Yet Implemented**:
- `/retry <task_id>` - Retry failed download task
- `/cancel <task_id>` - Cancel pending task
- `/priority <task_id>` - Increase task priority

To implement these, modify `telegram_receiver.go` functions starting at line 160.

## Database Schema

### download_queue (migration 001)
Persistent queue for file downloads.
- **Statuses**: `PENDING` → `DOWNLOADING` → `DOWNLOADED` → (assigned to batch) → `COMPLETED/FAILED`
- **Key fields**: `task_id`, `file_id`, `filename`, `file_type`, `file_size`, `sha256_hash`, `batch_id`
- **Indexes**: `idx_dq_status`, `idx_dq_priority`, `idx_dq_batch`

### batch_processing (migration 002)
Tracks batch lifecycle.
- **Statuses**: `QUEUED` → `EXTRACTING` → `CONVERTING` → `STORING` → `COMPLETED/FAILED`
- **Key fields**: `batch_id`, `file_count`, `archive_count`, `txt_count`, `worker_id`, duration fields
- **Indexes**: `idx_bp_status`, `idx_bp_created`, `idx_bp_worker`

### batch_files (migration 003)
Join table linking files to batches with per-file processing status.
- Links `download_queue.task_id` to `batch_processing.batch_id`
- Tracks individual file status within batch

### processing_metrics (migration 004)
Time-series metrics for analytics (download speed, stage durations, etc.)
- Used for Prometheus metrics aggregation
- Records metric_type: 'download_speed', 'extract_time', 'convert_time', 'store_time'

## Code Structure

```
coordinator/
├── main.go                  # Entry point, orchestrates all components
├── config.go                # Environment variable parsing and validation
├── logger.go                # Zap logger initialization
├── health.go                # Health check HTTP endpoint (:8080/health)
├── metrics.go               # Prometheus metrics collector (:9090/metrics)
├── telegram_receiver.go     # Telegram bot message handler
├── download_worker.go       # Download worker pool (3 workers)
├── batch_coordinator.go     # Batch creation and file routing
├── batch_worker.go          # Batch processing pool (5 workers)
└── crash_recovery.go        # Startup recovery and periodic health checks

database/migrations/
├── 001_create_download_queue.sql
├── 002_create_batch_processing.sql
├── 003_create_batch_files.sql
└── 004_create_metrics.sql

app/extraction/              # PRESERVED - do not modify
├── extract/extract.go       # Archive extraction (ZIP, RAR)
├── convert/convert.go       # Text conversion and parsing
└── store.go                 # Database storage logic
```

## Common Workflows

### Adding a New Feature

1. **Check if it requires modifying preserved files** (`extract.go`, `convert.go`, `store.go`)
2. If yes: Use configuration or environment variables instead - add to `config.go`
3. If no: Implement in appropriate coordinator file
4. Verify checksums haven't changed: `sha256sum -c checksums.txt`
5. Test locally before committing
6. Update CLAUDE.md if the feature changes workflows

### Debugging a Failed Batch

1. **Find batch ID** from database or logs:
   ```sql
   SELECT batch_id, status, error_message, started_at
   FROM batch_processing
   WHERE status = 'FAILED'
   ORDER BY created_at DESC;
   ```

2. **Check batch logs**:
   ```bash
   tail -100 batches/batch_XXX/logs/extract.log
   tail -100 batches/batch_XXX/logs/convert.log
   tail -100 batches/batch_XXX/logs/store.log
   ```

3. **Check which stage failed**:
   ```sql
   SELECT extract_duration_sec, convert_duration_sec, store_duration_sec, error_message
   FROM batch_processing
   WHERE batch_id = 'batch_XXX';
   ```

4. **Inspect failed files**:
   ```bash
   ls -lh batches/batch_XXX/app/extraction/files/errors/
   ls -lh batches/batch_XXX/app/extraction/files/nopass/
   ```

5. **Check working directory context**: Failed batches might indicate the working directory wasn't properly set

### Scaling to More Concurrent Batches

1. **Update `.env`**: Increase `MAX_BATCH_WORKERS` (e.g., from 5 to 10)
2. **Restart coordinator**: `pkill telegram-bot-coordinator && ./telegram-bot-coordinator`
3. **Monitor resources**:
   - Need ~2GB RAM + 2 CPU cores per batch worker
   - Check with: `curl localhost:8080/health | jq '.components.filesystem'`
4. **Monitor batch throughput**:
   ```sql
   SELECT COUNT(*) as completed_last_hour
   FROM batch_processing
   WHERE status = 'COMPLETED'
   AND completed_at > NOW() - INTERVAL '1 hour';
   ```
5. **Adjust `BATCH_SIZE` if needed**: Smaller batches (e.g., 5) = faster feedback, more overhead

### Recovering from Crashes

The system has automatic recovery built in (`crash_recovery.go`):

1. **On startup**, coordinator automatically:
   - Resets downloads stuck in `DOWNLOADING` for > 30 minutes → back to `PENDING`
   - Resets batches stuck in processing states for > 2 hours → back to `QUEUED`
   - Unlinks files from failed batches so they can be rebatched

2. **Periodic health checks** (every 5 minutes):
   - Detects stuck tasks and attempts recovery
   - Logs warnings for investigation

3. **Manual recovery** (if needed):
   ```sql
   -- Reset all stuck downloads
   UPDATE download_queue SET status = 'PENDING', started_at = NULL
   WHERE status = 'DOWNLOADING';

   -- Reset all stuck batches
   UPDATE batch_processing SET status = 'QUEUED', worker_id = NULL
   WHERE status IN ('EXTRACTING', 'CONVERTING', 'STORING');

   -- Unlink files from stuck batches
   UPDATE download_queue SET batch_id = NULL
   WHERE batch_id IN (
     SELECT batch_id FROM batch_processing WHERE status = 'QUEUED'
   );
   ```

## Troubleshooting

### Issue: Checksums don't match
- **Cause**: Preserved files (`extract.go`, `convert.go`, `store.go`) were modified
- **Fix**: `git checkout app/extraction/extract/extract.go app/extraction/convert/convert.go app/extraction/store.go`
- **Prevention**: Always check `git diff` before committing changes to `app/extraction/`

### Issue: "No space left on device"
- **Cause**: Completed batches not cleaned up, or large files accumulating
- **Check**: `df -h` and `du -sh batches/*`
- **Fix**: Manually delete old batches: `rm -rf batches/batch_0*`
- **Long-term**: Decrease `COMPLETED_BATCH_RETENTION_HOURS` in `.env`

### Issue: Downloads stuck at 3 concurrent
- **Cause**: This is **expected behavior** - Telegram API rate limit
- **Action**: Do NOT increase `MAX_DOWNLOAD_WORKERS` beyond 3 (config validation will reject it)

### Issue: Batch processing takes > 2 hours (100 files)
- **Check active workers**:
  ```sql
  SELECT COUNT(*) FROM batch_processing
  WHERE status IN ('EXTRACTING','CONVERTING','STORING');
  ```
  Should be 5 (or your MAX_BATCH_WORKERS value)

- **Check database performance**:
  ```sql
  EXPLAIN ANALYZE SELECT * FROM download_queue
  WHERE status = 'PENDING' ORDER BY priority DESC, created_at ASC LIMIT 1;
  ```
  Should use index `idx_dq_priority`

- **Check disk I/O**: Use `iotop` or `iostat` - consider SSD if using HDD

### Issue: Worker not processing tasks
- **Check worker logs**: Look for errors in coordinator logs
- **Check database locks**:
  ```sql
  SELECT * FROM pg_locks WHERE NOT granted;
  ```
- **Restart coordinator**: Workers might have panicked and not recovered

### Issue: "failed to change to batch directory"
- **Cause**: Batch directory doesn't exist (race condition or filesystem error)
- **Check**: `ls -ld batches/batch_XXX`
- **Fix**: This shouldn't happen - indicates bug in batch_coordinator.go
- **Recovery**: Batch will be marked FAILED and files will be unbatched for retry

## Performance Targets

| Metric | Target | How to Measure |
|--------|--------|----------------|
| Processing Time (100 files) | < 2 hours | End-to-end from first upload to last completion |
| Throughput | ≥ 50 files/hour sustained | `SELECT COUNT(*)/(EXTRACT(EPOCH FROM MAX(completed_at) - MIN(created_at))/3600) FROM download_queue WHERE status='DOWNLOADED' AND completed_at > NOW() - INTERVAL '24 hours';` |
| Success Rate | ≥ 98% | `SELECT (COUNT(CASE WHEN status='COMPLETED' THEN 1 END)::float / COUNT(*)) * 100 FROM batch_processing;` |
| RAM Usage | < 20% | `free -h` or health endpoint |
| CPU Usage | < 50% | `top` or health endpoint |
| Concurrent Batches | Exactly 5 max | `SELECT COUNT(*) FROM batch_processing WHERE status IN ('EXTRACTING','CONVERTING','STORING');` |

## Development Notes

- **Go version**: 1.21+ required (uses new error wrapping features)
- **PostgreSQL version**: 14+ required (uses modern JSON functions)
- **Dependencies**: Managed via `go.mod` - run `go mod download` after clone
- **Logging**: Structured JSON logs using `go.uber.org/zap` (configurable to text via `LOG_FORMAT=text`)
- **Metrics**: Prometheus format exposed on port 9090 - compatible with Grafana

## Documentation References

Critical design documents in `Docs/`:
- **`prd.md`**: Complete functional and non-functional requirements
- **`batch-parallel-design.md`**: Detailed architecture with batch isolation pattern
- **`implementation-plan.md`**: Phase 1-3 implementation guide
- **`implementation-plan-part2.md`**: Phase 4-7 implementation guide

**Read order for new developers**: PRD → Design → Implementation Plan Part 1 → Implementation Plan Part 2 → CLAUDE.md (this file)

## Git Workflow

Working on branch: `claude/init-codebase-analysis-01YLvkd52eZpCbs8Z4wKUNAG`

```bash
# Stage changes
git add <files>

# Commit with conventional commit format
git commit -m "feat: add admin queue command implementation"
git commit -m "fix: handle edge case in batch coordinator"
git commit -m "docs: update CLAUDE.md with new commands"

# Push to designated branch
git push -u origin claude/init-codebase-analysis-01YLvkd52eZpCbs8Z4wKUNAG
```

**Never commit**:
- `.env` files (use `.env.example` for templates)
- `batches/` directory contents (git-ignored)
- `downloads/` directory contents (git-ignored)
- `logs/` directory contents (git-ignored)
- Compiled binaries (e.g., `telegram-bot-coordinator`)
- `go.sum` changes unless dependencies actually changed
