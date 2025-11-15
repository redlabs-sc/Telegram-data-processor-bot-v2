# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a **Telegram Data Processor Bot** implementing **Option 2: Batch-Based Parallel Processing System**. The architecture achieves 6× faster processing compared to sequential processing by distributing files into isolated batch directories and running multiple extract/convert/store pipelines in parallel.

**Critical Design Principle**: Three core processing files (`extract.go`, `convert.go`, `store.go`) are **100% preserved** - never modify them. All parallelism is achieved through batch directory isolation and working directory changes.

## Architecture

### Core Components

1. **Download Workers (3 concurrent)**: Download files from Telegram to `downloads/` directory
2. **Batch Coordinator**: Groups downloaded files into batches of 10, creates isolated batch directories
3. **Batch Workers (5 concurrent)**: Process batches through extract → convert → store pipeline
4. **PostgreSQL Database**: Persistent queues for download tasks and batch tracking

### Batch Processing Flow

```
Telegram → Download Queue → downloads/ → Batch Creation → batches/batch_XXX/ → Process → Database
```

Each batch gets an isolated directory structure:
```
batches/batch_001/
├── app/extraction/files/
│   ├── all/          # Input archives
│   ├── pass/         # Extracted files
│   ├── txt/          # Converted text + direct TXT files
│   ├── nopass/       # Password-protected archives
│   └── errors/       # Failed extractions
├── logs/
│   ├── extract.log
│   ├── convert.log
│   └── store.log
└── pass.txt          # Password list (copied)
```

### Code Preservation Strategy

The existing extraction code processes files in fixed relative paths (`app/extraction/files/all/`, etc.). Instead of modifying this code, we:

1. Create the same directory structure inside each batch directory
2. Change the working directory to the batch root before calling the processing functions
3. The unchanged code operates on its expected relative paths, which now resolve to the batch-specific directories

**Example**:
```go
// In batch worker
originalDir, _ := os.Getwd()
os.Chdir("batches/batch_001/")  // Change to batch root

// extract.go uses "app/extraction/files/all" which now resolves to:
// batches/batch_001/app/extraction/files/all
extract.ExtractArchives()

os.Chdir(originalDir)  // Always restore
```

## Development Commands

### Database Setup

```bash
# Start PostgreSQL (if using Docker)
docker-compose up -d postgres

# Run database migrations
psql -U bot_user -d telegram_bot_option2 -f database/migrations/001_create_download_queue.sql
psql -U bot_user -d telegram_bot_option2 -f database/migrations/002_create_batch_processing.sql
psql -U bot_user -d telegram_bot_option2 -f database/migrations/003_create_batch_files.sql
psql -U bot_user -d telegram_bot_option2 -f database/migrations/004_create_metrics.sql
```

### Running the Application

```bash
# Development mode (with live reload if configured)
cd coordinator && go run .

# Build production binary
cd coordinator && go build -o telegram-bot-coordinator

# Run with specific log level
LOG_LEVEL=debug go run .
```

### Testing

```bash
# Run all tests
go test ./...

# Run with coverage
go test -cover ./coordinator/...

# Run specific test suite
go test -v ./tests/integration/...

# Load testing (100 files)
go test -v -timeout 3h ./tests/load -run TestLoad100Files
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
```

### Monitoring

```bash
# Check health endpoint
curl http://localhost:8080/health | jq .

# View Prometheus metrics
curl http://localhost:9090/metrics

# View coordinator logs
tail -f logs/coordinator.log

# View specific batch logs
tail -f batches/batch_001/logs/extract.log
```

## Configuration

All configuration is managed via environment variables in `.env`:

**Critical Settings**:
- `MAX_DOWNLOAD_WORKERS=3` - Fixed at 3 (Telegram API rate limit)
- `MAX_BATCH_WORKERS=5` - Maximum concurrent batches (configurable for scaling)
- `BATCH_SIZE=10` - Files per batch
- `BATCH_TIMEOUT_SEC=300` - Create batch if files wait > 5 minutes

**Database** (PostgreSQL required):
- `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD`
- `DB_SSL_MODE=require` in production, `disable` for local development

**Telegram Bot**:
- `TELEGRAM_BOT_TOKEN` - Bot API token
- `ADMIN_IDS` - Comma-separated user IDs (only these users can upload files)
- `USE_LOCAL_BOT_API=true` - Required for files > 20MB
- `LOCAL_BOT_API_URL` - Local Bot API Server endpoint

## Critical Implementation Rules

### 1. Code Preservation (ABSOLUTE REQUIREMENT)

**NEVER modify these files**:
- `app/extraction/extract/extract.go`
- `app/extraction/convert/convert.go`
- `app/extraction/store.go`

**Verification**: Run `sha256sum -c checksums.txt` after ANY changes. If checksums don't match, revert immediately.

### 2. Worker Concurrency Limits

**Fixed limits (do not change)**:
- Download workers: Exactly 3 (Telegram API constraint)
- Batch workers: Maximum 5 (tested configuration, can increase for scaling)

**Reason**: Extract and convert process entire directories. Multiple workers on the same directory cause race conditions and data corruption.

### 3. Database Locking Pattern

Use `FOR UPDATE SKIP LOCKED` for concurrent worker access:

```sql
SELECT * FROM download_queue
WHERE status='PENDING'
ORDER BY priority DESC, created_at ASC
LIMIT 1
FOR UPDATE SKIP LOCKED
```

This prevents duplicate processing without blocking other workers.

### 4. Working Directory Pattern

Always restore the original working directory:

```go
originalDir, _ := os.Getwd()
defer os.Chdir(originalDir)  // Use defer to ensure restoration

os.Chdir(batchDir)
// ... execute batch processing
```

