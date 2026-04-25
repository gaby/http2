#!/usr/bin/env bash

# Run package tests one-by-one with a timeout and emit goroutine dumps on hangs.
# Env vars:
#   TIMEOUT: per-package timeout in seconds (default 60)
#   COUNT:   test count/shuffle iterations (default 50)
#   GOCACHE: go build cache location (default /tmp/go-build)
#   LOG_DIR: where to store logs (default /tmp/go-hang-logs)

set -euo pipefail

TIMEOUT="${TIMEOUT:-60}"
COUNT="${COUNT:-50}"
GOCACHE="${GOCACHE:-/tmp/go-build}"
LOG_DIR="${LOG_DIR:-/tmp/go-hang-logs}"

mkdir -p "${GOCACHE}" "${LOG_DIR}"

run_pkg() {
  local pkg="$1"
  local name
  name="$(echo "$pkg" | tr '/.' '__')"

  echo "==> Building ${pkg}"
  go test -c -race -o "/tmp/${name}.test" "${pkg}"

  local log="${LOG_DIR}/${name}.log"
  echo "==> Running ${pkg} (log: ${log})"

  GOTRACEBACK=all "/tmp/${name}.test" \
    -test.v \
    -test.shuffle=on \
    -test.count="${COUNT}" \
    -test.timeout="${TIMEOUT}s" \
    >"${log}" 2>&1 &

  local pid=$!
  local deadline=$((SECONDS + TIMEOUT + 10)) # small grace

  while kill -0 "${pid}" 2>/dev/null; do
    if (( SECONDS >= deadline )); then
      echo "!! Timeout for ${pkg}, sending QUIT" | tee -a "${log}"
      kill -QUIT "${pid}" 2>/dev/null || true
      sleep 2
      kill -TERM "${pid}" 2>/dev/null || true
      break
    fi
    sleep 1
  done

  if ! wait "${pid}"; then
    echo "!! ${pkg} exited with non-zero status (see log)" | tee -a "${log}"
  fi
}

for pkg in $(go list ./...); do
  run_pkg "${pkg}"
done
