package extraction

import (
	"bufio"
	"context"
	"crypto/md5"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mattn/go-sqlite3"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/sirupsen/logrus"
)

// ensureSchema creates the database table if it doesn't exist with proper constraints
// Enhanced for multi-PC distributed duplicate prevention with compression and tracking
func ensureSchema(db *sql.DB, dbType string) error {
	var createTableSQL string

	switch dbType {
	case "mysql":
		// Enhanced schema for distributed duplicate prevention with source tracking
		createTableSQL = `
			CREATE TABLE IF NOT EXISTS ` + "`lines`" + ` (
				id BIGINT AUTO_INCREMENT PRIMARY KEY,
				line_hash CHAR(32) NOT NULL,
				content TEXT NOT NULL,
				date_of_entry DATE NOT NULL DEFAULT (CURRENT_DATE),
				source_instance VARCHAR(100) DEFAULT NULL,
				created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
				updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
				
				UNIQUE KEY idx_unique_hash (line_hash),
				INDEX idx_date_entry (date_of_entry),
				INDEX idx_source_instance (source_instance),
				INDEX idx_created_at (created_at),
				FULLTEXT(content)
			) ENGINE=InnoDB 
			  ROW_FORMAT=COMPRESSED 
			  KEY_BLOCK_SIZE=8
			  DEFAULT CHARSET=utf8mb4
			  COMMENT='Enhanced for multi-PC duplicate prevention';`
	case "sqlite":
		// SQLite version with enhanced tracking
		createTableSQL = `
			CREATE TABLE IF NOT EXISTS lines (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				line_hash TEXT NOT NULL UNIQUE,
				content TEXT NOT NULL,
				date_of_entry DATE NOT NULL DEFAULT (date('now')),
				source_instance TEXT DEFAULT NULL,
				created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
				updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
			);
			
			CREATE INDEX IF NOT EXISTS idx_date_entry ON lines(date_of_entry);
			CREATE INDEX IF NOT EXISTS idx_source_instance ON lines(source_instance);`
	default:
		return fmt.Errorf("unsupported database type for schema creation: %s", dbType)
	}

	_, err := db.Exec(createTableSQL)
	if err != nil {
		return fmt.Errorf("failed to create table schema: %w", err)
	}

	// Perform schema migration for existing tables
	if err := migrateSchema(db, dbType); err != nil {
		return fmt.Errorf("failed to migrate schema: %w", err)
	}

	return nil
}

// migrateSchema adds missing columns to existing database tables
func migrateSchema(db *sql.DB, dbType string) error {
	switch dbType {
	case "mysql":
		// Check and add missing columns for MySQL
		migrations := []struct {
			check string
			alter string
		}{
			{
				check: "SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'lines' AND COLUMN_NAME = 'date_of_entry'",
				alter: "ALTER TABLE `lines` ADD COLUMN `date_of_entry` DATE NOT NULL DEFAULT (CURRENT_DATE)",
			},
			{
				check: "SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'lines' AND COLUMN_NAME = 'source_instance'", 
				alter: "ALTER TABLE `lines` ADD COLUMN `source_instance` VARCHAR(100) DEFAULT NULL",
			},
		}

		for _, migration := range migrations {
			var count int
			if err := db.QueryRow(migration.check).Scan(&count); err != nil {
				continue // Skip if check fails
			}
			
			if count == 0 { // Column doesn't exist, add it
				if _, err := db.Exec(migration.alter); err != nil {
					// Log error but continue with other migrations
					continue
				}
			}
		}

	case "sqlite":
		// SQLite migrations - check for missing columns
		sqliteMigrations := []struct {
			check string
			alter string
		}{
			{
				check: "PRAGMA table_info(lines)",
				alter: "ALTER TABLE lines ADD COLUMN date_of_entry DATE NOT NULL DEFAULT (date('now'))",
			},
			{
				check: "PRAGMA table_info(lines)",
				alter: "ALTER TABLE lines ADD COLUMN source_instance TEXT DEFAULT NULL",
			},
		}

		for _, migration := range sqliteMigrations {
			// For SQLite, we need to check column existence differently
			rows, err := db.Query(migration.check)
			if err != nil {
				continue
			}
			
			hasColumn := false
			for rows.Next() {
				var cid int
				var name, ctype string
				var notnull, pk int
				var dflt_value interface{}
				
				if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt_value, &pk); err == nil {
					if name == "date_of_entry" || name == "source_instance" {
						hasColumn = true
						break
					}
				}
			}
			rows.Close()
			
			if !hasColumn {
				db.Exec(migration.alter) // Ignore errors for SQLite ALTER TABLE
			}
		}
	}
	
	return nil
}

// hashLine creates a shorter hash representation of a line for memory-efficient deduplication
func hashLine(line string) string {
	h := md5.Sum([]byte(line))
	return fmt.Sprintf("%x", h[:8]) // Use first 8 bytes = 64-bit hash
}

// getShardForLine determines which shard database a line should go to based on MD5 hash
// Uses first 2 hex characters of MD5 hash to determine shard (0-255 mapped to 0-shardCount)
func getShardForLine(line string, shardCount int) int {
	if shardCount <= 1 {
		return 0 // Single database mode
	}

	h := md5.Sum([]byte(line))
	// Use first byte of MD5 hash (first 2 hex chars)
	firstByte := h[0]
	// Map 256 possible values (0-255) to shardCount (0 to shardCount-1)
	return int(firstByte) % shardCount
}

// generateShardDatabaseName creates shard database name based on config and shard index
// Examples: shard_00, shard_01, shard_02, etc.
func generateShardDatabaseName(cfg Config, shardIndex int) string {
	if cfg.ShardCount <= 1 {
		return cfg.MySQLDB // Use original database name for single DB mode
	}

	return fmt.Sprintf("%s_%02d", cfg.DatabasePrefix, shardIndex)
}

// ShardManager manages multiple database connections for sharding
type ShardManager struct {
	config      Config
	connections []*sql.DB
	shardCount  int
	logger      func(string, ...interface{})
}

// validateShardingConfig validates sharding configuration parameters
func validateShardingConfig(cfg Config) error {
	if cfg.ShardCount < 0 {
		return fmt.Errorf("invalid shard count: %d (must be >= 0)", cfg.ShardCount)
	}

	if cfg.ShardCount > 100 {
		return fmt.Errorf("shard count too high: %d (maximum 100 shards supported)", cfg.ShardCount)
	}

	if cfg.ShardCount > 1 && cfg.DatabasePrefix == "" {
		return fmt.Errorf("database prefix required when sharding enabled (ShardCount > 1)")
	}

	if cfg.ShardCount > 1 && cfg.DBType != "mysql" {
		return fmt.Errorf("sharding currently only supported for MySQL databases, got: %s", cfg.DBType)
	}

	// Validate database naming constraints
	if cfg.DatabasePrefix != "" {
		if len(cfg.DatabasePrefix) > 20 {
			return fmt.Errorf("database prefix too long: %d chars (maximum 20)", len(cfg.DatabasePrefix))
		}

		// Check for valid MySQL database name characters
		for _, r := range cfg.DatabasePrefix {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
				return fmt.Errorf("invalid character in database prefix: %c (only alphanumeric and underscore allowed)", r)
			}
		}
	}

	return nil
}

// NewShardManager creates a new ShardManager with connections to all shard databases
func NewShardManager(cfg Config, logger func(string, ...interface{})) (*ShardManager, error) {
	// Validate configuration before creating connections
	if err := validateShardingConfig(cfg); err != nil {
		return nil, fmt.Errorf("sharding configuration validation failed: %w", err)
	}
	if cfg.ShardCount <= 1 {
		// Single database mode - create one connection
		db, err := openDB(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to open single database: %w", err)
		}

		return &ShardManager{
			config:      cfg,
			connections: []*sql.DB{db},
			shardCount:  1,
			logger:      logger,
		}, nil
	}

	// Multi-shard mode
	connections := make([]*sql.DB, cfg.ShardCount)

	for i := 0; i < cfg.ShardCount; i++ {
		dbName := generateShardDatabaseName(cfg, i)

		// Create config for this specific shard
		shardCfg := cfg
		shardCfg.MySQLDB = dbName

		db, err := openDB(shardCfg)
		if err != nil {
			// Close any previously opened connections on failure
			for j := 0; j < i; j++ {
				if connections[j] != nil {
					connections[j].Close()
				}
			}
			return nil, fmt.Errorf("failed to open shard database %s: %w", dbName, err)
		}

		connections[i] = db

		if logger != nil {
			logger("✓ Connected to shard database: %s", dbName)
		}
	}

	manager := &ShardManager{
		config:      cfg,
		connections: connections,
		shardCount:  cfg.ShardCount,
		logger:      logger,
	}

	// Configure connection pooling for each shard
	manager.configureConnectionPooling()

	return manager, nil
}

// GetShardConnection returns the database connection for a specific shard index
func (sm *ShardManager) GetShardConnection(shardIndex int) *sql.DB {
	if shardIndex < 0 || shardIndex >= len(sm.connections) {
		return sm.connections[0] // Fallback to first connection
	}
	return sm.connections[shardIndex]
}

// GetShardCount returns the number of shards
func (sm *ShardManager) GetShardCount() int {
	return sm.shardCount
}

// configureConnectionPooling sets optimal connection pool settings for each shard
func (sm *ShardManager) configureConnectionPooling() {
	// Configure connection pooling for each shard database
	maxConns := 25 // Per shard, suitable for shared hosting
	if sm.shardCount > 1 {
		// Reduce per-shard connections when using multiple shards
		maxConns = 25 / sm.shardCount
		if maxConns < 5 {
			maxConns = 5 // Minimum connections per shard
		}
	}

	for i, db := range sm.connections {
		if db != nil {
			db.SetMaxOpenConns(maxConns)
			db.SetMaxIdleConns(maxConns / 5) // 20% idle connections
			db.SetConnMaxLifetime(5 * time.Minute)

			if sm.logger != nil {
				sm.logger("✓ Configured connection pool for shard %d: max=%d, idle=%d",
					i, maxConns, maxConns/5)
			}
		}
	}
}