### 5. Batch Cleanup

- Completed batches: Delete within 1 hour (free disk space)
- Failed batches: Retain 7 days in `archive/failed/` for debugging
- Monitor disk space: Alert if < 50GB free

## Admin Telegram Commands

When bot is running, admins can send:

- `/queue` - Show queue statistics (pending, downloading, downloaded, failed)
- `/batches` - List active batches with status and progress
- `/stats` - Overall system statistics (throughput, success rate, avg time)
- `/health` - System health check (workers alive, disk space, memory)
- `/retry <task_id>` - Retry failed download task
- `/cancel <task_id>` - Cancel pending task
- `/priority <task_id>` - Increase task priority

## Documentation Structure

Critical design documents in `Docs/`:

- **`prd.md`**: Product Requirements Document - complete functional and non-functional requirements
- **`batch-parallel-design.md`**: Detailed architecture design with batch isolation pattern
- **`implementation-plan.md`**: Phase 1-3 implementation (Foundation, Download Pipeline, Batch Coordinator)
- **`implementation-plan-part2.md`**: Phase 4-7 implementation (Batch Workers, Admin Commands, Testing, Deployment)

**Implementation order**: Read PRD → Design → Implementation Plan Part 1 → Implementation Plan Part 2

## Performance Targets

| Metric | Target |
|--------|--------|
| Processing Time (100 files) | < 2 hours |
| Throughput | ≥ 50 files/hour sustained |
| Success Rate | ≥ 98% |
| RAM Usage | < 20% |
| CPU Usage | < 50% |
| Concurrent Batches | Exactly 5 max |

## Database Schema

### download_queue
Persistent queue for file downloads. Statuses: `PENDING`, `DOWNLOADING`, `DOWNLOADED`, `FAILED`

### batch_processing
Tracks batch lifecycle. Statuses: `QUEUED`, `EXTRACTING`, `CONVERTING`, `STORING`, `COMPLETED`, `FAILED`

### batch_files
Join table linking files to batches with per-file processing status.

### processing_metrics
Time-series metrics for analytics (download speed, stage durations, etc.)

## Common Workflows

### Adding a New Feature

1. Check if it requires modifying preserved files (`extract.go`, `convert.go`, `store.go`)
2. If yes: Use configuration or environment variables instead
3. If no: Implement in coordinator components
4. Add tests in `tests/unit/` or `tests/integration/`
5. Verify checksums: `sha256sum -c checksums.txt`
6. Update relevant documentation

### Debugging a Failed Batch

1. Find batch ID from `/batches` command or database
2. Check batch logs: `tail -f batches/batch_XXX/logs/*.log`
3. Check batch metadata: `cat batches/batch_XXX/status.json`
4. Query database: `SELECT * FROM batch_processing WHERE batch_id='batch_XXX'`
5. Inspect failed files in `batches/batch_XXX/app/extraction/files/errors/`

### Scaling to More Concurrent Batches

1. Update `.env`: Increase `MAX_BATCH_WORKERS` (e.g., from 5 to 10)
2. Verify hardware resources: Need ~2GB RAM + 2 CPU cores per batch
3. Monitor performance after change
4. Adjust `BATCH_SIZE` if needed (smaller batches = faster feedback)

## Troubleshooting

**Issue: Checksums don't match**
- **Cause**: Preserved files were modified
- **Fix**: `git checkout app/extraction/extract/extract.go app/extraction/convert/convert.go app/extraction/store.go`

**Issue: "No space left on device"**
- **Cause**: Completed batches not cleaned up
- **Fix**: Check `COMPLETED_BATCH_RETENTION_HOURS` in `.env`, manually delete old batches: `rm -rf batches/batch_0*`

**Issue: Downloads stuck at 3 concurrent**
- **Cause**: This is expected behavior (Telegram API rate limit)
- **Action**: Do not increase `MAX_DOWNLOAD_WORKERS` beyond 3

**Issue: Batch processing takes > 2 hours**
- **Check**: Are all 5 batch workers active? Query: `SELECT COUNT(*) FROM batch_processing WHERE status IN ('EXTRACTING','CONVERTING','STORING')`
- **Check**: Database query performance, add indexes if needed
- **Check**: Disk I/O bottleneck, consider SSD storage

## Development Notes

- **Go version**: 1.21+ required
- **PostgreSQL version**: 14+ required
- **Dependencies**: Managed via `go.mod` (when implemented)
- **Logging**: Structured JSON logs using `go.uber.org/zap`
- **Metrics**: Prometheus format exposed on port 9090

## Testing Before Deployment

1. **Code Preservation Test**: `sha256sum -c checksums.txt` must pass
2. **Unit Tests**: `go test ./coordinator/... -cover` - target > 80% coverage
3. **Integration Test**: Upload 10 test files, verify all complete successfully
4. **Load Test**: `go test -v ./tests/load -run TestLoad100Files` - must complete < 2 hours
5. **Crash Recovery**: Kill process mid-batch, restart, verify tasks resume correctly

## Git Workflow

Working on branch: `claude/init-codebase-analysis-01YLvkd52eZpCbs8Z4wKUNAG`

When making commits:
```bash
# Add specific files
git add <files>

# Commit with descriptive message
git commit -m "feat: implement batch coordinator logic"

# Push to designated branch
git push -u origin claude/init-codebase-analysis-01YLvkd52eZpCbs8Z4wKUNAG
```

**Never commit**:
- `.env` files (use `.env.example` for templates)
- `batches/` directory contents
- `downloads/` directory contents
- `logs/` directory contents
- Compiled binaries
