#!/usr/bin/env bash
#
# Prepares the dev Synapse: generates its config if needed, points it at the
# bridge's appservice registration, and relaxes the settings that get in the way
# of local testing.
#
# Safe to re-run; the override block is only appended once.
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE=(docker compose -f docker-compose.dev.yaml)
CONFIG=dev/synapse/homeserver.yaml
REGISTRATION=dev/synapse/nctalk-registration.yaml
MARKER="# --- matrix-nctalk dev overrides ---"

step() {
	printf '\n\033[1;34m==>\033[0m %s\n' "$1"
}

if [[ ! -f "$REGISTRATION" ]]; then
	echo "$REGISTRATION does not exist." >&2
	echo "Synapse refuses to start when app_service_config_files names a missing file," >&2
	echo "so generate it first: ./matrix-nctalk -c dev/config.yaml -g -r $REGISTRATION" >&2
	exit 1
fi

step "Generating the Synapse config"
if [[ -f "$CONFIG" ]]; then
	echo "$CONFIG already exists."
else
	"${COMPOSE[@]}" run --rm synapse generate
fi

step "Applying dev overrides"
if grep -qF "$MARKER" "$CONFIG"; then
	echo "Overrides are already present."
	changed=false
else
	# Appending is safe because the generated config sets none of these keys.
	cat >>"$CONFIG" <<EOF

$MARKER
# The bridge's appservice registration. Synapse will not start if this file is
# missing, which is why it has to be generated before Synapse first runs.
app_service_config_files:
  - /data/nctalk-registration.yaml

# Open registration so test accounts can be made from any client without email.
enable_registration: true
enable_registration_without_verification: true

# This is an isolated dev server with no federation partners.
suppress_key_server_warning: true

# Synapse's defaults (0.2 messages/second) throttle a bridge replaying a
# conversation almost immediately.
rc_message:
  per_second: 1000
  burst_count: 1000
rc_joins:
  local:
    per_second: 1000
    burst_count: 1000
  remote:
    per_second: 1000
    burst_count: 1000
rc_invites:
  per_room:
    per_second: 1000
    burst_count: 1000
  per_user:
    per_second: 1000
    burst_count: 1000
EOF
	echo "Appended dev overrides to $CONFIG."
	changed=true
fi

step "Starting Synapse"
"${COMPOSE[@]}" up -d synapse
if [[ "$changed" == true ]]; then
	# up -d is a no-op for an already-running container, so a config change needs
	# an explicit restart to take effect.
	"${COMPOSE[@]}" restart synapse
fi

for _ in $(seq 1 40); do
	if curl -fsS --max-time 2 http://localhost:8008/health >/dev/null 2>&1; then
		echo "Synapse is up at http://localhost:8008 (server name dev.local)."
		exit 0
	fi
	sleep 2
done

echo "Synapse did not become healthy. Try: ${COMPOSE[*]} logs synapse" >&2
exit 1