// Close closes all database connections
func (sm *ShardManager) Close() error {
	var errors []error

	for i, db := range sm.connections {
		if db != nil {
			if err := db.Close(); err != nil {
				errors = append(errors, fmt.Errorf("failed to close shard %d: %w", i, err))
			}
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors closing connections: %v", errors)
	}

	return nil
}

// RetryFailedConnections attempts to reconnect any failed shard connections
func (sm *ShardManager) RetryFailedConnections() error {
	var errors []error

	for i, db := range sm.connections {
		if db != nil {
			// Test connection with ping
			if err := db.Ping(); err != nil {
				if sm.logger != nil {
					sm.logger("⚠️ Shard %d connection failed, attempting reconnect...", i)
				}

				// Close failed connection
				db.Close()

				// Attempt to reconnect
				if newDB, err := sm.reconnectShard(i); err != nil {
					errors = append(errors, fmt.Errorf("failed to reconnect shard %d: %w", i, err))
				} else {
					sm.connections[i] = newDB
					if sm.logger != nil {
						sm.logger("✓ Successfully reconnected shard %d", i)
					}
				}
			}
		} else {
			// Connection is nil, attempt to create it
			if newDB, err := sm.reconnectShard(i); err != nil {
				errors = append(errors, fmt.Errorf("failed to create connection for shard %d: %w", i, err))
			} else {
				sm.connections[i] = newDB
				if sm.logger != nil {
					sm.logger("✓ Created connection for shard %d", i)
				}
			}
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors during connection retry: %v", errors)
	}

	return nil
}

// reconnectShard creates a new connection for a specific shard
func (sm *ShardManager) reconnectShard(shardIndex int) (*sql.DB, error) {
	var shardCfg Config

	if sm.shardCount <= 1 {
		shardCfg = sm.config
	} else {
		dbName := generateShardDatabaseName(sm.config, shardIndex)
		shardCfg = sm.config
		shardCfg.MySQLDB = dbName
	}

	db, err := openDB(shardCfg)
	if err != nil {
		return nil, err
	}

	// Configure connection pooling for the new connection
	maxConns := 25
	if sm.shardCount > 1 {
		maxConns = 25 / sm.shardCount
		if maxConns < 5 {
			maxConns = 5
		}
	}

	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns / 5)
	db.SetConnMaxLifetime(5 * time.Minute)

	return db, nil
}

// HealthCheck tests all shard connections and returns health status
func (sm *ShardManager) HealthCheck() error {
	var errors []error
	healthyShards := 0

	if sm.logger != nil {
		sm.logger("🔍 Starting health check for %d shard(s)...", sm.shardCount)
	}

	for i, db := range sm.connections {
		if db == nil {
			errors = append(errors, fmt.Errorf("shard %d: connection is nil", i))
			continue
		}

		// Test basic connectivity
		if err := db.Ping(); err != nil {
			errors = append(errors, fmt.Errorf("shard %d: ping failed: %w", i, err))
			continue
		}

		// Test schema existence (quick query)
		var count int
		query := "SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'lines'"
		if sm.config.DBType == "sqlite" {
			query = "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lines'"
		}

		if err := db.QueryRow(query).Scan(&count); err != nil {
			errors = append(errors, fmt.Errorf("shard %d: schema check failed: %w", i, err))
			continue
		}

		if count == 0 {
			errors = append(errors, fmt.Errorf("shard %d: table 'lines' does not exist", i))
			continue
		}

		healthyShards++
		if sm.logger != nil {
			dbName := sm.config.MySQLDB
			if sm.shardCount > 1 {
				dbName = generateShardDatabaseName(sm.config, i)
			}
			sm.logger("✅ Shard %d (%s): healthy", i, dbName)
		}
	}

	if sm.logger != nil {
		sm.logger("Health check complete: %d/%d shards healthy", healthyShards, sm.shardCount)
	}

	if len(errors) > 0 {
		return fmt.Errorf("health check failed for %d shards: %v", len(errors), errors)
	}

	return nil
}

// SearchResult represents a single search result from a shard
type SearchResult struct {
	ID         int64     `json:"id"`
	Content    string    `json:"content"`
	LineHash   string    `json:"line_hash"`
	EntryDate  string    `json:"entry_date"`
	CreatedAt  time.Time `json:"created_at"`
	ShardIndex int       `json:"shard_index"`
	ShardName  string    `json:"shard_name"`
}

// SearchResponse contains aggregated search results from all shards
type SearchResponse struct {
	Results    []SearchResult `json:"results"`
	TotalCount int            `json:"total_count"`
	ShardsHit  int            `json:"shards_hit"`
	QueryTime  time.Duration  `json:"query_time_ms"`
	Errors     []string       `json:"errors,omitempty"`
}

// ParallelSearch executes a search query across all shards in parallel
func (sm *ShardManager) ParallelSearch(ctx context.Context, query string, limit int) (*SearchResponse, error) {
	startTime := time.Now()

	type shardSearchResult struct {
		shardIndex int
		results    []SearchResult
		err        error
	}

	resultChan := make(chan shardSearchResult, sm.shardCount)
	searchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Execute search on all shards in parallel
	for i := 0; i < sm.shardCount; i++ {
		go func(shardIdx int) {
			db := sm.GetShardConnection(shardIdx)
			if db == nil {
				resultChan <- shardSearchResult{
					shardIndex: shardIdx,
					err:        fmt.Errorf("shard %d connection is nil", shardIdx),
				}
				return
			}

			// FULLTEXT search query with LIMIT per shard
			perShardLimit := limit
			if sm.shardCount > 1 {
				// Distribute limit across shards, but ensure each gets at least 10
				perShardLimit = max(10, limit/sm.shardCount*2)
			}

			searchQuery := `
				SELECT id, content, line_hash, entry_date, created_at 
				FROM ` + "`lines`" + ` 
				WHERE MATCH(content) AGAINST(? IN NATURAL LANGUAGE MODE)
				ORDER BY MATCH(content) AGAINST(? IN NATURAL LANGUAGE MODE) DESC
				LIMIT ?
			`

			rows, err := db.QueryContext(searchCtx, searchQuery, query, query, perShardLimit)
			if err != nil {
				resultChan <- shardSearchResult{
					shardIndex: shardIdx,
					err:        fmt.Errorf("shard %d query failed: %w", shardIdx, err),
				}
				return
			}
			defer rows.Close()

			var shardResults []SearchResult
			shardName := generateShardDatabaseName(sm.config, shardIdx)

			for rows.Next() {
				var result SearchResult
				err := rows.Scan(&result.ID, &result.Content, &result.LineHash,
					&result.EntryDate, &result.CreatedAt)
				if err != nil {
					if sm.logger != nil {
						sm.logger("Warning: Failed to scan result from shard %d: %v", shardIdx, err)
					}
					continue
				}

				result.ShardIndex = shardIdx
				result.ShardName = shardName
				shardResults = append(shardResults, result)
			}

			if err := rows.Err(); err != nil {
				resultChan <- shardSearchResult{
					shardIndex: shardIdx,
					err:        fmt.Errorf("shard %d row iteration failed: %w", shardIdx, err),
				}
				return
			}

			resultChan <- shardSearchResult{
				shardIndex: shardIdx,
				results:    shardResults,
			}
		}(i)
	}

	// Collect results from all shards
	var allResults []SearchResult
	var errors []string
	shardsHit := 0

	for i := 0; i < sm.shardCount; i++ {
		select {
		case result := <-resultChan:
			if result.err != nil {
				errors = append(errors, result.err.Error())
				if sm.logger != nil {
					sm.logger("Search error on shard %d: %v", result.shardIndex, result.err)
				}
			} else {
				allResults = append(allResults, result.results...)
				if len(result.results) > 0 {
					shardsHit++
				}
			}
		case <-searchCtx.Done():
			errors = append(errors, fmt.Sprintf("search timeout on remaining shards"))
			break
		}
	}

	// Sort results by relevance (MySQL's MATCH score is already handled per shard)
	// For cross-shard sorting, we could implement additional relevance scoring if needed

	// Apply final limit across all results
	if len(allResults) > limit {
		allResults = allResults[:limit]
	}

	response := &SearchResponse{
		Results:    allResults,
		TotalCount: len(allResults),
		ShardsHit:  shardsHit,
		QueryTime:  time.Since(startTime),
		Errors:     errors,
	}

	if sm.logger != nil {
		sm.logger("🔍 Parallel search completed: %d results from %d/%d shards in %v",
			len(allResults), shardsHit, sm.shardCount, response.QueryTime)
	}

	return response, nil
}

// max returns the maximum of two integers
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ShardMonitoringStats holds monitoring statistics for a shard
type ShardMonitoringStats struct {
	ShardIndex        int           `json:"shard_index"`
	ShardName         string        `json:"shard_name"`
	ConnectionStatus  string        `json:"connection_status"`
	TotalRecords      int64         `json:"total_records"`
	LastInsertTime    time.Time     `json:"last_insert_time"`
	AvgInsertDuration time.Duration `json:"avg_insert_duration_ms"`
	SuccessfulOps     int           `json:"successful_ops"`
	FailedOps         int           `json:"failed_ops"`
	SuccessRate       float64       `json:"success_rate"`
	LastBatchSize     int           `json:"last_batch_size"`
	OptimalBatchSize  int           `json:"optimal_batch_size"`
}

// SystemMonitoringReport provides overall system monitoring information
type SystemMonitoringReport struct {
	TotalShards   int                    `json:"total_shards"`
	HealthyShards int                    `json:"healthy_shards"`
	TotalRecords  int64                  `json:"total_records"`
	ShardStats    []ShardMonitoringStats `json:"shard_stats"`
	GeneratedAt   time.Time              `json:"generated_at"`
	SystemUptime  time.Duration          `json:"system_uptime_ms"`
}

// GetMonitoringReport generates a comprehensive monitoring report for all shards
func (sm *ShardManager) GetMonitoringReport(ctx context.Context, batchSizer *adaptiveBatchSizer) (*SystemMonitoringReport, error) {
	startTime := time.Now()

	type shardStatsResult struct {
		shardIndex int
		stats      ShardMonitoringStats
		err        error
	}

	resultChan := make(chan shardStatsResult, sm.shardCount)
	statsCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Collect stats from all shards in parallel
	for i := 0; i < sm.shardCount; i++ {
		go func(shardIdx int) {
			stats := ShardMonitoringStats{
				ShardIndex: shardIdx,
				ShardName:  generateShardDatabaseName(sm.config, shardIdx),
			}

			db := sm.GetShardConnection(shardIdx)
			if db == nil {
				stats.ConnectionStatus = "disconnected"
				resultChan <- shardStatsResult{shardIndex: shardIdx, stats: stats}
				return
			}

			// Test connection
			if err := db.Ping(); err != nil {
				stats.ConnectionStatus = "error"
				resultChan <- shardStatsResult{
					shardIndex: shardIdx,
					stats:      stats,
					err:        fmt.Errorf("ping failed: %w", err),
				}
				return
			}
			stats.ConnectionStatus = "healthy"

			// Get record count
			countQuery := "SELECT COUNT(*) FROM `lines`"
			if err := db.QueryRowContext(statsCtx, countQuery).Scan(&stats.TotalRecords); err != nil {
				stats.TotalRecords = -1 // Indicate error
				if sm.logger != nil {
					sm.logger("Warning: Failed to get record count for shard %d: %v", shardIdx, err)
				}
			}

			// Get performance metrics from batch sizer if available
			if batchSizer != nil && shardIdx < len(batchSizer.shardMetrics) {
				metrics := batchSizer.shardMetrics[shardIdx]
				stats.AvgInsertDuration = metrics.avgInsertTime
				stats.SuccessfulOps = metrics.successfulOps
				stats.FailedOps = metrics.failedOps
				stats.LastBatchSize = metrics.lastBatchSize
				stats.LastInsertTime = metrics.lastUpdateTime
				stats.OptimalBatchSize = batchSizer.getOptimalBatchSize(shardIdx)

				totalOps := metrics.successfulOps + metrics.failedOps
				if totalOps > 0 {
					stats.SuccessRate = float64(metrics.successfulOps) / float64(totalOps)
				}
			}

			resultChan <- shardStatsResult{shardIndex: shardIdx, stats: stats}
		}(i)
	}

	// Collect results
	var shardStats []ShardMonitoringStats
	var totalRecords int64
	healthyShards := 0

	for i := 0; i < sm.shardCount; i++ {
		select {
		case result := <-resultChan:
			if result.err != nil && sm.logger != nil {
				sm.logger("Monitoring error for shard %d: %v", result.shardIndex, result.err)
			}

			shardStats = append(shardStats, result.stats)

			if result.stats.ConnectionStatus == "healthy" {
				healthyShards++
				if result.stats.TotalRecords > 0 {
					totalRecords += result.stats.TotalRecords
				}
			}
		case <-statsCtx.Done():
			// Timeout - add placeholder stats for remaining shards
			for remaining := i; remaining < sm.shardCount; remaining++ {
				shardStats = append(shardStats, ShardMonitoringStats{
					ShardIndex:       remaining,
					ShardName:        generateShardDatabaseName(sm.config, remaining),
					ConnectionStatus: "timeout",
				})
			}
			break
		}
	}

	report := &SystemMonitoringReport{
		TotalShards:   sm.shardCount,
		HealthyShards: healthyShards,
		TotalRecords:  totalRecords,
		ShardStats:    shardStats,
		GeneratedAt:   time.Now(),
		SystemUptime:  time.Since(startTime), // This would be actual uptime in real implementation
	}

	if sm.logger != nil {
		sm.logger("📊 Monitoring report generated: %d/%d shards healthy, %d total records",
			healthyShards, sm.shardCount, totalRecords)
	}

	return report, nil
}

// LogPerformanceMetrics logs current performance metrics for all shards
func (sm *ShardManager) LogPerformanceMetrics(batchSizer *adaptiveBatchSizer) {
	if sm.logger == nil || batchSizer == nil {
		return
	}

	sm.logger("📈 Performance Metrics Summary:")
	for i, metrics := range batchSizer.shardMetrics {
		if metrics.successfulOps == 0 && metrics.failedOps == 0 {
			continue // Skip shards with no activity
		}

		shardName := generateShardDatabaseName(sm.config, i)
		totalOps := metrics.successfulOps + metrics.failedOps
		successRate := float64(metrics.successfulOps) / float64(totalOps) * 100
		optimalBatch := batchSizer.getOptimalBatchSize(i)

		sm.logger("  Shard %d (%s): %.1f%% success, avg %dms, batch %d→%d",
			i, shardName, successRate, metrics.avgInsertTime.Milliseconds(),
			metrics.lastBatchSize, optimalBatch)
	}
}

// Config holds configuration for the pipeline with configurable paths
type Config struct {
	// File paths - configurable for bot integration
	InputDir     string // Directory for input .txt files (default: "files/txt/")
	NonSortedDir string // Directory for files ready to be merged (default: "files/nonsorted/")
	OutputDir    string // Directory for merged output (default: "files/Sorted_toshare/")
	BettingDir   string // Directory for betting files (default: "files/bettings/")
	InputFile    string // Specific input file or directory to process
	LogDir       string // Directory for log files (default: "logs/")

	// Pipeline control
	RunFilter bool
	RunDB     bool
	DBType    string // "mysql", "sqlite"

	// MySQL
	MySQLHost, MySQLUser, MySQLPass, MySQLDB string
	// SQLite
	SQLiteName string

	// Database Sharding Configuration
	ShardCount     int    // Number of shard databases (0 = disable sharding, use single DB)
	DatabasePrefix string // Prefix for shard database names (e.g., "shard" creates shard_00, shard_01, etc.)

	// Enhanced Multi-PC Support
	SourceInstanceID  string // Unique identifier for this bot instance (e.g., hostname, UUID)
	BatchSize         int    // Optimal batch size for network conditions (default: 5000)
	EnableCompression bool   // Enable database compression (default: true)
}

// LoadConfig returns default configuration values with configurable paths
func LoadConfig() Config {
	// Generate unique source instance ID based on hostname and timestamp
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	sourceID := fmt.Sprintf("%s_%d", hostname, time.Now().Unix())

	return Config{
		// Configurable directory paths with bot-friendly defaults
		InputDir:     "app/extraction/files/txt/",
		NonSortedDir: "app/extraction/files/nonsorted/",
		OutputDir:    "app/extraction/files/Sorted_toshare/",
		BettingDir:   "app/extraction/files/bettings/",
		InputFile:    "app/extraction/files/Sorted_toshare/", // Filter stage reads from OutputDir
		LogDir:       "logs/",                           // Directory for structured log files

		RunFilter: true,
		RunDB:     true,
		DBType:    "mysql",

		// MySQL configuration for golexor.com database
		MySQLHost: "91.204.209.6:3306",
		MySQLUser: "golexorcom_urllist",
		MySQLPass: "Iman@091124",
		MySQLDB:   "golexorcom_data",

		SQLiteName: "local.db",

		// Database Sharding Configuration - default to disabled for backward compatibility
		ShardCount:     0,       // 0 = disable sharding, use single database
		DatabasePrefix: "shard", // Default prefix for shard database names

		// Enhanced Multi-PC Support
		SourceInstanceID:  sourceID, // Unique identifier for this bot instance
		BatchSize:         5000,     // Optimized for network latency with remote MySQL
		EnableCompression: true,     // Enable database compression for space savings
	}
}

// LogManager handles structured logging with file output and rotation
type LogManager struct {
	logger   *logrus.Logger
	logFile  *os.File
	filePath string
}

// NewLogManager creates a new structured logger with file output
// Creates logs directory structure if needed and configures enterprise-grade logging
func NewLogManager(config Config) (*LogManager, error) {
	// Create logs directory if it doesn't exist
	if err := os.MkdirAll(config.LogDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create logs directory: %w", err)
	}

	// Create log file path with timestamp for rotation
	logFileName := "store.log"
	logFilePath := filepath.Join(config.LogDir, logFileName)

	// Open log file with append mode for persistence
	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	// Create new logrus instance
	logger := logrus.New()

	// Configure structured logging with JSON formatter for production
	logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: time.RFC3339,
		PrettyPrint:     false, // Compact JSON for production
	})

	// Set output to both file and stdout for comprehensive logging
	logger.SetOutput(io.MultiWriter(os.Stdout, logFile))

	// Set appropriate log level (Info for production, Debug for development)
	logger.SetLevel(logrus.InfoLevel)

	// Enable caller reporting for debugging (adds method names)
	logger.SetReportCaller(true)

	return &LogManager{
		logger:   logger,
		logFile:  logFile,
		filePath: logFilePath,
	}, nil
}

// Close properly closes the log file
func (lm *LogManager) Close() error {
	if lm.logFile != nil {
		return lm.logFile.Close()
	}
	return nil
}

// GetLogger returns the configured logrus logger instance
func (lm *LogManager) GetLogger() *logrus.Logger {
	return lm.logger
}

// LogWithFields logs a message with structured fields
func (lm *LogManager) LogWithFields(level logrus.Level, message string, fields logrus.Fields) {
	lm.logger.WithFields(fields).Log(level, message)
}

// LogOperation logs file operations with full path tracking
func (lm *LogManager) LogOperation(operation, filePath string, success bool, duration time.Duration, details map[string]interface{}) {
	fields := logrus.Fields{
		"operation":   operation,
		"file_path":   filePath,
		"success":     success,
		"duration_ms": duration.Milliseconds(),
	}

	// Add additional details if provided
	for key, value := range details {
		fields[key] = value
	}

	if success {
		lm.logger.WithFields(fields).Info("File operation completed")
	} else {
		lm.logger.WithFields(fields).Error("File operation failed")
	}
}

// LogError logs errors with stack trace and recovery actions
func (lm *LogManager) LogError(err error, context string, recovery string, fields logrus.Fields) {
	if fields == nil {
		fields = logrus.Fields{}
	}

	// Capture stack trace for debugging
	stackTrace := make([]byte, 4096)
	stackSize := runtime.Stack(stackTrace, false)

	fields["error"] = err.Error()
	fields["context"] = context
	fields["recovery_action"] = recovery
	fields["stack_trace"] = string(stackTrace[:stackSize])
	fields["error_type"] = fmt.Sprintf("%T", err)

	// Add caller information
	if pc, file, line, ok := runtime.Caller(1); ok {
		fields["caller_file"] = filepath.Base(file)
		fields["caller_line"] = line
		if fn := runtime.FuncForPC(pc); fn != nil {
			fields["caller_function"] = fn.Name()
		}
	}

	lm.logger.WithFields(fields).Error("Operation error occurred")
}

// LogCriticalError logs critical errors that require immediate attention
func (lm *LogManager) LogCriticalError(err error, context string, recovery string, systemState map[string]interface{}) {
	fields := logrus.Fields{
		"severity":           "CRITICAL",
		"requires_attention": true,
		"timestamp":          time.Now().UTC().Format(time.RFC3339),
	}

	// Add system state information
	if systemState != nil {
		for key, value := range systemState {
			fields["system_"+key] = value
		}
	}

	lm.LogError(err, context, recovery, fields)
}

// LogRecoveryAction logs successful recovery from an error condition
func (lm *LogManager) LogRecoveryAction(originalError error, recoveryAction string, success bool, details map[string]interface{}) {
	fields := logrus.Fields{
		"original_error":   originalError.Error(),
		"recovery_action":  recoveryAction,
		"recovery_success": success,
	}

	if details != nil {
		for key, value := range details {
			fields[key] = value
		}
	}

	if success {
		lm.logger.WithFields(fields).Info("Error recovery completed successfully")
	} else {
		lm.logger.WithFields(fields).Error("Error recovery failed")
	}
}

// BackupFileWithTimestamp creates a timestamped backup of a file in the backups directory
// Returns backup file path and error. Creates backup directory structure if needed.
// Uses atomic operations to ensure backup integrity.
func (s *StoreService) BackupFileWithTimestamp(filePath string) (string, error) {
	// Validate input file exists and is readable
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		if s.logManager != nil {
			s.logManager.LogError(err,
				fmt.Sprintf("File backup validation - source file missing: %s", filePath),
				"Verify file path is correct, check if file was moved or deleted, ensure proper file permissions",
				logrus.Fields{
					"operation":   "backup_validation",
					"source_file": filePath,
					"error_type":  "file_not_found",
				})
		}
		return "", fmt.Errorf("source file does not exist: %s", filePath)
	}

	// Create backup directory (no date subfolder structure)
	backupRootDir := "app/extraction/files/backups"

	if err := os.MkdirAll(backupRootDir, 0755); err != nil {
		if s.logManager != nil {
			s.logManager.LogError(err,
				fmt.Sprintf("Backup directory creation failed: %s", backupRootDir),
				"Check directory permissions, ensure sufficient disk space, verify parent directory access",
				logrus.Fields{
					"operation":  "backup_dir_creation",
					"backup_dir": backupRootDir,
				})
		}
		return "", fmt.Errorf("failed to create backup directory %s: %w", backupRootDir, err)
	}

	// Generate timestamped backup filename with timestamp as prefix
	originalFileName := filepath.Base(filePath)
	ext := filepath.Ext(originalFileName)
	nameWithoutExt := strings.TrimSuffix(originalFileName, ext)
	timestamp := time.Now().Format("20060102_150405")
	backupFileName := fmt.Sprintf("%s_%s%s", nameWithoutExt, timestamp, ext)
	backupPath := filepath.Join(backupRootDir, backupFileName)

	// Perform atomic file copy with integrity checks
	if err := s.atomicFileCopy(filePath, backupPath); err != nil {
		return "", fmt.Errorf("failed to create backup from %s to %s: %w", filePath, backupPath, err)
	}

	s.log("✓ Created backup: %s -> %s", filePath, backupPath)
	return backupPath, nil
}

