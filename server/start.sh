#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/server/start.sh"
BIN="$ROOT/bin/build/docker-compose"
LOG_FILE="${LOG_FILE:-$ROOT/last-build.txt}"

air_build() {
	cd "$ROOT"
	: >"$LOG_FILE"
	{
		printf 'compose-server dev cycle started: %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
		printf 'build command: make\n'
	} | tee -a "$LOG_FILE"

	make 2>&1 | tee -a "$LOG_FILE"
	exit "${PIPESTATUS[0]}"
}

air_run() {
	cd "$ROOT"
	{
		printf 'run command: %q' "$BIN"
		for arg in "$@"; do
			printf ' %q' "$arg"
		done
		printf '\n'
	} | tee -a "$LOG_FILE"

	"$BIN" "$@" 2>&1 | tee -a "$LOG_FILE"
	exit "${PIPESTATUS[0]}"
}

write_air_file() {
	local air_dir="$1"
	shift
	local build_script="$air_dir/build.sh"
	local run_script="$air_dir/run.sh"
	local air_file="$air_dir/air.toml"

	mkdir -p "$air_dir"

	{
		printf '#!/usr/bin/env bash\n'
		printf 'exec %q --air-build\n' "$SCRIPT"
	} >"$build_script"

	{
		printf '#!/usr/bin/env bash\n'
		printf 'exec %q --air-run' "$SCRIPT"
		for arg in "$@"; do
			printf ' %q' "$arg"
		done
		printf '\n'
	} >"$run_script"

	chmod +x "$build_script" "$run_script"

	cat >"$air_file" <<EOF
root = "$ROOT"
tmp_dir = "tmp/air-compose-server"

[build]
cmd = "$build_script"
bin = "$BIN"
full_bin = "$run_script"
include_ext = ["go", "mod", "sum"]
exclude_dir = ["bin", "tmp", "vendor", "node_modules", ".git", ".jj"]
exclude_regex = ["_test.go"]
delay = 1000
stop_on_error = true
send_interrupt = true
kill_delay = "3s"

[log]
time = true
EOF

	printf '%s\n' "$air_file"
}

main() {
	case "${1:-}" in
		--air-build)
			shift
			air_build "$@"
			;;
		--air-run)
			shift
			air_run "$@"
			;;
	esac

	if ! command -v air >/dev/null 2>&1; then
		printf 'air is required for dev watch mode. Install it first, for example: go install github.com/air-verse/air@latest\n' >&2
		exit 127
	fi

	export HOST_UID="${HOST_UID:-$(id -u)}"
	export HOST_GID="${HOST_GID:-$(id -g)}"

	if [ "$#" -eq 0 ]; then
		export STACKS_PATH="${STACKS_PATH:-/Users/jan/Dev/xe/stacks}"
		set -- serve \
			--allowed-origins="${ALLOWED_ORIGINS:-http://127.0.0.1:5177}" \
			--port="${PORT:-8094}" \
			"$STACKS_PATH"
	elif [ "${1:-}" != "serve" ]; then
		set -- serve "$@"
	fi

	local air_dir
	air_dir="$(mktemp -d "${TMPDIR:-/tmp}/compose-server-air.XXXXXX")"
	trap 'rm -rf "$air_dir"' EXIT

	local air_file
	air_file="$(write_air_file "$air_dir" "$@")"

	printf 'Compose server dev watch enabled via air.\n'
	printf 'Last cycle output: %s\n' "$LOG_FILE"
	exec air -c "$air_file"
}

main "$@"
