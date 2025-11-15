# Option 2: Batch-Based Parallel Processing System
## Project Structure for Future Implementation

**Status**: Design Complete - Ready for Implementation
**Architecture**: Batch-based parallel processing with 6× performance improvement
**Code Preservation**: 100% - extract.go, convert.go, store.go unchanged

---

## 📁 Folder Structure

```
option2-project/
├── app/
│   └── extraction/
│       ├── extract/
│       │   └── extract.go           ✅ PRESERVED - Do not modify
│       ├── convert/
│       │   └── convert.go           ✅ PRESERVED - Do not modify
│       ├── store.go                 ✅ PRESERVED - Do not modify
│       ├── pass.txt                 Password list (shared)
│       └── files/                   Template structure (copied to each batch)
│           ├── all/                 Archives for extraction
│           ├── txt/                 Direct TXT files
│           ├── pass/                Extracted files
│           ├── nopass/              Password-protected archives
│           └── errors/              Extraction errors
│
├── batches/                         🔄 AUTO-MANAGED - Batch workspaces created dynamically
│   ├── batch_001/                  (Example structure - created at runtime)
│   │   ├── app/extraction/files/
│   │   └── logs/
│   └── .gitkeep
│
├── downloads/                       📥 Temporary download storage
│   └── .gitkeep
│
├── archive/                         🗄️ Failed batch archive (7-day retention)
│   └── failed/
│       └── .gitkeep
│
├── coordinator/                     🎯 TO BE IMPLEMENTED
│   ├── main.go
│   ├── config.go
│   ├── logger.go
│   ├── health.go
│   ├── metrics.go
│   ├── telegram_receiver.go
│   ├── download_worker.go
│   ├── batch_coordinator.go
│   ├── batch_worker.go
│   ├── batch_cleanup.go
│   ├── crash_recovery.go
│   └── admin_commands.go
│
├── database/
│   └── migrations/                 SQL migration files
│       ├── 001_create_download_queue.sql
│       ├── 002_create_batch_processing.sql
│       ├── 003_create_batch_files.sql
│       └── 004_create_metrics.sql
│
├── logs/                           📝 System logs
│   └── .gitkeep
│
├── scripts/                        🔧 Operational scripts
│   ├── migrate.sh
│   ├── setup.sh
│   └── cleanup.sh
│
├── tests/                          🧪 Test suites
│   ├── unit/
│   ├── integration/
│   └── load/
│
├── monitoring/                     📊 Monitoring configuration
│   ├── prometheus.yml
│   ├── prometheus-alerts.yml
│   └── dashboards/
│       └── telegram-bot.json
│
├── docs/                           📚 Additional documentation
│   └── RUNBOOK.md (to be created in Phase 7)
│
├── .env.example                    🔐 Environment template
├── .gitignore
├── go.mod
├── go.sum
├── Dockerfile
├── docker-compose.yml
├── checksums.txt                   ✅ Verification file for preserved code
└── README.md                       📖 This file
```

---

## 🎯 Purpose

This folder contains the **complete project structure** for Option 2 implementation. It includes:

1. **Preserved Code Files**: extract.go, convert.go, store.go (100% unchanged)
2. **Template Directory Structure**: Ready for batch processing
3. **Implementation Blueprint**: Follow the implementation plan to build coordinator

---

## 📋 Prerequisites for Implementation

### Required Software
- **Go 1.21+**: For building the coordinator service
- **PostgreSQL 14+**: Database for persistent queues and task tracking
- **Docker & Docker Compose**: For Local Bot API Server and development environment
- **Telegram Bot Token**: With Local Bot API Server configured for 4GB file support
- **Git**: Version control

### Required Credentials
- Telegram Bot Token (store in `.env`)
- Admin Telegram User IDs (comma-separated in `.env`)
- PostgreSQL credentials

---

## 🚀 Quick Start Guide

### Step 1: Initial Setup

```bash
# Navigate to this directory
cd docs/option2-project

# Create .env file from template
cp ../../docs/option2-implementation-plan.md .  # Reference for .env structure
# Edit .env with your values

# Verify preserved code files
ls -lh app/extraction/extract/extract.go
ls -lh app/extraction/convert/convert.go
ls -lh app/extraction/store.go

# Compute checksums for verification
sha256sum app/extraction/extract/extract.go > checksums.txt
sha256sum app/extraction/convert/convert.go >> checksums.txt
sha256sum app/extraction/store.go >> checksums.txt
echo "✅ Checksums saved to checksums.txt"
```

