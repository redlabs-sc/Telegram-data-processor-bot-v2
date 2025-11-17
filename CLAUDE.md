# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a **Telegram Data Processor Bot** implementing a **Pipelined Multi-Round Processing System**. The architecture achieves 2.6× faster processing compared to sequential processing by allowing extract, convert, and store stages to operate on different rounds simultaneously.

**Implementation Status**: Core pipelined architecture complete (Download Pipeline, Round Coordinator, Extract/Convert/Store Workers). Further optimizations and admin commands are optional enhancements.

**Critical Design Principle**: Three core processing files (`extract.go`, `convert.go`, `store.go`) are **100% preserved** - never modify them. Extract and convert are singleton workers processing global directories. Store workers process isolated task directories to enable parallelism.

## Architecture

### Core Components

1. **Telegram Receiver** (`telegram_receiver.go`): Handles bot commands and file uploads, validates admin users
2. **Download Workers** (`download_worker.go`): 3 concurrent workers download files from Telegram to `downloads/`
3. **Round Coordinator** (`round_coordinator.go`): Groups downloaded files into rounds of 50, moves to staging directories
4. **Extract Worker** (`extract_worker.go`): **Singleton worker** - only 1 instance, processes ALL archives through extract stage
5. **Convert Worker** (`convert_worker.go`): **Singleton worker** - only 1 instance, processes ALL text files through convert stage
6. **Store Workers** (`store_worker.go`): 5 concurrent workers process 2-file tasks through store stage in isolated directories
7. **Crash Recovery** (`crash_recovery.go`): Detects and recovers stuck downloads/rounds/tasks on startup
8. **Health/Metrics** (`health.go`, `metrics.go`): HTTP endpoints for monitoring

### Pipelined Round Processing Flow

```
Telegram → Download Queue → downloads/ → Round Creation → Staging Directories → Pipeline Stages → Database
                                              ↓
                    Archives → app/extraction/files/all/ (Extract Stage)
                    TXT files → app/extraction/files/txt/ (Convert Stage)
                                              ↓
                                    Store Tasks (2 files each)
                                              ↓
                                store_tasks/123/ (Isolated directory)
```

**Pipeline Parallelism**: Multiple rounds exist in different stages simultaneously:
```
Time 0-10:  Round 1 extracting
Time 10-15: Round 1 converting  | Round 2 extracting
Time 15-55: Round 1 storing      | Round 2 converting | Round 3 extracting
Time 55+:   Round 1 COMPLETE     | Round 2 storing    | Round 3 converting | Round 4 extracting
```

### Code Preservation Strategy

The preserved code processes files in specific ways:

1. **extract.go**: Processes ALL archives in `app/extraction/files/all/` directory
2. **convert.go**: Processes ALL text files in `CONVERT_INPUT_DIR` (env var)
3. **store.go**: Processes 2 files at a time from `app/extraction/files/txt/`

**Preservation approach**:
- **Extract & Convert**: Only ONE worker each (singleton) prevents simultaneous execution
- **Store**: Each task runs in isolated directory (`store_tasks/123/`) with only its 2 assigned files

