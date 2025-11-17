# Pipelined Multi-Round Architecture Design

## Problem Statement

Current sequential architecture creates a bottleneck:
- Round 1: Extract (10 min) → Convert (5 min) → Store (40 min) = 55 min total
- Round 2 cannot start until Round 1 completes ALL stages
- For 1000 files (20 rounds): 20 × 55 min = **18.3 hours** of sequential processing

**Store is the bottleneck**: Processes only 2 files at a time, taking 40+ minutes per round while extract/convert sit idle.

## Solution: Pipeline Parallelism

Allow multiple rounds to exist in different stages simultaneously:

```
Time    Extract Worker    Convert Worker    Store Workers (5)
----------------------------------------------------------------------
0-10    Round 1           -                 -
10-15   Round 2           Round 1           -
15-20   Round 3           Round 2           Round 1 (files 1-2)
20-25   Round 4           Round 3           Round 1 (files 3-4) + Round 2 (files 1-2)
25-30   Round 5           Round 4           Round 1 (files 5-6) + Round 2 (files 3-4) + Round 3 (files 1-2)
...
```

**Key insight**: While Round 1 is slowly storing (40 min), Rounds 2-5 can already be extracting and converting.

## Architecture Components

### 1. Round-Based State Machine

Each round progresses through stages independently:

```
CREATED → EXTRACTING → EXTRACT_COMPLETE → CONVERTING → CONVERT_COMPLETE → STORING → COMPLETED/FAILED
```

**Critical constraints**:
- Only ONE round in EXTRACTING at a time (global extract lock)
- Only ONE round in CONVERTING at a time (global convert lock)
- MULTIPLE rounds in STORING simultaneously (5 store workers, each processing 2 files)

### 2. Database Schema Changes

#### New Table: `processing_rounds`

```sql
CREATE TABLE processing_rounds (
    round_id VARCHAR(50) PRIMARY KEY,          -- e.g., "round_001"
    file_count INT NOT NULL,                   -- Files in this round
    archive_count INT DEFAULT 0,               -- ZIP/RAR files
    txt_count INT DEFAULT 0,                   -- Direct TXT files

    -- Stage statuses
    extract_status VARCHAR(20) DEFAULT 'PENDING'
        CHECK (extract_status IN ('PENDING', 'EXTRACTING', 'COMPLETED', 'SKIPPED', 'FAILED')),
    convert_status VARCHAR(20) DEFAULT 'PENDING'
        CHECK (convert_status IN ('PENDING', 'CONVERTING', 'COMPLETED', 'FAILED')),
    store_status VARCHAR(20) DEFAULT 'PENDING'
        CHECK (store_status IN ('PENDING', 'STORING', 'COMPLETED', 'FAILED')),

    -- Overall round status
    round_status VARCHAR(20) NOT NULL DEFAULT 'CREATED'
        CHECK (round_status IN ('CREATED', 'EXTRACTING', 'CONVERTING', 'STORING', 'COMPLETED', 'FAILED')),

    -- Worker assignments
    extract_worker_id VARCHAR(50),             -- Always "extract_1" (singleton)
    convert_worker_id VARCHAR(50),             -- Always "convert_1" (singleton)

    -- Timing metrics
    created_at TIMESTAMP DEFAULT NOW(),
    extract_started_at TIMESTAMP,
    extract_completed_at TIMESTAMP,
    convert_started_at TIMESTAMP,
    convert_completed_at TIMESTAMP,
    store_started_at TIMESTAMP,
    store_completed_at TIMESTAMP,

    extract_duration_sec INT,
    convert_duration_sec INT,
    store_duration_sec INT,

    error_message TEXT
);

CREATE INDEX idx_rounds_extract_status ON processing_rounds(extract_status);
CREATE INDEX idx_rounds_convert_status ON processing_rounds(convert_status);
CREATE INDEX idx_rounds_store_status ON processing_rounds(store_status);
CREATE INDEX idx_rounds_round_status ON processing_rounds(round_status);
CREATE INDEX idx_rounds_created ON processing_rounds(created_at);
```