### Step 2: Follow Implementation Plan

The implementation is divided into **7 phases over 12 weeks**:

**Phase 1 (Week 1-2)**: Foundation
- Database schema creation
- Configuration management
- Logging framework
- Health & metrics endpoints

**Phase 2 (Week 3-4)**: Download Pipeline
- Telegram bot receiver
- Download worker pool (3 concurrent)
- SHA256 hash computation
- Crash recovery

**Phase 3 (Week 5-6)**: Batch Coordinator
- Batch creation logic
- Directory structure generation
- File routing (archives → all/, TXT → txt/)
- Max concurrent batch enforcement

**Phase 4 (Week 7-8)**: Batch Processing Workers
- Batch worker pool (5 concurrent)
- Extract → Convert → Store pipeline
- Working directory management
- Batch cleanup service

**Phase 5 (Week 9)**: Admin Commands
- `/queue`, `/batches`, `/stats`, `/health` commands
- Real-time statistics from PostgreSQL

**Phase 6 (Week 10-11)**: Testing & Optimization
- Unit tests, integration tests, load tests
- Performance profiling and optimization

**Phase 7 (Week 12)**: Documentation & Deployment
- Docker Compose & Kubernetes setup
- Grafana dashboards
- Runbook and operational procedures

---

## 📚 Documentation References

**Complete documentation is located in the parent `docs/` directory:**

### Design Documents
- **`option2-prd.md`**: Product Requirements Document
  - Business objectives and success metrics
  - Functional and non-functional requirements
  - Database schema and API specifications

- **`option2-batch-parallel-design.md`**: Detailed Architecture Design
  - Batch directory structure and isolation
  - Working directory pattern for code preservation
  - Performance analysis and timeline comparisons

- **`option2-implementation-plan.md`**: Phase-by-Phase Implementation Guide (Part 1)
  - Phases 1-3: Foundation, Download Pipeline, Batch Coordinator
  - Detailed code implementations for each component

- **`option2-implementation-plan-part2.md`**: Implementation Guide (Part 2)
  - Phases 4-7: Batch Workers, Admin Commands, Testing, Deployment
  - Monitoring, rollback plans, and troubleshooting
  - Prompts for Claude Code

### Implementation Order

```
1. Read: option2-prd.md (understand requirements)
2. Read: option2-batch-parallel-design.md (understand architecture)
3. Follow: option2-implementation-plan.md (Phase 1-3)
4. Follow: option2-implementation-plan-part2.md (Phase 4-7)
5. Use: Prompts from Section 9 of Part 2 for Claude Code assistance
```

---

## 🤖 Using Claude Code for Implementation

### Recommended Approach

**Use the prompts from `option2-implementation-plan-part2.md` Section 9** to guide Claude Code through implementation. Each prompt is designed for a specific phase:

#### Phase 1: Foundation Setup
```
Use Prompt 9.2 from option2-implementation-plan-part2.md

This will implement:
- Config management
- Logger initialization
- Health check endpoint
- Metrics endpoint
- Main entry point skeleton
```

#### Phase 2: Download Pipeline
```
Use Prompt 9.3 from option2-implementation-plan-part2.md

This will implement:
- Telegram bot receiver
- Download worker pool
- SHA256 hash computation
- Crash recovery
```

#### Phase 3: Batch Coordinator
```
Use Prompt 9.4 from option2-implementation-plan-part2.md

This will implement:
- Batch creation logic
- Directory structure generation
- File routing
```

#### Phase 4: Batch Workers
```
Use Prompt 9.5 from option2-implementation-plan-part2.md

CRITICAL: This phase executes extract.go, convert.go, store.go
The prompt ensures code preservation through working directory changes.
```

#### Subsequent Phases
Continue with Prompts 9.6, 9.7, 9.8 for Admin Commands, Testing, and Deployment.

---

## ⚠️ Critical Implementation Rules

### Code Preservation (100% Requirement)