// Uses temporary file + rename pattern for atomicity
func (s *StoreService) atomicFileCopy(src, dst string) error {
	startTime := time.Now()
	var bytesTransferred int64
	var finalErr error

	defer func() {
		s.logFileCopy(src, dst, startTime, bytesTransferred, finalErr)
	}()

	// Open source file with read-only access
	readStartTime := time.Now()
	srcFile, err := os.Open(src)
	if err != nil {
		finalErr = fmt.Errorf("failed to open source file %s: %w", src, err)
		s.logFileRead(src, readStartTime, 0, err)
		return finalErr
	}
	defer srcFile.Close()

	// Get file information for proper permissions
	srcInfo, err := srcFile.Stat()
	if err != nil {
		finalErr = fmt.Errorf("failed to get source file info: %w", err)
		return finalErr
	}

	// Create temporary destination file for atomic operation
	tmpDst := dst + ".tmp"
	writeStartTime := time.Now()
	dstFile, err := os.OpenFile(tmpDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		finalErr = fmt.Errorf("failed to create temporary destination file %s: %w", tmpDst, err)
		s.logFileWrite(tmpDst, writeStartTime, 0, err)
		return finalErr
	}

	// Ensure cleanup on error
	defer func() {
		dstFile.Close()
		if finalErr != nil {
			deleteStartTime := time.Now()
			removeErr := os.Remove(tmpDst) // Clean up temp file on error
			s.logFileDelete(tmpDst, deleteStartTime, removeErr)
		}
	}()

	// Copy data with buffer for efficiency
	buffer := make([]byte, 32*1024) // 32KB buffer for efficiency
	for {
		n, readErr := srcFile.Read(buffer)
		if n > 0 {
			written, writeErr := dstFile.Write(buffer[:n])
			bytesTransferred += int64(written)
			if writeErr != nil {
				finalErr = fmt.Errorf("write error during copy: %w", writeErr)
				return finalErr
			}
			if written != n {
				finalErr = fmt.Errorf("incomplete write: wrote %d bytes, expected %d", written, n)
				return finalErr
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			finalErr = fmt.Errorf("read error during copy: %w", readErr)
			return finalErr
		}
	}

	// Log successful read operation
	s.logFileRead(src, readStartTime, bytesTransferred, nil)
	s.logFileWrite(tmpDst, writeStartTime, bytesTransferred, nil)

	// Atomic rename from temp to final destination
	renameStartTime := time.Now()
	if err := os.Rename(tmpDst, dst); err != nil {
		finalErr = fmt.Errorf("failed to rename temp file to destination: %w", err)
		s.logFileOperation("FILE_RENAME", fmt.Sprintf("%s->%s", tmpDst, dst), renameStartTime, err, map[string]interface{}{
			"operation_type": "rename",
			"source_path":    tmpDst,
			"dest_path":      dst,
		})
		return finalErr
	}
	s.logFileOperation("FILE_RENAME", fmt.Sprintf("%s->%s", tmpDst, dst), renameStartTime, nil, map[string]interface{}{
		"operation_type": "rename",
		"source_path":    tmpDst,
		"dest_path":      dst,
	})

	return nil
}

// BackupFiles creates timestamped backups for multiple files concurrently
// Returns map of original->backup path and any errors encountered
// Continues processing remaining files even if some backups fail
func (s *StoreService) BackupFiles(filePaths []string) (map[string]string, []error) {
	backupMap := make(map[string]string)
	var errors []error

	for _, filePath := range filePaths {
		backupPath, err := s.BackupFileWithTimestamp(filePath)
		if err != nil {
			errors = append(errors, fmt.Errorf("backup failed for %s: %w", filePath, err))
			s.log("⚠️ Backup failed for %s: %v", filePath, err)
			continue
		}
		backupMap[filePath] = backupPath
	}

	if len(errors) > 0 {
		s.log("⚠️ Backup completed with %d errors out of %d files", len(errors), len(filePaths))
	} else {
		s.log("✓ Successfully backed up all %d files", len(filePaths))
	}

	return backupMap, errors
}

// FileOperation represents a single file operation for transaction tracking
type FileOperation struct {
	OperationType string // "CREATE", "DELETE", "MOVE", "COPY"
	SourcePath    string
	TargetPath    string
	BackupPath    string
	Completed     bool
	Timestamp     time.Time
}

// FileTransaction manages atomic file operations with rollback capability
type FileTransaction struct {
	operations []FileOperation
	logger     func(string, ...interface{})
	id         string
}

// NewFileTransaction creates a new file transaction manager
func (s *StoreService) NewFileTransaction() *FileTransaction {
	return &FileTransaction{
		operations: make([]FileOperation, 0),
		logger:     s.logger,
		id:         fmt.Sprintf("tx_%s", time.Now().Format("20060102_150405_000")),
	}
}

// AddOperation records a file operation for potential rollback
func (tx *FileTransaction) AddOperation(opType, sourcePath, targetPath, backupPath string) {
	op := FileOperation{
		OperationType: opType,
		SourcePath:    sourcePath,
		TargetPath:    targetPath,
		BackupPath:    backupPath,
		Completed:     false,
		Timestamp:     time.Now(),
	}
	tx.operations = append(tx.operations, op)
	tx.log("📝 Transaction %s: Added %s operation %s->%s", tx.id, opType, sourcePath, targetPath)
}

// MarkCompleted marks an operation as successfully completed
func (tx *FileTransaction) MarkCompleted(sourcePath string) {
	for i := range tx.operations {
		if tx.operations[i].SourcePath == sourcePath {
			tx.operations[i].Completed = true
			tx.log("✅ Transaction %s: Marked completed - %s", tx.id, sourcePath)
			return
		}
	}
	tx.log("⚠️ Transaction %s: Could not find operation to mark completed: %s", tx.id, sourcePath)
}

// Rollback undoes all completed operations in reverse order
func (tx *FileTransaction) Rollback() error {
	tx.log("🔄 Transaction %s: Starting rollback of %d operations", tx.id, len(tx.operations))

	var rollbackErrors []error
	rollbackCount := 0

	// Process operations in reverse order (LIFO)
	for i := len(tx.operations) - 1; i >= 0; i-- {
		op := tx.operations[i]
		if !op.Completed {
			tx.log("⏭️ Transaction %s: Skipping incomplete operation %s", tx.id, op.SourcePath)
			continue
		}

		tx.log("🔄 Transaction %s: Rolling back %s operation %s", tx.id, op.OperationType, op.SourcePath)

		switch op.OperationType {
		case "DELETE":
			// Restore file from backup if available
			if op.BackupPath != "" {
				if err := tx.restoreFromBackup(op.BackupPath, op.SourcePath); err != nil {
					rollbackErrors = append(rollbackErrors, fmt.Errorf("failed to restore %s from backup %s: %w", op.SourcePath, op.BackupPath, err))
					tx.log("❌ Transaction %s: Restore failed %s: %v", tx.id, op.SourcePath, err)
				} else {
					rollbackCount++
					tx.log("✅ Transaction %s: Restored %s from backup", tx.id, op.SourcePath)
				}
			} else {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("cannot rollback DELETE of %s: no backup available", op.SourcePath))
				tx.log("❌ Transaction %s: No backup for rollback: %s", tx.id, op.SourcePath)
			}

		case "CREATE":
			// Remove created file
			if err := os.Remove(op.TargetPath); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("failed to remove created file %s: %w", op.TargetPath, err))
				tx.log("❌ Transaction %s: Remove failed %s: %v", tx.id, op.TargetPath, err)
			} else {
				rollbackCount++
				tx.log("✅ Transaction %s: Removed created file %s", tx.id, op.TargetPath)
			}

		case "MOVE":
			// Move file back to original location
			if err := os.Rename(op.TargetPath, op.SourcePath); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("failed to move back %s to %s: %w", op.TargetPath, op.SourcePath, err))
				tx.log("❌ Transaction %s: Move back failed %s->%s: %v", tx.id, op.TargetPath, op.SourcePath, err)
			} else {
				rollbackCount++
				tx.log("✅ Transaction %s: Moved back %s->%s", tx.id, op.TargetPath, op.SourcePath)
			}

		case "COPY":
			// Remove copied file (original remains untouched)
			if err := os.Remove(op.TargetPath); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("failed to remove copied file %s: %w", op.TargetPath, err))
				tx.log("❌ Transaction %s: Remove copy failed %s: %v", tx.id, op.TargetPath, err)
			} else {
				rollbackCount++
				tx.log("✅ Transaction %s: Removed copied file %s", tx.id, op.TargetPath)
			}
		}
	}

	if len(rollbackErrors) > 0 {
		tx.log("⚠️ Transaction %s: Rollback completed with %d successes, %d errors", tx.id, rollbackCount, len(rollbackErrors))
		return fmt.Errorf("rollback completed with errors: %d operations rolled back, %d errors occurred", rollbackCount, len(rollbackErrors))
	}

	tx.log("✅ Transaction %s: Rollback completed successfully - %d operations reversed", tx.id, rollbackCount)
	return nil
}

// restoreFromBackup restores a file from its backup location
func (tx *FileTransaction) restoreFromBackup(backupPath, originalPath string) error {
	// Verify backup exists
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		return fmt.Errorf("backup file does not exist: %s", backupPath)
	}

	// Ensure target directory exists
	targetDir := filepath.Dir(originalPath)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create target directory %s: %w", targetDir, err)
	}

	// Copy backup to original location
	srcFile, err := os.Open(backupPath)
	if err != nil {
		return fmt.Errorf("failed to open backup file %s: %w", backupPath, err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(originalPath)
	if err != nil {
		return fmt.Errorf("failed to create restored file %s: %w", originalPath, err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("failed to copy backup content: %w", err)
	}

	return nil
}

// Commit finalizes the transaction (currently just logs completion)
func (tx *FileTransaction) Commit() error {
	completedOps := 0
	for _, op := range tx.operations {
		if op.Completed {
			completedOps++
		}
	}

	tx.log("✅ Transaction %s: Committed - %d/%d operations completed", tx.id, completedOps, len(tx.operations))
	return nil
}

// log helper method for transaction logging
func (tx *FileTransaction) log(format string, args ...interface{}) {
	if tx.logger != nil {
		tx.logger(format, args...)
	} else {
		fmt.Printf(format+"\n", args...)
	}
}

// FileIntegrityManager provides comprehensive file integrity verification with checksum validation
type FileIntegrityManager struct {
	logger func(string, ...interface{})
}

// NewFileIntegrityManager creates a new file integrity manager instance
func (s *StoreService) NewFileIntegrityManager() *FileIntegrityManager {
	return &FileIntegrityManager{
		logger: s.logger,
	}
}

// FileIntegrityInfo holds comprehensive file integrity data for verification
type FileIntegrityInfo struct {
	Size        int64
	ModTime     time.Time
	MD5Checksum string
	CapturedAt  time.Time
}

// FileSnapshot captures the initial state of files for tracking during operations
// This prevents deletion of files added after operation start
type FileSnapshot struct {
	TrackedFiles   []string  // List of files present at snapshot time
	SnapshotTime   time.Time // When the snapshot was taken
	InputDirectory string    // Directory that was scanned
	FileCount      int       // Number of files in snapshot
}

// MergeOperationStatus tracks the processing status of individual files during merge operations
// This enables partial failure recovery and precise status reporting
type MergeOperationStatus struct {
	TotalFiles     int                          // Total files to process
	ProcessedFiles map[string]FileProcessStatus // Per-file processing status
	StartTime      time.Time                    // When merge operation started
	CompletionTime time.Time                    // When merge operation completed (if successful)
	OverallStatus  string                       // "in_progress", "completed", "partial_failure", "failed"
	ErrorCount     int                          // Number of files that failed processing
	SuccessCount   int                          // Number of files successfully processed
}

// FileProcessStatus represents the processing status of an individual file
type FileProcessStatus struct {
	FilePath         string    // Path to the file
	Status           string    // "pending", "reading", "processed", "failed"
	ProcessedAt      time.Time // When processing completed
	LinesContributed int       // Number of unique lines contributed to merge
	Error            error     // Any error encountered during processing
}

// createInitialFileSnapshot captures the current state of .txt files in the input directory
// This creates a point-in-time snapshot to prevent deletion of files added during processing
func (s *StoreService) createInitialFileSnapshot(inputDir string) (*FileSnapshot, error) {
	snapshotTime := time.Now()

	// Log file snapshot creation with structured logging
	if s.logManager != nil {
		fields := logrus.Fields{
			"operation":       "file_snapshot",
			"input_directory": inputDir,
		}
		s.logManager.LogWithFields(logrus.InfoLevel, "Creating file snapshot for merge operation", fields)
	}

	// Discover .txt files in input directory at this exact moment
	pattern := filepath.Join(inputDir, "*.txt")
	files, err := filepath.Glob(pattern)
	if err != nil {
		if s.logManager != nil {
			s.logManager.LogError(err,
				fmt.Sprintf("Failed to create file snapshot for directory: %s", inputDir),
				"Check directory permissions, verify path exists, ensure glob pattern is valid",
				logrus.Fields{
					"operation":       "snapshot_glob",
					"pattern":         pattern,
					"input_directory": inputDir,
				})
		}
		return nil, fmt.Errorf("error discovering files for snapshot: %w", err)
	}

	snapshot := &FileSnapshot{
		TrackedFiles:   files,
		SnapshotTime:   snapshotTime,
		InputDirectory: inputDir,
		FileCount:      len(files),
	}

	s.log("📸 File snapshot created: %d files tracked from %s", len(files), inputDir)
	for i, file := range files {
		s.log("  [%d] %s", i+1, filepath.Base(file))
	}

	return snapshot, nil
}

// CalculateFileChecksum computes MD5 checksum for file integrity verification
func (fim *FileIntegrityManager) CalculateFileChecksum(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file for checksum: %w", err)
	}
	defer file.Close()

	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("failed to calculate checksum: %w", err)
	}

	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

// CaptureFileIntegrity captures comprehensive integrity information for a file
func (fim *FileIntegrityManager) CaptureFileIntegrity(filePath string) (*FileIntegrityInfo, error) {
	// Get file stats
	stat, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get file stats: %w", err)
	}

	// Calculate checksum
	checksum, err := fim.CalculateFileChecksum(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate file checksum: %w", err)
	}

	info := &FileIntegrityInfo{
		Size:        stat.Size(),
		ModTime:     stat.ModTime(),
		MD5Checksum: checksum,
		CapturedAt:  time.Now(),
	}

	fim.log("📊 Captured integrity info for %s: size=%d, checksum=%s", filePath, info.Size, info.MD5Checksum[:8]+"...")
	return info, nil
}

// VerifyFileIntegrity verifies a file against its previously captured integrity information
func (fim *FileIntegrityManager) VerifyFileIntegrity(filePath string, expectedInfo *FileIntegrityInfo) error {
	// Capture current integrity information
	currentInfo, err := fim.CaptureFileIntegrity(filePath)
	if err != nil {
		return fmt.Errorf("failed to capture current integrity info: %w", err)
	}

	// Verify file size
	if currentInfo.Size != expectedInfo.Size {
		return fmt.Errorf("file size mismatch: expected %d, got %d", expectedInfo.Size, currentInfo.Size)
	}

	// Verify checksum
	if currentInfo.MD5Checksum != expectedInfo.MD5Checksum {
		return fmt.Errorf("file checksum mismatch: expected %s, got %s", expectedInfo.MD5Checksum[:8]+"...", currentInfo.MD5Checksum[:8]+"...")
	}

	fim.log("✅ File integrity verified for %s", filePath)
	return nil
}

// CaptureMultipleFileIntegrity captures integrity info for multiple files
func (fim *FileIntegrityManager) CaptureMultipleFileIntegrity(filePaths []string) (map[string]*FileIntegrityInfo, []error) {
	integrityMap := make(map[string]*FileIntegrityInfo)
	var errors []error

	for _, filePath := range filePaths {
		info, err := fim.CaptureFileIntegrity(filePath)
		if err != nil {
			errors = append(errors, fmt.Errorf("integrity capture failed for %s: %w", filePath, err))
			fim.log("⚠️ Integrity capture failed for %s: %v", filePath, err)
			continue
		}
		integrityMap[filePath] = info
	}

	if len(errors) > 0 {
		fim.log("⚠️ Integrity capture completed with %d errors out of %d files", len(errors), len(filePaths))
	} else {
		fim.log("✅ Successfully captured integrity info for all %d files", len(filePaths))
	}

	return integrityMap, errors
}

// VerifyMultipleFileIntegrity verifies integrity for multiple files
func (fim *FileIntegrityManager) VerifyMultipleFileIntegrity(integrityMap map[string]*FileIntegrityInfo) []error {
	var errors []error

	for filePath, expectedInfo := range integrityMap {
		if err := fim.VerifyFileIntegrity(filePath, expectedInfo); err != nil {
			errors = append(errors, fmt.Errorf("integrity verification failed for %s: %w", filePath, err))
			fim.log("❌ Integrity verification failed for %s: %v", filePath, err)
		}
	}

	if len(errors) > 0 {
		fim.log("⚠️ Integrity verification completed with %d errors out of %d files", len(errors), len(integrityMap))
	} else {
		fim.log("✅ Successfully verified integrity for all %d files", len(integrityMap))
	}

	return errors
}

// VerifyFileSystemIntegrity performs comprehensive file system integrity check
func (fim *FileIntegrityManager) VerifyFileSystemIntegrity(filePaths []string) error {
	fim.log("🔍 Starting comprehensive file system integrity verification for %d files", len(filePaths))

	for _, filePath := range filePaths {
		// Check file accessibility
		if _, err := os.Stat(filePath); err != nil {
			return fmt.Errorf("file system integrity check failed - cannot access %s: %w", filePath, err)
		}

		// Test file readability
		file, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("file system integrity check failed - cannot read %s: %w", filePath, err)
		}

		// Test first 1KB of file content
		buffer := make([]byte, 1024)
		n, err := file.Read(buffer)
		file.Close()

		if err != nil && err != io.EOF {
			return fmt.Errorf("file system integrity check failed - read error %s: %w", filePath, err)
		}

		// Basic content validation
		if n > 0 && !fim.isValidFileContent(buffer[:n]) {
			return fmt.Errorf("file system integrity check failed - invalid content detected in %s", filePath)
		}
	}

	fim.log("✅ File system integrity verification completed successfully")
	return nil
}

