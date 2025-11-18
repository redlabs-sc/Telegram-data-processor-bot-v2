#!/bin/bash
#
# Telegram Bot Coordinator - Setup, Build, and Start Script
#
# This script performs a complete setup of the pipelined architecture system:
# - Validates environment and prerequisites
# - Sets up PostgreSQL database and runs migrations
# - Creates required directories
# - Builds the coordinator
# - Starts all services with health monitoring
#
# Usage: ./setup.sh [--skip-db] [--skip-build] [--dev]
#

set -euo pipefail  # Exit on error, undefined variables, and pipe failures

#=============================================================================
# Configuration
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${SCRIPT_DIR}"
LOG_FILE="${PROJECT_ROOT}/logs/setup.log"
COORDINATOR_BIN="${PROJECT_ROOT}/coordinator/telegram-bot-coordinator"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Flags
SKIP_DB=false
SKIP_BUILD=false
DEV_MODE=false
AUTO_YES=false

#=============================================================================
# Utility Functions
#=============================================================================

log() {
    local level="$1"
    shift
    local message="$*"
    local timestamp=$(date '+%Y-%m-%d %H:%M:%S')
    echo "[${timestamp}] [${level}] ${message}" | tee -a "${LOG_FILE}"
}

log_info() {
    echo -e "${BLUE}ℹ${NC} $*"
    log "INFO" "$*"
}

log_success() {
    echo -e "${GREEN}✓${NC} $*"
    log "SUCCESS" "$*"
}

log_warning() {
    echo -e "${YELLOW}⚠${NC} $*"
    log "WARNING" "$*"
}

log_error() {
    echo -e "${RED}✗${NC} $*" >&2
    log "ERROR" "$*"
}

die() {
    log_error "$*"
    log_error "Setup failed. Check ${LOG_FILE} for details."
    exit 1
}

confirm() {
    local prompt="$1"

    # Auto-yes mode bypasses confirmation
    if ${AUTO_YES}; then
        log_info "${prompt} [AUTO-YES]"
        return 0
    fi

    read -r -p "${prompt} [y/N] " response
    case "$response" in
        [yY][eE][sS]|[yY])
            return 0
            ;;
        *)
            return 1
            ;;
    esac
}

detect_os() {
    if [[ -f /etc/os-release ]]; then
        . /etc/os-release
        echo "${ID}"
    elif [[ -f /etc/redhat-release ]]; then
        echo "rhel"
    elif [[ "$(uname)" == "Darwin" ]]; then
        echo "macos"
    else
        echo "unknown"
    fi
}

install_package() {
    local package="$1"
    local os=$(detect_os)

    log_info "Attempting to install ${package}..."

    case "${os}" in
        ubuntu|debian)
            sudo apt-get update >> "${LOG_FILE}" 2>&1
            sudo apt-get install -y "${package}" >> "${LOG_FILE}" 2>&1
            ;;
        fedora|rhel|centos)
            sudo dnf install -y "${package}" >> "${LOG_FILE}" 2>&1 || \
            sudo yum install -y "${package}" >> "${LOG_FILE}" 2>&1
            ;;
        arch|manjaro)
            sudo pacman -Sy --noconfirm "${package}" >> "${LOG_FILE}" 2>&1
            ;;
        macos)
            if command -v brew &> /dev/null; then
                brew install "${package}" >> "${LOG_FILE}" 2>&1
            else
                die "Homebrew not found. Please install ${package} manually."
            fi
            ;;
        *)
            die "Unknown OS. Please install ${package} manually."
            ;;
    esac

    if [[ $? -eq 0 ]]; then
        log_success "${package} installed successfully"
        return 0
    else
        log_error "Failed to install ${package}"
        return 1
    fi
}