**NEVER modify these files:**
- `app/extraction/extract/extract.go`
- `app/extraction/convert/convert.go`
- `app/extraction/store.go`

**Verification**:
```bash
# After ANY changes, verify checksums match
sha256sum -c checksums.txt

# Expected output:
# app/extraction/extract/extract.go: OK
# app/extraction/convert/convert.go: OK
# app/extraction/store.go: OK
```

If checksums don't match, **REVERT IMMEDIATELY** and restore from backup.

### Worker Concurrency Limits

**Fixed Limits (NEVER CHANGE)**:
- **Download Workers**: Exactly 3 (Telegram API rate limit)
- **Batch Workers**: Maximum 5 (configurable, but tested at 5)

**Reason**: Extract and convert process entire directories. Multiple workers on the same directory cause race conditions and corruption.

### Batch Processing Pattern

**Working Directory Approach**:
```go
// CORRECT: Change working directory before execution
originalWD, _ := os.Getwd()
defer os.Chdir(originalWD)  // Always restore

os.Chdir("batches/batch_001")  // Change to batch root
exec.Command("go", "run", "../../app/extraction/extract/extract.go")  // Relative path to preserved code
```

**Why this works**:
- extract.go processes files in `app/extraction/files/all/` (relative to working directory)
- By changing to `batches/batch_001/`, extract.go processes `batches/batch_001/app/extraction/files/all/`
- Code remains 100% unchanged

---

## 🧪 Testing Strategy

### Unit Tests
```bash
# Test individual components
go test ./coordinator/...

# Test with coverage
go test -cover ./coordinator/...
```

### Integration Tests
```bash
# Full pipeline test
go test -v ./tests/integration/...
```

### Load Tests
```bash
# 100 files baseline
go test -v -timeout 3h ./tests/load -run TestLoad100Files

# 1000 files stress test
go test -v -timeout 24h ./tests/load -run TestLoad1000Files
```

### Verification Tests
```bash
# Verify code preservation
sha256sum -c checksums.txt

# Verify worker limits
# Upload files and check: SELECT COUNT(*) FROM download_queue WHERE status='DOWNLOADING';
# Should NEVER exceed 3

# Verify batch isolation
# Check that batch directories are independent
ls -lh batches/
```

---

## 📊 Performance Targets

| Metric | Target | Measurement |
|--------|--------|-------------|
| **Processing Time** (100 files) | < 2 hours | End-to-end from upload to completion |
| **Throughput** | ≥ 50 files/hour | Sustained over 24-hour period |
| **Success Rate** | ≥ 98% | (Successful / Total) × 100 |
| **RAM Usage** | < 20% | Peak during load test |
| **CPU Usage** | < 50% | Sustained average |
| **Concurrent Batches** | Exactly 5 max | Never exceeds MAX_BATCH_WORKERS |

---

## 🛠️ Common Operations

### Development Mode
```bash
# Start PostgreSQL and Local Bot API
docker-compose up -d postgres local-bot-api

# Run migrations
./scripts/migrate.sh

# Start coordinator in development mode
cd coordinator && go run .
```

### Production Deployment
```bash
# Build Docker image
docker build -t telegram-bot-option2:latest .

# Deploy with Docker Compose
docker-compose up -d

# OR Deploy to Kubernetes
kubectl apply -f k8s/
```

### Monitoring
```bash
# Check health
curl http://localhost:8080/health | jq .

# View metrics
curl http://localhost:9090/metrics

# Grafana Dashboard
open http://localhost:3000
```

### Troubleshooting
```bash
# View coordinator logs
tail -f logs/coordinator.log

# View batch logs
tail -f batches/batch_001/logs/extract.log

# Check queue status via Telegram
# Send /queue to bot

# Check database
psql -U bot_user -d telegram_bot_option2 -c "SELECT * FROM batch_processing WHERE status='FAILED';"
```

---

## 🔄 Migration from Option 1 (If Applicable)

If you currently have Option 1 (sequential processing) deployed:

### Migration Steps

1. **Backup Current System**
   ```bash
   pg_dump -U bot_user current_db > backup.sql
   tar -czf app_backup.tar.gz app/
   ```

2. **Deploy Option 2 in Parallel**
   - Use different database name
   - Use different Telegram bot token (create new bot for testing)
   - Deploy to separate infrastructure

