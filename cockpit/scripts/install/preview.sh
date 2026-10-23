#!/usr/bin/env bash
# Safe, offline preview of the real installer renderer. No installation,
# enrollment, services, permissions, or model setup is performed.
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

function show_help() {
    cat <<'HELP'
Usage: bash scripts/install/preview.sh [options]
  --scenario=installed|current|models-pending|failure
             uninstalled|disconnected|uninstall-failure|uninstall-plan
  --plain       No animation, color, or artwork
  --verbose     Show the synthetic diagnostics
  --help        Show this help
This preview writes only a diagnostic log in a temporary directory.
HELP
}

function install_ui_log_directory() {
    mktemp -d "${TMPDIR:-/tmp}/memql-install-preview.XXXXXX"
}

# shellcheck disable=SC2329 # invoked by the summary in lib.sh
function read_binary_version() { printf '0.16.0\n'; }

function preview_step() {
    echo "INFO: preview only; no machine changes"
    sleep 0.8
    INSTALL_UI_RESULT="$1"
}

function preview_binary() {
    preview_step 'Cockpit v0.16.0 installed'
    if [[ "$SCENARIO" == current ]]; then
        INSTALL_UI_RESULT='Cockpit v0.16.0 is up to date'
    elif [[ "$SCENARIO" == failure ]]; then
        echo 'ERROR: simulated download interrupted; existing installation preserved' >&2
        return 5
    fi
}

function preview_models() {
    preview_step 'Local models need your approval'
    INSTALL_MODELS_STATE=pending
    INSTALL_UI_STAGE_STATE=pending
}

function preview_uninstall() {
    preview_step 'Removal checked'
    case "$SCENARIO" in
        disconnected)
            OTHER_HOMES=1
            CLUSTER_URL=https://api.memql.localhost
            ;;
        uninstalled) record_removed 'the worker and selected enrollment' ;;
        uninstall-failure)
            record_removed 'the selected enrollment'
            record_kept 'the worker service (could not restart)'
            echo 'ERROR: The worker for the remaining enrollment could not restart.' >&2
            return 5
            ;;
    esac
    exit 0
}

function main() {
    SCENARIO=installed; INSTALL_VERBOSE=no; INSTALL_PLAIN=no
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --scenario=*) SCENARIO="${1#*=}" ;;
            --plain) INSTALL_PLAIN=yes ;;
            --verbose) INSTALL_VERBOSE=yes ;;
            --help|-h) show_help; return 0 ;;
            *) show_help >&2; return 2 ;;
        esac
        shift
    done
    case "$SCENARIO" in installed|current|models-pending|failure|uninstalled|disconnected|uninstall-failure|uninstall-plan) ;; *) show_help >&2; return 2 ;; esac
    printf '\n  Preview only — no changes to this machine.\n'
    case "$SCENARIO" in
        uninstalled|disconnected|uninstall-failure|uninstall-plan)
            PURGE=no; DRY_RUN=no
            [[ "$SCENARIO" != uninstall-plan ]] || DRY_RUN=yes
            install_ui_init uninstall
            install_ui_stage 'Checking this machine' preview_uninstall
            return
            ;;
    esac
    install_ui_init install
    install_ui_stage 'Checking the release' preview_step 'Release available'
    install_ui_stage 'Installing Cockpit' preview_binary
    install_ui_stage 'Configuring this machine' preview_step 'Machine configured'
    install_ui_stage 'Starting the worker' preview_step 'Worker started'
    INSTALL_SERVICE_STATE=started
    INSTALLED_BINARY=/usr/local/bin/memql
    if [[ "$SCENARIO" == models-pending ]]; then
        install_ui_stage 'Preparing local models' preview_models
    fi
    install_ui_finish
}

main "$@"