check_command() {
    local cmd="$1"
    local package="${2:-$1}"
    local auto_install="${3:-true}"

    if ! command -v "${cmd}" &> /dev/null; then
        log_warning "${cmd} not found"

        if [[ "${auto_install}" == "true" ]]; then
            if confirm "Would you like to install ${package}?"; then
                install_package "${package}" || die "Failed to install ${package}. Please install manually."

                # Verify installation
                if ! command -v "${cmd}" &> /dev/null; then
                    die "${cmd} installation failed. Please install ${package} manually."
                fi
            else
                die "${cmd} is required. Please install ${package} and try again."
            fi
        else
            die "${cmd} not found. Please install ${package} first."
        fi
    fi
}

#=============================================================================
# Parse Arguments
#=============================================================================

parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --skip-db)
                SKIP_DB=true
                log_warning "Skipping database setup"
                shift
                ;;
            --skip-build)
                SKIP_BUILD=true
                log_warning "Skipping build step"
                shift
                ;;
            --dev)
                DEV_MODE=true
                log_info "Running in development mode"
                shift
                ;;
            -y|--yes)
                AUTO_YES=true
                log_info "Auto-yes mode enabled (no confirmations)"
                shift
                ;;
            -h|--help)
                cat << EOF
Usage: $0 [OPTIONS]

Options:
  --skip-db      Skip database setup and migrations
  --skip-build   Skip building the coordinator
  --dev          Run in development mode (foreground, verbose)
  -y, --yes      Auto-yes mode (no confirmations, auto-install)
  -h, --help     Show this help message

Examples:
  $0                    # Interactive setup
  $0 --yes              # Fully autonomous setup
  $0 --skip-db --dev    # Skip DB setup and run in foreground

EOF
                exit 0
                ;;
            *)
                die "Unknown option: $1. Use --help for usage information."
                ;;
        esac
    done
}

#=============================================================================
# Prerequisites Check
#=============================================================================

start_postgresql() {
    local os=$(detect_os)

    log_info "Attempting to start PostgreSQL..."

    case "${os}" in
        ubuntu|debian)
            sudo systemctl start postgresql >> "${LOG_FILE}" 2>&1 || \
            sudo service postgresql start >> "${LOG_FILE}" 2>&1
            ;;
        fedora|rhel|centos)
            sudo systemctl start postgresql >> "${LOG_FILE}" 2>&1
            ;;
        arch|manjaro)
            sudo systemctl start postgresql >> "${LOG_FILE}" 2>&1
            ;;
        macos)
            if command -v brew &> /dev/null; then
                brew services start postgresql >> "${LOG_FILE}" 2>&1 || \
                pg_ctl -D /usr/local/var/postgres start >> "${LOG_FILE}" 2>&1
            else
                pg_ctl -D /usr/local/var/postgres start >> "${LOG_FILE}" 2>&1
            fi
            ;;
        *)
            log_warning "Unknown OS. Trying systemctl..."
            sudo systemctl start postgresql >> "${LOG_FILE}" 2>&1
            ;;
    esac

    # Wait for PostgreSQL to be ready
    local max_wait=30
    local waited=0
    while ! pg_isready -h localhost > /dev/null 2>&1; do
        if [[ ${waited} -ge ${max_wait} ]]; then
            return 1
        fi
        sleep 1
        ((waited++))
    done

    return 0
}