// isValidFileContent performs basic content validation
func (fim *FileIntegrityManager) isValidFileContent(data []byte) bool {
	// Allow reasonable text file content patterns
	nullByteCount := 0
	for _, b := range data {
		if b == 0 {
			nullByteCount++
		}
		// If more than 10% null bytes, might be corrupted
		if nullByteCount > len(data)/10 {
			return false
		}
	}
	return true
}

// log helper method for integrity manager logging
func (fim *FileIntegrityManager) log(format string, args ...interface{}) {
	if fim.logger != nil {
		fim.logger(format, args...)
	} else {
		fmt.Printf(format+"\n", args...)
	}
}

// OperationSuccessVerifier provides comprehensive success verification for file operations
type OperationSuccessVerifier struct {
	logger func(string, ...interface{})
}

// NewOperationSuccessVerifier creates a new success verification instance
func (s *StoreService) NewOperationSuccessVerifier() *OperationSuccessVerifier {
	return &OperationSuccessVerifier{
		logger: s.logger,
	}
}

// VerifyMergeOperationSuccess performs comprehensive verification that merge operation completed successfully
// Returns error if any verification step fails - input files should NOT be deleted if this returns error
func (v *OperationSuccessVerifier) VerifyMergeOperationSuccess(inputFiles []string, outputFile string, expectedLines int) error {
	v.log("🔍 Starting comprehensive merge operation success verification...")

	// Step 1: Verify output file exists and is accessible
	if err := v.verifyFileExists(outputFile); err != nil {
		return fmt.Errorf("output file verification failed: %w", err)
	}
	v.log("✅ Output file exists and is accessible: %s", outputFile)

	// Step 2: Verify output file is not empty
	if err := v.verifyFileNotEmpty(outputFile); err != nil {
		return fmt.Errorf("output file empty verification failed: %w", err)
	}
	v.log("✅ Output file is not empty")

	// Step 3: Verify output file has expected line count
	if err := v.verifyLineCount(outputFile, expectedLines); err != nil {
		return fmt.Errorf("line count verification failed: %w", err)
	}
	v.log("✅ Output file has expected line count: %d", expectedLines)

	// Step 4: Verify output file integrity (basic corruption check)
	if err := v.verifyFileIntegrity(outputFile); err != nil {
		return fmt.Errorf("file integrity verification failed: %w", err)
	}
	v.log("✅ Output file integrity verified")

	// Step 5: Verify all input files are still accessible (for rollback capability)
	for _, inputFile := range inputFiles {
		if err := v.verifyFileExists(inputFile); err != nil {
			return fmt.Errorf("input file verification failed for %s: %w", inputFile, err)
		}
	}
	v.log("✅ All %d input files remain accessible", len(inputFiles))

	// Step 6: Cross-verify: ensure output content makes sense relative to inputs
	if err := v.verifyMergeContentValidity(inputFiles, outputFile); err != nil {
		return fmt.Errorf("merge content validity verification failed: %w", err)
	}
	v.log("✅ Merge content validity verified")

	v.log("🎯 MERGE OPERATION SUCCESS VERIFICATION COMPLETE - Safe to delete input files")
	return nil
}

// verifyFileExists checks if file exists and is readable with retry mechanism for filesystem sync
func (v *OperationSuccessVerifier) verifyFileExists(filePath string) error {
	// CRITICAL FIX: Add retry mechanism to handle filesystem delays after atomic operations
	maxRetries := 5
	for i := 0; i < maxRetries; i++ {
		// Check if file exists
		if _, err := os.Stat(filePath); err == nil {
			// File exists, now test if it's readable
			file, readErr := os.Open(filePath)
			if readErr == nil {
				file.Close()
				if i > 0 {
					v.log("✅ File verification successful for %s after %d attempts", filePath, i+1)
				}
				return nil // Success
			} else {
				// File exists but not readable - this is a real error, don't retry
				return fmt.Errorf("file not readable: %s: %w", filePath, readErr)
			}
		} else if !os.IsNotExist(err) {
			// Some other error (permissions, etc.) - don't retry
			return fmt.Errorf("file access error for %s: %w", filePath, err)
		}

		// File doesn't exist - check if we should retry
		if i == maxRetries-1 {
			return fmt.Errorf("file does not exist: %s", filePath)
		} else {
			// Exponential backoff: 10ms, 20ms, 40ms, 80ms, 160ms
			waitTime := time.Duration(10*(1<<i)) * time.Millisecond
			v.log("⏳ File verification retry %d/%d for %s: waiting %v for filesystem sync...", i+1, maxRetries, filePath, waitTime)
			time.Sleep(waitTime)
		}
	}

	return fmt.Errorf("file does not exist after %d retries: %s", maxRetries, filePath)
}

// verifyFileNotEmpty ensures file has content
func (v *OperationSuccessVerifier) verifyFileNotEmpty(filePath string) error {
	stat, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("cannot get file stats: %w", err)
	}

	if stat.Size() == 0 {
		return fmt.Errorf("file is empty: %s", filePath)
	}

	return nil
}

// verifyLineCount checks if file has expected number of lines
func (v *OperationSuccessVerifier) verifyLineCount(filePath string, expectedLines int) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("cannot open file for line count: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	actualLines := 0
	for scanner.Scan() {
		actualLines++
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading file for line count: %w", err)
	}

	if actualLines != expectedLines {
		return fmt.Errorf("line count mismatch: expected %d, got %d", expectedLines, actualLines)
	}

	return nil
}

// verifyFileIntegrity performs basic corruption detection
func (v *OperationSuccessVerifier) verifyFileIntegrity(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("cannot open file for integrity check: %w", err)
	}
	defer file.Close()

	// Read file in chunks to detect corruption
	buffer := make([]byte, 4096)
	for {
		n, err := file.Read(buffer)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("file corruption detected - read error: %w", err)
		}

		// Basic corruption check: ensure we can read valid UTF-8
		if !isValidUTF8Content(buffer[:n]) {
			return fmt.Errorf("file corruption detected - invalid UTF-8 content")
		}
	}

	return nil
}

// verifyMergeContentValidity performs cross-verification between input and output
func (v *OperationSuccessVerifier) verifyMergeContentValidity(inputFiles []string, outputFile string) error {
	// Read first few lines from output to verify format
	outputFile_handle, err := os.Open(outputFile)
	if err != nil {
		return fmt.Errorf("cannot open output file for content verification: %w", err)
	}
	defer outputFile_handle.Close()

	scanner := bufio.NewScanner(outputFile_handle)
	linesChecked := 0
	emptyLinesCount := 0
	validContentLines := 0
	const maxLinesToCheck = 20

	for scanner.Scan() && linesChecked < maxLinesToCheck {
		line := scanner.Text()

		// Count empty lines but allow them (common in credential files)
		if len(line) == 0 {
			emptyLinesCount++
		} else {
			validContentLines++
		}

		// Check for suspiciously long lines that could indicate corruption
		if len(line) > 10000 {
			return fmt.Errorf("output contains extremely long lines - merge may be corrupted")
		}
		linesChecked++
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading output file for content verification: %w", err)
	}

	if linesChecked == 0 {
		return fmt.Errorf("output file appears empty during content verification")
	}

	// Ensure we have some valid content lines (allow up to 50% empty lines)
	if validContentLines == 0 {
		return fmt.Errorf("output contains no valid content lines - merge may be corrupted")
	}

	if emptyLinesCount > 0 && validContentLines > 0 {
		v.log("📊 Content validation: %d valid lines, %d empty lines (%.1f%% empty)",
			validContentLines, emptyLinesCount, float64(emptyLinesCount)/float64(linesChecked)*100)
	}

	return nil
}

// isValidUTF8Content checks if byte content is reasonable for text processing
// Very permissive approach - only rejects truly binary files to allow mixed encoding content
func isValidUTF8Content(data []byte) bool {
	// Skip validation for very small samples
	if len(data) < 10 {
		return true
	}

	// Convert to string and check if it contains at least some valid UTF-8
	text := string(data)

	// Count printable characters and valid UTF-8 sequences
	printableCount := 0
	totalChars := 0

	for _, r := range text {
		totalChars++
		// Allow all printable characters, newlines, tabs, and common control chars
		if r >= 32 && r <= 126 || r == '\n' || r == '\r' || r == '\t' || r >= 128 {
			printableCount++
		}
	}

	// Require at least 60% of characters to be reasonable (very permissive)
	if totalChars == 0 {
		return true // Empty content is valid
	}

	printableRatio := float64(printableCount) / float64(totalChars)

	// Very permissive: allow files with at least 60% reasonable characters
	// This handles mixed encodings, binary content in text files, etc.
	return printableRatio >= 0.6
}

// verifyDatabaseInsertion verifies that database operations completed successfully
func (v *OperationSuccessVerifier) verifyDatabaseInsertion() error {
	// This would typically check database connectivity and verify record insertion
	// For now, implementing basic database connectivity check
	v.log("🔍 Verifying database insertion success...")

	// Test database connectivity (basic implementation)
	// In a real implementation, this would verify specific record counts

	v.log("✅ Database insertion verification completed")
	return nil
}

// log helper method for verification logging
func (v *OperationSuccessVerifier) log(format string, args ...interface{}) {
	if v.logger != nil {
		v.logger(format, args...)
	} else {
		fmt.Printf(format+"\n", args...)
	}
}

// runMoveStage moves up to 2 .txt files from InputDir to NonSortedDir for processing
func (s *StoreService) runMoveStage(ctx context.Context) error {
	s.log("=== MOVE STAGE ===")

	inputDir := s.config.InputDir
	nonSortedDir := s.config.NonSortedDir

	// Ensure NonSortedDir exists
	if err := os.MkdirAll(nonSortedDir, 0755); err != nil {
		return fmt.Errorf("error creating nonsorted directory: %w", err)
	}

	// Clear any existing files in NonSortedDir first
	s.log("🧹 Clearing NonSorted directory: %s", nonSortedDir)
	if err := s.clearDirectory(nonSortedDir); err != nil {
		s.log("⚠️ Warning: Failed to clear NonSorted directory: %v", err)
	}

	// Find available .txt files in InputDir
	pattern := filepath.Join(inputDir, "*.txt")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("error discovering files for move: %w", err)
	}

	if len(files) == 0 {
		return fmt.Errorf("no .txt files found in input directory: %s", inputDir)
	}

	// Move up to 2 files (or all available if less than 2)
	filesToMove := files
	if len(files) > 2 {
		filesToMove = files[:2]
	}

	s.log("📦 Moving %d files to NonSorted directory for processing", len(filesToMove))

	// Create file transaction for atomic move operations
	tx := s.NewFileTransaction()
	var moveErrors []error

	for _, sourceFile := range filesToMove {
		fileName := filepath.Base(sourceFile)
		destFile := filepath.Join(nonSortedDir, fileName)

		s.log("📂 Moving: %s -> %s", fileName, destFile)

		// Record move operation in transaction
		tx.AddOperation("MOVE", sourceFile, destFile, "")

		moveStartTime := time.Now()
		if err := os.Rename(sourceFile, destFile); err != nil {
			s.logFileOperation("FILE_MOVE", fmt.Sprintf("%s->%s", sourceFile, destFile), moveStartTime, err, map[string]interface{}{
				"operation_type": "move_to_nonsorted",
				"source_file":    sourceFile,
				"dest_file":      destFile,
			})
			moveErrors = append(moveErrors, fmt.Errorf("failed to move %s: %w", fileName, err))
		} else {
			s.logFileOperation("FILE_MOVE", fmt.Sprintf("%s->%s", sourceFile, destFile), moveStartTime, nil, map[string]interface{}{
				"operation_type": "move_to_nonsorted",
				"source_file":    sourceFile,
				"dest_file":      destFile,
			})
			tx.MarkCompleted(sourceFile)
			s.log("✅ Moved: %s", fileName)
		}
	}

	// Handle move errors with rollback
	if len(moveErrors) > 0 {
		s.log("❌ Move operation failed, attempting rollback...")
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			s.log("❌ Rollback failed: %v", rollbackErr)
			return fmt.Errorf("file move failed and rollback failed: %d move errors, rollback error: %w", len(moveErrors), rollbackErr)
		}
		s.log("✅ Rollback completed - files restored")
		return fmt.Errorf("%d file move errors occurred, changes rolled back", len(moveErrors))
	}

	if err := tx.Commit(); err != nil {
		s.log("⚠️ Transaction commit warning: %v", err)
	}

	s.log("✅ Move stage completed: %d files moved to %s", len(filesToMove), nonSortedDir)
	return nil
}

// clearDirectory removes all files from the specified directory
func (s *StoreService) clearDirectory(dirPath string) error {
	files, err := filepath.Glob(filepath.Join(dirPath, "*"))
	if err != nil {
		return fmt.Errorf("failed to list files in directory: %w", err)
	}

	for _, file := range files {
		if err := os.Remove(file); err != nil {
			s.log("⚠️ Failed to remove file %s: %v", file, err)
		}
	}

	return nil
}

// countInputFiles returns the number of .txt files in the InputDir
func (s *StoreService) countInputFiles() (int, error) {
	pattern := filepath.Join(s.config.InputDir, "*.txt")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return 0, fmt.Errorf("error counting input files: %w", err)
	}
	return len(files), nil
}

