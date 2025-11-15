# Product Requirements Document (PRD)
## Telegram Data Processor Bot - Option 2: Batch-Based Parallel Processing System

**Version**: 1.0
**Date**: 2025-11-13
**Status**: Design Phase
**Target Implementation**: Future Release

---

## Executive Summary

This PRD defines the requirements for **Option 2: Batch-Based Parallel Processing System**, a high-performance architecture for the Telegram Data Processor Bot that achieves 6× faster processing compared to the current sequential approach while preserving 100% of the existing extraction, conversion, and storage code.

### Key Objectives
- Process 1000+ files continuously without downtime
- Reduce processing time from 4.4 hours to 1.7 hours for 100 files (6× speedup)
- Maintain zero code changes to extract.go, convert.go, and store.go
- Support 10MB-4GB files with Telegram Premium compatibility
- Achieve 99.9% uptime with comprehensive crash recovery

### Success Metrics
- **Throughput**: Process 100 files in < 2 hours (vs 4.4 hours baseline)
- **Concurrency**: Handle 5 concurrent batches processing 50 files simultaneously
- **Reliability**: Zero data loss with transaction logging and persistent queues
- **User Experience**: Batch notifications within 2 hours of upload
- **Resource Efficiency**: < 20% RAM usage, < 50% CPU utilization

---

## 1. Business Context

### 1.1 Problem Statement

The current Telegram Data Processor Bot processes files sequentially:
- **Download**: 3 concurrent downloads (36 min for 100 files)
- **Extract**: Single-threaded processing of ALL files (60 min)
- **Convert**: Single-threaded processing of ALL files (60 min)
- **Store**: Batch processing in groups of 2 (86 min)
- **Total**: 4.4 hours for 100 archive files

**Critical Limitations**:
1. Extract and convert process entire directories, cannot run multiple instances on same directory
2. Users uploading 1000+ files face unacceptable wait times (40+ hours)
3. Sequential bottleneck prevents scaling
4. Store pipeline blocks user notifications

### 1.2 Solution Overview

Option 2 implements **batch-based parallelism** by creating isolated workspace directories:
- Divide files into batches of 10
- Create isolated directories for each batch (`batch_001/`, `batch_002/`, etc.)
- Run up to 5 concurrent batches simultaneously
- Preserve 100% of extract.go, convert.go, store.go code by changing working directory

**Architecture Innovation**: Instead of modifying the directory-processing code, we replicate the directory structure inside batch folders and change the working directory before calling the unchanged functions.

### 1.3 Business Impact