**Critical Implementation** (from `store_worker.go:150-165`):
```go
// Create isolated task directory
taskDir := fmt.Sprintf("store_tasks/%d", task.TaskID)

// Copy only the 2 assigned files to task directory
setupStoreTaskDirectory(taskDir, task.FilePaths)

// Save current working directory
originalDir, _ := os.Getwd()
defer os.Chdir(originalDir) // Always restore

// Change to task directory
os.Chdir(taskDir)

// Now store.go processes only these 2 files in isolation
extraction.RunPipeline(ctx)
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
# Should show: download_queue, processing_rounds, store_tasks, processing_metrics
# Note: batch_processing and batch_files are deprecated but kept for rollback
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

# Monitor queue in database
psql -U bot_user -d telegram_bot_option2 -c \
  "SELECT status, COUNT(*) FROM download_queue GROUP BY status;"

# Monitor active rounds
psql -U bot_user -d telegram_bot_option2 -c \
  "SELECT round_id, round_status, extract_status, convert_status, store_status, created_at
   FROM processing_rounds
   WHERE round_status != 'COMPLETED'
   ORDER BY created_at DESC;"

# Monitor store tasks
psql -U bot_user -d telegram_bot_option2 -c \
  "SELECT round_id, COUNT(*) as total_tasks,
          SUM(CASE WHEN status='COMPLETED' THEN 1 ELSE 0 END) as completed,
          SUM(CASE WHEN status='STORING' THEN 1 ELSE 0 END) as in_progress,
          SUM(CASE WHEN status='PENDING' THEN 1 ELSE 0 END) as pending
   FROM store_tasks
   GROUP BY round_id
   ORDER BY round_id DESC
   LIMIT 10;"

# View round details
psql -U bot_user -d telegram_bot_option2 -c \
  "SELECT round_id, file_count, archive_count, txt_count,
          extract_duration_sec, convert_duration_sec, store_duration_sec
   FROM processing_rounds
   WHERE round_status = 'COMPLETED'
   ORDER BY created_at DESC
   LIMIT 10;"
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
- `MAX_STORE_WORKERS=5` - Concurrent store workers (5 is tested, can increase to 10-20 for scaling)
- `ROUND_SIZE=50` - Files per round (10-100 allowed)
- `ROUND_TIMEOUT_SEC=300` - Create round if files wait > 5 minutes (even if not full)
- **Note**: Extract and convert workers are always 1 (singleton requirement, not configurable)

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
- `COMPLETED_ROUND_RETENTION_HOURS=1` - Delete successful rounds after 1 hour
- `FAILED_ROUND_RETENTION_DAYS=7` - Keep failed rounds for debugging

## Critical Implementation Rules

### 1. Code Preservation (ABSOLUTE REQUIREMENT)

**NEVER modify these files**:
- `app/extraction/extract/extract.go`
- `app/extraction/convert/convert.go`
- `app/extraction/store.go`

**Verification**: Run `sha256sum -c checksums.txt` after ANY changes. If checksums don't match, revert immediately.

**Why**: The entire pipelined architecture is designed around preserving these files. Any modification breaks the working directory pattern and defeats the purpose of this implementation.

### 2. Worker Concurrency Limits

**Fixed limits**:
- Download workers: **Exactly 3** (Telegram API constraint, enforced in `config.go` validation)
- Extract worker: **Exactly 1** (Singleton requirement - extract.go processes ALL files in global directory)
- Convert worker: **Exactly 1** (Singleton requirement - convert.go processes ALL files in global directory)
- Store workers: **5 default, configurable 1-20** (Each processes 2-file tasks in isolated directories)

**Reason**: Extract and convert process entire directories. Only ONE instance can run at a time to prevent race conditions. Store workers process isolated task directories (store_tasks/123/), allowing parallelism.

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

**Store workers restore the original working directory**:

```go
originalDir, _ := os.Getwd()
defer os.Chdir(originalDir)  // Use defer to ensure restoration even on error