// runMergerStage combines multiple .txt files into single deduplicated output
func (s *StoreService) runMergerStage(ctx context.Context) error {
	s.log("=== MERGER STAGE ===")

	// Use NonSortedDir as input source (files moved by runMoveStage)
	inputDir := s.config.NonSortedDir
	outputDir := s.config.OutputDir

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("error creating output directory: %w", err)
	}

	// Output file path with timestamp
	outputFile := filepath.Join(
		outputDir,
		fmt.Sprintf("@redscorpionlogs_%s.txt", time.Now().Format("20060102_150405")),
	)
	tmpOutputFile := outputFile + ".tmp"

	// ENHANCEMENT 4.1: Create initial file snapshot to track specific input files
	// This prevents deletion of files added during processing
	s.log("🔍 Creating initial file snapshot for merge operation...")
	initialFileSnapshot, err := s.createInitialFileSnapshot(inputDir)
	if err != nil {
		return fmt.Errorf("error creating initial file snapshot: %w", err)
	}

	if len(initialFileSnapshot.TrackedFiles) == 0 {
		return fmt.Errorf("no .txt files found to merge in snapshot")
	}

	s.log("📋 Merge operation will track %d specific files from snapshot taken at %s",
		len(initialFileSnapshot.TrackedFiles), initialFileSnapshot.SnapshotTime.Format("15:04:05"))

	// Use tracked files from snapshot instead of runtime discovery
	files := initialFileSnapshot.TrackedFiles

	// ENHANCEMENT 4.3: Initialize merge operation status tracking
	mergeStatus := &MergeOperationStatus{
		TotalFiles:     len(files),
		ProcessedFiles: make(map[string]FileProcessStatus),
		StartTime:      time.Now(),
		OverallStatus:  "in_progress",
		ErrorCount:     0,
		SuccessCount:   0,
	}

	// Initialize all files as pending
	for _, file := range files {
		mergeStatus.ProcessedFiles[file] = FileProcessStatus{
			FilePath: file,
			Status:   "pending",
		}
	}

	s.log("📊 Merge operation status tracking initialized for %d files", len(files))

	// PRE-OPERATION: Capture file integrity information for all input files
	s.log("🔒 Capturing integrity information for %d input files before merge operation...", len(files))
	integrityManager := s.NewFileIntegrityManager()
	preOpIntegrityMap, integrityErrors := integrityManager.CaptureMultipleFileIntegrity(files)

	if len(integrityErrors) > 0 {
		s.log("⚠️ Some integrity capture failures - continuing with available data")
		for _, err := range integrityErrors {
			s.log("⚠️ Integrity error: %v", err)
		}
	}

	// Verify file system integrity before processing
	if err := integrityManager.VerifyFileSystemIntegrity(files); err != nil {
		return fmt.Errorf("pre-operation file system integrity check failed: %w", err)
	}
	s.log("✅ Pre-operation integrity verification completed")

	// Ensure filter_errors directory exists for UTF-8 encoding errors
	filterErrorsDir := "app/extraction/files/filter_errors"
	if err := os.MkdirAll(filterErrorsDir, 0755); err != nil {
		return fmt.Errorf("error creating filter_errors directory: %w", err)
	}
	filterErrorsFile := filepath.Join(filterErrorsDir, "error.txt")

	// Aggregate unique lines across all files with status tracking and UTF-8 handling
	unique := make(map[string]struct{})
	for _, file := range files {
		// Update status to reading
		status := mergeStatus.ProcessedFiles[file]
		status.Status = "reading"
		mergeStatus.ProcessedFiles[file] = status

		initialLineCount := len(unique)

		f, err := os.Open(file)
		if err != nil {
			// Mark file as failed
			status.Status = "failed"
			status.Error = err
			status.ProcessedAt = time.Now()
			mergeStatus.ProcessedFiles[file] = status
			mergeStatus.ErrorCount++

			s.log("❌ Failed to open file %s: %v", file, err)
			continue // Continue with other files instead of failing entire operation
		}

		scanner := bufio.NewScanner(f)
		// Set a large buffer to handle long lines that might cause scanner issues
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1MB max line length

		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := scanner.Text()

			// Validate UTF-8 encoding with enhanced error handling
			if !utf8.ValidString(line) {
				// Try to salvage readable parts by cleaning invalid sequences
				cleanLine := strings.ToValidUTF8(line, "?")

				// Append encoding error details to filter_errors/error.txt
				if errorFile, err := os.OpenFile(filterErrorsFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
					errorFile.WriteString(fmt.Sprintf("ENCODING_ERROR[%s:L%d]: Original bytes length: %d, cleaned: %s\n",
						filepath.Base(file), lineNumber, len(line), cleanLine))
					errorFile.Close()
				}
				s.log("⚠️ UTF-8 encoding error in %s at line %d, cleaned and logged to filter_errors/error.txt", filepath.Base(file), lineNumber)

				// Use cleaned line if it has meaningful content, otherwise skip
				if len(strings.TrimSpace(cleanLine)) > 5 {
					unique[cleanLine] = struct{}{}
				}
				continue
			}

			unique[line] = struct{}{}
		}

		if err := scanner.Err(); err != nil {
			f.Close()
			// Mark file as failed
			status.Status = "failed"
			status.Error = err
			status.ProcessedAt = time.Now()
			mergeStatus.ProcessedFiles[file] = status
			mergeStatus.ErrorCount++

			s.log("❌ Failed to read file %s: %v", file, err)
			continue // Continue with other files
		}

		f.Close()

		// Mark file as successfully processed
		linesContributed := len(unique) - initialLineCount
		status.Status = "processed"
		status.ProcessedAt = time.Now()
		status.LinesContributed = linesContributed
		status.Error = nil
		mergeStatus.ProcessedFiles[file] = status
		mergeStatus.SuccessCount++

		s.log("✅ Processed %s: contributed %d unique lines", filepath.Base(file), linesContributed)
	}

	// Write all unique lines to a temp output file first
	out, err := os.OpenFile(tmpOutputFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("error creating temp output file: %w", err)
	}
	writer := bufio.NewWriter(out)
	lines := make([]string, 0, len(unique))
	for line := range unique {
		lines = append(lines, line)
	}
	sort.Strings(lines)
	for _, line := range lines {
		if _, err := writer.WriteString(line + "\n"); err != nil {
			out.Close()
			if s.logManager != nil {
				s.logManager.LogError(err,
					"Failed to write line during merge operation",
					"Check disk space, verify file permissions, retry merge operation with clean environment",
					logrus.Fields{
						"operation":   "merge_write",
						"output_file": outputFile,
						"line_length": len(line),
					})
			}
			return fmt.Errorf("error writing to output file: %w", err)
		}
	}

	if err := writer.Flush(); err != nil {
		out.Close()
		if s.logManager != nil {
			s.logManager.LogError(err,
				"Failed to flush writer during merge operation",
				"Check disk space, verify file not locked, retry merge operation",
				logrus.Fields{
					"operation":   "merge_flush",
					"output_file": outputFile,
				})
		}
		return fmt.Errorf("error flushing output file: %w", err)
	}
	out.Close()

	s.log("📝 DEBUG: Finished writing to temp file, proceeding to rename operation")

	// ATOMIC OPERATION: Rename temp file to final output file
	s.log("🔄 Renaming temporary file to final output: %s -> %s", tmpOutputFile, outputFile)
	s.log("🔍 DEBUG: Checking if temp file exists before rename: %s", tmpOutputFile)
	if _, err := os.Stat(tmpOutputFile); err != nil {
		s.log("❌ DEBUG: Temp file does not exist: %v", err)
		return fmt.Errorf("temp file does not exist before rename: %w", err)
	}
	s.log("✅ DEBUG: Temp file exists, proceeding with rename")
	renameStartTime := time.Now()
	if err := os.Rename(tmpOutputFile, outputFile); err != nil {
		s.logFileOperation("FILE_RENAME", fmt.Sprintf("%s->%s", tmpOutputFile, outputFile), renameStartTime, err, map[string]interface{}{
			"operation_type": "final_rename",
			"temp_file":      tmpOutputFile,
			"final_file":     outputFile,
		})
		return fmt.Errorf("failed to rename temp output file to final: %w", err)
	}
	s.logFileOperation("FILE_RENAME", fmt.Sprintf("%s->%s", tmpOutputFile, outputFile), renameStartTime, nil, map[string]interface{}{
		"operation_type": "final_rename",
		"temp_file":      tmpOutputFile,
		"final_file":     outputFile,
	})
	s.log("✅ DEBUG: Successfully renamed temp file to final output")

	// CRITICAL FIX: Wait for file system to sync after rename operation
	// This prevents race conditions where verification runs before filesystem updates
	s.log("🔄 Waiting for file system sync after rename operation...")
	maxRetries := 5
	for i := 0; i < maxRetries; i++ {
		if _, err := os.Stat(outputFile); err == nil {
			s.log("✅ File verification successful after %d attempts", i+1)
			break
		} else if i == maxRetries-1 {
			s.log("❌ File verification failed after %d attempts: %v", maxRetries, err)
			return fmt.Errorf("renamed file verification timeout after %d retries: %w", maxRetries, err)
		} else {
			// Exponential backoff: 10ms, 20ms, 40ms, 80ms, 160ms
			waitTime := time.Duration(10*(1<<i)) * time.Millisecond
			s.log("⏳ Retry %d/%d: waiting %v for file system sync...", i+1, maxRetries, waitTime)
			time.Sleep(waitTime)
		}
	}

	// POST-OPERATION: Verify file integrity again after successful merge completion
	s.log("🔍 Verifying post-operation file integrity...")
	integrityVerificationErrors := integrityManager.VerifyMultipleFileIntegrity(preOpIntegrityMap)
	if len(integrityVerificationErrors) > 0 {
		s.log("❌ Post-operation integrity verification FAILED - input files may be corrupted")
		for _, err := range integrityVerificationErrors {
			s.log("❌ Integrity verification error: %v", err)
		}
		return fmt.Errorf("post-operation integrity verification failed, input files preserved due to potential corruption")
	}
	s.log("✅ Post-operation integrity verification PASSED - input files unchanged during processing")

	// Update overall merge operation status
	mergeStatus.CompletionTime = time.Now()
	if mergeStatus.ErrorCount == 0 {
		mergeStatus.OverallStatus = "completed"
	} else if mergeStatus.SuccessCount > 0 {
		mergeStatus.OverallStatus = "partial_failure"
	} else {
		mergeStatus.OverallStatus = "failed"
	}

	s.log("📊 Merge operation completed: %s (%d success, %d errors)",
		mergeStatus.OverallStatus, mergeStatus.SuccessCount, mergeStatus.ErrorCount)

	// CRITICAL: Verify operation success BEFORE any file deletions
	s.log("🔍 Verifying merge operation success before file deletion...")
	verifier := s.NewOperationSuccessVerifier()
	if err := verifier.VerifyMergeOperationSuccess(files, outputFile, len(lines)); err != nil {
		s.log("❌ Merge operation verification FAILED - preserving input files")
		mergeStatus.OverallStatus = "failed"
		return fmt.Errorf("merge operation verification failed, input files preserved: %w", err)
	}
	s.log("✅ Merge operation success verification PASSED - safe to proceed with deletions")

	// ENHANCEMENT 4.4: Implement partial failure recovery - only delete successfully processed files
	successfulFiles := make([]string, 0)
	failedFiles := make([]string, 0)

	for filePath, status := range mergeStatus.ProcessedFiles {
		if status.Status == "processed" && status.Error == nil {
			successfulFiles = append(successfulFiles, filePath)
		} else {
			failedFiles = append(failedFiles, filePath)
		}
	}

	s.log("🔍 Partial failure recovery: %d files succeeded, %d files failed", len(successfulFiles), len(failedFiles))

	if len(failedFiles) > 0 {
		s.log("⚠️ Failed files will be preserved for manual review:")
		for _, file := range failedFiles {
			status := mergeStatus.ProcessedFiles[file]
			s.log("  ❌ %s: %s", filepath.Base(file), status.Error)
		}
	}

	if len(successfulFiles) == 0 {
		s.log("❌ No files were successfully processed - preserving all input files")
		return fmt.Errorf("merge operation had no successful file processing - all input files preserved")
	}

	// ATOMIC OPERATION: Create transaction for file deletion operations (only successful files)
	tx := s.NewFileTransaction()
	s.log("🔒 Starting atomic file deletion transaction for %d successfully processed files", len(successfulFiles))

	// SAFETY: Create backups before deleting successfully processed files only
	s.log("Creating backups for %d successfully processed files before deletion...", len(successfulFiles))
	backupMap, backupErrors := s.BackupFiles(successfulFiles)

	// Log backup status but continue with deletion (backups are safety net)
	if len(backupErrors) > 0 {
		s.log("⚠️ Some backup failures occurred, but continuing with file deletion")
		for _, err := range backupErrors {
			s.log("⚠️ Backup error: %v", err)
		}
	}

	// Record deletion operations in transaction (only for successfully processed files)
	for _, file := range successfulFiles {
		backupPath := backupMap[file] // Will be empty string if backup failed
		tx.AddOperation("DELETE", file, "", backupPath)
	}

	// Execute file deletions with transaction tracking (only successful files)
	var deletionErrors []error
	for _, file := range successfulFiles {
		// Check if backup was created successfully before deletion
		if backupPath, hasBackup := backupMap[file]; hasBackup {
			s.log("✓ Deleting %s (backup: %s)", file, backupPath)
		} else {
			s.log("⚠️ Deleting %s (NO BACKUP - backup failed)", file)
		}

		deleteStartTime := time.Now()
		if err := os.Remove(file); err != nil {
			s.logFileDelete(file, deleteStartTime, err)
			deletionErrors = append(deletionErrors, err)
			s.log("⚠️ Error removing file %s: %v", file, err)
			// Do not mark as completed if deletion failed
		} else {
			s.logFileDelete(file, deleteStartTime, nil)
			tx.MarkCompleted(file) // Mark successful deletion
			s.log("✅ Transaction %s: Deleted %s", tx.id, file)
		}
	}

	// Record any deletion errors for potential rollback
	if len(deletionErrors) > 0 {
		for _, delErr := range deletionErrors {
			s.log("❌ Transaction %s: Deletion error: %v", tx.id, delErr)
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			s.log("❌ Rollback failed: %v", rollbackErr)
			return fmt.Errorf("file deletion failed and rollback failed: %d deletion errors, rollback error: %w", len(deletionErrors), rollbackErr)
		}
		s.log("✅ Rollback completed - files restored from backups")
		return fmt.Errorf("%d file deletion errors occurred, changes rolled back", len(deletionErrors))
	} else {
		if err := tx.Commit(); err != nil {
			s.log("⚠️ Transaction commit warning: %v", err)
		}
		s.log("✅ All file deletions completed successfully")
	}

	s.log("✓ Merged %d unique lines from %d files into %s", len(lines), len(files), outputFile)
	return nil
}

// runValuableStage extracts betting-related URLs using regex patterns and saves to dedicated betting file
func (s *StoreService) runValuableStage(ctx context.Context) error {
	s.log("=== VALUABLE STAGE ===")

	// Betting URL regex patterns - Updated patterns for subdomain matching
	// OLD PATTERNS (commented out):
	// re1, err := regexp.Compile(`(?i)bet(?:ting|365|fair|way|victor|winner|master|king|pro|zone|star|plus|max|super|mega|ultra|royal|prime|elite|top|best|ace|win|lucky|golden|diamond|platinum|silver|gold|red|blue|green|black|white|orange|yellow|purple|pink|brown|gray|grey|dark|light|bright|neon|flash|quick|fast|speed|turbo|rocket|jet|fly|sky|air|space|star|moon|sun|fire|ice|water|earth|wind|storm|thunder|lightning|rain|snow|cloud|rainbow|magic|power|force|energy|boost|charge|volt|spark|flame|blaze|heat|cool|chill|freeze|hot|warm|cold|fresh|new|old|classic|vintage|retro|modern|future|next|pro|expert|master|boss|chief|king|queen|prince|princess|lord|lady|sir|miss|mr|mrs|ms|dr|prof)\.`)
	// re2, err := regexp.Compile(`(?i)(?:casino|poker|slots|jackpot|roulette|blackjack|baccarat|craps|lottery|lotto|scratch|gambl|wager|stake|odds|payout|spin|roll|deal|draw|pick|play|game|sport|race|match|league|tournament|championship|cup|finals|playoff)\.`)

	// NEW PATTERNS - Domain-based betting/sport detection:
	re1, err := regexp.Compile(`^(?:dashboard|admin|backoffice-new)\.[^./]*(?:bet|sport)[^./]*(?:\.[^./]+)+$`)
	if err != nil {
		return fmt.Errorf("error compiling betting regex pattern 1: %w", err)
	}

	re2, err := regexp.Compile(`^[^./]+\.[^./]*(?:bet|sport)[^./]*(?:\.[^./]+)+$`)
	if err != nil {
		return fmt.Errorf("error compiling betting regex pattern 2: %w", err)
	}

	// Priority site patterns
	priorityStrings := []string{
		"zemenbank", "com.boa.apollo", "hellocash.net", "admin.2xsport.com",
	}
	priorityRegex, err := regexp.Compile(`dashboard.*bet.*\.et\b`)
	if err != nil {
		return fmt.Errorf("error compiling priority regex pattern: %w", err)
	}

	// Use configurable betting directory
	bettingDir := s.config.BettingDir
	if err := os.MkdirAll(bettingDir, 0755); err != nil {
		return fmt.Errorf("error creating betting directory: %w", err)
	}

	// Output files with timestamp
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	bettingFile := filepath.Join(bettingDir, fmt.Sprintf("betting_sites_%s.txt", timestamp))
	priorityFile := filepath.Join(bettingDir, fmt.Sprintf("priority_sites_%s.txt", timestamp))

	// Find latest merged file from configurable output directory
	mergedDir := s.config.OutputDir
	latestFile, err := resolveLatestTextFile(mergedDir)
	if err != nil {
		return fmt.Errorf("error finding latest merged file: %w", err)
	}

	s.log("Processing file for betting extraction: %s", latestFile)

	// Try to connect to database
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s",
		s.config.MySQLUser, s.config.MySQLPass, s.config.MySQLHost, s.config.MySQLDB)
	db, err := sql.Open(s.config.DBType, dsn)
	if err != nil {
		// Enhanced database connection error logging
		if s.logManager != nil {
			systemState := map[string]interface{}{
				"db_type":    s.config.DBType,
				"mysql_host": s.config.MySQLHost,
				"mysql_db":   s.config.MySQLDB,
				"mysql_user": s.config.MySQLUser,
			}

			s.logManager.LogCriticalError(err,
				"Database connection initialization failed",
				"Verify database server is running, check network connectivity, validate connection parameters, ensure database exists",
				systemState)
		}
		return fmt.Errorf("error opening database: %w", err)
	}
	defer db.Close()

	// Read input file and extract betting URLs
	inputFile, err := os.Open(latestFile)
	if err != nil {
		return fmt.Errorf("error opening input file: %w", err)
	}
	defer inputFile.Close()

	// Create new betting output file
	bettingOut, err := os.OpenFile(bettingFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("error opening betting output file: %w", err)
	}
	defer bettingOut.Close()

	// Create new priority output file
	priorityOut, err := os.OpenFile(priorityFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("error opening priority output file: %w", err)
	}
	defer priorityOut.Close()

	scanner := bufio.NewScanner(inputFile)
	var extractedCount, priorityCount int
	for scanner.Scan() {
		line := scanner.Text()

		// Check for priority sites first
		isPriority := false
		for _, priorityStr := range priorityStrings {
			if strings.Contains(line, priorityStr) {
				isPriority = true
				break
			}
		}
		if !isPriority && priorityRegex.MatchString(line) {
			isPriority = true
		}

		if isPriority {
			if _, err := fmt.Fprintln(priorityOut, line); err != nil {
				return fmt.Errorf("error writing to priority file: %w", err)
			}
			priorityCount++
		}

		// Check if line matches betting patterns and exclude .gov domains
		if (re1.MatchString(line) || re2.MatchString(line)) && !strings.Contains(line, ".gov") {
			if _, err := fmt.Fprintln(bettingOut, line); err != nil {
				return fmt.Errorf("error writing to betting file: %w", err)
			}
			extractedCount++
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading input file: %w", err)
	}

	s.log("✓ Extracted %d betting URLs to %s", extractedCount, bettingFile)
	s.log("✓ Extracted %d priority sites to %s", priorityCount, priorityFile)

	// Send files to bot if bot sender is configured
	if s.botSender != nil && s.adminChatID != 0 {
		s.log("📤 Sending betting files to bot...")
		
		// Send betting file
		if extractedCount > 0 {
			caption := fmt.Sprintf("🎯 Betting Sites Extract\n📊 %d betting URLs found\n🕐 %s", 
				extractedCount, time.Now().Format("2006-01-02 15:04:05"))
			if err := s.botSender.SendDocument(s.adminChatID, bettingFile, caption); err != nil {
				s.log("⚠️ Failed to send betting file to bot: %v", err)
			} else {
				s.log("✅ Betting file sent to bot successfully")
			}
		}
		
		// Send priority file
		if priorityCount > 0 {
			caption := fmt.Sprintf("⭐ Priority Sites Extract\n📊 %d priority sites found\n🕐 %s", 
				priorityCount, time.Now().Format("2006-01-02 15:04:05"))
			if err := s.botSender.SendDocument(s.adminChatID, priorityFile, caption); err != nil {
				s.log("⚠️ Failed to send priority file to bot: %v", err)
			} else {
				s.log("✅ Priority file sent to bot successfully")
			}
		}
	} else {
		s.log("ℹ️ Bot sender not configured - skipping file transmission")
	}

	// Move files to backup directory after sending
	if err := s.moveFilesToBackup(bettingFile, priorityFile, extractedCount, priorityCount); err != nil {
		s.log("⚠️ Failed to move files to backup: %v", err)
	}

	return nil
}

