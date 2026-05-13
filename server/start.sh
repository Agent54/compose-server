#!/usr/bin/env bash
set -euo pipefail
exec dtach -n server/air.dtach air -c server/air.toml
