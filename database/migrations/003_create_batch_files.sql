-- Batch Files Join Table
CREATE TABLE batch_files (
    batch_id VARCHAR(50) NOT NULL,
    task_id BIGINT NOT NULL,
    file_type VARCHAR(10) NOT NULL,
    processing_status VARCHAR(20) DEFAULT 'PENDING' CHECK (processing_status IN ('PENDING', 'EXTRACTED', 'CONVERTED', 'STORED', 'FAILED')),
    error_message TEXT,

    PRIMARY KEY (batch_id, task_id),
    FOREIGN KEY (batch_id) REFERENCES batch_processing(batch_id) ON DELETE CASCADE,
    FOREIGN KEY (task_id) REFERENCES download_queue(task_id) ON DELETE CASCADE
);

-- Indexes
CREATE INDEX idx_bf_batch ON batch_files(batch_id);
CREATE INDEX idx_bf_task ON batch_files(task_id);
CREATE INDEX idx_bf_status ON batch_files(processing_status);
