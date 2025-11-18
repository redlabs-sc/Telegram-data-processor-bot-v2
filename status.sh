#!/bin/bash
#
# Telegram Bot Coordinator - Status Script
#
# Shows current system status, metrics, and health information
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${SCRIPT_DIR}"
PID_FILE="${PROJECT_ROOT}/coordinator.pid"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

# Load environment
if [[ -f "${PROJECT_ROOT}/.env" ]]; then
    set -a
    source "${PROJECT_ROOT}/.env"
    set +a
fi

DB_HOST="${DB_HOST:-localhost}"
DB_PORT="${DB_PORT:-5432}"
DB_NAME="${DB_NAME:-telegram_bot_option2}"
DB_USER="${DB_USER:-bot_user}"
HEALTH_PORT="${HEALTH_CHECK_PORT:-8080}"
METRICS_PORT="${METRICS_PORT:-9090}"

#=============================================================================
# Status Functions
#=============================================================================

print_header() {
    local title="$1"
    echo ""
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${CYAN}  ${title}${NC}"
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
}

status_ok() {
    echo -e "${GREEN}✓${NC} $*"
}

status_warn() {
    echo -e "${YELLOW}⚠${NC} $*"
}

status_error() {
    echo -e "${RED}✗${NC} $*"
}

status_info() {
    echo -e "${BLUE}ℹ${NC} $*"
}

check_process_status() {
    print_header "Process Status"

    # Check PID file
    if [[ -f "${PID_FILE}" ]]; then
        local pid=$(cat "${PID_FILE}")

        if ps -p "${pid}" > /dev/null 2>&1; then
            local uptime=$(ps -p "${pid}" -o etime= | tr -d ' ')
            local cpu=$(ps -p "${pid}" -o %cpu= | tr -d ' ')
            local mem=$(ps -p "${pid}" -o %mem= | tr -d ' ')

            status_ok "Coordinator is running"
            echo "    PID: ${pid}"
            echo "    Uptime: ${uptime}"
            echo "    CPU: ${cpu}%"
            echo "    Memory: ${mem}%"
        else
            status_error "Coordinator PID file exists but process not running (PID: ${pid})"
            status_warn "Stale PID file detected"
        fi
    else
        status_warn "No PID file found"
    fi

    # Check for any running coordinator processes
    local pids=$(pgrep -f "telegram-bot-coordinator" || true)

    if [[ -n "${pids}" ]]; then
        echo ""
        status_info "All coordinator processes:"
        ps aux | grep "telegram-bot-coordinator" | grep -v grep | awk '{printf "    PID: %-6s CPU: %-5s MEM: %-5s CMD: %s\n", $2, $3"%", $4"%", $11}'
    else
        if [[ ! -f "${PID_FILE}" ]]; then
            status_error "No coordinator processes running"
        fi
    fi
}

check_ports() {
    print_header "Network Ports"

    # Health check port
    if lsof -Pi :${HEALTH_PORT} -sTCP:LISTEN -t >/dev/null 2>&1; then
        status_ok "Health check port ${HEALTH_PORT} is listening"
    else
        status_error "Health check port ${HEALTH_PORT} is not listening"
    fi

    # Metrics port
    if lsof -Pi :${METRICS_PORT} -sTCP:LISTEN -t >/dev/null 2>&1; then
        status_ok "Metrics port ${METRICS_PORT} is listening"
    else
        status_error "Metrics port ${METRICS_PORT} is not listening"
    fi
}

check_health_endpoint() {
    print_header "Health Status"

    if ! command -v curl &> /dev/null; then
        status_warn "curl not installed, skipping health check"
        return
    fi

    local health_url="http://localhost:${HEALTH_PORT}/health"

    if ! curl -s -f "${health_url}" > /dev/null 2>&1; then
        status_error "Health endpoint not responding"
        return
    fi

    local response=$(curl -s "${health_url}")

    if ! command -v jq &> /dev/null; then
        status_warn "jq not installed, showing raw response:"
        echo "${response}"
        return
    fi

    local status=$(echo "${response}" | jq -r '.status')
    local timestamp=$(echo "${response}" | jq -r '.timestamp')

    case "${status}" in
        healthy)
            status_ok "Status: ${status}"
            ;;
        degraded)
            status_warn "Status: ${status}"
            ;;
        *)
            status_error "Status: ${status}"
            ;;
    esac

    echo "    Last check: ${timestamp}"

    # Component status
    echo ""
    status_info "Component Status:"

    echo "${response}" | jq -r '.components | to_entries[] | "    \(.key): \(.value)"' 2>/dev/null || true
}