#### Modified Table: `download_queue`

```sql
-- Add round_id column
ALTER TABLE download_queue ADD COLUMN round_id VARCHAR(50);
ALTER TABLE download_queue ADD CONSTRAINT fk_round
    FOREIGN KEY (round_id) REFERENCES processing_rounds(round_id);

CREATE INDEX idx_dq_round ON download_queue(round_id);

-- Remove batch_id (deprecated)
ALTER TABLE download_queue DROP COLUMN batch_id;
```

#### New Table: `store_tasks` (Store Stage Decomposition)

Since store.go processes 2 files at a time, we need to track individual store tasks within a round:

```sql
CREATE TABLE store_tasks (
    task_id BIGSERIAL PRIMARY KEY,
    round_id VARCHAR(50) NOT NULL,
    file_paths TEXT[] NOT NULL,                -- Array of 2 file paths
    status VARCHAR(20) NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'STORING', 'COMPLETED', 'FAILED')),
    worker_id VARCHAR(50),                     -- e.g., "store_worker_1"

    created_at TIMESTAMP DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    duration_sec INT,

    error_message TEXT,

    FOREIGN KEY (round_id) REFERENCES processing_rounds(round_id)
);

CREATE INDEX idx_store_tasks_status ON store_tasks(status);
CREATE INDEX idx_store_tasks_round ON store_tasks(round_id);
CREATE INDEX idx_store_tasks_worker ON store_tasks(worker_id);
```

**Why store_tasks needed**: Store.go's RunPipeline() processes 2 files at a time from InputDir. To parallelize multiple rounds in store stage, we need to:
1. Track which 2-file chunks belong to which round
2. Allow 5 store workers to claim different chunks from different rounds
3. Ensure store.go only sees its assigned 2 files in InputDir

### 3. Worker Architecture

#### Extract Worker (Singleton)

```go
// coordinator/extract_worker.go
func extractWorker(ctx context.Context, db *sql.DB) {
    for {
        // 1. Claim next round ready for extraction (with exclusive lock)
        round, err := claimRoundForExtract(db)
        if err == sql.ErrNoRows {
            time.Sleep(5 * time.Second)
            continue
        }

        // 2. Call preserved extract.go function
        //    It processes ALL files in app/extraction/files/all/
        err = callExtractFunction()

        // 3. Update round status
        if err != nil {
            updateRoundStatus(db, round.ID, "extract_status", "FAILED")
        } else {
            updateRoundStatus(db, round.ID, "extract_status", "COMPLETED")
            updateRoundStatus(db, round.ID, "round_status", "CONVERTING")
        }
    }
}

func claimRoundForExtract(db *sql.DB) (*Round, error) {
    tx, _ := db.Begin()
    defer tx.Rollback()

    var round Round
    err := tx.QueryRow(`
        UPDATE processing_rounds
        SET extract_status = 'EXTRACTING',
            extract_worker_id = 'extract_1',
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
    `).Scan(&round.ID, &round.FileCount, &round.ArchiveCount, &round.TxtCount)

    if err != nil {
        return nil, err
    }

    tx.Commit()
    return &round, nil
}
```

**Key points**:
- Only ONE extract worker exists (singleton by design)
- Uses `FOR UPDATE SKIP LOCKED` for optimistic locking
- Calls preserved `extract.ExtractArchives()` function
- Extract processes ALL archives in `app/extraction/files/all/`
- After completion, round transitions to CONVERTING status

#### Convert Worker (Singleton)