check_postgresql() {
    # First check if PostgreSQL client tools are installed
    if ! command -v psql &> /dev/null; then
        log_warning "PostgreSQL client (psql) not found"
        if confirm "Would you like to install PostgreSQL?"; then
            local os=$(detect_os)
            case "${os}" in
                ubuntu|debian)
                    install_package "postgresql postgresql-contrib"
                    ;;
                fedora|rhel|centos)
                    install_package "postgresql-server postgresql-contrib"
                    # Initialize database for RHEL-based systems
                    sudo postgresql-setup --initdb >> "${LOG_FILE}" 2>&1 || true
                    ;;
                arch|manjaro)
                    install_package "postgresql"
                    # Initialize database for Arch-based systems
                    sudo -u postgres initdb -D /var/lib/postgres/data >> "${LOG_FILE}" 2>&1 || true
                    ;;
                macos)
                    install_package "postgresql"
                    ;;
                *)
                    die "Unable to install PostgreSQL automatically. Please install manually."
                    ;;
            esac
        else
            die "PostgreSQL is required. Please install it and try again."
        fi
    fi

    # Check if createdb is available
    if ! command -v createdb &> /dev/null; then
        log_warning "createdb command not found (part of postgresql)"
        local os=$(detect_os)
        case "${os}" in
            ubuntu|debian)
                install_package "postgresql-client-common"
                ;;
            *)
                log_warning "createdb should be part of postgresql package"
                ;;
        esac
    fi

    # Check if pg_isready is available
    if ! command -v pg_isready &> /dev/null; then
        log_warning "pg_isready not found, using alternative check"
    fi

    # Check if PostgreSQL is running
    if command -v pg_isready &> /dev/null; then
        if ! pg_isready -h localhost > /dev/null 2>&1; then
            log_warning "PostgreSQL is not running"

            if confirm "Would you like to start PostgreSQL?"; then
                if start_postgresql; then
                    log_success "PostgreSQL started successfully"
                else
                    die "Failed to start PostgreSQL. Please start it manually with: sudo systemctl start postgresql"
                fi
            else
                die "PostgreSQL must be running. Please start it with: sudo systemctl start postgresql"
            fi
        else
            log_success "PostgreSQL is running"
        fi
    else
        # Fallback check using psql
        if ! psql -h localhost -U postgres -c "SELECT 1" > /dev/null 2>&1 && \
           ! psql -h localhost -c "SELECT 1" > /dev/null 2>&1; then
            log_warning "Cannot connect to PostgreSQL"

            if confirm "Would you like to start PostgreSQL?"; then
                if start_postgresql; then
                    log_success "PostgreSQL started successfully"
                else
                    die "Failed to start PostgreSQL. Please start it manually."
                fi
            else
                die "PostgreSQL must be running."
            fi
        else
            log_success "PostgreSQL is accessible"
        fi
    fi
}

check_prerequisites() {
    log_info "Checking prerequisites..."

    # Check Go (don't auto-install - too complex)
    if ! command -v go &> /dev/null; then
        die "Go is not installed. Please install Go 1.24+ from https://golang.org/dl/"
    fi

    # Check Go version
    local go_version=$(go version | grep -oP '(?<=go)\d+\.\d+' || echo "0.0")
    local required_version="1.24"

    if ! awk -v ver="${go_version}" -v req="${required_version}" 'BEGIN { exit (ver < req) }'; then
        die "Go version ${go_version} is too old. Required: >= ${required_version}"
    fi

    log_success "Go ${go_version} found"

    # Check PostgreSQL (with auto-install and auto-start)
    check_postgresql

    # Check optional tools (with auto-install)
    check_command "jq" "jq" "true"
    check_command "curl" "curl" "true"
    check_command "lsof" "lsof" "true"
}

#=============================================================================
# Environment Configuration
#=============================================================================

setup_environment() {
    log_info "Setting up environment..."

    # Create logs directory
    mkdir -p "${PROJECT_ROOT}/logs"

    # Check if .env exists
    if [[ ! -f "${PROJECT_ROOT}/.env" ]]; then
        if [[ -f "${PROJECT_ROOT}/.env.example" ]]; then
            log_warning ".env file not found. Creating from .env.example..."
            cp "${PROJECT_ROOT}/.env.example" "${PROJECT_ROOT}/.env"
            log_warning "IMPORTANT: Edit .env file with your credentials before continuing!"

            if ! ${DEV_MODE}; then
                confirm "Have you configured .env with your credentials?" || die "Please configure .env first"
            else
                log_warning "DEV MODE: Skipping .env configuration prompt"
            fi
        else
            die ".env.example not found. Cannot create .env file."
        fi
    fi

    # Load environment variables
    set -a
    source "${PROJECT_ROOT}/.env"
    set +a

    # Validate critical env vars
    [[ -z "${TELEGRAM_BOT_TOKEN:-}" ]] && die "TELEGRAM_BOT_TOKEN not set in .env"
    [[ -z "${ADMIN_IDS:-}" ]] && die "ADMIN_IDS not set in .env"
    [[ -z "${DB_PASSWORD:-}" ]] && die "DB_PASSWORD not set in .env"

    log_success "Environment configured"
}