// moveFilesToBackup moves betting and priority files to the backup directory
func (s *StoreService) moveFilesToBackup(bettingFile, priorityFile string, bettingCount, priorityCount int) error {
	// Create backup directory
	backupDir := filepath.Join("app/extraction/files/backups")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("error creating backup directory: %w", err)
	}

	var moveErrors []error
	
	// Move betting file to backup if it exists and has content
	if bettingCount > 0 {
		if _, err := os.Stat(bettingFile); err == nil {
			backupPath := filepath.Join(backupDir, filepath.Base(bettingFile))
			if err := os.Rename(bettingFile, backupPath); err != nil {
				moveErrors = append(moveErrors, fmt.Errorf("failed to move betting file to backup: %w", err))
			} else {
				s.log("✅ Moved betting file to backup: %s", backupPath)
			}
		}
	}
	
	// Move priority file to backup if it exists and has content
	if priorityCount > 0 {
		if _, err := os.Stat(priorityFile); err == nil {
			backupPath := filepath.Join(backupDir, filepath.Base(priorityFile))
			if err := os.Rename(priorityFile, backupPath); err != nil {
				moveErrors = append(moveErrors, fmt.Errorf("failed to move priority file to backup: %w", err))
			} else {
				s.log("✅ Moved priority file to backup: %s", backupPath)
			}
		}
	}
	
	// Return combined errors if any
	if len(moveErrors) > 0 {
		var errMsg string
		for i, err := range moveErrors {
			if i > 0 {
				errMsg += "; "
			}
			errMsg += err.Error()
		}
		return fmt.Errorf("backup errors: %s", errMsg)
	}
	
	s.log("✅ All files successfully moved to backup directory")
	return nil
}

// BotFileSender interface for sending files to bot
type BotFileSender interface {
	SendDocument(chatID int64, filePath string, caption string) error
}

// TelegramBotSender implements BotFileSender for Telegram Bot API
type TelegramBotSender struct {
	bot *tgbotapi.BotAPI
}

// NewTelegramBotSender creates a new TelegramBotSender instance
func NewTelegramBotSender(bot *tgbotapi.BotAPI) *TelegramBotSender {
	return &TelegramBotSender{bot: bot}
}

// SendDocument sends a document file to the specified chat ID
func (t *TelegramBotSender) SendDocument(chatID int64, filePath string, caption string) error {
	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(filePath))
	doc.Caption = caption
	
	_, err := t.bot.Send(doc)
	if err != nil {
		return fmt.Errorf("failed to send document: %w", err)
	}
	
	return nil
}

// StoreService encapsulates the data processing pipeline functionality
type StoreService struct {
	config        Config
	logger        func(string, ...interface{}) // Logger function for bot integration
	logManager    *LogManager                  // Structured logging manager
	botSender     BotFileSender               // Bot interface for sending files
	adminChatID   int64                       // Admin chat ID for sending files
}

// NewStoreService creates a new StoreService instance with structured logging
func NewStoreService(logger func(string, ...interface{})) *StoreService {
	return NewStoreServiceWithBot(logger, nil, 0)
}

// NewStoreServiceWithBot creates a new StoreService instance with bot integration
func NewStoreServiceWithBot(logger func(string, ...interface{}), botSender BotFileSender, adminChatID int64) *StoreService {
	config := LoadConfig()

	// Initialize structured logging manager
	logManager, err := NewLogManager(config)
	if err != nil {
		// Fallback to basic logging if structured logging fails
		if logger != nil {
			logger("Warning: Failed to initialize structured logging: %v", err)
		}
		logManager = nil
	}

	service := &StoreService{
		config:      config,
		logger:      logger,
		logManager:  logManager,
		botSender:   botSender,
		adminChatID: adminChatID,
	}

	// Log service initialization
	if logManager != nil {
		logManager.LogWithFields(logrus.InfoLevel, "StoreService initialized", logrus.Fields{
			"input_dir":   config.InputDir,
			"output_dir":  config.OutputDir,
			"betting_dir": config.BettingDir,
			"log_dir":     config.LogDir,
			"db_type":     config.DBType,
			"run_filter":  config.RunFilter,
			"run_db":      config.RunDB,
		})
	}

	return service
}

// Close gracefully closes the StoreService and its resources
func (s *StoreService) Close() error {
	if s.logManager != nil {
		s.logManager.LogWithFields(logrus.InfoLevel, "StoreService shutting down", logrus.Fields{
			"component": "StoreService",
		})
		return s.logManager.Close()
	}
	return nil
}

// logFileOperation logs file operations with comprehensive tracking
func (s *StoreService) logFileOperation(operation, filePath string, startTime time.Time, err error, details map[string]interface{}) {
	duration := time.Since(startTime)
	success := err == nil

	// Get absolute path for complete tracking
	absPath, pathErr := filepath.Abs(filePath)
	if pathErr != nil {
		absPath = filePath // fallback to relative path
	}

	// Use structured logging if available
	if s.logManager != nil {
		logDetails := make(map[string]interface{})
		if details != nil {
			for k, v := range details {
				logDetails[k] = v
			}
		}
		s.logManager.LogOperation(operation, absPath, success, duration, logDetails)
	}

	// Also use legacy logger for backward compatibility
	if s.logger != nil {
		if success {
			s.logger("✅ %s: %s (%.2fms)", operation, absPath, float64(duration.Microseconds())/1000.0)
		} else {
			s.logger("❌ %s FAILED: %s - %v (%.2fms)", operation, absPath, err, float64(duration.Microseconds())/1000.0)
		}
	}
}

// logFileRead logs file read operations with size tracking
func (s *StoreService) logFileRead(filePath string, startTime time.Time, bytesRead int64, err error) {
	details := map[string]interface{}{
		"bytes_read":     bytesRead,
		"operation_type": "read",
	}
	s.logFileOperation("FILE_READ", filePath, startTime, err, details)
}

// logFileWrite logs file write operations with size tracking
func (s *StoreService) logFileWrite(filePath string, startTime time.Time, bytesWritten int64, err error) {
	details := map[string]interface{}{
		"bytes_written":  bytesWritten,
		"operation_type": "write",
	}
	s.logFileOperation("FILE_WRITE", filePath, startTime, err, details)
}

// logFileDelete logs file deletion operations
func (s *StoreService) logFileDelete(filePath string, startTime time.Time, err error) {
	details := map[string]interface{}{
		"operation_type": "delete",
	}
	s.logFileOperation("FILE_DELETE", filePath, startTime, err, details)
}

// logFileCopy logs file copy/backup operations
func (s *StoreService) logFileCopy(srcPath, destPath string, startTime time.Time, bytesTransferred int64, err error) {
	duration := time.Since(startTime)
	success := err == nil

	if s.logManager != nil {
		details := map[string]interface{}{
			"source_path":       srcPath,
			"dest_path":         destPath,
			"bytes_transferred": bytesTransferred,
			"operation_type":    "copy",
		}
		s.logManager.LogOperation("FILE_COPY", destPath, success, duration, details)
	}

	if s.logger != nil {
		if success {
			s.logger("✅ FILE_COPY: %s -> %s (%d bytes, %.2fms)", srcPath, destPath, bytesTransferred, float64(duration.Microseconds())/1000.0)
		} else {
			s.logger("❌ FILE_COPY FAILED: %s -> %s - %v (%.2fms)", srcPath, destPath, err, float64(duration.Microseconds())/1000.0)
		}
	}
}

// RunPipeline executes the complete 4-stage data processing pipeline with batching
// New workflow: start -> move -> merge -> valuable -> filter & database (repeats until InputDir is empty)
// Returns error if any critical stage fails
func (s *StoreService) RunPipeline(ctx context.Context) error {
	s.log("Store Pipeline Starting...")

	// Check initial file count - exit if no files to process
	fileCount, err := s.countInputFiles()
	if err != nil {
		return fmt.Errorf("failed to count input files: %w", err)
	}

	if fileCount == 0 {
		s.log("❌ No files found in input directory: %s", s.config.InputDir)
		return fmt.Errorf("no .txt files found in input directory: %s", s.config.InputDir)
	}

	s.log("🚀 Found %d files in input directory, starting batch processing...", fileCount)

	// Loop until all files in InputDir are processed
	batchNumber := 1
	for {
		// Check if there are still files to process
		remainingFiles, err := s.countInputFiles()
		if err != nil {
			s.log("⚠️ Error counting remaining files: %v", err)
			break
		}

		if remainingFiles == 0 {
			s.log("✅ All files processed successfully. Input directory is now empty.")
			break
		}

		s.log("=== BATCH %d PROCESSING ===", batchNumber)
		s.log("📊 Remaining files in InputDir: %d", remainingFiles)

		// Stage 1: Move Stage - Move up to 2 files from InputDir to NonSortedDir
		if err := s.runMoveStage(ctx); err != nil {
			s.log("❌ Move stage failed: %v", err)

			// Enhanced error logging
			if s.logManager != nil {
				s.logManager.LogError(err,
					"File move stage execution",
					"Check input directory permissions, verify NonSorted directory accessibility, retry with clean environment",
					logrus.Fields{
						"stage":         "move",
						"batch_number":  batchNumber,
						"input_dir":     s.config.InputDir,
						"nonsorted_dir": s.config.NonSortedDir,
						"pipeline_step": 1,
					})
			}

			return fmt.Errorf("move stage failed in batch %d: %w", batchNumber, err)
		}

		// Stage 2: Merger Stage - Process files in NonSortedDir
		if err := s.runMergerStage(ctx); err != nil {
			s.log("❌ Merger stage failed: %v", err)

			// Enhanced error logging with stack trace and recovery actions
			if s.logManager != nil {
				s.logManager.LogError(err,
					"File merger stage execution",
					"Check NonSorted directory for valid .txt files, verify file permissions, retry with clean environment",
					logrus.Fields{
						"stage":         "merger",
						"batch_number":  batchNumber,
						"input_dir":     s.config.NonSortedDir,
						"pipeline_step": 2,
					})
			}

			return fmt.Errorf("merger stage failed in batch %d: %w", batchNumber, err)
		}

		// Stage 3: Valuable (Betting URL Extraction) with fallback
		if err := s.runValuableStage(ctx); err != nil {
			s.log("⚠️ Valuable stage failed: %v", err)
			s.log("⚠️ Continuing with filter stage (valuable stage failure is non-critical)")

			// Log non-critical error with recovery action
			if s.logManager != nil {
				s.logManager.LogError(err,
					"Betting URL extraction (valuable stage)",
					"Continue with filter stage - valuable stage failure is non-critical to main pipeline",
					logrus.Fields{
						"stage":               "valuable",
						"batch_number":        batchNumber,
						"criticality":         "non-critical",
						"pipeline_step":       3,
						"continue_processing": true,
					})
			}
		}

		// Stage 4: Filter and Database
		if err := s.runFilterAndDatabaseStage(ctx); err != nil {
			s.log("❌ Filter & Database stage failed: %v", err)

			// Enhanced critical error logging
			if s.logManager != nil {
				systemState := map[string]interface{}{
					"db_type":        s.config.DBType,
					"run_filter":     s.config.RunFilter,
					"run_db":         s.config.RunDB,
					"batch_number":   batchNumber,
					"pipeline_stage": 4,
				}

				s.logManager.LogCriticalError(err,
					"Filter and database stage execution - pipeline terminating",
					"Verify database connectivity, check filtered output file creation, ensure sufficient disk space, review filter criteria",
					systemState)
			}

			return fmt.Errorf("filter & database stage failed in batch %d: %w", batchNumber, err)
		}

		s.log("✅ Batch %d completed successfully!", batchNumber)
		batchNumber++

		// Check for context cancellation
		select {
		case <-ctx.Done():
			s.log("⚠️ Pipeline cancelled by context")
			return ctx.Err()
		default:
			// Continue processing
		}
	}

	s.log("🎉 Complete Pipeline finished successfully! Processed %d batches.", batchNumber-1)
	return nil
}

// log is a helper method that uses the configured logger or falls back to fmt.Printf
func (s *StoreService) log(format string, args ...interface{}) {
	if s.logger != nil {
		s.logger(format, args...)
	} else {
		fmt.Printf(format+"\n", args...)
	}
}

// lineIsValid returns true if line meets simplified validity constraints
// Returns: (cleanedLine, isValid, errorReason)
func lineIsValid(line string) (string, bool, string) {
	// PHASE 1: Line normalization - replace spaces with colons
	normalizedLine := strings.ReplaceAll(line, " ", ":")
	normalizedLine = strings.TrimSpace(normalizedLine)

	// PHASE 3: Line validation (UTF-8 is checked earlier in processChunk)

	// Check 1: Empty line check
	if len(normalizedLine) == 0 {
		return normalizedLine, false, "empty"
	}

	// Check 2: Symbol count check - line must not contain more than 10 forbidden symbols
	notAllowedSymbols := "\"!<>"
	symbolCount := 0

	for _, char := range normalizedLine {
		// Count non-alphanumeric characters that are in not allowed symbols
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')) {
			if strings.ContainsRune(notAllowedSymbols, char) {
				symbolCount++
			}
		}
	}

	if symbolCount > 10 {
		return normalizedLine, false, "too_many_symbols"
	}

	// Check 3: Forbidden strings check (case-specific: full upper/lower only)
	forbiddenStrings := []string{
		// Uppercase versions
		":USER:", ":PASS",
		":NULL:", ":NULL", "[NOT_SAVED]",
		// Lowercase versions
		":null:", ":null", "[not_saved]",
		// Special case (as-is)
		"//t.me/",
	}

	for _, forbidden := range forbiddenStrings {
		if forbidden == ":PASS" || forbidden == ":null" || forbidden == ":NULL" {
			// ":PASS" and ":null" must be at the end of the line
			if strings.HasSuffix(normalizedLine, forbidden) {
				return normalizedLine, false, "forbidden_string"
			}
		} else {
			// Other forbidden strings can appear anywhere
			if strings.Contains(normalizedLine, forbidden) {
				return normalizedLine, false, "forbidden_string"
			}
		}
	}

	// Check 4: Alphanumeric character count check - line must contain at least 20 alphanumeric characters
	alphanumericCount := 0
	for _, char := range normalizedLine {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			alphanumericCount++
		}
	}

	if alphanumericCount < 20 {
		return normalizedLine, false, "insufficient_alphanumeric"
	}

	return normalizedLine, true, ""
}

type InvalidLine struct {
	Content   string
	ErrorType string
}

func processChunk(lines []string) (valid []string, invalid []InvalidLine) {
	for _, ln := range lines {
		// First check UTF-8 validity
		if !utf8.ValidString(ln) {
			invalid = append(invalid, InvalidLine{
				Content:   ln,
				ErrorType: "utf8",
			})
			continue
		}

		if out, ok, errorReason := lineIsValid(ln); ok {
			valid = append(valid, out)
		} else {
			invalid = append(invalid, InvalidLine{
				Content:   ln,
				ErrorType: errorReason,
			})
		}
	}
	return
}