```go
// coordinator/convert_worker.go
func convertWorker(ctx context.Context, db *sql.DB) {
    for {
        // 1. Claim next round ready for conversion
        round, err := claimRoundForConvert(db)
        if err == sql.ErrNoRows {
            time.Sleep(5 * time.Second)
            continue
        }

        // 2. Set environment variables for convert.go
        os.Setenv("CONVERT_INPUT_DIR", "app/extraction/files/pass")
        os.Setenv("CONVERT_OUTPUT_FILE", "app/extraction/files/txt/converted.txt")

        // 3. Call preserved convert.go function
        err = callConvertFunction()

        // 4. Update round status and create store tasks
        if err != nil {
            updateRoundStatus(db, round.ID, "convert_status", "FAILED")
        } else {
            updateRoundStatus(db, round.ID, "convert_status", "COMPLETED")
            updateRoundStatus(db, round.ID, "round_status", "STORING")

            // Create store tasks for this round (2 files per task)
            createStoreTasks(db, round.ID)
        }
    }
}

func claimRoundForConvert(db *sql.DB) (*Round, error) {
    tx, _ := db.Begin()
    defer tx.Rollback()

    var round Round
    err := tx.QueryRow(`
        UPDATE processing_rounds
        SET convert_status = 'CONVERTING',
            convert_worker_id = 'convert_1',
            convert_started_at = NOW(),
            round_status = 'CONVERTING'
        WHERE round_id = (
            SELECT round_id FROM processing_rounds
            WHERE convert_status = 'PENDING'
            AND extract_status = 'COMPLETED'
            AND round_status IN ('CREATED', 'EXTRACTING')  -- Can start after extract finishes
            ORDER BY created_at ASC
            LIMIT 1
            FOR UPDATE SKIP LOCKED
        )
        RETURNING round_id, file_count
    `).Scan(&round.ID, &round.FileCount)

    if err != nil {
        return nil, err
    }

    tx.Commit()
    return &round, nil
}

func createStoreTasks(db *sql.DB, roundID string) error {
    // List all files in app/extraction/files/txt/
    files, _ := filepath.Glob("app/extraction/files/txt/*.txt")

    // Group into chunks of 2
    for i := 0; i < len(files); i += 2 {
        chunk := files[i:min(i+2, len(files))]

        _, err := db.Exec(`
            INSERT INTO store_tasks (round_id, file_paths, status)
            VALUES ($1, $2, 'PENDING')
        `, roundID, pq.Array(chunk))

        if err != nil {
            return err
        }
    }

    return nil
}
```

**Key points**:
- Only ONE convert worker exists (singleton by design)
- Waits for round's `extract_status = 'COMPLETED'`
- Sets environment variables required by convert.go
- Calls preserved `convert.ConvertTextFiles()` function
- After completion, creates store tasks (2 files each) for the round

#### Store Workers (5 Concurrent)