#=============================================================================
# Verify Preserved Files
#=============================================================================

verify_preserved_files() {
    log_info "Verifying preserved files integrity..."

    if [[ ! -f "${PROJECT_ROOT}/checksums.txt" ]]; then
        log_warning "checksums.txt not found. Skipping integrity check."
        return 0
    fi

    cd "${PROJECT_ROOT}"
    if sha256sum -c checksums.txt >> "${LOG_FILE}" 2>&1; then
        log_success "Preserved files integrity verified"
    else
        log_error "Checksum verification failed!"
        log_error "Preserved files may have been modified."

        if [[ -f "${PROJECT_ROOT}/backup.zip" ]]; then
            if confirm "Restore from backup.zip?"; then
                log_info "Restoring from backup..."
                unzip -o backup.zip -d app/extraction/ >> "${LOG_FILE}" 2>&1
                log_success "Files restored from backup"
            else
                die "Cannot proceed with modified preserved files"
            fi
        else
            die "backup.zip not found. Cannot restore preserved files."
        fi
    fi
}

#=============================================================================
# Database Setup
#=============================================================================

setup_database() {
    if ${SKIP_DB}; then
        log_info "Skipping database setup (--skip-db flag)"
        return 0
    fi

    log_info "Setting up database..."

    local db_name="${DB_NAME:-telegram_bot_option2}"
    local db_user="${DB_USER:-bot_user}"
    local db_password="${DB_PASSWORD}"
    local db_host="${DB_HOST:-localhost}"
    local db_port="${DB_PORT:-5432}"

    # Test if we can connect with existing credentials first
    if PGPASSWORD="${db_password}" psql -h "${db_host}" -p "${db_port}" -U "${db_user}" -d "${db_name}" -c "SELECT 1" >> "${LOG_FILE}" 2>&1; then
        log_success "Database ${db_name} already exists and accessible"

        # Run migrations anyway (in case they weren't applied)
        log_info "Running database migrations..."
        local migration_dir="${PROJECT_ROOT}/database/migrations"

        if [[ ! -d "${migration_dir}" ]]; then
            die "Migrations directory not found: ${migration_dir}"
        fi

        for migration in "${migration_dir}"/*.sql; do
            if [[ -f "${migration}" ]]; then
                local migration_name=$(basename "${migration}")
                log_info "  Applying ${migration_name}..."

                PGPASSWORD="${db_password}" psql -h "${db_host}" -p "${db_port}" -U "${db_user}" -d "${db_name}" \
                    -f "${migration}" >> "${LOG_FILE}" 2>&1 || \
                    log_warning "    Migration ${migration_name} may have already been applied (this is OK)"
            fi
        done

        log_success "Database migrations completed"

        # Verify tables
        verify_database_tables "${db_host}" "${db_port}" "${db_user}" "${db_password}" "${db_name}"
        return 0
    fi

    # Database doesn't exist or isn't accessible - need to create it
    log_info "Database not accessible. Setting up database and user..."

    # Try to use sudo -u postgres for peer authentication (common on Linux)
    local use_sudo=false
    if sudo -u postgres psql -c "SELECT 1" >> "${LOG_FILE}" 2>&1; then
        use_sudo=true
        log_info "Using sudo for PostgreSQL administration (peer authentication)"
    else
        log_warning "Cannot use sudo for PostgreSQL. Will try direct connection."
        log_warning "You may be prompted for the postgres user password."
    fi

    # Check if database exists
    local db_exists=false
    if ${use_sudo}; then
        if sudo -u postgres psql -lqt | cut -d \| -f 1 | grep -qw "${db_name}"; then
            db_exists=true
        fi
    else
        if psql -h "${db_host}" -p "${db_port}" -U postgres -lqt 2>> "${LOG_FILE}" | cut -d \| -f 1 | grep -qw "${db_name}"; then
            db_exists=true
        fi
    fi

    if ${db_exists}; then
        log_success "Database ${db_name} already exists"
    else
        log_info "Creating database ${db_name}..."
        if ${use_sudo}; then
            sudo -u postgres createdb "${db_name}" >> "${LOG_FILE}" 2>&1 || \
                die "Failed to create database. Check ${LOG_FILE} for details."
        else
            createdb -h "${db_host}" -p "${db_port}" -U postgres "${db_name}" >> "${LOG_FILE}" 2>&1 || \
                die "Failed to create database. You may need to configure PostgreSQL authentication or run: sudo -u postgres createdb ${db_name}"
        fi
        log_success "Database created"
    fi

    # Check if user exists
    local user_exists=false
    if ${use_sudo}; then
        if sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='${db_user}'" 2>> "${LOG_FILE}" | grep -q 1; then
            user_exists=true
        fi
    else
        if psql -h "${db_host}" -p "${db_port}" -U postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname='${db_user}'" 2>> "${LOG_FILE}" | grep -q 1; then
            user_exists=true
        fi
    fi

    if ${user_exists}; then
        log_success "User ${db_user} already exists"

        # Update password in case it changed
        log_info "Updating user password..."
        if ${use_sudo}; then
            sudo -u postgres psql -c "ALTER USER ${db_user} WITH PASSWORD '${db_password}';" >> "${LOG_FILE}" 2>&1
        else
            psql -h "${db_host}" -p "${db_port}" -U postgres -c "ALTER USER ${db_user} WITH PASSWORD '${db_password}';" >> "${LOG_FILE}" 2>&1
        fi
    else
        log_info "Creating user ${db_user}..."
        if ${use_sudo}; then
            sudo -u postgres psql -c "CREATE USER ${db_user} WITH PASSWORD '${db_password}';" >> "${LOG_FILE}" 2>&1 || \
                die "Failed to create user. Check ${LOG_FILE} for details."
        else
            psql -h "${db_host}" -p "${db_port}" -U postgres -c "CREATE USER ${db_user} WITH PASSWORD '${db_password}';" >> "${LOG_FILE}" 2>&1 || \
                die "Failed to create user. You may need to run: sudo -u postgres psql -c \"CREATE USER ${db_user} WITH PASSWORD '${db_password}';\""
        fi
        log_success "User created"
    fi

    # Grant privileges
    log_info "Granting privileges..."
    if ${use_sudo}; then
        sudo -u postgres psql -c "GRANT ALL PRIVILEGES ON DATABASE ${db_name} TO ${db_user};" >> "${LOG_FILE}" 2>&1
        sudo -u postgres psql -d "${db_name}" -c "GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO ${db_user};" >> "${LOG_FILE}" 2>&1
        sudo -u postgres psql -d "${db_name}" -c "GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO ${db_user};" >> "${LOG_FILE}" 2>&1
        sudo -u postgres psql -d "${db_name}" -c "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO ${db_user};" >> "${LOG_FILE}" 2>&1
        sudo -u postgres psql -d "${db_name}" -c "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO ${db_user};" >> "${LOG_FILE}" 2>&1
    else
        psql -h "${db_host}" -p "${db_port}" -U postgres -c "GRANT ALL PRIVILEGES ON DATABASE ${db_name} TO ${db_user};" >> "${LOG_FILE}" 2>&1
        psql -h "${db_host}" -p "${db_port}" -U postgres -d "${db_name}" -c "GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO ${db_user};" >> "${LOG_FILE}" 2>&1
        psql -h "${db_host}" -p "${db_port}" -U postgres -d "${db_name}" -c "GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO ${db_user};" >> "${LOG_FILE}" 2>&1
        psql -h "${db_host}" -p "${db_port}" -U postgres -d "${db_name}" -c "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO ${db_user};" >> "${LOG_FILE}" 2>&1
        psql -h "${db_host}" -p "${db_port}" -U postgres -d "${db_name}" -c "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO ${db_user};" >> "${LOG_FILE}" 2>&1
    fi
    log_success "Privileges granted"

    # Test connection with new user
    log_info "Testing database connection..."
    if ! PGPASSWORD="${db_password}" psql -h "${db_host}" -p "${db_port}" -U "${db_user}" -d "${db_name}" -c "SELECT 1" >> "${LOG_FILE}" 2>&1; then
        die "Cannot connect to database with user ${db_user}. Check credentials in .env"
    fi
    log_success "Database connection successful"

    # Run migrations
    log_info "Running database migrations..."
    local migration_dir="${PROJECT_ROOT}/database/migrations"

    if [[ ! -d "${migration_dir}" ]]; then
        die "Migrations directory not found: ${migration_dir}"
    fi

    for migration in "${migration_dir}"/*.sql; do
        if [[ -f "${migration}" ]]; then
            local migration_name=$(basename "${migration}")
            log_info "  Applying ${migration_name}..."

            PGPASSWORD="${db_password}" psql -h "${db_host}" -p "${db_port}" -U "${db_user}" -d "${db_name}" \
                -f "${migration}" >> "${LOG_FILE}" 2>&1 || \
                log_warning "    Migration ${migration_name} may have already been applied (this is OK)"
        fi
    done

    log_success "Database migrations completed"

    # Verify tables
    verify_database_tables "${db_host}" "${db_port}" "${db_user}" "${db_password}" "${db_name}"
}

verify_database_tables() {
    local db_host="$1"
    local db_port="$2"
    local db_user="$3"
    local db_password="$4"
    local db_name="$5"

    log_info "Verifying database schema..."
    local tables=$(PGPASSWORD="${db_password}" psql -h "${db_host}" -p "${db_port}" -U "${db_user}" -d "${db_name}" \
        -t -c "SELECT tablename FROM pg_tables WHERE schemaname='public';" 2>> "${LOG_FILE}")

    local required_tables=("download_queue" "processing_rounds" "store_tasks" "processing_metrics")
    for table in "${required_tables[@]}"; do
        if echo "${tables}" | grep -qw "${table}"; then
            log_success "  Table ${table} exists"
        else
            die "Required table ${table} not found"
        fi
    done
}

#=============================================================================
# Directory Setup
#=============================================================================

setup_directories() {
    log_info "Creating required directories..."

    local dirs=(
        "downloads"
        "logs"
        "store_tasks"
        "archive/failed"
        "app/extraction/files/all"
        "app/extraction/files/pass"
        "app/extraction/files/txt"
        "app/extraction/files/nonsorted"
        "app/extraction/files/done"
        "app/extraction/files/errors"
        "app/extraction/files/etbanks"
        "app/extraction/files/nopass"
    )

    for dir in "${dirs[@]}"; do
        mkdir -p "${PROJECT_ROOT}/${dir}"
        log_success "  Created ${dir}"
    done

    # Copy password file if it doesn't exist
    if [[ ! -f "${PROJECT_ROOT}/app/extraction/pass.txt" ]]; then
        if [[ -f "${PROJECT_ROOT}/pass.txt.example" ]]; then
            cp "${PROJECT_ROOT}/pass.txt.example" "${PROJECT_ROOT}/app/extraction/pass.txt"
            log_warning "Created app/extraction/pass.txt from example. Edit if needed."
        else
            touch "${PROJECT_ROOT}/app/extraction/pass.txt"
            log_warning "Created empty app/extraction/pass.txt. Add passwords if needed."
        fi
    fi
}

#=============================================================================
# Build
#=============================================================================

build_coordinator() {
    if ${SKIP_BUILD}; then
        log_info "Skipping build (--skip-build flag)"
        return 0
    fi

    log_info "Building coordinator..."

    # Ensure we're in project root
    cd "${PROJECT_ROOT}"

    # Ensure logs directory exists
    mkdir -p "${PROJECT_ROOT}/logs"

    # Clean dependencies
    log_info "  Cleaning Go module cache..."
    go mod tidy >> "${LOG_FILE}" 2>&1 || die "go mod tidy failed"

    # Build
    log_info "  Compiling coordinator binary..."

    # Build from project root to avoid path issues
    cd "${PROJECT_ROOT}"

    if ${DEV_MODE}; then
        # Development build (faster, with debug info)
        (cd coordinator && go build -o telegram-bot-coordinator) >> "${LOG_FILE}" 2>&1 || die "Build failed"
    else
        # Production build (optimized)
        (cd coordinator && go build -ldflags="-s -w" -o telegram-bot-coordinator) >> "${LOG_FILE}" 2>&1 || die "Build failed"
    fi

    # Verify binary
    if [[ ! -f "${COORDINATOR_BIN}" ]]; then
        die "Binary not found after build: ${COORDINATOR_BIN}"
    fi

    chmod +x "${COORDINATOR_BIN}"
    local binary_size=$(du -h "${COORDINATOR_BIN}" | cut -f1)
    log_success "Coordinator built successfully (${binary_size})"
}

#=============================================================================
# Service Management
#=============================================================================

stop_existing_services() {
    log_info "Stopping existing coordinator processes..."

    # Find and kill existing processes
    local pids=$(pgrep -f "telegram-bot-coordinator" || true)

    if [[ -n "${pids}" ]]; then
        log_info "  Found existing processes: ${pids}"
        echo "${pids}" | xargs kill -TERM 2>/dev/null || true
        sleep 2

        # Force kill if still running
        pids=$(pgrep -f "telegram-bot-coordinator" || true)
        if [[ -n "${pids}" ]]; then
            log_warning "  Force killing stubborn processes..."
            echo "${pids}" | xargs kill -KILL 2>/dev/null || true
        fi

        log_success "Existing processes stopped"
    else
        log_success "No existing processes found"
    fi
}

start_coordinator() {
    log_info "Starting coordinator..."

    cd "${PROJECT_ROOT}/coordinator"

    if ${DEV_MODE}; then
        # Run in foreground for development
        log_info "Running in foreground (DEV MODE). Press Ctrl+C to stop."
        log_info "----------------------------------------"
        ./telegram-bot-coordinator
    else
        # Run in background
        nohup ./telegram-bot-coordinator > "../logs/coordinator.log" 2>&1 &
        local pid=$!

        # Save PID
        echo "${pid}" > "${PROJECT_ROOT}/coordinator.pid"

        log_success "Coordinator started (PID: ${pid})"
        log_info "Logs: tail -f ${PROJECT_ROOT}/logs/coordinator.log"
    fi
}

#=============================================================================
# Health Checks
#=============================================================================

wait_for_service() {
    local service_name="$1"
    local url="$2"
    local max_attempts="${3:-30}"
    local wait_time=2

    log_info "Waiting for ${service_name} to be ready..."

    for ((i=1; i<=max_attempts; i++)); do
        if curl -s -f "${url}" > /dev/null 2>&1; then
            log_success "${service_name} is ready"
            return 0
        fi

        if ((i % 5 == 0)); then
            log_info "  Still waiting for ${service_name}... (${i}/${max_attempts})"
        fi

        sleep "${wait_time}"
    done

    log_error "${service_name} did not become ready after ${max_attempts} attempts"
    return 1
}

verify_health() {
    local health_port="${HEALTH_CHECK_PORT:-8080}"
    local metrics_port="${METRICS_PORT:-9090}"

    log_info "Verifying service health..."

    # Wait for health endpoint
    if ! wait_for_service "Health Check" "http://localhost:${health_port}/health" 30; then
        log_error "Health check failed. Coordinator may not have started correctly."
        log_error "Check logs: tail -f ${PROJECT_ROOT}/logs/coordinator.log"
        return 1
    fi

    # Check health endpoint
    local health_response=$(curl -s "http://localhost:${health_port}/health")
    local health_status=$(echo "${health_response}" | jq -r '.status' 2>/dev/null || echo "unknown")

    if [[ "${health_status}" == "healthy" ]]; then
        log_success "Health check: ${health_status}"
    else
        log_warning "Health check: ${health_status}"
        log_warning "Response: ${health_response}"
    fi

    # Check metrics endpoint
    if curl -s -f "http://localhost:${metrics_port}/metrics" > /dev/null 2>&1; then
        log_success "Metrics endpoint: available"
    else
        log_warning "Metrics endpoint: not responding"
    fi

    # Show some initial metrics
    log_info "Initial queue status:"
    local db_name="${DB_NAME:-telegram_bot_option2}"
    local db_user="${DB_USER:-bot_user}"
    local db_password="${DB_PASSWORD}"
    local db_host="${DB_HOST:-localhost}"
    local db_port="${DB_PORT:-5432}"

    PGPASSWORD="${db_password}" psql -h "${db_host}" -p "${db_port}" -U "${db_user}" -d "${db_name}" \
        -c "SELECT status, COUNT(*) as count FROM download_queue GROUP BY status ORDER BY status;" \
        2>/dev/null || log_warning "Could not query database"
}

#=============================================================================
# Summary and Instructions
#=============================================================================

print_summary() {
    local health_port="${HEALTH_CHECK_PORT:-8080}"
    local metrics_port="${METRICS_PORT:-9090}"

    echo ""
    echo "========================================"
    echo "🎉 Setup Complete!"
    echo "========================================"
    echo ""
    echo "Service Status:"
    echo "  Coordinator: Running (PID: $(cat ${PROJECT_ROOT}/coordinator.pid 2>/dev/null || echo 'N/A'))"
    echo "  Health Check: http://localhost:${health_port}/health"
    echo "  Metrics: http://localhost:${metrics_port}/metrics"
    echo ""
    echo "Monitoring Commands:"
    echo "  Logs:    tail -f ${PROJECT_ROOT}/logs/coordinator.log"
    echo "  Health:  curl http://localhost:${health_port}/health | jq ."
    echo "  Metrics: curl http://localhost:${metrics_port}/metrics | grep telegram_bot"
    echo ""
    echo "Management Commands:"
    echo "  Stop:    pkill -f telegram-bot-coordinator"
    echo "  Restart: ${0}"
    echo "  Status:  ps aux | grep telegram-bot-coordinator"
    echo ""
    echo "Next Steps:"
    echo "  1. Send /start to your bot on Telegram"
    echo "  2. Upload test files (ZIP, RAR, or TXT)"
    echo "  3. Monitor processing with:"
    echo "     watch -n 2 'psql -U ${DB_USER:-bot_user} -d ${DB_NAME:-telegram_bot_option2} -c \"SELECT round_status, COUNT(*) FROM processing_rounds GROUP BY round_status;\"'"
    echo ""
    echo "Documentation:"
    echo "  Quick Start: ${PROJECT_ROOT}/QUICKSTART.md"
    echo "  Architecture: ${PROJECT_ROOT}/CLAUDE.md"
    echo "  Design: ${PROJECT_ROOT}/Docs/pipelined-architecture-design.md"
    echo ""
    echo "========================================"
}

#=============================================================================
# Cleanup on Exit
#=============================================================================

cleanup() {
    local exit_code=$?

    if [[ ${exit_code} -ne 0 ]]; then
        log_error "Setup failed with exit code ${exit_code}"
        log_error "Check ${LOG_FILE} for details"
    fi
}

trap cleanup EXIT

#=============================================================================
# Main Execution
#=============================================================================

main() {
    echo "========================================"
    echo "Telegram Bot Coordinator - Setup Script"
    echo "========================================"
    echo ""

    # Initialize log file
    mkdir -p "$(dirname "${LOG_FILE}")"
    echo "=== Setup started at $(date) ===" > "${LOG_FILE}"

    # Parse arguments
    parse_args "$@"

    # Execute setup steps
    check_prerequisites
    setup_environment
    verify_preserved_files
    setup_database
    setup_directories
    build_coordinator

    # Start services
    stop_existing_services

    if ${DEV_MODE}; then
        start_coordinator  # Will run in foreground
    else
        start_coordinator &
        sleep 3
        verify_health
        print_summary
    fi
}

# Run main function
main "$@"