**For Users**:
- 6× faster processing (1.7 hours vs 4.4 hours for 100 files)
- Continuous file uploads without queue buildup
- Batch progress notifications
- Better failure isolation (one batch failure doesn't affect others)

**For Operations**:
- Horizontal scalability (add more batch workers)
- Easier debugging (isolated batch logs)
- Better resource utilization
- Simplified monitoring (per-batch metrics)

---

## 2. Functional Requirements

### 2.1 Core Components

#### FR-1: Telegram Receiver (Admin-Only)
**Priority**: P0 (Critical)

**Requirements**:
- Accept files from admin users only (check against `ADMIN_IDS` config)
- Support file types: ZIP, RAR, TXT (10MB-4GB)
- Validate file size ≤ 4GB (Telegram Premium limit)
- Create task record in PostgreSQL with status `PENDING`
- Return immediate confirmation to user
- No artificial restrictions on number of files accepted

**Acceptance Criteria**:
- ✅ Non-admin users receive "Unauthorized" message
- ✅ Files > 4GB are rejected with clear error message
- ✅ Unsupported file types receive "Invalid file type" message
- ✅ Admin receives "File queued for processing: {filename}" confirmation
- ✅ Task record created with: task_id, file_id, user_id, filename, file_type, file_size, status, created_at

#### FR-2: Download Queue (PostgreSQL-Backed)
**Priority**: P0 (Critical)

**Requirements**:
- Store pending downloads in PostgreSQL table
- Support FIFO queue ordering (oldest first)
- Handle concurrent access from 3 download workers
- Implement optimistic locking to prevent duplicate downloads
- Track download attempts and errors
- Support priority override for admin-flagged tasks

**Database Schema**:
```sql
CREATE TABLE download_queue (
    task_id BIGSERIAL PRIMARY KEY,
    file_id VARCHAR(255) NOT NULL,
    user_id BIGINT NOT NULL,
    filename TEXT NOT NULL,
    file_type VARCHAR(10) NOT NULL, -- 'ZIP', 'RAR', 'TXT'
    file_size BIGINT NOT NULL,
    status VARCHAR(20) NOT NULL, -- 'PENDING', 'DOWNLOADING', 'DOWNLOADED', 'FAILED'
    download_attempts INT DEFAULT 0,
    last_error TEXT,
    priority INT DEFAULT 0, -- Higher = process first
    created_at TIMESTAMP DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    batch_id VARCHAR(50) -- Assigned when added to batch
);

CREATE INDEX idx_download_queue_status ON download_queue(status);
CREATE INDEX idx_download_queue_priority ON download_queue(priority DESC, created_at ASC);
```

**Acceptance Criteria**:
- ✅ Queue persists across bot restarts
- ✅ Exactly 3 concurrent downloads (never more)
- ✅ No duplicate downloads of same file
- ✅ Failed downloads retry with exponential backoff
- ✅ Admin can query queue size: `/queue` command

#### FR-3: Download Workers (3 Concurrent)
**Priority**: P0 (Critical)

**Requirements**:
- Run exactly 3 concurrent download workers
- Poll download_queue for `status='PENDING'` tasks
- Update status to `DOWNLOADING` with optimistic lock
- Download file using Telegram Bot API or Local Bot API
- Compute SHA256 hash for deduplication
- Store file to temporary location: `downloads/{task_id}_{filename}`
- Update status to `DOWNLOADED` on success
- Update status to `FAILED` with error message on failure
- Respect Telegram rate limits: 20 requests/minute per chat

**Download Flow**:
1. Query: `SELECT * FROM download_queue WHERE status='PENDING' ORDER BY priority DESC, created_at ASC LIMIT 1 FOR UPDATE SKIP LOCKED`
2. Update: `UPDATE download_queue SET status='DOWNLOADING', started_at=NOW() WHERE task_id={id}`
3. Download file from Telegram
4. Compute hash: `sha256sum {filename}`
5. Move to: `downloads/{task_id}_{filename}`
6. Update: `UPDATE download_queue SET status='DOWNLOADED', completed_at=NOW() WHERE task_id={id}`

**Error Handling**:
- Network errors: Retry 3 times with exponential backoff (2s, 4s, 8s)
- File too large: Mark as FAILED, notify admin
- Invalid file_id: Mark as FAILED, notify admin
- Rate limit exceeded: Wait and retry

**Acceptance Criteria**:
- ✅ Exactly 3 files download concurrently
- ✅ Downloads resume after bot restart
- ✅ SHA256 hash stored in database
- ✅ Failed downloads logged with error details
- ✅ Download speed monitored (MB/s)

#### FR-4: Batch Coordinator
**Priority**: P0 (Critical)

**Requirements**:
- Monitor `download_queue` for `status='DOWNLOADED'` files
- Create batches of 10 files each when threshold reached
- Support configurable batch sizes (default: 10)
- Create isolated batch directories with internal structure
- Assign batch_id to tasks in download_queue
- Route files to appropriate batch directories based on type
- Track active batches in PostgreSQL
- Limit concurrent batches to 5 (configurable)
- Start batch processing when batch is full or timeout reached

**Batch Creation Logic**:
```python
# Pseudocode
while True:
    downloaded_files = query("SELECT * FROM download_queue WHERE status='DOWNLOADED' AND batch_id IS NULL LIMIT 10")

    if len(downloaded_files) >= 10 OR (len(downloaded_files) > 0 AND oldest_file_age > 5_minutes):
        batch_id = generate_batch_id()  # e.g. "batch_001"
        create_batch_directory(batch_id)

        for file in downloaded_files:
            if file.file_type in ['ZIP', 'RAR']:
                move_to_path = f"batches/{batch_id}/app/extraction/files/all/{file.filename}"
            elif file.file_type == 'TXT':
                move_to_path = f"batches/{batch_id}/app/extraction/files/txt/{file.filename}"

            move_file(f"downloads/{file.task_id}_{file.filename}", move_to_path)
            update("UPDATE download_queue SET batch_id='{batch_id}' WHERE task_id={file.task_id}")

        insert_batch_record(batch_id, file_count=len(downloaded_files))
        trigger_batch_processing(batch_id)

    sleep(30)  # Check every 30 seconds
```

**Batch Directory Structure**:
```
batches/
├── batch_001/
│   ├── app/
│   │   └── extraction/
│   │       ├── files/
│   │       │   ├── all/          # Archives for extraction
│   │       │   ├── txt/          # TXT files (skip extraction)
│   │       │   ├── pass/         # Extracted files
│   │       │   ├── nopass/       # Password-protected
│   │       │   └── errors/       # Extraction errors
│   │       └── pass.txt          # Password list (copied)
│   ├── logs/
│   │   ├── extract.log
│   │   ├── convert.log
│   │   └── store.log
│   └── status.json               # Batch metadata
├── batch_002/
│   └── [same structure]
└── batch_003/
    └── [same structure]
```

**Batch Record Schema**:
```sql
CREATE TABLE batch_processing (
    batch_id VARCHAR(50) PRIMARY KEY,
    file_count INT NOT NULL,
    archive_count INT DEFAULT 0,
    txt_count INT DEFAULT 0,
    status VARCHAR(20) NOT NULL, -- 'QUEUED', 'EXTRACTING', 'CONVERTING', 'STORING', 'COMPLETED', 'FAILED'
    worker_id VARCHAR(50), -- Which worker is processing
    created_at TIMESTAMP DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    extract_duration_sec INT,
    convert_duration_sec INT,
    store_duration_sec INT,
    error_message TEXT
);

CREATE INDEX idx_batch_status ON batch_processing(status);
```

**Acceptance Criteria**:
- ✅ Batches created when 10 files downloaded OR 5 minutes elapsed
- ✅ Batch directories isolated (no cross-contamination)
- ✅ Maximum 5 concurrent batches enforced
- ✅ Files routed correctly (archives → `all/`, TXT → `txt/`)
- ✅ Batch metadata tracked in database

#### FR-5: Batch Processing Workers (5 Concurrent)
**Priority**: P0 (Critical)

**Requirements**:
- Run up to 5 concurrent batch workers
- Poll `batch_processing` table for `status='QUEUED'` batches
- Claim batch with optimistic lock
- Execute sequential pipeline for each batch:
  1. **Extract Stage** (archives only)
  2. **Convert Stage** (uses extract output)
  3. **Store Stage** (uses convert output + direct TXT files)
- Change working directory to batch folder before calling extract/convert/store
- Preserve 100% of extract.go, convert.go, store.go code (zero changes)
- Log each stage to batch-specific log files
- Update batch status after each stage
- Notify admin when batch completes
- Clean up batch directory after successful completion

**Processing Pipeline Pseudocode**:
```python
def process_batch(batch_id):
    # 1. Claim batch
    update("UPDATE batch_processing SET status='EXTRACTING', worker_id={worker_id}, started_at=NOW() WHERE batch_id='{batch_id}'")

    # 2. Change working directory
    os.chdir(f"/path/to/batches/{batch_id}")

    # 3. Extract Stage (archives only)
    start_time = time.now()
    try:
        # Call unchanged extract.go - it processes app/extraction/files/all/
        result = subprocess.run(["go", "run", "app/extraction/extract/extract.go"],
                                capture_output=True,
                                timeout=30*60)  # 30 min timeout

        log_to_file(f"logs/extract.log", result.stdout)

        if result.returncode != 0:
            raise Exception(f"Extract failed: {result.stderr}")

        extract_duration = time.now() - start_time
        update("UPDATE batch_processing SET extract_duration_sec={duration} WHERE batch_id='{batch_id}'")

    except Exception as e:
        update("UPDATE batch_processing SET status='FAILED', error_message='{e}' WHERE batch_id='{batch_id}'")
        notify_admin(f"Batch {batch_id} extraction failed: {e}")
        return

    # 4. Convert Stage
    update("UPDATE batch_processing SET status='CONVERTING' WHERE batch_id='{batch_id}'")
    start_time = time.now()

    try:
        # Set environment variables for convert.go
        env = os.environ.copy()
        env["CONVERT_INPUT_DIR"] = "app/extraction/files/pass"
        env["CONVERT_OUTPUT_FILE"] = "app/extraction/files/all_extracted.txt"

        result = subprocess.run(["go", "run", "app/extraction/convert/convert.go"],
                                env=env,
                                capture_output=True,
                                timeout=30*60)

        log_to_file(f"logs/convert.log", result.stdout)

        if result.returncode != 0:
            raise Exception(f"Convert failed: {result.stderr}")

        convert_duration = time.now() - start_time
        update("UPDATE batch_processing SET convert_duration_sec={duration} WHERE batch_id='{batch_id}'")

    except Exception as e:
        update("UPDATE batch_processing SET status='FAILED', error_message='{e}' WHERE batch_id='{batch_id}'")
        notify_admin(f"Batch {batch_id} conversion failed: {e}")
        return

    # 5. Store Stage (TXT files + converted output)
    update("UPDATE batch_processing SET status='STORING' WHERE batch_id='{batch_id}'")
    start_time = time.now()

    try:
        # Call store.go - processes app/extraction/files/txt/ and converted files
        result = subprocess.run(["go", "run", "app/extraction/store.go"],
                                capture_output=True,
                                timeout=60*60)  # 60 min timeout

        log_to_file(f"logs/store.log", result.stdout)

        if result.returncode != 0:
            raise Exception(f"Store failed: {result.stderr}")

        store_duration = time.now() - start_time
        update("UPDATE batch_processing SET store_duration_sec={duration}, status='COMPLETED', completed_at=NOW() WHERE batch_id='{batch_id}'")

    except Exception as e:
        update("UPDATE batch_processing SET status='FAILED', error_message='{e}' WHERE batch_id='{batch_id}'")
        notify_admin(f"Batch {batch_id} storage failed: {e}")
        return

    # 6. Notify admin
    task_ids = query("SELECT task_id FROM download_queue WHERE batch_id='{batch_id}'")
    notify_admin(f"✅ Batch {batch_id} completed: {len(task_ids)} files processed")

    # 7. Cleanup
    os.chdir("/path/to/project/root")
    shutil.rmtree(f"batches/{batch_id}")
```

**Code Preservation Strategy**:
- **extract.go**: Zero changes. Processes `app/extraction/files/all/` relative to working directory
- **convert.go**: Zero changes. Reads `CONVERT_INPUT_DIR` from environment variables
- **store.go**: Zero changes. Processes configured input directories

**Acceptance Criteria**:
- ✅ Maximum 5 batches process concurrently
- ✅ Extract, convert, store run sequentially within each batch
- ✅ Working directory changes before calling each function
- ✅ Zero modifications to extract.go, convert.go, store.go
- ✅ Each batch logs independently
- ✅ Failed batches don't crash the worker
- ✅ Admin notified on batch completion

#### FR-6: Admin Commands
**Priority**: P1 (High)

**Requirements**:
- `/start` - Welcome message with instructions
- `/help` - List all available commands
- `/queue` - Show queue statistics (pending, downloading, downloaded, failed)
- `/batches` - List active batches with status
- `/stats` - Overall system statistics (total processed, success rate, avg time)
- `/retry <task_id>` - Retry failed task
- `/cancel <task_id>` - Cancel pending task
- `/priority <task_id>` - Increase task priority
- `/health` - System health check (workers alive, disk space, memory)
- `/logs <batch_id>` - Retrieve logs for specific batch

**Example Outputs**:

`/queue`:
```
📊 Queue Status:
• Pending: 47 files
• Downloading: 3 files
• Downloaded: 12 files (waiting for batch)
• Failed: 2 files

Next batch in: 2 minutes or when 10 files ready
```

`/batches`:
```
🔄 Active Batches:
• batch_042: EXTRACTING (5/10 files, 12 min elapsed)
• batch_043: CONVERTING (10/10 files, 8 min elapsed)
• batch_044: STORING (10/10 files, 23 min elapsed)

Recently Completed:
• batch_041: ✅ COMPLETED (10 files, 45 min total)
• batch_040: ✅ COMPLETED (10 files, 38 min total)
```

`/stats`:
```
📈 System Statistics (Last 24 Hours):
• Total Processed: 1,247 files
• Success Rate: 98.3%
• Avg Processing Time: 42 minutes/batch
• Throughput: 52 files/hour

Current Load:
• Download Workers: 3/3 active
• Batch Workers: 5/5 active
• Queue Size: 47 pending
```

**Acceptance Criteria**:
- ✅ All commands admin-only
- ✅ Real-time statistics from PostgreSQL
- ✅ Clear, formatted output
- ✅ Error handling for invalid inputs

---

## 3. Non-Functional Requirements

### 3.1 Performance

#### NFR-1: Processing Speed
**Requirement**: Process 100 files in < 2 hours

**Baseline** (Sequential):
- Download: 36 min (3 concurrent)
- Extract: 60 min (single-threaded)
- Convert: 60 min (single-threaded)
- Store: 86 min (batch of 2)
- **Total**: 4.4 hours

**Target** (Batch Parallel):
- Download: 36 min (3 concurrent, unchanged)
- Batches: 10 batches of 10 files
  - 5 concurrent batches
  - Each batch: 12 min extract + 12 min convert + 17 min store = 41 min
  - First 5 batches: 41 min
  - Second 5 batches: 41 min
  - **Total**: 36 min + 82 min = 118 min = **1.97 hours**

**Measurement**:
- Track per-batch processing times in `batch_processing.extract_duration_sec`, `convert_duration_sec`, `store_duration_sec`
- Report P50, P95, P99 latencies
- Monitor throughput: files/hour

**Acceptance Criteria**:
- ✅ P50 latency for 100 files < 2 hours
- ✅ Throughput ≥ 50 files/hour sustained

#### NFR-2: Resource Utilization
**Requirement**: Maintain < 20% RAM and < 50% CPU usage

**Constraints**:
- Each batch worker may process 4GB files
- 5 concurrent workers = potential 20GB RAM if all process max files
- Must use streaming processing (never load full file into memory)

**Monitoring**:
- Track system RAM usage every 10 seconds
- Alert if RAM > 20% or CPU > 50% for > 5 minutes
- Auto-throttle batch workers if resources exceeded

**Acceptance Criteria**:
- ✅ RAM usage < 20% during normal operation
- ✅ CPU usage < 50% sustained
- ✅ Streaming processing confirmed (no files loaded entirely)

#### NFR-3: Concurrency Limits
**Requirement**: Enforce strict concurrency limits

**Limits**:
- Download workers: Exactly 3
- Batch workers: Maximum 5
- Extract per batch: 1 (sequential within batch)
- Convert per batch: 1 (sequential within batch)
- Store per batch: 1 (sequential within batch)

**Enforcement**:
- Database locks (FOR UPDATE SKIP LOCKED)
- Worker pool size limits
- Semaphore-based concurrency control

**Acceptance Criteria**:
- ✅ Never exceed concurrency limits
- ✅ Graceful queueing when limits reached
- ✅ No race conditions or deadlocks

### 3.2 Reliability

#### NFR-4: Zero Data Loss
**Requirement**: Guarantee no file loss during processing

**Mechanisms**:
- **Persistent Queue**: PostgreSQL-backed download_queue survives crashes
- **Transaction Logging**: All state changes logged before execution
- **Atomic Operations**: File moves use atomic rename operations
- **Checksum Verification**: SHA256 hash verified after download
- **Crash Recovery**: Resume processing after restart

**Recovery Flow**:
```python
def crash_recovery():
    # 1. Recover stuck downloads
    stuck_downloads = query("SELECT * FROM download_queue WHERE status='DOWNLOADING' AND started_at < NOW() - INTERVAL '30 minutes'")
    for task in stuck_downloads:
        update("UPDATE download_queue SET status='PENDING', download_attempts=download_attempts+1 WHERE task_id={task.task_id}")

    # 2. Recover stuck batches
    stuck_batches = query("SELECT * FROM batch_processing WHERE status IN ('EXTRACTING', 'CONVERTING', 'STORING') AND started_at < NOW() - INTERVAL '2 hours'")
    for batch in stuck_batches:
        # Reset batch to QUEUED for retry
        update("UPDATE batch_processing SET status='QUEUED', worker_id=NULL WHERE batch_id='{batch.batch_id}'")
        # Reset files in batch to DOWNLOADED
        update("UPDATE download_queue SET batch_id=NULL WHERE batch_id='{batch.batch_id}'")

    # 3. Verify file integrity
    for task in query("SELECT * FROM download_queue WHERE status='DOWNLOADED'"):
        if not verify_checksum(task.task_id, task.sha256_hash):
            update("UPDATE download_queue SET status='PENDING' WHERE task_id={task.task_id}")
```

**Acceptance Criteria**:
- ✅ Zero file loss during bot restart
- ✅ All in-progress tasks resume correctly
- ✅ Checksum mismatches trigger re-download
- ✅ Transaction log enables full audit trail

#### NFR-5: Fault Isolation
**Requirement**: Failures in one batch don't affect others

**Design**:
- Each batch runs in isolated directory
- Batch workers run in separate goroutines with panic recovery
- Database transactions scoped to single batch
- Logs separated by batch_id

**Error Handling**:
```python
def batch_worker():
    while True:
        try:
            batch = claim_next_batch()
            if batch is None:
                time.sleep(10)
                continue

            process_batch(batch.batch_id)  # May panic/crash

        except Exception as e:
            # Catch all errors - don't crash worker
            log_error(f"Batch worker error: {e}")
            if batch:
                update("UPDATE batch_processing SET status='FAILED', error_message='{e}' WHERE batch_id='{batch.batch_id}'")
            time.sleep(5)  # Brief pause before retrying
```

**Acceptance Criteria**:
- ✅ One batch failure doesn't stop other batches
- ✅ Failed batches marked in database with error details
- ✅ Workers automatically recover from panics
- ✅ Admin notified of failures

#### NFR-6: Uptime
**Requirement**: Achieve 99.9% uptime (< 9 hours downtime/year)

**Mechanisms**:
- Graceful shutdown: Wait for in-progress batches to complete
- Health monitoring: Auto-restart crashed workers
- Dependency monitoring: Check database, filesystem, Telegram API
- Circuit breakers: Prevent cascading failures

**Acceptance Criteria**:
- ✅ Graceful shutdown within 5 minutes
- ✅ Auto-recovery from worker crashes within 10 seconds
- ✅ Uptime > 99.9% over 30-day period

### 3.3 Scalability

#### NFR-7: Horizontal Scaling
**Requirement**: Support scaling to 1000+ files continuously

**Current Capacity**:
- 100 files in 2 hours = 50 files/hour
- To process 1000 files: 20 hours

**Scaling Strategy**:
- **Short-term** (< 1000 files): Increase batch workers to 10 (2× throughput)
- **Medium-term** (< 5000 files): Distribute batch workers across multiple servers
- **Long-term** (> 5000 files): Kubernetes-based auto-scaling

**Configuration**:
```env
# Adjust based on load
MAX_DOWNLOAD_WORKERS=3        # Fixed (Telegram limit)
MAX_BATCH_WORKERS=5           # Increase to 10-20 for higher throughput
BATCH_SIZE=10                 # Decrease to 5 for faster batch creation
BATCH_TIMEOUT_SEC=300         # Create batch if files wait > 5 min
```

**Acceptance Criteria**:
- ✅ Linear scaling with number of batch workers
- ✅ No bottlenecks in database or filesystem
- ✅ Configuration-based scaling (no code changes)

#### NFR-8: Storage Management
**Requirement**: Efficient storage usage for 10MB-4GB files

**Constraints**:
- 5 concurrent batches × 10 files × 4GB = 200GB worst-case storage
- Must clean up completed batches to free space
- Retain failed batches for 7 days for debugging

**Cleanup Policy**:
```python
def cleanup_completed_batches():
    # Delete batch directories for COMPLETED batches
    completed_batches = query("SELECT batch_id FROM batch_processing WHERE status='COMPLETED' AND completed_at < NOW() - INTERVAL '1 hour'")
    for batch in completed_batches:
        shutil.rmtree(f"batches/{batch.batch_id}")
        log_info(f"Cleaned up batch {batch.batch_id}")

def archive_failed_batches():
    # Move failed batches to archive for debugging
    old_failed = query("SELECT batch_id FROM batch_processing WHERE status='FAILED' AND completed_at < NOW() - INTERVAL '7 days'")
    for batch in old_failed:
        shutil.move(f"batches/{batch.batch_id}", f"archive/failed/{batch.batch_id}")
```

**Acceptance Criteria**:
- ✅ Completed batches deleted within 1 hour
- ✅ Failed batches retained 7 days
- ✅ Disk space monitored, alert if < 50GB free
- ✅ Storage usage < 200GB sustained

### 3.4 Maintainability

#### NFR-9: Code Preservation
**Requirement**: Zero changes to extract.go, convert.go, store.go

**Validation**:
- Use git to track file checksums
- Automated tests verify file hashes match original
- Code review process blocks any modifications

**Files to Preserve**:
- `app/extraction/extract/extract.go` - SHA256: [compute at implementation]
- `app/extraction/convert/convert.go` - SHA256: [compute at implementation]
- `app/extraction/store.go` - SHA256: [compute at implementation]

**Acceptance Criteria**:
- ✅ File checksums match original
- ✅ No modifications to preserved files in git history
- ✅ Automated tests fail if files modified

#### NFR-10: Observability
**Requirement**: Comprehensive logging and monitoring

**Logging Levels**:
- **DEBUG**: Detailed execution flow (batch worker steps)
- **INFO**: Normal operations (batch started, completed)
- **WARN**: Recoverable errors (retry after network timeout)
- **ERROR**: Failures requiring attention (batch failed)
- **FATAL**: System crash (database unreachable)

**Log Structure** (JSON):
```json
{
  "timestamp": "2025-11-13T10:23:45Z",
  "level": "INFO",
  "component": "batch_worker",
  "batch_id": "batch_042",
  "message": "Extract stage completed",
  "duration_sec": 720,
  "file_count": 10,
  "worker_id": "worker_003"
}
```

**Metrics to Track**:
- Queue sizes (pending, downloading, downloaded)
- Active workers (download, batch)
- Processing times (P50, P95, P99)
- Success/failure rates
- Resource usage (CPU, RAM, disk)
- Telegram API rate limit status

**Acceptance Criteria**:
- ✅ All batch operations logged with batch_id
- ✅ Logs queryable by batch_id, worker_id, timestamp
- ✅ Metrics exported to Prometheus/Grafana
- ✅ Alert rules configured for critical failures

---

## 4. Technical Specifications

### 4.1 Technology Stack

**Core**:
- **Language**: Go 1.21+
- **Bot Framework**: `github.com/go-telegram-bot-api/telegram-bot-api/v5`
- **Database**: PostgreSQL 14+ (persistent queues, task tracking)
- **Caching**: Redis 7+ (optional, for global deduplication)

**Infrastructure**:
- **Local Bot API Server**: `tdlib/telegram-bot-api` (for 4GB file support)
- **Logging**: `go.uber.org/zap` (structured JSON logging)
- **Monitoring**: Prometheus + Grafana
- **Deployment**: Docker + Docker Compose (development), Kubernetes (production)

**Preserved Components** (no dependencies changed):
- `app/extraction/extract/extract.go` - Uses stdlib only
- `app/extraction/convert/convert.go` - Uses stdlib only
- `app/extraction/store.go` - Uses database/sql

### 4.2 Database Schema

#### download_queue
```sql
CREATE TABLE download_queue (
    task_id BIGSERIAL PRIMARY KEY,
    file_id VARCHAR(255) NOT NULL,
    user_id BIGINT NOT NULL,
    filename TEXT NOT NULL,
    file_type VARCHAR(10) NOT NULL,
    file_size BIGINT NOT NULL,
    sha256_hash VARCHAR(64),
    status VARCHAR(20) NOT NULL DEFAULT 'PENDING',
    download_attempts INT DEFAULT 0,
    last_error TEXT,
    priority INT DEFAULT 0,
    batch_id VARCHAR(50),
    created_at TIMESTAMP DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP,

    CONSTRAINT fk_batch FOREIGN KEY (batch_id) REFERENCES batch_processing(batch_id) ON DELETE SET NULL
);

CREATE INDEX idx_dq_status ON download_queue(status);
CREATE INDEX idx_dq_priority ON download_queue(priority DESC, created_at ASC);
CREATE INDEX idx_dq_batch ON download_queue(batch_id);
```

#### batch_processing
```sql
CREATE TABLE batch_processing (
    batch_id VARCHAR(50) PRIMARY KEY,
    file_count INT NOT NULL,
    archive_count INT DEFAULT 0,
    txt_count INT DEFAULT 0,
    status VARCHAR(20) NOT NULL DEFAULT 'QUEUED',
    worker_id VARCHAR(50),
    created_at TIMESTAMP DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    extract_duration_sec INT,
    convert_duration_sec INT,
    store_duration_sec INT,
    error_message TEXT
);

CREATE INDEX idx_bp_status ON batch_processing(status);
CREATE INDEX idx_bp_created ON batch_processing(created_at);
```

#### batch_files (join table)
```sql
CREATE TABLE batch_files (
    batch_id VARCHAR(50) NOT NULL,
    task_id BIGINT NOT NULL,
    file_type VARCHAR(10) NOT NULL,
    processing_status VARCHAR(20) DEFAULT 'PENDING', -- PENDING, EXTRACTED, CONVERTED, STORED, FAILED
    error_message TEXT,

    PRIMARY KEY (batch_id, task_id),
    FOREIGN KEY (batch_id) REFERENCES batch_processing(batch_id) ON DELETE CASCADE,
    FOREIGN KEY (task_id) REFERENCES download_queue(task_id) ON DELETE CASCADE
);
```

#### processing_metrics (for analytics)
```sql
CREATE TABLE processing_metrics (
    metric_id BIGSERIAL PRIMARY KEY,
    batch_id VARCHAR(50),
    metric_type VARCHAR(50) NOT NULL, -- 'download_speed', 'extract_time', 'convert_time', 'store_time'
    metric_value DECIMAL(10, 2) NOT NULL,
    recorded_at TIMESTAMP DEFAULT NOW(),

    FOREIGN KEY (batch_id) REFERENCES batch_processing(batch_id) ON DELETE CASCADE
);

CREATE INDEX idx_pm_type ON processing_metrics(metric_type, recorded_at);
```

### 4.3 File System Layout

```
/home/user/telegram-bot/
├── app/
│   └── extraction/
│       ├── extract/
│       │   └── extract.go          # PRESERVED - no changes
│       ├── convert/
│       │   └── convert.go          # PRESERVED - no changes
│       ├── store.go                # PRESERVED - no changes
│       └── pass.txt                # Password list (shared)
│
├── batches/                        # NEW - batch workspaces
│   ├── batch_001/
│   │   ├── app/extraction/files/
│   │   │   ├── all/               # Input for extract
│   │   │   ├── txt/               # Direct TXT files
│   │   │   ├── pass/              # Extract output
│   │   │   ├── nopass/            # Password-protected
│   │   │   └── errors/            # Extraction errors
│   │   ├── logs/
│   │   │   ├── extract.log
│   │   │   ├── convert.log
│   │   │   └── store.log
│   │   └── status.json
│   ├── batch_002/
│   └── ...
│
├── downloads/                      # NEW - temporary download storage
│   ├── 123_archive.zip
│   ├── 124_document.txt
│   └── ...
│
├── archive/                        # NEW - failed batch archive
│   └── failed/
│       ├── batch_007/
│       └── ...
│
├── coordinator/                    # NEW - orchestration service
│   ├── main.go
│   ├── download_worker.go
│   ├── batch_coordinator.go
│   ├── batch_worker.go
│   └── config.go
│
├── database/
│   └── migrations/
│       ├── 001_create_download_queue.sql
│       ├── 002_create_batch_processing.sql
│       └── ...
│
├── logs/                           # System logs
│   ├── coordinator.log
│   ├── download_worker_1.log
│   ├── download_worker_2.log
│   ├── download_worker_3.log
│   ├── batch_worker_1.log
│   └── ...
│
├── scripts/
│   ├── setup.sh
│   ├── start.sh
│   ├── cleanup.sh
│   └── migrate.sh
│
├── .env
├── go.mod
├── go.sum
└── README.md
```

### 4.4 Configuration

**Environment Variables** (`.env`):
```bash
# Telegram Bot
TELEGRAM_BOT_TOKEN=your_token_here
ADMIN_IDS=123456789,987654321
USE_LOCAL_BOT_API=true
LOCAL_BOT_API_URL=http://localhost:8081
MAX_FILE_SIZE_MB=4096

# Database
DB_HOST=localhost
DB_PORT=5432
DB_NAME=telegram_bot
DB_USER=bot_user
DB_PASSWORD=secure_password
DB_SSL_MODE=require

# Redis (optional)
REDIS_HOST=localhost
REDIS_PORT=6379
REDIS_PASSWORD=

# Worker Configuration
MAX_DOWNLOAD_WORKERS=3             # Fixed (Telegram limit)
MAX_BATCH_WORKERS=5                # Increase for higher throughput
BATCH_SIZE=10                      # Files per batch
BATCH_TIMEOUT_SEC=300              # Create batch if files wait > 5 min

# Timeouts
DOWNLOAD_TIMEOUT_SEC=1800          # 30 minutes
EXTRACT_TIMEOUT_SEC=1800           # 30 minutes
CONVERT_TIMEOUT_SEC=1800           # 30 minutes
STORE_TIMEOUT_SEC=3600             # 60 minutes

# Resource Limits
MAX_RAM_PERCENT=20
MAX_CPU_PERCENT=50

# Logging
LOG_LEVEL=info                     # debug, info, warn, error
LOG_FORMAT=json                    # json, text
LOG_FILE=logs/coordinator.log

# Cleanup
COMPLETED_BATCH_RETENTION_HOURS=1
FAILED_BATCH_RETENTION_DAYS=7

# Monitoring
METRICS_PORT=9090
HEALTH_CHECK_PORT=8080
```

### 4.5 API Endpoints (Health/Metrics)

**Health Check** (`:8080/health`):
```json
{
  "status": "healthy",
  "timestamp": "2025-11-13T10:23:45Z",
  "components": {
    "database": "healthy",
    "telegram_api": "healthy",
    "filesystem": "healthy",
    "download_workers": {
      "status": "healthy",
      "active": 3,
      "expected": 3
    },
    "batch_workers": {
      "status": "healthy",
      "active": 5,
      "expected": 5
    }
  },
  "queue": {
    "pending": 47,
    "downloading": 3,
    "downloaded": 12,
    "failed": 2
  },
  "batches": {
    "queued": 1,
    "processing": 5,
    "completed_last_hour": 12
  }
}
```

**Metrics** (`:9090/metrics` - Prometheus format):
```
# HELP telegram_bot_queue_size Number of tasks in each queue status
# TYPE telegram_bot_queue_size gauge
telegram_bot_queue_size{status="pending"} 47
telegram_bot_queue_size{status="downloading"} 3
telegram_bot_queue_size{status="downloaded"} 12
telegram_bot_queue_size{status="failed"} 2

# HELP telegram_bot_batch_processing_duration_seconds Time to process a batch
# TYPE telegram_bot_batch_processing_duration_seconds histogram
telegram_bot_batch_processing_duration_seconds_bucket{stage="extract",le="600"} 145
telegram_bot_batch_processing_duration_seconds_bucket{stage="extract",le="1200"} 198
telegram_bot_batch_processing_duration_seconds_bucket{stage="extract",le="+Inf"} 200

# HELP telegram_bot_worker_status Worker health status (1=healthy, 0=unhealthy)
# TYPE telegram_bot_worker_status gauge
telegram_bot_worker_status{type="download",id="worker_1"} 1
telegram_bot_worker_status{type="download",id="worker_2"} 1
telegram_bot_worker_status{type="download",id="worker_3"} 1
telegram_bot_worker_status{type="batch",id="worker_1"} 1
telegram_bot_worker_status{type="batch",id="worker_2"} 1
```

---

## 5. User Stories

### 5.1 Admin User Stories

**US-1: Upload Large Archive**
```
As an admin,
I want to upload a 3GB RAR archive to the bot,
So that it can be processed and credentials extracted.

Acceptance Criteria:
- Upload archive via Telegram
- Receive immediate confirmation: "File queued for processing: huge_archive.rar (3.2GB)"
- Receive batch notification when processing completes
- Processing completes within 2 hours for 100-file batch
```

**US-2: Monitor Processing Progress**
```
As an admin,
I want to check the status of my uploaded files,
So that I know when processing will complete.

Acceptance Criteria:
- Send /queue command
- See: "47 pending, 3 downloading, 12 downloaded (batch forming)"
- Send /batches command
- See: "batch_042: EXTRACTING (12 min elapsed)"
- Understand ETA for completion
```

**US-3: Retry Failed Task**
```
As an admin,
I want to retry a task that failed due to network error,
So that the file gets processed successfully.

Acceptance Criteria:
- Receive failure notification: "Task 12345 failed: network timeout"
- Send /retry 12345
- Receive confirmation: "Task 12345 requeued for download"
- File successfully processes on retry
```

**US-4: Bulk Upload**
```
As an admin,
I want to upload 500 files continuously over 2 hours,
So that all files are queued and processed efficiently.

Acceptance Criteria:
- All 500 files accepted immediately (no throttling)
- Files processed in batches of 10
- Total processing time < 12 hours
- No file loss or corruption
- Receive batch completion notifications
```

**US-5: View System Health**
```
As an admin,
I want to check system health,
So that I know if the bot is functioning correctly.

Acceptance Criteria:
- Send /health command
- See: "Database: ✅, Workers: 3/3 download, 5/5 batch, Queue: 47 pending"
- See: "RAM: 12%, CPU: 38%, Disk: 120GB free"
- Receive alerts if any component unhealthy
```

### 5.2 System User Stories

**US-6: Crash Recovery**
```
As the system,
I want to recover gracefully from unexpected crashes,
So that no data is lost and processing resumes automatically.

Acceptance Criteria:
- Bot crashes during batch processing
- On restart, detect stuck tasks in database
- Reset stuck downloads to PENDING
- Reset stuck batches to QUEUED
- Resume processing without manual intervention
- No file loss
```

**US-7: Resource Management**
```
As the system,
I want to monitor resource usage and throttle if necessary,
So that the host server remains stable.

Acceptance Criteria:
- Monitor RAM every 10 seconds
- If RAM > 20%, pause new batch creation
- If RAM > 25%, kill oldest batch and requeue
- Resume normal operation when RAM < 15%
- Log all throttling events
```

**US-8: Parallel Batch Execution**
```
As the system,
I want to process 5 batches concurrently,
So that throughput is maximized.

Acceptance Criteria:
- When 50 downloaded files available, create 5 batches
- Assign each batch to separate worker
- Workers execute extract → convert → store independently
- No interference between batches
- All 5 batches complete successfully
```

---

## 6. Success Metrics

### 6.1 Primary Metrics

| Metric | Target | Measurement Method |
|--------|--------|-------------------|
| **Processing Speed** | < 2 hours for 100 files | End-to-end time from first upload to last completion |
| **Throughput** | ≥ 50 files/hour sustained | Files completed per hour over 24-hour period |
| **Success Rate** | ≥ 98% | (Successful tasks / Total tasks) × 100 |
| **Uptime** | ≥ 99.9% | (Total time - Downtime) / Total time × 100 |
| **Resource Usage** | RAM < 20%, CPU < 50% | Peak usage over 24-hour period |

### 6.2 Secondary Metrics

| Metric | Target | Measurement Method |
|--------|--------|-------------------|
| **Download Speed** | ≥ 10 MB/s | Average download throughput |
| **Extract Time (per file)** | < 12 min P95 | 95th percentile extraction time |
| **Convert Time (per file)** | < 12 min P95 | 95th percentile conversion time |
| **Store Time (per file)** | < 17 min P95 | 95th percentile storage time |
| **Queue Wait Time** | < 10 min | Time from upload to download start |
| **Batch Formation Time** | < 5 min | Time from 1st file to batch creation |
| **Failed Task Retry Success** | ≥ 90% | Successful retries / Total retries |

### 6.3 Quality Metrics

| Metric | Target | Measurement Method |
|--------|--------|-------------------|
| **Data Loss Rate** | 0% | Lost files / Total files uploaded |
| **Duplicate Processing** | 0% | Files processed >1 time (except retries) |
| **Code Preservation** | 100% | SHA256 hash match for extract/convert/store |
| **Log Completeness** | 100% | Batches with complete logs / Total batches |
| **Alert Response Time** | < 5 min | Time from failure to admin notification |

---

## 7. Implementation Plan

### 7.1 Development Phases

#### Phase 1: Foundation (Week 1-2)
**Goal**: Setup infrastructure and database

**Tasks**:
1. Database schema creation (download_queue, batch_processing, batch_files, processing_metrics)
2. PostgreSQL setup with connection pooling
3. Migration scripts
4. Basic logging framework (zap)
5. Configuration management (.env parsing)
6. Health check endpoint
7. Metrics endpoint (Prometheus)

**Deliverables**:
- Working PostgreSQL database with tables
- Configuration loading from .env
- Health and metrics endpoints accessible
- Logging to files and stdout

**Success Criteria**:
- ✅ All tables created successfully
- ✅ /health endpoint returns 200 OK
- ✅ /metrics endpoint returns Prometheus format
- ✅ Logs written to logs/ directory

#### Phase 2: Download Pipeline (Week 3-4)
**Goal**: Implement download queue and workers

**Tasks**:
1. Telegram receiver service (admin-only validation)
2. Download queue implementation (PostgreSQL-backed)
3. Download worker pool (3 concurrent workers)
4. File download logic (Local Bot API support)
5. SHA256 hash computation
6. Optimistic locking (FOR UPDATE SKIP LOCKED)
7. Retry logic with exponential backoff
8. Crash recovery for downloads

**Deliverables**:
- Telegram bot accepts file uploads
- Files queued in PostgreSQL
- 3 workers download concurrently
- Files stored in downloads/ with hashes
- Failed downloads retry automatically

**Success Criteria**:
- ✅ Upload 100 files, all queued immediately
- ✅ Exactly 3 concurrent downloads verified
- ✅ All files downloaded successfully
- ✅ SHA256 hashes match Telegram file_unique_id
- ✅ Bot restart resumes downloads

#### Phase 3: Batch Coordinator (Week 5-6)
**Goal**: Implement batch creation and routing

**Tasks**:
1. Batch coordinator service
2. Batch directory creation
3. File routing (archives → all/, TXT → txt/)
4. Batch record creation in database
5. Batch worker pool (5 concurrent workers)
6. Working directory management
7. Batch cleanup after completion

**Deliverables**:
- Downloaded files grouped into batches of 10
- Batch directories created with correct structure
- Files moved to appropriate subdirectories
- Batch records tracked in database
- Batch workers claim and process batches

**Success Criteria**:
- ✅ 100 files create 10 batches automatically
- ✅ Each batch has 10 files
- ✅ Archives routed to all/, TXT to txt/
- ✅ Maximum 5 batches process concurrently
- ✅ Completed batches deleted within 1 hour

#### Phase 4: Batch Processing (Week 7-8)
**Goal**: Integrate extract, convert, store into batch pipeline

**Tasks**:
1. Copy extract.go, convert.go, store.go to new project structure
2. Verify zero modifications (SHA256 hash)
3. Batch worker extract stage (subprocess execution)
4. Batch worker convert stage (environment variables)
5. Batch worker store stage (input directory configuration)
6. Stage-specific logging (extract.log, convert.log, store.log)
7. Error handling and batch failure marking
8. Admin notifications on batch completion

**Deliverables**:
- Batch workers execute extract → convert → store sequentially
- Each stage runs in batch-specific working directory
- Logs separated by batch_id
- Admins notified on completion
- Failed batches marked with error details

**Success Criteria**:
- ✅ Extract.go processes files in batch/app/extraction/files/all/
- ✅ Convert.go processes extract output
- ✅ Store.go processes convert output + TXT files
- ✅ Zero modifications to preserved files verified
- ✅ Batch completes end-to-end successfully
- ✅ Admin receives completion notification

#### Phase 5: Admin Commands (Week 9)
**Goal**: Implement admin control and monitoring commands

**Tasks**:
1. /start, /help commands
2. /queue command (real-time statistics)
3. /batches command (active batch list)
4. /stats command (system-wide analytics)
5. /retry, /cancel, /priority commands
6. /health command (component status)
7. /logs command (retrieve batch logs)

**Deliverables**:
- All admin commands functional
- Real-time statistics from PostgreSQL
- Clear, formatted command outputs
- Error handling for invalid inputs

**Success Criteria**:
- ✅ /queue shows accurate counts
- ✅ /batches shows active batches with status
- ✅ /stats shows 24-hour analytics
- ✅ /retry successfully requeues failed tasks
- ✅ /health shows all component statuses

#### Phase 6: Testing & Optimization (Week 10-11)
**Goal**: Load testing and performance tuning

**Tasks**:
1. Unit tests for each component
2. Integration tests (end-to-end pipeline)
3. Load testing (500 files, 1000 files)
4. Performance profiling (CPU, RAM)
5. Optimize slow queries
6. Tune worker pool sizes
7. Adjust batch sizes and timeouts
8. Stress testing (crash recovery)

**Deliverables**:
- Test suite with > 80% coverage
- Load test results (throughput, latency)
- Performance profiling reports
- Optimized configuration values

**Success Criteria**:
- ✅ 100 files processed in < 2 hours
- ✅ 1000 files processed in < 20 hours
- ✅ RAM usage < 20%, CPU < 50%
- ✅ Zero data loss in crash tests
- ✅ Success rate > 98%

#### Phase 7: Documentation & Deployment (Week 12)
**Goal**: Production-ready deployment

**Tasks**:
1. Update CLAUDE.md with Option 2 architecture
2. Write deployment guide (Docker Compose)
3. Create Kubernetes manifests (production)
4. Setup Grafana dashboards
5. Configure alerting rules
6. Write runbook for common issues
7. Production deployment

**Deliverables**:
- CLAUDE.md updated with Option 2 details
- Docker Compose file for development
- Kubernetes manifests for production
- Grafana dashboards configured
- Alerting rules active
- Runbook documentation

**Success Criteria**:
- ✅ Bot deployed to production
- ✅ Metrics visible in Grafana
- ✅ Alerts firing for test failures
- ✅ Team trained on runbook procedures

### 7.2 Timeline Summary

| Phase | Duration | Milestone |
|-------|----------|-----------|
| Phase 1: Foundation | Week 1-2 | Database and infrastructure ready |
| Phase 2: Download Pipeline | Week 3-4 | Files downloading and queuing |
| Phase 3: Batch Coordinator | Week 5-6 | Batches forming and routing |
| Phase 4: Batch Processing | Week 7-8 | End-to-end processing working |
| Phase 5: Admin Commands | Week 9 | Admin controls functional |
| Phase 6: Testing & Optimization | Week 10-11 | Performance targets met |
| Phase 7: Documentation & Deployment | Week 12 | Production deployment |

**Total Duration**: 12 weeks (3 months)

### 7.3 Resource Requirements

**Development Team**:
- 1 Senior Go Developer (full-time, 12 weeks)
- 1 DevOps Engineer (part-time, weeks 7-12)
- 1 QA Engineer (part-time, weeks 10-12)

**Infrastructure**:
- Development: 4 vCPU, 16GB RAM, 500GB SSD
- Staging: 8 vCPU, 32GB RAM, 1TB SSD
- Production: 16 vCPU, 64GB RAM, 2TB SSD
- PostgreSQL: Managed instance (AWS RDS or equivalent)
- Monitoring: Prometheus + Grafana (can be shared)

**External Services**:
- Telegram Local Bot API Server
- (Optional) Redis for global deduplication

---

## 8. Risk Assessment

### 8.1 Technical Risks

| Risk | Probability | Impact | Mitigation |
|------|-------------|--------|------------|
| **Extract/convert subprocess failures** | Medium | High | Circuit breakers, graceful degradation, automatic retries |
| **Database connection pool exhaustion** | Low | High | Connection pooling configuration, monitoring, auto-scaling |
| **Disk space exhaustion** | Medium | Critical | Proactive cleanup, disk space monitoring, alerts at 80% |
| **PostgreSQL performance bottleneck** | Low | Medium | Indexing, query optimization, connection pooling |
| **Batch worker deadlock** | Low | High | Timeout enforcement, deadlock detection, auto-recovery |
| **File corruption during processing** | Low | Critical | SHA256 verification, atomic operations, transaction logging |

### 8.2 Operational Risks

| Risk | Probability | Impact | Mitigation |
|------|-------------|--------|------------|
| **Telegram API rate limit exceeded** | Medium | High | Respect 3 concurrent limit, exponential backoff, circuit breakers |
| **Telegram account ban** | Low | Critical | Strict compliance with ToS, rate limiting, admin-only access |
| **Local Bot API server crash** | Medium | High | Health monitoring, auto-restart, fallback to standard API |
| **Worker process crash** | Low | Medium | Panic recovery, automatic worker restart, batch requeue |
| **Network partition** | Low | High | Connection retry logic, offline queue, graceful degradation |

### 8.3 Business Risks

| Risk | Probability | Impact | Mitigation |
|------|-------------|--------|------------|
| **Processing time exceeds 2-hour SLA** | Medium | Medium | Performance monitoring, auto-scaling, batch size tuning |
| **Cost overrun (infrastructure)** | Low | Medium | Resource utilization monitoring, cost alerts, optimization |
| **Data loss during migration** | Low | Critical | Backup strategy, migration testing, rollback plan |
| **Inadequate testing before launch** | Medium | High | Comprehensive test suite, staging environment, gradual rollout |

---

## 9. Compliance & Security

### 9.1 Telegram Terms of Service Compliance

**Rate Limiting**:
- Maximum 3 concurrent downloads (enforced)
- Maximum 20 messages/minute per chat (enforced)
- Exponential backoff on errors (2s, 4s, 8s, 16s)

**Bot API Usage**:
- Use Local Bot API Server for files > 20MB
- Respect file size limits (4GB for Premium)
- Handle file_unique_id for deduplication
- Proper error handling (don't hammer API on errors)

**Account Security**:
- Admin-only access (ADMIN_IDS whitelist)
- No public bot access
- Secure token storage (.env not in git)
- Regular token rotation

### 9.2 Data Security

**File Handling**:
- SHA256 hash verification after download
- Secure temporary storage (permissions 0600)
- Encrypted database connections (SSL required)
- Automatic cleanup of processed files

**Access Control**:
- Admin user validation on every request
- Database credentials in .env (not hardcoded)
- PostgreSQL role-based access control
- Audit logging of admin actions

**Data Privacy**:
- No logging of file contents
- Credential data stored encrypted in database
- Automatic deletion of failed batches after 7 days
- GDPR compliance (right to deletion)

### 9.3 Code Security

**Subprocess Execution**:
- Timeout enforcement (prevent infinite loops)
- Resource limits (CPU, memory)
- Input validation (prevent injection attacks)
- Error output sanitization (no sensitive data in logs)

**Database Security**:
- Prepared statements (prevent SQL injection)
- Parameterized queries
- Connection pooling with timeouts
- SSL/TLS encryption in transit

---

## 10. Appendices

### 10.1 Glossary

- **Batch**: A group of 10 files processed together in an isolated workspace
- **Batch Worker**: A goroutine that processes one batch at a time (5 concurrent)
- **Download Worker**: A goroutine that downloads one file at a time (3 concurrent)
- **Local Bot API Server**: Self-hosted Telegram Bot API for large file support
- **Optimistic Locking**: Database concurrency control using FOR UPDATE SKIP LOCKED
- **Persistent Queue**: PostgreSQL-backed queue that survives crashes
- **SHA256 Hash**: Cryptographic hash for file integrity verification
- **Working Directory**: The current directory for subprocess execution

### 10.2 References

- [Telegram Bot API Documentation](https://core.telegram.org/bots/api)
- [Telegram Premium Features](https://telegram.org/blog/700-million-and-premium)
- [Local Bot API Server Setup](https://github.com/tdlib/telegram-bot-api)
- [PostgreSQL FOR UPDATE SKIP LOCKED](https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE)
- [Go Subprocess Best Practices](https://pkg.go.dev/os/exec)
- [Prometheus Metrics](https://prometheus.io/docs/concepts/metric_types/)

### 10.3 Change Log

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2025-11-13 | Claude Code | Initial PRD for Option 2 architecture |

---

**END OF PRD**
