#!/usr/bin/env bash
# EN: Thin wrapper so `make smoke` works. Usage: scripts/smoke.sh [base_url]
# TR: `make smoke` çalışsın diye ince bir sarmalayıcı.
set -euo pipefail
exec python3 "$(dirname "$0")/smoke.py" "${1:-http://localhost:8080}"