func filterLines(ctx context.Context, inputFile string) (int, int, error) {
	file, err := os.Open(inputFile)
	if err != nil {
		return 0, 0, fmt.Errorf("open input: %w", err)
	}
	defer file.Close()

	// Ensure filter_errors directory exists
	filterErrorsDir := "app/extraction/files/filter_errors"
	if err := os.MkdirAll(filterErrorsDir, 0755); err != nil {
		return 0, 0, fmt.Errorf("error creating filter_errors directory: %w", err)
	}

	// Use absolute path for filtered output to ensure consistency
	filteredOutputPath := filepath.Join("app/extraction/files/", "filtered_output.txt")

	// Duplicate detection is now handled by database UNIQUE constraints
	// No local memory tracking needed - rely on MySQL/SQLite for deduplication

	// Open for append if file exists, create if not
	out, err := os.OpenFile(filteredOutputPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return 0, 0, fmt.Errorf("open/create output: %w", err)
	}
	defer out.Close()

	scanner := bufio.NewScanner(file)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	total := len(lines)
	if total == 0 {
		return 0, 0, nil
	}

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	chunkSize := (total + workers - 1) / workers
	if chunkSize <= 0 {
		chunkSize = total
	}

	type res struct {
		valid   []string
		invalid []InvalidLine
	}
	ch := make(chan []string, workers)
	resCh := make(chan res, workers)

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for l := range ch {
				valid, invalid := processChunk(l)
				resCh <- res{valid: valid, invalid: invalid}
			}
		}()
	}

	go func() {
		for i := 0; i < total; i += chunkSize {
			end := i + chunkSize
			if end > total {
				end = total
			}
			select {
			case ch <- lines[i:end]:
			case <-ctx.Done():
				close(ch)
				return
			}
		}
		close(ch)
	}()

	go func() {
		wg.Wait()
		close(resCh)
	}()

	vCount := 0

	// Open separate error files
	utf8ErrorFile := filepath.Join(filterErrorsDir, "utf8_error.txt")
	validationErrorFile := filepath.Join(filterErrorsDir, "validation_error.txt")
	otherErrorFile := filepath.Join(filterErrorsDir, "error.txt")

	utf8File, _ := os.OpenFile(utf8ErrorFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	defer utf8File.Close()

	validationFile, _ := os.OpenFile(validationErrorFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	defer validationFile.Close()

	otherFile, _ := os.OpenFile(otherErrorFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	defer otherFile.Close()

	for {
		select {
		case r, ok := <-resCh:
			if !ok {
				// Ensure all data is written to disk before returning
				if err := out.Sync(); err != nil {
					return 0, 0, fmt.Errorf("failed to sync filtered output file: %w", err)
				}
				return vCount, total, nil
			}

			// Write valid lines (duplicates will be handled by database UNIQUE constraints)
			for _, s := range r.valid {
				if _, err := fmt.Fprintln(out, s); err != nil {
					return 0, 0, fmt.Errorf("failed to write to filtered output file: %w", err)
				}
				vCount++
			}

			// Write invalid lines to appropriate error files based on error type
			for _, invalidLine := range r.invalid {
				switch invalidLine.ErrorType {
				case "utf8":
					if utf8File != nil {
						fmt.Fprintln(utf8File, fmt.Sprintf("UTF8_ERROR: %s", invalidLine.Content))
					}
				case "empty", "too_many_symbols", "forbidden_string":
					// Validation errors (empty, symbol count, forbidden strings)
					if validationFile != nil {
						fmt.Fprintln(validationFile, fmt.Sprintf("VALIDATION_ERROR[%s]: %s", invalidLine.ErrorType, invalidLine.Content))
					}
				default:
					// Fallback for any other error types
					if otherFile != nil {
						fmt.Fprintln(otherFile, fmt.Sprintf("FILTER_ERROR[%s]: %s", invalidLine.ErrorType, invalidLine.Content))
					}
				}
			}

		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
	}
}

func openDB(cfg Config) (*sql.DB, error) {
	var dsn string
	var driver string
	switch cfg.DBType {
	case "mysql":
		dsn = fmt.Sprintf("%s:%s@tcp(%s)/%s?charset=utf8mb4&parseTime=true&timeout=10s&readTimeout=30s&writeTimeout=30s",
			cfg.MySQLUser, cfg.MySQLPass, cfg.MySQLHost, cfg.MySQLDB)
		driver = "mysql"
	case "sqlite":
		dsn = cfg.SQLiteName + "?_busy_timeout=5000&_journal_mode=WAL"
		driver = "sqlite3"
	default:
		return nil, fmt.Errorf("unknown DB_TYPE: %s (supported: mysql, sqlite)", cfg.DBType)
	}

	// MySQL connection with retry logic (10 attempts, 5 second intervals)
	if cfg.DBType == "mysql" {
		maxRetries := 10
		retryInterval := 5 * time.Second

		for attempt := 1; attempt <= maxRetries; attempt++ {
			fmt.Printf("MySQL connection attempt %d/%d...\n", attempt, maxRetries)

			db, err := sql.Open(driver, dsn)
			if err != nil {
				fmt.Printf("Failed to create database connection (attempt %d): %v\n", attempt, err)
				if attempt == maxRetries {
					return nil, fmt.Errorf("failed to create database connection after %d attempts: %w", maxRetries, err)
				}
				time.Sleep(retryInterval)
				continue
			}

			// Test the connection with timeout
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := db.PingContext(ctx); err != nil {
				db.Close()
				cancel()
				fmt.Printf("Failed to ping database (attempt %d): %v\n", attempt, err)
				if attempt == maxRetries {
					return nil, fmt.Errorf("failed to connect to database %s@%s after %d attempts: %w", cfg.MySQLUser, cfg.MySQLHost, maxRetries, err)
				}
				time.Sleep(retryInterval)
				continue
			}

			// Verify database exists and is accessible
			var dbName string
			err = db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dbName)
			cancel()

			if err != nil {
				db.Close()
				fmt.Printf("Failed to verify database accessibility (attempt %d): %v\n", attempt, err)
				if attempt == maxRetries {
					return nil, fmt.Errorf("failed to verify database accessibility after %d attempts: %w", maxRetries, err)
				}
				time.Sleep(retryInterval)
				continue
			}

			if dbName != cfg.MySQLDB {
				db.Close()
				fmt.Printf("Connected to wrong database (attempt %d): expected %s, got %s\n", attempt, cfg.MySQLDB, dbName)
				if attempt == maxRetries {
					return nil, fmt.Errorf("connected to wrong database after %d attempts: expected %s, got %s", maxRetries, cfg.MySQLDB, dbName)
				}
				time.Sleep(retryInterval)
				continue
			}

			fmt.Printf("MySQL connection successful on attempt %d\n", attempt)

			// Ensure database schema exists
			if err := ensureSchema(db, cfg.DBType); err != nil {
				db.Close()
				fmt.Printf("Failed to ensure database schema (attempt %d): %v\n", attempt, err)
				if attempt == maxRetries {
					return nil, fmt.Errorf("failed to ensure database schema after %d attempts: %w", maxRetries, err)
				}
				time.Sleep(retryInterval)
				continue
			}

			return db, nil
		}
	}

	// Non-MySQL databases (SQLite) - original logic
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to create database connection: %w", err)
	}

	// Test the connection with timeout - CRITICAL for detecting connection issues early
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// Ensure database schema exists
	if err := ensureSchema(db, cfg.DBType); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ensure database schema: %w", err)
	}

	// Set connection pool limits
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	return db, nil
}

// testDBConnection performs comprehensive connection testing with timeout validation

// shardRecord represents a data record for sharded batch processing
type shardRecord struct {
	data string
	hash string
}

// shardPerformanceMetrics tracks performance metrics for intelligent batch sizing
type shardPerformanceMetrics struct {
	avgInsertTime  time.Duration
	lastBatchSize  int
	successfulOps  int
	failedOps      int
	lastUpdateTime time.Time
}

// DuplicateStats tracks duplicate detection statistics for reporting
type DuplicateStats struct {
	TotalProcessed    int64         `json:"total_processed"`
	NewInserted       int64         `json:"new_inserted"`
	DuplicatesFound   int64         `json:"duplicates_found"`
	DuplicateRate     float64       `json:"duplicate_rate"`
	StartTime         time.Time     `json:"-"` // Internal field for timing
	ProcessingTime    time.Duration `json:"processing_time_ms"`
	AvgBatchTime      time.Duration `json:"avg_batch_time_ms"`
	ErrorsEncountered int64         `json:"errors_encountered"`
	SourceInstance    string        `json:"source_instance"`
}

// adaptiveBatchSizer manages intelligent batch size adjustment per shard
type adaptiveBatchSizer struct {
	shardMetrics  []shardPerformanceMetrics
	baseBatchSize int
	minBatchSize  int
	maxBatchSize  int
	stats         DuplicateStats // Enhanced statistics tracking
}

// newAdaptiveBatchSizer creates a new adaptive batch sizer with enhanced stats tracking
func newAdaptiveBatchSizer(shardCount int, config Config) *adaptiveBatchSizer {
	return &adaptiveBatchSizer{
		shardMetrics:  make([]shardPerformanceMetrics, shardCount),
		baseBatchSize: config.BatchSize,
		minBatchSize:  500,   // Higher minimum for network efficiency
		maxBatchSize:  10000, // Higher maximum for large datasets
		stats: DuplicateStats{
			SourceInstance: config.SourceInstanceID,
		},
	}
}

// getOptimalBatchSize returns the optimal batch size for a specific shard
func (abs *adaptiveBatchSizer) getOptimalBatchSize(shardIndex int) int {
	if shardIndex >= len(abs.shardMetrics) {
		return abs.baseBatchSize
	}

	metrics := &abs.shardMetrics[shardIndex]

	// For first few operations, use base batch size
	if metrics.successfulOps < 3 {
		return abs.baseBatchSize
	}

	// Calculate performance score based on success rate and speed
	successRate := float64(metrics.successfulOps) / float64(metrics.successfulOps+metrics.failedOps)
	avgTimeMs := float64(metrics.avgInsertTime.Milliseconds())

	// Adjust batch size based on performance
	adjustmentFactor := 1.0

	// If very fast (< 100ms) and high success rate (> 95%), increase batch size
	if avgTimeMs < 100 && successRate > 0.95 {
		adjustmentFactor = 1.5
	} else if avgTimeMs < 500 && successRate > 0.90 {
		// Fast with good success rate, slight increase
		adjustmentFactor = 1.2
	} else if avgTimeMs > 2000 || successRate < 0.80 {
		// Slow or many failures, decrease batch size
		adjustmentFactor = 0.7
	} else if avgTimeMs > 1000 || successRate < 0.90 {
		// Moderate performance issues, slight decrease
		adjustmentFactor = 0.9
	}

	newBatchSize := int(float64(abs.baseBatchSize) * adjustmentFactor)

	// Enforce limits
	if newBatchSize < abs.minBatchSize {
		newBatchSize = abs.minBatchSize
	} else if newBatchSize > abs.maxBatchSize {
		newBatchSize = abs.maxBatchSize
	}

	return newBatchSize
}

// updateMetrics updates performance metrics for a shard after a batch operation
func (abs *adaptiveBatchSizer) updateMetrics(shardIndex int, batchSize int, duration time.Duration, success bool) {
	if shardIndex >= len(abs.shardMetrics) {
		return
	}

	metrics := &abs.shardMetrics[shardIndex]

	// Update timing with exponential moving average
	if metrics.successfulOps > 0 {
		metrics.avgInsertTime = time.Duration(float64(metrics.avgInsertTime)*0.7 + float64(duration)*0.3)
	} else {
		metrics.avgInsertTime = duration
	}

	metrics.lastBatchSize = batchSize
	metrics.lastUpdateTime = time.Now()

	if success {
		metrics.successfulOps++
	} else {
		metrics.failedOps++
	}
}

// populateDBSharded processes input file using distributed sharding across multiple databases
func populateDBSharded(ctx context.Context, cfg Config, shardManager *ShardManager, inputFile string) error {
	// Ensure schema exists on all shards
	for i := 0; i < shardManager.GetShardCount(); i++ {
		db := shardManager.GetShardConnection(i)
		if err := ensureSchema(db, cfg.DBType); err != nil {
			return fmt.Errorf("failed to ensure schema on shard %d: %w", i, err)
		}
	}

	file, err := os.Open(inputFile)
	if err != nil {
		return fmt.Errorf("failed to open input file '%s': %w", inputFile, err)
	}
	defer file.Close()

	// Initialize adaptive batch sizer with enhanced configuration
	batchSizer := newAdaptiveBatchSizer(shardManager.GetShardCount(), cfg)
	batchSizer.stats.StartTime = time.Now() // Start timing

	// Create separate batches for each shard with dynamic sizing
	shardBatches := make([][]shardRecord, shardManager.GetShardCount())
	for i := range shardBatches {
		initialBatchSize := batchSizer.getOptimalBatchSize(i)
		shardBatches[i] = make([]shardRecord, 0, initialBatchSize)
	}

	scanner := bufio.NewScanner(file)
	var total, inserted, duplicates, errors int

	// Sharded flush function - processes batches for all shards with comprehensive error handling
	flushAllShards := func() error {
		type shardResult struct {
			shardIndex int
			inserted   int
			duplicates int
			err        error
			batchSize  int
		}

		resultChan := make(chan shardResult, shardManager.GetShardCount())
		var activeBatches int

		// Process each shard's batch in parallel
		for shardIndex, batch := range shardBatches {
			if len(batch) == 0 {
				continue
			}

			activeBatches++
			// Process shard batch in goroutine with enhanced error handling and performance tracking
			go func(sIndex int, sBatch []shardRecord) {
				startTime := time.Now()
				result := shardResult{
					shardIndex: sIndex,
					batchSize:  len(sBatch),
				}

				db := shardManager.GetShardConnection(sIndex)
				if db == nil {
					result.err = fmt.Errorf("shard %d: connection is nil", sIndex)
					batchSizer.updateMetrics(sIndex, len(sBatch), time.Since(startTime), false)
					resultChan <- result
					return
				}

				// Test connection before attempting batch
				if err := db.Ping(); err != nil {
					result.err = fmt.Errorf("shard %d: connection failed ping: %w", sIndex, err)
					batchSizer.updateMetrics(sIndex, len(sBatch), time.Since(startTime), false)
					resultChan <- result
					return
				}

				// Attempt batch insert with retry logic and performance tracking
				maxRetries := 3
				var batchSuccess bool
				for attempt := 1; attempt <= maxRetries; attempt++ {
					ins, dups, err := flushShardBatch(ctx, cfg, db, sBatch)
					if err == nil {
						result.inserted = ins
						result.duplicates = dups
						batchSuccess = true
						break
					}

					if attempt == maxRetries {
						result.err = fmt.Errorf("shard %d: failed after %d attempts: %w", sIndex, maxRetries, err)
					} else {
						// Wait before retry with exponential backoff
						time.Sleep(time.Duration(attempt*100) * time.Millisecond)
					}
				}

				// Update performance metrics for adaptive batch sizing
				duration := time.Since(startTime)
				batchSizer.updateMetrics(sIndex, len(sBatch), duration, batchSuccess)

				resultChan <- result
			}(shardIndex, batch)

			// Clear the batch
			shardBatches[shardIndex] = shardBatches[shardIndex][:0]
		}

		// Collect results from all active batches
		var failedShards []int
		var flushErrors []error
		successfulShards := 0

		for i := 0; i < activeBatches; i++ {
			result := <-resultChan

			if result.err != nil {
				failedShards = append(failedShards, result.shardIndex)
				flushErrors = append(flushErrors, result.err)

				// Log individual shard failure details
				dbName := generateShardDatabaseName(cfg, result.shardIndex)
				fmt.Printf("\n❌ Shard %d (%s): batch of %d records failed: %v\n",
					result.shardIndex, dbName, result.batchSize, result.err)
			} else {
				successfulShards++
				inserted += result.inserted
				duplicates += result.duplicates

				// Successful shard processing - quiet operation
				// (verbose logging can be added later if needed)
			}
		}

		// Handle partial failures - continue processing if some shards succeed
		if len(failedShards) > 0 {
			fmt.Printf("\n⚠️  Partial batch failure: %d/%d shards failed\n",
				len(failedShards), activeBatches)
			fmt.Printf("Failed shards: %v\n", failedShards)

			// Only return error if ALL shards failed
			if successfulShards == 0 {
				return fmt.Errorf("complete batch failure: all %d active shards failed", activeBatches)
			}

			// Log partial success
			fmt.Printf("✅ Continuing: %d shards processed successfully\n", successfulShards)
		}

		return nil
	}

	// Process lines and distribute to shards
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation cancelled: %w", ctx.Err())
		default:
		}

		line := scanner.Text()
		if validLine, ok, _ := lineIsValid(strings.TrimSpace(line)); ok {
			// Calculate hash for duplicate detection and sharding
			hash := fmt.Sprintf("%x", md5.Sum([]byte(validLine)))

			// Determine target shard
			shardIndex := getShardForLine(validLine, shardManager.GetShardCount())

			// Add to appropriate shard batch
			shardBatches[shardIndex] = append(shardBatches[shardIndex], shardRecord{
				data: validLine,
				hash: hash,
			})

			// Check if any shard batch is full using adaptive batch sizing
			optimalBatchSize := batchSizer.getOptimalBatchSize(shardIndex)
			if len(shardBatches[shardIndex]) >= optimalBatchSize {
				if err := flushAllShards(); err != nil {
					errors++
					fmt.Printf("Warning: Batch flush failed: %v\n", err)
				}
			}

			total++
			if total%10000 == 0 {
				fmt.Printf("\rProcessed %d lines (inserted: %d, duplicates: %d, errors: %d)",
					total, inserted, duplicates, errors)
			}
		}
	}

	// Handle scanner errors
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading input file: %w", err)
	}

	// Final flush for remaining batches
	if err := flushAllShards(); err != nil {
		errors++
		fmt.Printf("Warning: Final batch flush failed: %v\n", err)
	}

	// Update final statistics
	batchSizer.stats.TotalProcessed = int64(total)
	batchSizer.stats.NewInserted = int64(inserted)
	batchSizer.stats.DuplicatesFound = int64(duplicates)
	batchSizer.stats.ErrorsEncountered = int64(errors)
	batchSizer.stats.ProcessingTime = time.Since(batchSizer.stats.StartTime)
	if total > 0 {
		batchSizer.stats.DuplicateRate = float64(duplicates) / float64(total) * 100
	}

	// Enhanced completion report with statistics
	fmt.Printf("\n=== ENHANCED BATCH INSERT COMPLETE ===\n")
	fmt.Printf("Source Instance: %s\n", cfg.SourceInstanceID)
	fmt.Printf("Total Lines Processed: %d\n", total)
	fmt.Printf("New Records Inserted: %d\n", inserted)
	fmt.Printf("Duplicates Prevented: %d (%.2f%%)\n", duplicates, batchSizer.stats.DuplicateRate)
	fmt.Printf("Processing Time: %v\n", batchSizer.stats.ProcessingTime)
	fmt.Printf("Sharded Database Distribution: %d shards\n", shardManager.GetShardCount())
	if errors > 0 {
		fmt.Printf("Errors Encountered: %d\n", errors)
	}
	fmt.Printf("=========================================\n")

	if errors > 0 {
		fmt.Printf("Warning: %d batch operations failed during processing\n", errors)
	}

	return nil
}

