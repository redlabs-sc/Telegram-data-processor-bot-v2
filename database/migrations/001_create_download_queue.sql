-- Download Queue Table
CREATE TABLE download_queue (
    task_id BIGSERIAL PRIMARY KEY,
    file_id VARCHAR(255) NOT NULL,
    user_id BIGINT NOT NULL,
    filename TEXT NOT NULL,
    file_type VARCHAR(10) NOT NULL CHECK (file_type IN ('ZIP', 'RAR', 'TXT')),
    file_size BIGINT NOT NULL,
    sha256_hash VARCHAR(64),
    status VARCHAR(20) NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'DOWNLOADING', 'DOWNLOADED', 'FAILED')),
    download_attempts INT DEFAULT 0,
    last_error TEXT,
    priority INT DEFAULT 0,
    batch_id VARCHAR(50),
    created_at TIMESTAMP DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP
);

-- Indexes for performance
CREATE INDEX idx_dq_status ON download_queue(status);
CREATE INDEX idx_dq_priority ON download_queue(priority DESC, created_at ASC);
CREATE INDEX idx_dq_batch ON download_queue(batch_id);
CREATE INDEX idx_dq_created ON download_queue(created_at);

-- Comments
COMMENT ON TABLE download_queue IS 'Queue for file downloads from Telegram';
COMMENT ON COLUMN download_queue.status IS 'PENDING: queued, DOWNLOADING: in progress, DOWNLOADED: ready for batch, FAILED: error occurred';
COMMENT ON COLUMN download_queue.priority IS 'Higher values processed first (default 0)';
