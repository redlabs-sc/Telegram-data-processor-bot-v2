-- Migration 005: Pipelined Multi-Round Architecture
-- This migration introduces a new round-based processing model that allows
-- extract, convert, and store stages to operate on different rounds simultaneously

-- 1. Create processing_rounds table
CREATE TABLE processing_rounds (
    round_id VARCHAR(50) PRIMARY KEY,
    file_count INT NOT NULL,
    archive_count INT DEFAULT 0,
    txt_count INT DEFAULT 0,

    -- Stage-specific statuses
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
    extract_worker_id VARCHAR(50),
    convert_worker_id VARCHAR(50),

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

-- Indexes for performance
CREATE INDEX idx_rounds_extract_status ON processing_rounds(extract_status);
CREATE INDEX idx_rounds_convert_status ON processing_rounds(convert_status);
CREATE INDEX idx_rounds_store_status ON processing_rounds(store_status);
CREATE INDEX idx_rounds_round_status ON processing_rounds(round_status);
CREATE INDEX idx_rounds_created ON processing_rounds(created_at);

-- Comments
COMMENT ON TABLE processing_rounds IS 'Tracks processing rounds through pipeline stages';
COMMENT ON COLUMN processing_rounds.extract_status IS 'PENDING: awaiting extraction, EXTRACTING: in progress, COMPLETED: done, SKIPPED: no archives, FAILED: error';
COMMENT ON COLUMN processing_rounds.convert_status IS 'PENDING: awaiting conversion, CONVERTING: in progress, COMPLETED: done, FAILED: error';
COMMENT ON COLUMN processing_rounds.store_status IS 'PENDING: awaiting storage, STORING: in progress, COMPLETED: done, FAILED: error';
COMMENT ON COLUMN processing_rounds.round_status IS 'Overall pipeline status: CREATED → EXTRACTING → CONVERTING → STORING → COMPLETED/FAILED';

-- 2. Create store_tasks table
-- Store stage processes 2 files at a time, so each round generates multiple tasks
CREATE TABLE store_tasks (
    task_id BIGSERIAL PRIMARY KEY,
    round_id VARCHAR(50) NOT NULL,
    file_paths TEXT[] NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'STORING', 'COMPLETED', 'FAILED')),
    worker_id VARCHAR(50),

    created_at TIMESTAMP DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    duration_sec INT,

    error_message TEXT,

    FOREIGN KEY (round_id) REFERENCES processing_rounds(round_id) ON DELETE CASCADE
);

-- Indexes for performance
CREATE INDEX idx_store_tasks_status ON store_tasks(status);
CREATE INDEX idx_store_tasks_round ON store_tasks(round_id);
CREATE INDEX idx_store_tasks_worker ON store_tasks(worker_id);
CREATE INDEX idx_store_tasks_created ON store_tasks(created_at);

-- Comments
COMMENT ON TABLE store_tasks IS 'Individual store tasks (2 files each) for parallel processing';
COMMENT ON COLUMN store_tasks.file_paths IS 'Array of 2 file paths to process together';
COMMENT ON COLUMN store_tasks.status IS 'PENDING: queued, STORING: in progress, COMPLETED: done, FAILED: error';

-- 3. Modify download_queue table
-- Add round_id column to link files to rounds
ALTER TABLE download_queue ADD COLUMN round_id VARCHAR(50);
ALTER TABLE download_queue ADD CONSTRAINT fk_round
    FOREIGN KEY (round_id) REFERENCES processing_rounds(round_id) ON DELETE SET NULL;

CREATE INDEX idx_dq_round ON download_queue(round_id);

COMMENT ON COLUMN download_queue.round_id IS 'Links file to processing round (NULL if not yet assigned)';

-- 4. Migrate existing batch_processing data (if any)
-- Convert old batches to new rounds format
INSERT INTO processing_rounds (
    round_id,
    file_count,
    archive_count,
    txt_count,
    extract_status,
    convert_status,
    store_status,
    round_status,
    created_at,
    extract_started_at,
    extract_completed_at,
    convert_started_at,
    convert_completed_at,
    store_started_at,
    store_completed_at,
    extract_duration_sec,
    convert_duration_sec,
    store_duration_sec,
    error_message
)
SELECT
    'migrated_' || batch_id AS round_id,
    file_count,
    archive_count,
    txt_count,
    CASE
        WHEN status = 'EXTRACTING' THEN 'EXTRACTING'
        WHEN status IN ('CONVERTING', 'STORING', 'COMPLETED') THEN 'COMPLETED'
        ELSE 'FAILED'
    END AS extract_status,
    CASE
        WHEN status = 'CONVERTING' THEN 'CONVERTING'
        WHEN status IN ('STORING', 'COMPLETED') THEN 'COMPLETED'
        WHEN status = 'EXTRACTING' THEN 'PENDING'
        ELSE 'FAILED'
    END AS convert_status,
    CASE
        WHEN status = 'STORING' THEN 'STORING'
        WHEN status = 'COMPLETED' THEN 'COMPLETED'
        WHEN status IN ('EXTRACTING', 'CONVERTING') THEN 'PENDING'
        ELSE 'FAILED'
    END AS store_status,
    CASE
        WHEN status = 'COMPLETED' THEN 'COMPLETED'
        WHEN status = 'FAILED' THEN 'FAILED'
        ELSE status
    END AS round_status,
    created_at,
    CASE WHEN status IN ('EXTRACTING', 'CONVERTING', 'STORING', 'COMPLETED') THEN started_at END AS extract_started_at,
    CASE WHEN status IN ('CONVERTING', 'STORING', 'COMPLETED') THEN started_at + (extract_duration_sec || ' seconds')::INTERVAL END AS extract_completed_at,
    CASE WHEN status IN ('CONVERTING', 'STORING', 'COMPLETED') THEN started_at + (extract_duration_sec || ' seconds')::INTERVAL END AS convert_started_at,
    CASE WHEN status IN ('STORING', 'COMPLETED') THEN started_at + ((extract_duration_sec + convert_duration_sec) || ' seconds')::INTERVAL END AS convert_completed_at,
    CASE WHEN status IN ('STORING', 'COMPLETED') THEN started_at + ((extract_duration_sec + convert_duration_sec) || ' seconds')::INTERVAL END AS store_started_at,
    completed_at AS store_completed_at,
    extract_duration_sec,
    convert_duration_sec,
    store_duration_sec,
    error_message
FROM batch_processing
WHERE batch_id IS NOT NULL;

-- Update download_queue to use new round_id instead of batch_id
UPDATE download_queue
SET round_id = 'migrated_' || batch_id
WHERE batch_id IS NOT NULL;

-- 5. Drop deprecated batch_id column (after migration)
-- Wait until new system is stable before running this
-- ALTER TABLE download_queue DROP COLUMN batch_id;

-- Note: batch_processing and batch_files tables are kept for now
-- to allow rollback. Drop them after confirming new system works:
-- DROP TABLE batch_files;
-- DROP TABLE batch_processing;
