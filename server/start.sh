#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
exec air -c server/air.toml
