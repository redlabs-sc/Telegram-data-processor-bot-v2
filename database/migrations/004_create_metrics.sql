-- Processing Metrics Table
CREATE TABLE processing_metrics (
    metric_id BIGSERIAL PRIMARY KEY,
    batch_id VARCHAR(50),
    metric_type VARCHAR(50) NOT NULL,
    metric_value DECIMAL(10, 2) NOT NULL,
    recorded_at TIMESTAMP DEFAULT NOW(),

    FOREIGN KEY (batch_id) REFERENCES batch_processing(batch_id) ON DELETE CASCADE
);

-- Indexes
CREATE INDEX idx_pm_type ON processing_metrics(metric_type, recorded_at);
CREATE INDEX idx_pm_batch ON processing_metrics(batch_id);

-- Comments
COMMENT ON TABLE processing_metrics IS 'Time-series metrics for analytics';
COMMENT ON COLUMN processing_metrics.metric_type IS 'download_speed, extract_time, convert_time, store_time, etc.';