check_database() {
    print_header "Database Status"

    if ! command -v psql &> /dev/null; then
        status_warn "psql not installed, skipping database checks"
        return
    fi

    # Check connection
    if ! PGPASSWORD="${DB_PASSWORD}" psql -h "${DB_HOST}" -p "${DB_PORT}" -U "${DB_USER}" -d "${DB_NAME}" \
        -c "SELECT 1;" > /dev/null 2>&1; then
        status_error "Cannot connect to database"
        return
    fi

    status_ok "Database connection: OK"

    # Queue statistics
    echo ""
    status_info "Download Queue:"

    local queue_stats=$(PGPASSWORD="${DB_PASSWORD}" psql -h "${DB_HOST}" -p "${DB_PORT}" -U "${DB_USER}" -d "${DB_NAME}" \
        -t -c "SELECT status, COUNT(*) FROM download_queue GROUP BY status ORDER BY status;" 2>/dev/null || echo "")

    if [[ -n "${queue_stats}" ]]; then
        echo "${queue_stats}" | while read -r line; do
            if [[ -n "${line}" ]]; then
                echo "    ${line}"
            fi
        done
    else
        echo "    No files in queue"
    fi

    # Round statistics
    echo ""
    status_info "Processing Rounds:"

    local round_stats=$(PGPASSWORD="${DB_PASSWORD}" psql -h "${DB_HOST}" -p "${DB_PORT}" -U "${DB_USER}" -d "${DB_NAME}" \
        -t -c "SELECT round_status, COUNT(*) FROM processing_rounds GROUP BY round_status ORDER BY round_status;" 2>/dev/null || echo "")

    if [[ -n "${round_stats}" ]]; then
        echo "${round_stats}" | while read -r line; do
            if [[ -n "${line}" ]]; then
                echo "    ${line}"
            fi
        done
    else
        echo "    No rounds found"
    fi

    # Active store tasks
    echo ""
    status_info "Store Tasks:"

    local task_stats=$(PGPASSWORD="${DB_PASSWORD}" psql -h "${DB_HOST}" -p "${DB_PORT}" -U "${DB_USER}" -d "${DB_NAME}" \
        -t -c "SELECT status, COUNT(*) FROM store_tasks GROUP BY status ORDER BY status;" 2>/dev/null || echo "")

    if [[ -n "${task_stats}" ]]; then
        echo "${task_stats}" | while read -r line; do
            if [[ -n "${line}" ]]; then
                echo "    ${line}"
            fi
        done
    else
        echo "    No tasks found"
    fi
}

check_metrics() {
    print_header "Prometheus Metrics (Sample)"

    if ! command -v curl &> /dev/null; then
        status_warn "curl not installed, skipping metrics"
        return
    fi

    local metrics_url="http://localhost:${METRICS_PORT}/metrics"

    if ! curl -s -f "${metrics_url}" > /dev/null 2>&1; then
        status_error "Metrics endpoint not responding"
        return
    fi

    status_ok "Metrics endpoint: OK"
    echo ""

    # Show key metrics
    local metrics=$(curl -s "${metrics_url}" | grep '^telegram_bot_' | grep -v '^#')

    if [[ -n "${metrics}" ]]; then
        status_info "Active Rounds:"
        echo "${metrics}" | grep 'telegram_bot_rounds_active' | sed 's/^/    /'

        echo ""
        status_info "Store Tasks:"
        echo "${metrics}" | grep 'telegram_bot_store_tasks_active' | sed 's/^/    /'

        echo ""
        status_info "Queue Size:"
        echo "${metrics}" | grep 'telegram_bot_queue_size' | sed 's/^/    /'
    fi
}

check_disk_space() {
    print_header "Disk Space"

    local dirs=("downloads" "store_tasks" "logs" "app/extraction/files")

    for dir in "${dirs[@]}"; do
        local full_path="${PROJECT_ROOT}/${dir}"

        if [[ -d "${full_path}" ]]; then
            local size=$(du -sh "${full_path}" 2>/dev/null | cut -f1)
            local count=$(find "${full_path}" -type f 2>/dev/null | wc -l)

            echo "  ${dir}:"
            echo "    Size: ${size}"
            echo "    Files: ${count}"
        fi
    done

    echo ""
    status_info "Available disk space:"
    df -h "${PROJECT_ROOT}" | tail -1 | awk '{printf "    Total: %s, Used: %s, Available: %s (%s)\n", $2, $3, $4, $5}'
}

check_logs() {
    print_header "Recent Logs (Last 10 lines)"

    local log_file="${PROJECT_ROOT}/logs/coordinator.log"

    if [[ ! -f "${log_file}" ]]; then
        status_warn "Log file not found: ${log_file}"
        return
    fi

    tail -10 "${log_file}" | sed 's/^/    /'

    echo ""
    status_info "Full logs: tail -f ${log_file}"
}

show_quick_actions() {
    print_header "Quick Actions"

    echo "  View logs:        tail -f ${PROJECT_ROOT}/logs/coordinator.log"
    echo "  Stop service:     ${PROJECT_ROOT}/stop.sh"
    echo "  Restart service:  ${PROJECT_ROOT}/setup.sh"
    echo "  Health check:     curl http://localhost:${HEALTH_PORT}/health | jq ."
    echo "  Metrics:          curl http://localhost:${METRICS_PORT}/metrics"
    echo ""
    echo "  Monitor queue:    watch -n 2 'psql -U ${DB_USER} -d ${DB_NAME} -c \"SELECT status, COUNT(*) FROM download_queue GROUP BY status;\"'"
    echo "  Monitor rounds:   watch -n 5 'psql -U ${DB_USER} -d ${DB_NAME} -c \"SELECT round_status, COUNT(*) FROM processing_rounds GROUP BY round_status;\"'"
}

#=============================================================================
# Main
#=============================================================================

main() {
    clear

    echo -e "${CYAN}"
    echo "╔════════════════════════════════════════════════════════════╗"
    echo "║      Telegram Bot Coordinator - System Status             ║"
    echo "╚════════════════════════════════════════════════════════════╝"
    echo -e "${NC}"

    check_process_status
    check_ports
    check_health_endpoint
    check_database
    check_metrics
    check_disk_space
    check_logs
    show_quick_actions

    echo ""
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
}

main "$@"