os.Chdir(taskDir)  // e.g., "store_tasks/123/"
// ... execute store.RunPipeline()
```

**Why `defer` is critical**: If processing fails and panics, defer ensures we restore the working directory. Without this, subsequent tasks might process in the wrong directory.

**Extract and convert workers** do NOT change directories - they process global staging directories:
- Extract: `app/extraction/files/all/`
- Convert: `app/extraction/files/pass/` and `app/extraction/files/txt/`

### 5. Round and Task Cleanup

- Completed rounds: Deleted after `COMPLETED_ROUND_RETENTION_HOURS` (default 1 hour) to free disk space
- Failed rounds: Should be retained for `FAILED_ROUND_RETENTION_DAYS` (default 7 days) for debugging
- Store task directories: Cleaned up immediately after task completion (successful or failed)
- Monitor disk space: Health endpoint warns if < 50GB free

## Admin Telegram Commands

**Currently Implemented** (basic stubs in `telegram_receiver.go`):
- `/start` - Welcome message with bot information
- `/help` - List available commands

**Stub Implementations** (placeholders for future):
- `/queue` - Show queue statistics (pending, downloading, downloaded, failed)
- `/rounds` - List active rounds with status and progress (extract, convert, store stages)
- `/stats` - Overall system statistics (throughput, success rate, avg time)
- `/health` - System health check (workers alive, disk space, memory)

**Not Yet Implemented**:
- `/retry <task_id>` - Retry failed download task
- `/cancel <task_id>` - Cancel pending task
- `/priority <task_id>` - Increase task priority
- `/round <round_id>` - Show detailed status of a specific round

To implement these, modify `telegram_receiver.go` functions starting at line 160.

## Database Schema

### download_queue (migration 001)
Persistent queue for file downloads.
- **Statuses**: `PENDING` → `DOWNLOADING` → `DOWNLOADED` → (assigned to round) → `COMPLETED/FAILED`
- **Key fields**: `task_id`, `file_id`, `filename`, `file_type`, `file_size`, `sha256_hash`, `round_id`
- **Indexes**: `idx_dq_status`, `idx_dq_priority`, `idx_dq_round`

### processing_rounds (migration 005)
Tracks round lifecycle through pipeline stages.
- **Round Status**: `CREATED` → `EXTRACTING` → `CONVERTING` → `STORING` → `COMPLETED/FAILED`
- **Stage Statuses**:
  - `extract_status`: `PENDING` → `EXTRACTING` → `COMPLETED/SKIPPED/FAILED`
  - `convert_status`: `PENDING` → `CONVERTING` → `COMPLETED/FAILED`
  - `store_status`: `PENDING` → `STORING` → `COMPLETED/FAILED`
- **Key fields**: `round_id`, `file_count`, `archive_count`, `txt_count`, worker IDs, duration fields
- **Indexes**: `idx_rounds_extract_status`, `idx_rounds_convert_status`, `idx_rounds_store_status`, `idx_rounds_round_status`

### store_tasks (migration 005)
Individual store tasks (2 files each) for parallel processing.
- **Statuses**: `PENDING` → `STORING` → `COMPLETED/FAILED`
- **Key fields**: `task_id`, `round_id`, `file_paths` (array of 2 files), `worker_id`, `duration_sec`
- **Indexes**: `idx_store_tasks_status`, `idx_store_tasks_round`, `idx_store_tasks_worker`
- **Purpose**: Decomposes store stage into parallel 2-file tasks, allowing 5 workers to process simultaneously

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
├── round_coordinator.go     # Round creation and file routing to staging directories
├── extract_worker.go        # Extract worker (singleton - only 1)
├── convert_worker.go        # Convert worker (singleton - only 1)
├── store_worker.go          # Store worker pool (5 workers, isolated tasks)
└── crash_recovery.go        # Startup recovery and periodic health checks

database/migrations/
├── 001_create_download_queue.sql
├── 002_create_batch_processing.sql (deprecated)
├── 003_create_batch_files.sql (deprecated)
├── 004_create_metrics.sql
└── 005_create_pipelined_architecture.sql  # New round-based tables

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

### Debugging a Failed Round

1. **Find round ID** from database or logs:
   ```sql
   SELECT round_id, round_status, extract_status, convert_status, store_status, error_message
   FROM processing_rounds
   WHERE round_status = 'FAILED'
   ORDER BY created_at DESC;
   ```

2. **Check coordinator logs**:
   ```bash
   tail -100 logs/coordinator.log | grep "round_XXX"
   ```

3. **Check which stage failed**:
   ```sql
   SELECT extract_duration_sec, convert_duration_sec, store_duration_sec, error_message
   FROM processing_rounds
   WHERE round_id = 'round_XXX';
   ```

4. **Check store tasks for the round** (if store stage failed):
   ```sql
   SELECT task_id, status, error_message, started_at
   FROM store_tasks
   WHERE round_id = 'round_XXX' AND status = 'FAILED';
   ```

5. **Inspect failed files**:
   ```bash
   ls -lh app/extraction/files/errors/
   ls -lh app/extraction/files/nopass/
   ```

### Scaling to More Concurrent Store Workers

1. **Update `.env`**: Increase `MAX_STORE_WORKERS` (e.g., from 5 to 10)
2. **Restart coordinator**: `pkill telegram-bot-coordinator && ./telegram-bot-coordinator`
3. **Monitor resources**:
   - Need ~1GB RAM + 1 CPU core per store worker
   - Check with: `curl localhost:8080/health | jq '.components.filesystem'`
4. **Monitor round throughput**:
   ```sql
   SELECT COUNT(*) as completed_last_hour
   FROM processing_rounds
   WHERE round_status = 'COMPLETED'
   AND store_completed_at > NOW() - INTERVAL '1 hour';
   ```
5. **Adjust `ROUND_SIZE` if needed**: Larger rounds (e.g., 75) = better throughput, slower feedback

### Recovering from Crashes

The system has automatic recovery built in (`crash_recovery.go`):

1. **On startup**, coordinator automatically:
   - Resets downloads stuck in `DOWNLOADING` for > 30 minutes → back to `PENDING`
   - Resets rounds stuck in `EXTRACTING` for > 2 hours → back to `CREATED` (extract_status = PENDING)
   - Resets rounds stuck in `CONVERTING` for > 2 hours → back to `EXTRACTING` (convert_status = PENDING)
   - Resets store tasks stuck in `STORING` for > 2 hours → back to `PENDING`

2. **Periodic health checks** (every 5 minutes):
   - Detects stuck downloads, rounds, and store tasks
   - Attempts automatic recovery
   - Logs warnings for investigation

3. **Manual recovery** (if needed):
   ```sql
   -- Reset all stuck downloads
   UPDATE download_queue SET status = 'PENDING', started_at = NULL
   WHERE status = 'DOWNLOADING';

   -- Reset stuck extract rounds
   UPDATE processing_rounds
   SET extract_status = 'PENDING', extract_worker_id = NULL,
       extract_started_at = NULL, round_status = 'CREATED'
   WHERE extract_status = 'EXTRACTING';

   -- Reset stuck convert rounds
   UPDATE processing_rounds
   SET convert_status = 'PENDING', convert_worker_id = NULL,
       convert_started_at = NULL, round_status = 'EXTRACTING'
   WHERE convert_status = 'CONVERTING';

   -- Reset stuck store tasks
   UPDATE store_tasks
   SET status = 'PENDING', worker_id = NULL, started_at = NULL
   WHERE status = 'STORING';
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
