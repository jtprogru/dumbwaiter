#!/bin/sh
set -eu
if [ -z "${TEST_ID:-}" ]; then
    read -r TEST_ID < /proc/sys/kernel/random/uuid
fi
export TEST_ID
printf 'k6 profile=%s testid=%s\n' "${LOAD_PROFILE:-smoke}" "$TEST_ID" >&2
# No retained end-of-test Trend summary for a 24-million-request run. Native
# histogram remote-write + no Trend thresholds keep the long run bounded.
if [ "${LOAD_PROFILE:-smoke}" = background ] && [ "${1:-}" = run ]; then
    shift
    exec k6 run --summary-mode=disabled "$@"
fi
exec k6 "$@"
