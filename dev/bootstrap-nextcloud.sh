#!/usr/bin/env bash
#
# Prepares the dev Nextcloud for bridging: installs Talk, creates test users,
# and registers the bridge bot.
#
# Safe to re-run; every step checks whether it has already been done.
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE=(docker compose -f docker-compose.dev.yaml)

# Fixed dev credentials. This whole stack is localhost-only and disposable.
BOT_NAME="matrix-nctalk-dev"
BOT_URL="http://host.docker.internal:29337/_nctalk/webhook"
# Talk requires 40-128 characters (BotService::validateBotParameters).
BOT_SECRET="nctalk-dev-shared-secret-not-for-production-0001"
TEST_USERS=(alice bob)
# Nextcloud checks new passwords against the Have I Been Pwned breach list, so an
# obvious one like "devpassword123" is rejected outright.
TEST_USER_PASSWORD="Talk-dev-8f3a-bridge"

if [[ ${#BOT_SECRET} -lt 40 || ${#BOT_SECRET} -gt 128 ]]; then
	echo "BOT_SECRET is ${#BOT_SECRET} characters; Talk requires 40-128" >&2
	exit 1
fi

# occ refuses to run as root, and the web server user owns the data directory.
occ() {
	"${COMPOSE[@]}" exec -T --user www-data nextcloud php occ "$@"
}

step() {
	printf '\n\033[1;34m==>\033[0m %s\n' "$1"
}

step "Waiting for Nextcloud to finish installing"
# The image runs the installer on first boot, which takes a while. The healthcheck
# is the authoritative signal, so wait on that rather than guessing.
for _ in $(seq 1 120); do
	status=$("${COMPOSE[@]}" ps --format json nextcloud 2>/dev/null | jq -r 'select(.Service=="nextcloud") | .Health' | head -1)
	if [[ "$status" == "healthy" ]]; then
		break
	fi
	sleep 5
done
if [[ "${status:-}" != "healthy" ]]; then
	echo "Nextcloud did not become healthy. Try: ${COMPOSE[*]} logs nextcloud" >&2
	exit 1
fi
echo "Nextcloud is up."

step "Installing and enabling Talk"
if occ app:list --output=json | jq -e '.enabled.spreed' >/dev/null 2>&1; then
	echo "Talk $(occ app:list --output=json | jq -r '.enabled.spreed') is already enabled."
else
	# app:install downloads from the app store and enables in one step, but fails
	# if the app is present and merely disabled.
	occ app:install spreed || occ app:enable spreed
fi

step "Creating test users"
for user in "${TEST_USERS[@]}"; do
	if occ user:info "$user" >/dev/null 2>&1; then
		echo "$user already exists."
	else
		"${COMPOSE[@]}" exec -T --user www-data \
			-e "OC_PASS=$TEST_USER_PASSWORD" nextcloud \
			php occ user:add --password-from-env --display-name "${user^}" "$user"
	fi
done

step "Registering the bridge bot"
# The bot must be installed in the enabled state, not --no-setup: the bridge
# enables itself per conversation over OCS as a moderator, and Talk rejects that
# for bots an admin has pinned.
#
# The features must be spelled out. Talk's default is webhook+response only, and
# without "reaction" it silently never delivers Like/Undo activities, so
# reactions made in Talk would just never reach Matrix.
BOT_FEATURES=(--feature webhook --feature response --feature reaction)
if occ talk:bot:list --output=json | jq -e --arg n "$BOT_NAME" '.[] | select(.name==$n)' >/dev/null 2>&1; then
	echo "Bot '$BOT_NAME' is already installed."
else
	occ talk:bot:install "${BOT_FEATURES[@]}" "$BOT_NAME" "$BOT_SECRET" "$BOT_URL" \
		"Bridges this conversation to Matrix (development)"
fi
# Reconcile the feature list even for an already-installed bot, so a stack
# bootstrapped before this was fixed picks up the change.
bot_id=$(occ talk:bot:list --output=json | jq -r --arg n "$BOT_NAME" '.[] | select(.name==$n) | .id')
occ talk:bot:state "${BOT_FEATURES[@]}" "$bot_id" 1
occ talk:bot:list

step "Ready"
cat <<EOF
Nextcloud   http://localhost:8081
  admin     admin / adminpassword
$(for u in "${TEST_USERS[@]}"; do printf '  %-9s %s / %s\n' "$u" "$u" "$TEST_USER_PASSWORD"; done)

Bot         $BOT_NAME
  webhook   $BOT_URL
  secret    $BOT_SECRET

The bot secret above must match network.bot_secret in the bridge config.
EOF