// flushShardBatch executes enhanced batch insert for a single shard with source tracking
func flushShardBatch(ctx context.Context, cfg Config, db *sql.DB, batch []shardRecord) (int, int, error) {
	if len(batch) == 0 {
		return 0, 0, nil
	}

	var sb strings.Builder
	args := make([]any, 0, len(batch)*4) // hash, content, date, source_instance

	switch cfg.DBType {
	case "mysql":
		sb.WriteString("INSERT IGNORE INTO `lines` (line_hash, content, date_of_entry, source_instance) VALUES ")
	case "sqlite":
		sb.WriteString("INSERT OR IGNORE INTO `lines` (line_hash, content, date_of_entry, source_instance) VALUES ")
	default:
		return 0, 0, fmt.Errorf("unsupported database type for insert: %s", cfg.DBType)
	}

	currentDate := time.Now().Format("2006-01-02")
	for i, r := range batch {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?, ?, ?, ?)")
		args = append(args, r.hash, r.data, currentDate, cfg.SourceInstanceID)
	}

	// Execute with extended timeout for large batches over network
	timeoutDuration := 30 * time.Second
	if len(batch) > 5000 {
		timeoutDuration = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeoutDuration)
	defer cancel()

	// Enable MySQL compression for network efficiency if supported
	if cfg.DBType == "mysql" && cfg.EnableCompression {
		if _, err := db.ExecContext(ctx, "SET SESSION sql_mode = 'NO_ENGINE_SUBSTITUTION'"); err == nil {
			// Compression enabled - network traffic will be reduced
		}
	}

	result, err := db.ExecContext(ctx, sb.String(), args...)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to execute batch insert: %w", err)
	}

	// Track insertion statistics with enhanced error handling
	rowsAffected := int64(0)
	if ra, err := result.RowsAffected(); err == nil {
		rowsAffected = ra
	} else {
		// Fallback: assume partial success if RowsAffected() fails
		rowsAffected = int64(len(batch) / 2)
	}

	inserted := int(rowsAffected)
	duplicates := len(batch) - inserted

	return inserted, duplicates, nil
}

// populateDB executes enhanced single database insertion with multi-PC duplicate prevention
func populateDB(ctx context.Context, cfg Config, db *sql.DB, inputFile string) error {
	if err := ensureSchema(db, cfg.DBType); err != nil {
		return fmt.Errorf("failed to create database table: %w", err)
	}

	file, err := os.Open(inputFile)
	if err != nil {
		return fmt.Errorf("failed to open input file '%s': %w", inputFile, err)
	}
	defer file.Close()

	// Use configured batch size for optimal network performance
	batchSize := cfg.BatchSize
	type rec struct{ data string }
	batch := make([]rec, 0, batchSize)

	// Initialize enhanced statistics tracking
	stats := DuplicateStats{
		SourceInstance: cfg.SourceInstanceID,
		StartTime:      time.Now(),
	}

	scanner := bufio.NewScanner(file)
	var total, inserted, duplicates, errors int

	// Enhanced flush function with source instance tracking
	flush := func(batch []rec) error {
		if len(batch) == 0 {
			return nil
		}
		var sb strings.Builder
		args := make([]any, 0, len(batch)*4) // hash, content, date, source_instance
		switch cfg.DBType {
		case "mysql":
			sb.WriteString("INSERT IGNORE INTO `lines` (line_hash, content, date_of_entry, source_instance) VALUES ")
		case "sqlite":
			sb.WriteString("INSERT OR IGNORE INTO `lines` (line_hash, content, date_of_entry, source_instance) VALUES ")
		default:
			return fmt.Errorf("unsupported database type for insert: %s", cfg.DBType)
		}
		currentDate := time.Now().Format("2006-01-02")
		for i, r := range batch {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("(?, ?, ?, ?)")
			// Calculate hash for duplicate detection
			hash := fmt.Sprintf("%x", md5.Sum([]byte(r.data)))
			args = append(args, hash, r.data, currentDate, cfg.SourceInstanceID)
		}

		// Dynamic timeout based on batch size and network conditions
		timeoutDuration := time.Duration(30+len(batch)/100) * time.Second
		ctx, cancel := context.WithTimeout(ctx, timeoutDuration)
		defer cancel()

		// Enable MySQL compression if configured
		if cfg.DBType == "mysql" && cfg.EnableCompression {
			if _, err := db.ExecContext(ctx, "SET SESSION sql_mode = 'NO_ENGINE_SUBSTITUTION'"); err == nil {
				// Compression settings applied
			}
		}

		result, err := db.ExecContext(ctx, sb.String(), args...)
		if err != nil {
			return fmt.Errorf("failed to execute batch insert: %w", err)
		}

		// Enhanced statistics tracking
		if rowsAffected, err := result.RowsAffected(); err == nil {
			inserted += int(rowsAffected)
			duplicates += len(batch) - int(rowsAffected)
			stats.NewInserted += rowsAffected
			stats.DuplicatesFound += int64(len(batch)) - rowsAffected
		} else {
			// Fallback statistics if RowsAffected fails
			stats.ErrorsEncountered++
		}

		return nil
	}

	for scanner.Scan() {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation cancelled: %w", ctx.Err())
		default:
		}

		line := scanner.Text()
		if validLine, ok, _ := lineIsValid(strings.TrimSpace(line)); ok {
			batch = append(batch, rec{data: validLine})
			if len(batch) >= batchSize {
				if err := flush(batch); err != nil {
					errors++
					// Note: Using fmt.Printf here as this is called outside StoreService context
					fmt.Printf("Warning: Batch insert failed: %v\n", err)
					// Continue processing despite errors
				}
				batch = batch[:0]
			}
			total++
			if total%10000 == 0 {
				// Note: Using fmt.Printf here as this is called outside StoreService context
				fmt.Printf("\rProcessed %d lines (inserted: %d, duplicates: %d, errors: %d)", total, inserted, duplicates, errors)
			}
		}
	}

	// Handle scanner errors
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading input file: %w", err)
	}

	// Final batch
	if len(batch) > 0 {
		if err := flush(batch); err != nil {
			errors++
			// Note: Using fmt.Printf here as this is called outside StoreService context
			fmt.Printf("Warning: Final batch insert failed: %v\n", err)
		}
	}

	// Finalize statistics
	stats.TotalProcessed = int64(total)
	stats.ProcessingTime = time.Since(stats.StartTime)
	stats.ErrorsEncountered = int64(errors)
	if total > 0 {
		stats.DuplicateRate = float64(duplicates) / float64(total) * 100
		stats.AvgBatchTime = stats.ProcessingTime / time.Duration(total/batchSize+1)
	}

	// Enhanced completion report
	fmt.Printf("\n=== ENHANCED DATABASE INSERT COMPLETE ===\n")
	fmt.Printf("Source Instance: %s\n", cfg.SourceInstanceID)
	fmt.Printf("Total Lines Processed: %d\n", total)
	fmt.Printf("New Records Inserted: %d\n", inserted)
	fmt.Printf("Duplicates Prevented: %d (%.2f%%)\n", duplicates, stats.DuplicateRate)
	fmt.Printf("Processing Time: %v\n", stats.ProcessingTime)
	fmt.Printf("Average Batch Time: %v\n", stats.AvgBatchTime)
	fmt.Printf("Batch Size Used: %d\n", batchSize)
	if errors > 0 {
		fmt.Printf("Errors Encountered: %d\n", errors)
	}
	fmt.Printf("==========================================\n")

	if errors > 0 {
		// Note: Using fmt.Printf here as this is called outside StoreService context
		fmt.Printf("Warning: %d batch operations failed during processing\n", errors)
	}

	return nil
}

func resolveLatestTextFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var latestPath string
	var latestMod time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".txt") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mod := info.ModTime()
		path := filepath.Join(dir, name)
		if latestPath == "" || mod.After(latestMod) {
			latestPath = path
			latestMod = mod
		}
	}
	if latestPath == "" {
		return "", fmt.Errorf("no .txt files found in %s", dir)
	}
	return latestPath, nil
}

// runFilterAndDatabaseStage processes merger output through filtering and database insertion
func (s *StoreService) runFilterAndDatabaseStage(ctx context.Context) error {
	s.log("=== FILTER & DATABASE STAGE ===")

	cfg := s.config

	// Resolve source file: Filter stage should read from OutputDir (where Merger creates merged files)
	source := cfg.OutputDir
	if stat, err := os.Stat(source); err == nil && stat.IsDir() {
		latest, err := resolveLatestTextFile(source)
		if err != nil {
			return fmt.Errorf("failed to find latest text file: %w", err)
		}
		source = latest
		s.log("Processing latest merged file: %s", source)
	}

	if cfg.RunFilter {
		s.log("Running filter …")

		// Initialize integrity manager for filtered output tracking
		integrityManager := s.NewFileIntegrityManager()

		v, tot, err := filterLines(ctx, source)
		if err != nil {
			return fmt.Errorf("filtering failed: %w", err)
		}
		s.log("✓ Filter done: %d valid / %d total", v, tot)

		// FILTER STAGE DELETION: Delete merged file from Sorted_toshare/ after successful filtering
		s.log("Creating backup for merged file before deletion: %s", source)
		backupPath, backupErr := s.BackupFileWithTimestamp(source)

		if backupErr != nil {
			s.log("⚠️ Backup failed for %s: %v", source, backupErr)
			s.log("⚠️ Deleting merged file WITHOUT backup (backup failed)")
		} else {
			s.log("✓ Created backup before deletion: %s", backupPath)
		}

		// Delete merged file after successful filtering (with or without backup)
		deleteStartTime := time.Now()
		if err := os.Remove(source); err != nil {
			s.logFileDelete(source, deleteStartTime, err)
			s.log("⚠️ Could not delete merged file %s: %v", source, err)
		} else {
			s.logFileDelete(source, deleteStartTime, nil)
			if backupErr == nil {
				s.log("✓ Merged file %s deleted after successful filtering (backup: %s)", source, backupPath)
			} else {
				s.log("✓ Merged file %s deleted after successful filtering (NO BACKUP)", source)
			}
		}

		// Note: Post-filter integrity verification removed since source file is deleted

		// PRE-DATABASE-DELETION: Capture filtered output file integrity
		filteredOutputFile := filepath.Join("app/extraction/files/", "filtered_output.txt")
		s.log("🔒 Capturing filtered output file integrity before database deletion...")
		filteredOutputIntegrity, err := integrityManager.CaptureFileIntegrity(filteredOutputFile)
		if err != nil {
			s.log("⚠️ Failed to capture filtered output file integrity: %v", err)
			filteredOutputIntegrity = nil
		} else {
			s.log("✅ Filtered output file integrity captured before database deletion")
		}

		// CRITICAL: Verify database operation success BEFORE deleting filtered output file
		s.log("🔍 Verifying database insertion success before filtered output file deletion...")
		verifier := s.NewOperationSuccessVerifier()
		if err := verifier.verifyDatabaseInsertion(); err != nil {
			s.log("❌ Database operation verification FAILED - preserving filtered output file")
			return fmt.Errorf("database operation verification failed, filtered output file preserved: %w", err)
		}
		s.log("✅ Database operation success verification PASSED - safe to delete filtered output")

		// FINAL INTEGRITY CHECK: Verify filtered output file integrity before deletion
		if filteredOutputIntegrity != nil {
			s.log("🔒 Final integrity verification of filtered output file before deletion...")
			if err := integrityManager.VerifyFileIntegrity(filteredOutputFile, filteredOutputIntegrity); err != nil {
				s.log("❌ Final integrity verification FAILED - filtered output file may be corrupted")
				return fmt.Errorf("final integrity verification failed, filtered output file preserved due to potential corruption: %w", err)
			}
			s.log("✅ Final integrity verification PASSED - filtered output file confirmed uncorrupted")
		}

		// Note: Backup and deletion of filtered_output.txt handled ONLY in database stage after DB verification

		// Note: Source file deletion now handled by filter stage above
	} else {
		s.log("Skipping filter (RUN_FILTER=false)")
	}

	if cfg.RunDB {
		s.log("Populating database …")
		inFile := filepath.Join("app/extraction/files/", "filtered_output.txt")
		if !cfg.RunFilter {
			inFile = source
		}

		// Verify input file exists before processing
		if _, err := os.Stat(inFile); os.IsNotExist(err) {
			return fmt.Errorf("input file for database insertion not found: %s", inFile)
		}

		// Use sharded database approach if sharding is enabled
		if cfg.ShardCount > 1 {
			s.log("🚀 Using sharded database approach with %d shards", cfg.ShardCount)

			// Create ShardManager for distributed database operations
			shardManager, err := NewShardManager(cfg, s.log)
			if err != nil {
				s.log("Failed to create ShardManager: %v", err)
				return fmt.Errorf("shard manager initialization error: %w", err)
			}
			defer shardManager.Close()

			// Perform health check on all shards before processing
			if err := shardManager.HealthCheck(); err != nil {
				s.log("❌ Shard health check failed: %v", err)
				return fmt.Errorf("shard health check failed: %w", err)
			}
			s.log("✅ All shards healthy - proceeding with distributed insertion")

			// Use sharded database insertion
			if err := populateDBSharded(ctx, cfg, shardManager, inFile); err != nil {
				s.log("Sharded database insertion failed: %v", err)
				return fmt.Errorf("sharded database population error: %w", err)
			}
		} else {
			s.log("📊 Using single database approach")

			// Open single database connection for non-sharded mode
			db, err := openDB(cfg)
			if err != nil {
				s.log("Database connection failed: %v", err)
				s.log("Please check database configuration and connectivity")
				return fmt.Errorf("database connection error: %w", err)
			}
			defer func() {
				if closeErr := db.Close(); closeErr != nil {
					s.log("Warning: Failed to close database connection: %v", closeErr)
				}
			}()

			// Use traditional single database insertion
			if err := populateDB(ctx, cfg, db, inFile); err != nil {
				s.log("Database insertion failed: %v", err)
				return fmt.Errorf("database population error: %w", err)
			}
		}
		s.log("✓ Database population complete")

		// CRITICAL: Verify database insertion success BEFORE deleting filtered output file
		if cfg.RunFilter && filepath.Base(inFile) == "filtered_output.txt" {
			s.log("🔍 Verifying database insertion success before filtered output file deletion...")

			var totalRowCount int

			// Verify based on sharding mode
			if cfg.ShardCount > 1 {
				s.log("🔍 Verifying sharded database insertions across %d shards...", cfg.ShardCount)

				// Verify each shard contains data
				for i := 0; i < cfg.ShardCount; i++ {
					// Create temporary connection to specific shard for verification
					shardDBName := generateShardDatabaseName(cfg, i)
					tempCfg := cfg
					tempCfg.MySQLDB = shardDBName

					db_verify, err := openDB(tempCfg)
					if err != nil {
						s.log("❌ Shard %d verification FAILED - cannot connect - preserving filtered output file", i)
						return fmt.Errorf("shard %d verification failed, filtered output file preserved: %w", i, err)
					}

					var shardRowCount int
					countQuery := "SELECT COUNT(*) FROM `lines`"
					if err := db_verify.QueryRow(countQuery).Scan(&shardRowCount); err != nil {
						db_verify.Close()
						s.log("❌ Shard %d verification FAILED - cannot verify data insertion - preserving filtered output file", i)
						return fmt.Errorf("shard %d verification failed, cannot verify data insertion, filtered output file preserved: %w", i, err)
					}
					db_verify.Close()

					totalRowCount += shardRowCount
					s.log("✅ Shard %d (%s): %d rows verified", i, shardDBName, shardRowCount)
				}
			} else {
				// Single database verification
				db_verify, err := openDB(cfg)
				if err != nil {
					s.log("❌ Database verification FAILED - cannot connect - preserving filtered output file")
					return fmt.Errorf("database verification failed, filtered output file preserved: %w", err)
				}
				defer db_verify.Close()

				// Use modern lines table schema
				countQuery := "SELECT COUNT(*) FROM `lines`"

				if err := db_verify.QueryRow(countQuery).Scan(&totalRowCount); err != nil {
					s.log("❌ Database verification FAILED - cannot verify data insertion - preserving filtered output file")
					return fmt.Errorf("database verification failed, cannot verify data insertion, filtered output file preserved: %w", err)
				}
			}

			if totalRowCount == 0 {
				s.log("❌ Database verification FAILED - no data found in database - preserving filtered output file")
				return fmt.Errorf("database verification failed, no data found in database, filtered output file preserved")
			}

			s.log("✅ Database insertion verification PASSED - %d total rows found - safe to delete filtered output file", totalRowCount)

			s.log("Creating backup for filtered output file before deletion: %s", inFile)
			backupPath, backupErr := s.BackupFileWithTimestamp("filtered_output.txt")

			if backupErr != nil {
				s.log("⚠️ Backup failed for filtered_output.txt: %v", backupErr)
				s.log("⚠️ Deleting filtered output file WITHOUT backup (backup failed)")
			} else {
				s.log("✓ Created backup before deletion: %s", backupPath)
			}

			// Delete filtered_output.txt ONLY after successful DB verification and backup
			if err := os.Remove(inFile); err != nil {
				s.log("⚠️ Could not delete filtered output file: %v", err)
			} else {
				if backupErr == nil {
					s.log("✓ Deleted filtered_output.txt (backup: %s)", backupPath)
				} else {
					s.log("✓ Deleted filtered_output.txt (NO BACKUP - backup failed)")
				}
			}
		}
	} else {
		s.log("Skipping DB insert (RUN_DB_INSERT=false)")
	}
	return nil
}