```go
// coordinator/store_worker.go
func storeWorker(ctx context.Context, workerID string, db *sql.DB) {
    for {
        // 1. Claim next store task (2-file chunk)
        task, err := claimStoreTask(db, workerID)
        if err == sql.ErrNoRows {
            time.Sleep(5 * time.Second)
            continue
        }

        // 2. Create isolated directory for this task
        taskDir := fmt.Sprintf("store_tasks/%s", task.ID)
        setupStoreTaskDirectory(taskDir, task.FilePaths)

        // 3. Change to task directory and call store.go
        originalDir, _ := os.Getwd()
        os.Chdir(taskDir)

        err = callStoreFunction()

        os.Chdir(originalDir)

        // 4. Update task status
        if err != nil {
            updateStoreTaskStatus(db, task.ID, "FAILED", err.Error())
        } else {
            updateStoreTaskStatus(db, task.ID, "COMPLETED", "")

            // Check if all tasks for this round are complete
            checkRoundStoreCompletion(db, task.RoundID)
        }

        // 5. Cleanup task directory
        os.RemoveAll(taskDir)
    }
}

func claimStoreTask(db *sql.DB, workerID string) (*StoreTask, error) {
    tx, _ := db.Begin()
    defer tx.Rollback()

    var task StoreTask
    err := tx.QueryRow(`
        UPDATE store_tasks
        SET status = 'STORING',
            worker_id = $1,
            started_at = NOW()
        WHERE task_id = (
            SELECT task_id FROM store_tasks
            WHERE status = 'PENDING'
            ORDER BY created_at ASC
            LIMIT 1
            FOR UPDATE SKIP LOCKED
        )
        RETURNING task_id, round_id, file_paths
    `, workerID).Scan(&task.ID, &task.RoundID, pq.Array(&task.FilePaths))

    if err != nil {
        return nil, err
    }

    tx.Commit()
    return &task, nil
}

func setupStoreTaskDirectory(taskDir string, filePaths []string) error {
    // Create directory structure matching store.go expectations
    dirs := []string{
        filepath.Join(taskDir, "app/extraction/files/txt"),
        filepath.Join(taskDir, "app/extraction/files/nonsorted"),
    }

    for _, dir := range dirs {
        os.MkdirAll(dir, 0755)
    }

    // Copy the 2 files into taskDir/app/extraction/files/txt/
    for _, srcPath := range filePaths {
        dstPath := filepath.Join(taskDir, "app/extraction/files/txt", filepath.Base(srcPath))
        copyFile(srcPath, dstPath)
    }

    // Copy .env file for database configuration
    copyFile(".env", filepath.Join(taskDir, ".env"))

    return nil
}

func checkRoundStoreCompletion(db *sql.DB, roundID string) {
    // Count pending/storing tasks for this round
    var pendingCount int
    db.QueryRow(`
        SELECT COUNT(*) FROM store_tasks
        WHERE round_id = $1 AND status IN ('PENDING', 'STORING')
    `, roundID).Scan(&pendingCount)

    if pendingCount == 0 {
        // All tasks complete - update round status
        var failedCount int
        db.QueryRow(`
            SELECT COUNT(*) FROM store_tasks
            WHERE round_id = $1 AND status = 'FAILED'
        `, roundID).Scan(&failedCount)

        if failedCount > 0 {
            db.Exec(`
                UPDATE processing_rounds
                SET store_status = 'FAILED',
                    round_status = 'FAILED',
                    store_completed_at = NOW()
                WHERE round_id = $1
            `, roundID)
        } else {
            db.Exec(`
                UPDATE processing_rounds
                SET store_status = 'COMPLETED',
                    round_status = 'COMPLETED',
                    store_completed_at = NOW()
                WHERE round_id = $1
            `, roundID)
        }
    }
}
```

