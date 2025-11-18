# Quick Start Guide - Pipelined Architecture

This guide will help you get the Telegram Bot Coordinator running with the new pipelined multi-round architecture.

## Fully Autonomous Setup (Recommended)

```bash
./setup.sh --yes
```

This will automatically:
- ✅ Install missing packages (jq, curl, lsof)
- ✅ Install PostgreSQL if not found
- ✅ Start PostgreSQL if not running
- ✅ Set up database and migrations
- ✅ Build and start the coordinator
- ✅ Verify system health

**No manual intervention required!**

## Prerequisites

The setup script will handle most of these automatically:

- **Go 1.24+** (must be pre-installed - download from https://golang.org/dl/)
- **PostgreSQL 14+** (auto-installed if missing)
- **Telegram Bot Token** from @BotFather (configure in .env)
- **Admin User IDs** (your Telegram user ID - configure in .env)
- **Optional**: Local Bot API Server for files >20MB

## Step 1: Database Setup

```bash
# Create database and user
createdb telegram_bot_option2
createuser bot_user

# Grant privileges
psql -c "ALTER USER bot_user WITH PASSWORD 'your_secure_password';"
psql telegram_bot_option2 -c "GRANT ALL PRIVILEGES ON DATABASE telegram_bot_option2 TO bot_user;"

# Run all migrations
cd database/migrations
for file in *.sql; do
    echo "Running $file..."
    psql -U bot_user -d telegram_bot_option2 -f "$file"
done

# Verify tables created
psql -U bot_user -d telegram_bot_option2 -c "\dt"
```

Expected tables:
- `download_queue` - File download queue
- `processing_rounds` - Round tracking through pipeline stages
- `store_tasks` - Individual store tasks (2 files each)
- `processing_metrics` - Time-series metrics
- `batch_processing`, `batch_files` - Deprecated (kept for rollback)

## Step 2: Configuration

```bash
# Copy example configuration
cp .env.example .env

# Edit configuration
nano .env
```

**Required settings**:
```bash
# Telegram Bot
TELEGRAM_BOT_TOKEN=your_bot_token_from_botfather
ADMIN_IDS=your_telegram_user_id,other_user_id  # Comma-separated

# Database
DB_HOST=localhost
DB_PORT=5432
DB_NAME=telegram_bot_option2
DB_USER=bot_user
DB_PASSWORD=your_secure_password
DB_SSL_MODE=disable  # Use 'require' in production

# Workers (defaults are optimal for most cases)
MAX_DOWNLOAD_WORKERS=3      # MUST be 3 (Telegram API limit)
MAX_STORE_WORKERS=5         # Increase to 10-20 for high throughput
ROUND_SIZE=50               # Files per round (10-100)
ROUND_TIMEOUT_SEC=300       # Create round if files wait >5min

# Optional: Local Bot API (for files >20MB)
USE_LOCAL_BOT_API=false
LOCAL_BOT_API_URL=http://localhost:8081
```

## Step 3: Verify Preserved Files

```bash
# CRITICAL: Verify checksums of preserved code
sha256sum -c checksums.txt
```

Expected output:
```
app/extraction/extract/extract.go: OK
app/extraction/convert/convert.go: OK
app/extraction/store.go: OK
```

⚠️ **If checksums don't match, DO NOT PROCEED** - restore from backup.zip:
```bash
unzip -o backup.zip -d app/extraction/
```

## Step 4: Build Coordinator

```bash
# Clean dependencies
go mod tidy

# Build coordinator
cd coordinator
go build -o telegram-bot-coordinator

# Verify binary created
ls -lh telegram-bot-coordinator
```

## Step 5: Create Required Directories

```bash
# From project root
mkdir -p downloads logs store_tasks archive/failed
mkdir -p app/extraction/files/{all,pass,txt,nonsorted,done,errors,etbanks,nopass}

# Copy password list for archive extraction
cp pass.txt.example app/extraction/pass.txt  # Edit with your passwords
```

## Step 6: Start Coordinator

```bash
cd coordinator

# Run in foreground (for testing)
./telegram-bot-coordinator

# Or run in background
nohup ./telegram-bot-coordinator > ../logs/coordinator.log 2>&1 &

# Or use systemd service (recommended for production)
sudo cp telegram-bot-coordinator.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl start telegram-bot-coordinator
sudo systemctl enable telegram-bot-coordinator
```

## Step 7: Verify System is Running

```bash
# Check health endpoint
curl http://localhost:8080/health | jq .

# Expected response:
{
  "status": "healthy",
  "timestamp": "2025-11-18T14:51:00Z",
  "components": {
    "database": "healthy",
    "filesystem": "healthy"
  }
}

# Check Prometheus metrics
curl http://localhost:9090/metrics | grep telegram_bot

# View logs
tail -f ../logs/coordinator.log
```

## Step 8: Test with Telegram

1. **Start conversation with your bot** on Telegram
2. **Send `/start`** - should receive welcome message
3. **Upload a test file** (ZIP, RAR, or TXT)
4. **Monitor processing**:

```bash
# Watch queue status
watch -n 2 'psql -U bot_user -d telegram_bot_option2 -c \
  "SELECT status, COUNT(*) FROM download_queue GROUP BY status;"'

# Watch active rounds
watch -n 5 'psql -U bot_user -d telegram_bot_option2 -c \
  "SELECT round_id, round_status, extract_status, convert_status, store_status
   FROM processing_rounds
   WHERE round_status != '\''COMPLETED'\''
   ORDER BY created_at DESC;"'

# Monitor store tasks
watch -n 5 'psql -U bot_user -d telegram_bot_option2 -c \
  "SELECT COUNT(*) as active FROM store_tasks WHERE status='\''STORING'\'';"'
```

## Architecture Verification

After uploading files, verify the pipelined architecture is working:

```sql
-- Check round progression (should see multiple rounds in different stages)
SELECT round_id, round_status, extract_status, convert_status, store_status
FROM processing_rounds
ORDER BY created_at DESC
LIMIT 10;
```

**Expected pipeline behavior**:
- Round 1: STORING (store tasks in progress)
- Round 2: CONVERTING (convert worker processing)
- Round 3: EXTRACTING (extract worker processing)
- Round 4: CREATED (waiting for extract)

This confirms multiple rounds are progressing through stages simultaneously.

## Monitoring Queries

```sql
-- Overall system statistics
SELECT
  round_status,
  COUNT(*) as count,
  AVG(extract_duration_sec) as avg_extract_sec,
  AVG(convert_duration_sec) as avg_convert_sec,
  AVG(store_duration_sec) as avg_store_sec
FROM processing_rounds
GROUP BY round_status;

-- Store task performance
SELECT
  round_id,
  COUNT(*) as total_tasks,
  SUM(CASE WHEN status='COMPLETED' THEN 1 ELSE 0 END) as completed,
  SUM(CASE WHEN status='STORING' THEN 1 ELSE 0 END) as in_progress,
  AVG(duration_sec) as avg_duration_sec
FROM store_tasks
GROUP BY round_id
ORDER BY round_id DESC
LIMIT 10;

-- Throughput (files per hour)
SELECT
  COUNT(*)::float /
  EXTRACT(EPOCH FROM (MAX(completed_at) - MIN(created_at)))/3600 as files_per_hour
FROM download_queue
WHERE status = 'DOWNLOADED'
  AND completed_at > NOW() - INTERVAL '24 hours';
```

## Troubleshooting

### Issue: "Database connection failed"
```bash
# Check PostgreSQL is running
sudo systemctl status postgresql

# Test connection manually
psql -U bot_user -d telegram_bot_option2 -c "SELECT 1;"

# Check .env file for correct credentials
grep DB_ .env
```

### Issue: "Telegram API rate limit"
```bash
# Verify exactly 3 download workers
grep MAX_DOWNLOAD_WORKERS .env

# Check current rate
tail -f logs/coordinator.log | grep "rate limit"
```

### Issue: "Checksums don't match"
```bash
# Restore from backup
cd app/extraction
unzip -o ../../backup.zip

# Verify restoration
cd ../..
sha256sum -c checksums.txt
```

### Issue: "Store tasks stuck in STORING"
```bash
# Check for stuck tasks (>2 hours)
psql -U bot_user -d telegram_bot_option2 -c \
  "SELECT task_id, round_id, status, started_at
   FROM store_tasks
   WHERE status = 'STORING'
     AND started_at < NOW() - INTERVAL '2 hours';"

# Crash recovery will automatically reset these on next restart
sudo systemctl restart telegram-bot-coordinator
```

## Performance Tuning

### Scaling Store Workers

For higher throughput, increase store workers:

```bash
# Edit .env
MAX_STORE_WORKERS=10  # Increase from 5 to 10

# Restart coordinator
sudo systemctl restart telegram-bot-coordinator

# Monitor resource usage
htop  # Watch CPU and RAM

# Rule of thumb: ~1GB RAM + 1 CPU core per store worker
```

### Adjusting Round Size

```bash
# Smaller rounds (faster feedback, more overhead)
ROUND_SIZE=25

# Larger rounds (better throughput, slower feedback)
ROUND_SIZE=75

# Restart coordinator
sudo systemctl restart telegram-bot-coordinator
```

## Production Deployment Checklist

- [ ] PostgreSQL configured for production (SSL, backups)
- [ ] `.env` file has production credentials
- [ ] `DB_SSL_MODE=require` in .env
- [ ] Systemd service configured for auto-restart
- [ ] Log rotation configured
- [ ] Monitoring set up (Prometheus + Grafana)
- [ ] Backup strategy for database
- [ ] Disk space monitoring (need ample space for downloads/rounds)
- [ ] Local Bot API server running (for files >20MB)
- [ ] Firewall rules allow only necessary ports
- [ ] Checksums verified: `sha256sum -c checksums.txt`

## Next Steps

1. **Test load**: Upload 100+ files and monitor throughput
2. **Validate pipeline**: Confirm multiple rounds progress simultaneously
3. **Optimize**: Adjust ROUND_SIZE and MAX_STORE_WORKERS based on performance
4. **Monitor**: Set up Grafana dashboards for Prometheus metrics
5. **Backup**: Configure automated database backups
6. **Scale**: Increase MAX_STORE_WORKERS to 10-20 for production loads

## Getting Help

- Check `CLAUDE.md` for detailed architecture documentation
- Review `Docs/pipelined-architecture-design.md` for design rationale
- See `IMPLEMENTATION_SUMMARY.md` for complete implementation details
- Inspect logs: `tail -f logs/coordinator.log`
- Database queries: See CLAUDE.md "Monitoring" section

## Performance Targets

| Metric | Target | Measurement |
|--------|--------|-------------|
| Processing time (1000 files) | < 8 hours | End-to-end from upload to DB |
| Throughput | ≥ 125 files/hour | Sustained rate |
| Success rate | ≥ 98% | Completed rounds / total rounds |
| Pipeline stages active | 3 concurrent | Extract + Convert + Store |
| Store workers utilized | ≥ 80% | Active tasks / max workers |

Achieving **2.6× speedup** over sequential processing through pipeline parallelism.
