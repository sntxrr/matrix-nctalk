#!/usr/bin/env bash
#
# Brings the whole dev stack up from nothing, in the one order that works.
#
# The ordering is not arbitrary: Synapse refuses to start when its config names
# an appservice registration file that does not exist, and only the bridge can
# generate that file, and only once it has a config to generate it from.
#
# Safe to re-run; every step is idempotent.
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE=(docker compose -f docker-compose.dev.yaml)
CONFIG=dev/config.yaml
REGISTRATION=dev/synapse/nctalk-registration.yaml

step() {
	printf '\n\033[1;35m###\033[0m \033[1m%s\033[0m\n' "$1"
}

step "1/5 Starting Nextcloud"
"${COMPOSE[@]}" up -d db nextcloud

step "2/5 Configuring Nextcloud"
./dev/bootstrap-nextcloud.sh

step "3/5 Building the bridge"
make build

step "4/5 Generating the bridge config and appservice registration"
if [[ -f "$CONFIG" ]]; then
	echo "$CONFIG already exists, leaving it alone."
else
	cp dev/config.template.yaml "$CONFIG"
	echo "Created $CONFIG from the template."
fi
if [[ -f "$REGISTRATION" ]]; then
	echo "$REGISTRATION already exists, leaving it alone."
else
	# The bridge writes the registration with a plain file write, and on a fresh
	# clone this directory does not exist yet: Docker would create it, but not
	# until Synapse first runs, which is after this.
	mkdir -p "$(dirname "$REGISTRATION")"
	# This also fills every unset key in the config with its default and writes
	# the generated as_token/hs_token back into it.
	./matrix-nctalk -c "$CONFIG" -g -r "$REGISTRATION"
fi

step "5/5 Starting Synapse"
./dev/bootstrap-synapse.sh

step "Done"
cat <<'EOF'
Run the bridge:

    make dev-bridge

Then, in a Matrix client pointed at http://localhost:8008:

    1. Register an account on dev.local (registration is open).
    2. Start a chat with @nctalkbot:dev.local.
    3. Send: login
    4. Follow the Nextcloud link, sign in as alice / Talk-dev-8f3a-bridge,
       and grant access.

Talk is at http://localhost:8081. Messages in a conversation alice moderates
should appear in Matrix, and replies should come back as alice.
EOF
