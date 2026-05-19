#!/usr/bin/env bash
set -euo pipefail
dtach -n /tmp/compose-server-air.dtach air -c server/air.toml
