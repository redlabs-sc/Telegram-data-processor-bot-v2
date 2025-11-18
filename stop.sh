#!/bin/bash
#
# Telegram Bot Coordinator - Stop Script
#
# Cleanly stops the coordinator service with proper signal handling
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${SCRIPT_DIR}"
PID_FILE="${PROJECT_ROOT}/coordinator.pid"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() {
    echo -e "${YELLOW}ℹ${NC} $*"
}

log_success() {
    echo -e "${GREEN}✓${NC} $*"
}

log_error() {
    echo -e "${RED}✗${NC} $*" >&2
}

stop_coordinator() {
    log_info "Stopping coordinator..."

    # Check PID file
    if [[ -f "${PID_FILE}" ]]; then
        local pid=$(cat "${PID_FILE}")

        if ps -p "${pid}" > /dev/null 2>&1; then
            log_info "  Sending SIGTERM to PID ${pid}..."
            kill -TERM "${pid}" 2>/dev/null || true

            # Wait for graceful shutdown (max 30 seconds)
            local count=0
            while ps -p "${pid}" > /dev/null 2>&1 && [[ ${count} -lt 30 ]]; do
                sleep 1
                ((count++))
            done

            # Force kill if still running
            if ps -p "${pid}" > /dev/null 2>&1; then
                log_info "  Process still running, sending SIGKILL..."
                kill -KILL "${pid}" 2>/dev/null || true
                sleep 1
            fi

            rm -f "${PID_FILE}"
            log_success "Coordinator stopped (PID: ${pid})"
        else
            log_info "  Process ${pid} not running (stale PID file)"
            rm -f "${PID_FILE}"
        fi
    fi

    # Find any remaining processes
    local pids=$(pgrep -f "telegram-bot-coordinator" || true)

    if [[ -n "${pids}" ]]; then
        log_info "  Found additional processes: ${pids}"
        echo "${pids}" | xargs kill -TERM 2>/dev/null || true
        sleep 2

        # Force kill if still running
        pids=$(pgrep -f "telegram-bot-coordinator" || true)
        if [[ -n "${pids}" ]]; then
            log_info "  Force killing remaining processes..."
            echo "${pids}" | xargs kill -KILL 2>/dev/null || true
        fi
    fi

    log_success "All coordinator processes stopped"
}

verify_stopped() {
    log_info "Verifying services stopped..."

    local pids=$(pgrep -f "telegram-bot-coordinator" || true)

    if [[ -z "${pids}" ]]; then
        log_success "No coordinator processes running"

        # Check if ports are still in use
        local health_port=8080
        local metrics_port=9090

        if lsof -Pi :${health_port} -sTCP:LISTEN -t >/dev/null 2>&1; then
            log_error "Port ${health_port} still in use"
            return 1
        fi

        if lsof -Pi :${metrics_port} -sTCP:LISTEN -t >/dev/null 2>&1; then
            log_error "Port ${metrics_port} still in use"
            return 1
        fi

        log_success "All ports released"
    else
        log_error "Processes still running: ${pids}"
        return 1
    fi
}

main() {
    echo "========================================"
    echo "Stopping Telegram Bot Coordinator"
    echo "========================================"
    echo ""

    stop_coordinator
    verify_stopped

    echo ""
    echo "Coordinator stopped successfully."
    echo ""
    echo "To restart: ./setup.sh"
    echo ""
}

main "$@"