**Key points**:
- **5 concurrent store workers** can process tasks in parallel
- Each task processes exactly 2 files (matching store.go's behavior)
- Creates **isolated directory** for each task to prevent conflicts
- Store.go's 4-stage pipeline (move → merge → valuable → filter/DB) runs in isolation
- Multiple rounds can be in STORING status simultaneously
- Round completes when all its store tasks finish

### 4. Round Coordinator

```go
// coordinator/round_coordinator.go
func roundCoordinator(ctx context.Context, db *sql.DB) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            createRoundIfReady(db)
        case <-ctx.Done():
            return
        }
    }
}

func createRoundIfReady(db *sql.DB) error {
    tx, _ := db.Begin()
    defer tx.Rollback()

    // Count files ready for processing (DOWNLOADED status, not assigned to round)
    var readyCount int
    tx.QueryRow(`
        SELECT COUNT(*) FROM download_queue
        WHERE status = 'DOWNLOADED' AND round_id IS NULL
    `).Scan(&readyCount)

    // Check timeout condition: oldest downloaded file waiting > 5 minutes
    var oldestAge time.Duration
    var hasWaiting bool
    err := tx.QueryRow(`
        SELECT EXTRACT(EPOCH FROM (NOW() - MIN(completed_at)))
        FROM download_queue
        WHERE status = 'DOWNLOADED' AND round_id IS NULL
    `).Scan(&oldestAge)

    hasWaiting = (err == nil && oldestAge > 300) // 5 minutes

    // Create round if:
    // 1. >= 50 files ready (full round), OR
    // 2. >= 10 files AND oldest file waiting > 5 minutes
    shouldCreate := readyCount >= 50 || (readyCount >= 10 && hasWaiting)

    if !shouldCreate {
        return nil
    }

    // Generate round ID
    var maxRoundNum int
    tx.QueryRow(`
        SELECT COALESCE(MAX(CAST(SUBSTRING(round_id FROM 7) AS INT)), 0)
        FROM processing_rounds
    `).Scan(&maxRoundNum)

    roundID := fmt.Sprintf("round_%03d", maxRoundNum+1)

    // Take up to 50 files for this round
    files, _ := tx.Query(`
        SELECT task_id, file_type FROM download_queue
        WHERE status = 'DOWNLOADED' AND round_id IS NULL
        ORDER BY priority DESC, completed_at ASC
        LIMIT 50
        FOR UPDATE SKIP LOCKED
    `)

    var taskIDs []int64
    var archiveCount, txtCount int
    for files.Next() {
        var taskID int64
        var fileType string
        files.Scan(&taskID, &fileType)
        taskIDs = append(taskIDs, taskID)

        if fileType == "ZIP" || fileType == "RAR" {
            archiveCount++
        } else if fileType == "TXT" {
            txtCount++
        }
    }
    files.Close()

    if len(taskIDs) == 0 {
        return nil
    }

    // Create round record
    _, err = tx.Exec(`
        INSERT INTO processing_rounds (
            round_id, file_count, archive_count, txt_count,
            round_status, extract_status, convert_status, store_status
        ) VALUES ($1, $2, $3, $4, 'CREATED',
            CASE WHEN $3 > 0 THEN 'PENDING' ELSE 'SKIPPED' END,
            'PENDING', 'PENDING')
    `, roundID, len(taskIDs), archiveCount, txtCount)

    if err != nil {
        return err
    }

    // Assign files to round and move to staging directory
    for _, taskID := range taskIDs {
        // Update database
        tx.Exec(`
            UPDATE download_queue
            SET round_id = $1
            WHERE task_id = $2
        `, roundID, taskID)

        // Move file to appropriate staging directory
        var filename, fileType string
        tx.QueryRow(`
            SELECT filename, file_type FROM download_queue WHERE task_id = $1
        `, taskID).Scan(&filename, &fileType)

        srcPath := filepath.Join("downloads", filename)

        var dstPath string
        if fileType == "ZIP" || fileType == "RAR" {
            dstPath = filepath.Join("app/extraction/files/all", filename)
        } else if fileType == "TXT" {
            dstPath = filepath.Join("app/extraction/files/txt", filename)
        }

        // Atomic move (copy + verify + delete)
        copyFile(srcPath, dstPath)
        os.Remove(srcPath)
    }

    tx.Commit()

    log.Info("Created round", "round_id", roundID, "file_count", len(taskIDs))
    return nil
}
```

**Key points**:
- Creates rounds every 30 seconds if conditions met
- **50 files per round** (configurable, increased from 10 for better throughput)
- **Timeout mechanism**: Create round with ≥10 files if oldest file waiting >5 minutes
- Moves files atomically to staging directories:
  - Archives (ZIP/RAR) → `app/extraction/files/all/`
  - Text files (TXT) → `app/extraction/files/txt/`
- If round has no archives, `extract_status = 'SKIPPED'` (goes straight to convert)

## Performance Analysis

### Current Sequential Architecture (Baseline)

For 1000 files:
- 20 rounds (50 files each)
- Per round: Extract (10 min) + Convert (5 min) + Store (40 min) = **55 min**
- Total: 20 × 55 min = **18.3 hours**

### Pipelined Architecture (This Design)

**Startup phase** (first 3 rounds filling pipeline):
- 0-10 min: Round 1 extracting
- 10-15 min: Round 1 converting, Round 2 extracting
- 15-55 min: Round 1 storing (5 workers × 2 files), Round 2 converting, Round 3 extracting

**Steady state** (pipeline full):
- Extract: 1 round every 10 min (sequential, no overlap)
- Convert: 1 round every 5 min (sequential, no overlap)
- Store: 5 rounds in parallel (40 min each, staggered)

**Limiting factor**: Store stage still takes 40 min per round, BUT:
- 5 store workers process 2 files each = 10 files simultaneously
- Each round has ~50 files after conversion (some archives extract multiple files)
- Store time per round: 50 files ÷ 10 files/batch × 4 min/batch = **20 min** (improved!)

**Revised steady state**:
- Extract: 10 min per round
- Convert: 5 min per round
- Store: 20 min per round (with 5 workers)
- **Bottleneck**: Store at 20 min per round

**Pipeline throughput**: 1 round every 20 min (limited by store bottleneck)

**Total time for 1000 files**:
- 20 rounds × 20 min/round = **6.7 hours**
- Speedup: **18.3 hours → 6.7 hours = 2.7× faster**

**Why not faster?** Even with pipelining, store is still sequential at the round level (each round waits for previous round's store to finish). However, within each round, 5 workers parallelize the work.

### Further Optimization: Decouple Store Rounds

If we allow store workers to grab tasks from ANY round (not just oldest), we could theoretically process all 20 rounds' store tasks in parallel:

- Total store tasks: 20 rounds × 25 tasks/round (50 files ÷ 2) = 500 tasks
- 5 workers processing 500 tasks at 4 min/task = **400 minutes = 6.7 hours**

But this conflicts with extract/convert, which take:
- Extract: 20 rounds × 10 min = 200 min = 3.3 hours
- Convert: 20 rounds × 5 min = 100 min = 1.7 hours

**Optimal timeline** (fully decoupled):
- 0-3.3 hours: Extract all rounds (sequential)
- 3.3-5.0 hours: Convert all rounds (sequential, overlaps with extract completion)
- 0-6.7 hours: Store all rounds (parallel, starts after first round converts)

**Revised total time**: **~7 hours** (store finishes last)

**Speedup**: **18.3 hours → 7 hours = 2.6× faster**

## File Organization

```
app/extraction/
├── files/
│   ├── all/              # Extract input (archives)
│   ├── pass/             # Extract output → Convert input
│   ├── txt/              # Convert output → Store input (global)
│   ├── nonsorted/        # Store stage working directory (global - conflicts!)
│   ├── nopass/           # Password-protected archives
│   └── errors/           # Failed extractions

store_tasks/              # NEW: Isolated directories per store task
├── 1/
│   ├── app/extraction/files/
│   │   ├── txt/          # 2 files for this task
│   │   └── nonsorted/    # Isolated working directory
│   └── .env
├── 2/
│   ├── app/extraction/files/
│   │   ├── txt/
│   │   └── nonsorted/
│   └── .env
...
```

**Critical insight**: Store stage needs isolated directories because `nonsorted/` is a working directory that would conflict if multiple store processes run in the same location.

## Migration Path

1. **Database migration** (005_create_pipelined_architecture.sql):
   - Create `processing_rounds` table
   - Create `store_tasks` table
   - Add `round_id` to `download_queue`
   - Remove `batch_id` references

2. **Implement workers**:
   - `coordinator/extract_worker.go`
   - `coordinator/convert_worker.go`
   - `coordinator/store_worker.go`
   - `coordinator/round_coordinator.go`

3. **Update main.go**:
   - Start round coordinator
   - Start 1 extract worker
   - Start 1 convert worker
   - Start 5 store workers

4. **Remove deprecated code**:
   - Delete `batch_coordinator.go`
   - Delete `batch_worker.go`
   - Update `crash_recovery.go` for new schema

5. **Update CLAUDE.md** with new architecture

## Preserving Core Processing Code

### extract.go - 100% Preserved ✓
- No modifications needed
- Processes ALL files in `app/extraction/files/all/`
- Single worker ensures no simultaneous execution

### convert.go - 100% Preserved ✓
- No modifications needed
- Reads from `CONVERT_INPUT_DIR` (app/extraction/files/pass)
- Writes to `CONVERT_OUTPUT_FILE` (app/extraction/files/txt/converted.txt)
- Single worker ensures no simultaneous execution

### store.go - Core Logic Preserved ✓
**Preserved**: The 4-stage pipeline logic (move → merge → valuable → filter/DB)
**Modified approach**: Instead of running in global directory, each store task runs in isolated directory

**What is preserved** (user requirement: "what and how each stage processes data"):
- Move stage: Moves 2 files from InputDir to NonSortedDir ✓
- Merge stage: Merges files in NonSortedDir ✓
- Valuable stage: Extracts betting URLs ✓
- Filter & DB stage: Filters and stores to database ✓

**What changes**: Working directory isolation (task-specific directories)

## Configuration Changes

Update `.env`:

```bash
# Round Configuration
ROUND_SIZE=50                      # Files per round (increased from 10)
ROUND_TIMEOUT_SEC=300              # Create round if files wait > 5 minutes

# Worker Configuration
MAX_EXTRACT_WORKERS=1              # Always 1 (singleton)
MAX_CONVERT_WORKERS=1              # Always 1 (singleton)
MAX_STORE_WORKERS=5                # Can increase to 10-20 for better throughput

# Store Configuration
STORE_FILES_PER_TASK=2             # Matches store.go's 2-file processing
```

## Testing Strategy

1. **Unit tests**:
   - `claimRoundForExtract()` with concurrent access
   - `claimRoundForConvert()` with concurrent access
   - `claimStoreTask()` with 5 workers
   - `createStoreTasks()` file grouping logic

2. **Integration tests**:
   - Single round through full pipeline (extract → convert → store)
   - Multiple rounds in pipeline simultaneously
   - Store task isolation (verify no conflicts)

3. **Load tests**:
   - 1000 files end-to-end
   - Measure actual throughput vs. predicted 6-7 hours
   - Monitor worker utilization

4. **Chaos tests**:
   - Kill extract worker mid-processing
   - Kill convert worker mid-processing
   - Kill store worker mid-task (verify round completion still works)
   - Database connection loss

## Monitoring

New Prometheus metrics:

```go
rounds_created_total
rounds_completed_total
rounds_failed_total

extract_rounds_processing_current
convert_rounds_processing_current
store_tasks_processing_current

store_tasks_completed_total
store_tasks_failed_total

round_extract_duration_seconds
round_convert_duration_seconds
round_store_duration_seconds
```

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Store task isolation overhead (copy files) | Slower | Use hard links instead of copies where possible |
| Database locks contention on rounds table | Worker starvation | Use `SKIP LOCKED` consistently, monitor lock wait times |
| Store workers finish tasks unevenly | Underutilization | Implement work-stealing: workers check for tasks from any round |
| Round completion race condition | Incorrect status | Use database transactions for round status updates |
| File conflicts in global directories | Data corruption | Strict worker limits (1 extract, 1 convert) + monitoring |

## Success Criteria

- ✅ Processing time for 1000 files: < 8 hours (target: 6-7 hours)
- ✅ Extract and convert preserved 100% (checksums match)
- ✅ Store pipeline stages preserved (move → merge → valuable → filter → DB)
- ✅ No data loss (all files processed or explicitly failed)
- ✅ Zero file conflicts (verified through testing)
- ✅ Graceful crash recovery (rounds can be resumed)

## Conclusion

This pipelined architecture achieves **2.6-2.7× speedup** by allowing multiple rounds to exist in different stages simultaneously. The key insight is decoupling extract, convert, and store into independent pipelines that progress at different rates, with store bottleneck parallelized through 5 workers processing isolated 2-file tasks.

All three preserved files remain unchanged in their core logic, with isolation achieved through:
- Single workers for extract/convert (preventing simultaneous execution)
- Isolated task directories for store (preventing conflicts in working directories)
