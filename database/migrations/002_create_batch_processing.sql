-- Batch Processing Table
CREATE TABLE batch_processing (
    batch_id VARCHAR(50) PRIMARY KEY,
    file_count INT NOT NULL,
    archive_count INT DEFAULT 0,
    txt_count INT DEFAULT 0,
    status VARCHAR(20) NOT NULL DEFAULT 'QUEUED' CHECK (status IN ('QUEUED', 'EXTRACTING', 'CONVERTING', 'STORING', 'COMPLETED', 'FAILED')),
    worker_id VARCHAR(50),
    created_at TIMESTAMP DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    extract_duration_sec INT,
    convert_duration_sec INT,
    store_duration_sec INT,
    error_message TEXT
);

-- Indexes
CREATE INDEX idx_bp_status ON batch_processing(status);
CREATE INDEX idx_bp_created ON batch_processing(created_at);
CREATE INDEX idx_bp_worker ON batch_processing(worker_id);

-- Comments
COMMENT ON TABLE batch_processing IS 'Tracks batch processing lifecycle';
COMMENT ON COLUMN batch_processing.status IS 'QUEUED → EXTRACTING → CONVERTING → STORING → COMPLETED/FAILED';
