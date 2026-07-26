#!/bin/sh
# Entrypoint for the matrix-nctalk container.
#
# Walks a first run through the two things that cannot be automated — filling in
# the config and handing the registration to the homeserver — and stops until
# each is done, rather than failing later with something less obvious.
set -eu

CONFIG=/data/config.yaml
REGISTRATION=/data/registration.yaml

# A bind-mounted host directory arrives owned by whoever created it, so the
# ownership is fixed here and privileges dropped, rather than demanding the
# operator chown it to a uid they would have to go and look up.
UID_=${UID:-1337}
GID_=${GID:-$UID_}
if [ "$(id -u)" = "0" ]; then
	chown -R "$UID_:$GID_" /data
	exec su-exec "$UID_:$GID_" "$0" "$@"
fi

# Subcommands run on their own, before any of the gating below: bot-install in
# particular exists to be used when there is not yet a usable config.
if [ "${1:-}" = "bot-install" ]; then
	shift
	# Our -c comes first so an explicitly passed one overrides it.
	exec matrix-nctalk bot-install -c "$CONFIG" "$@"
fi

if [ ! -f "$CONFIG" ]; then
	matrix-nctalk -c "$CONFIG" -e
	cat <<EOF

A starting config has been written to $CONFIG.

Fill in at least:
  homeserver.address / homeserver.domain  where your homeserver is
  appservice.public_address               the URL Nextcloud can reach this bridge at
  bridge.permissions                      who may use the bridge

For network.bot_secret, run:
  docker compose run --rm matrix-nctalk bot-install

Then start the container again.
EOF
	exit 1
fi

if [ ! -f "$REGISTRATION" ]; then
	matrix-nctalk -c "$CONFIG" -g -r "$REGISTRATION"
	cat <<EOF

An appservice registration has been written to $REGISTRATION.

Copy it to your homeserver, add it to app_service_config_files in its config,
and restart the homeserver. Then start this container again.
EOF
	exit 1
fi

exec matrix-nctalk -c "$CONFIG" -r "$REGISTRATION" "$@"