3. **Gradual Migration**
   - Test Option 2 with small file sets
   - Compare results with Option 1
   - Verify data integrity

4. **Cutover**
   - Stop Option 1 bot
   - Point main bot token to Option 2
   - Monitor for 24 hours

5. **Rollback Plan**
   - Keep Option 1 infrastructure for 1 week
   - Ready to revert if critical issues found

---

## 📞 Support & Resources

### Documentation
- **PRD**: `../option2-prd.md`
- **Design**: `../option2-batch-parallel-design.md`
- **Implementation Plan Part 1**: `../option2-implementation-plan.md`
- **Implementation Plan Part 2**: `../option2-implementation-plan-part2.md`

### Implementation Assistance
Use the **Prompts for Claude Code** (Section 9 in Part 2) to get step-by-step guidance for each phase.

### Troubleshooting
Refer to **Runbook** (created in Phase 7) for common issues and solutions.

---

## ✅ Implementation Checklist

Track your progress through implementation:

### Setup
- [ ] Reviewed PRD and design documents
- [ ] Set up development environment (Go, PostgreSQL, Docker)
- [ ] Created `.env` file with required values
- [ ] Computed checksums for preserved code files
- [ ] Initialized Git repository

### Phase 1: Foundation (Week 1-2)
- [ ] Database schema created (4 migration files)
- [ ] Config management implemented
- [ ] Logger initialized
- [ ] Health endpoint working
- [ ] Metrics endpoint working
- [ ] Tests passing

### Phase 2: Download Pipeline (Week 3-4)
- [ ] Telegram receiver implemented
- [ ] Download workers implemented (exactly 3)
- [ ] SHA256 hash computation working
- [ ] Crash recovery implemented
- [ ] File uploads working end-to-end

### Phase 3: Batch Coordinator (Week 5-6)
- [ ] Batch creation logic implemented
- [ ] Directory structure created correctly
- [ ] File routing working (archives → all/, TXT → txt/)
- [ ] Max concurrent batches enforced
- [ ] Batch timeout logic working

### Phase 4: Batch Processing Workers (Week 7-8)
- [ ] Batch workers implemented (max 5)
- [ ] Extract stage working (subprocess execution)
- [ ] Convert stage working (subprocess execution)
- [ ] Store stage working (subprocess execution)
- [ ] Working directory pattern verified
- [ ] **Code preservation verified (checksums match)** ✅
- [ ] Batch cleanup service working

### Phase 5: Admin Commands (Week 9)
- [ ] `/queue` command working
- [ ] `/batches` command working
- [ ] `/stats` command working
- [ ] `/health` command working
- [ ] All commands admin-only verified

### Phase 6: Testing & Optimization (Week 10-11)
- [ ] Unit tests written and passing
- [ ] Integration tests passing
- [ ] Load test (100 files) < 2 hours
- [ ] Load test (1000 files) maintaining throughput
- [ ] Performance profiling completed
- [ ] Bottlenecks optimized
- [ ] Resource usage within targets

### Phase 7: Documentation & Deployment (Week 12)
- [ ] Docker Compose file created
- [ ] Dockerfile created and tested
- [ ] Kubernetes manifests created
- [ ] Grafana dashboard configured
- [ ] Runbook written
- [ ] Staging deployment successful
- [ ] Production deployment successful
- [ ] Team trained on operations

---

## 🎉 Success Criteria

Your Option 2 implementation is successful when:

1. ✅ **Performance**: 100 files processed in < 2 hours
2. ✅ **Code Preservation**: Checksums of extract.go, convert.go, store.go match original
3. ✅ **Reliability**: Success rate > 98% over 1000-file test
4. ✅ **Resource Efficiency**: RAM < 20%, CPU < 50%
5. ✅ **Zero Data Loss**: Crash recovery tests pass with 100% data integrity
6. ✅ **Monitoring**: Grafana dashboards show all metrics
7. ✅ **Operations**: Team can troubleshoot using runbook

---

**Last Updated**: 2025-11-13
**Status**: Ready for Implementation
**Estimated Completion**: 12 weeks from start

For questions or issues, refer to the comprehensive documentation in `../docs/` or use the Claude Code prompts in `option2-implementation-plan-part2.md` Section 9.
