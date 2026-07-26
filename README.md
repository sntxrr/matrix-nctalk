# matrix-nctalk

A Matrix ↔ Nextcloud Talk bridge built on [mautrix-go bridgev2](https://pkg.go.dev/maunium.net/go/mautrix/bridgev2).

matrix.org's [bridge ecosystem page](https://matrix.org/ecosystem/bridges/) lists Nextcloud Talk as having no bridge. The only existing option, [talk_matterbridge](https://github.com/nextcloud/talk_matterbridge), is a plain relay: one shared bot account, `<username> message` prefixes, and no ghost users, reactions, edits or attachments.

This is a double-puppeting bridge instead:

- Nextcloud users appear in Matrix as **ghost users** with their real names and avatars.
- Matrix users post into Talk **as their own Nextcloud account**, not as a relay bot.
- Ingress uses Talk's **bot webhook API**, so messages are pushed rather than polled.

> **Status: early development.** Login, metadata sync, and Talk → Matrix message and reaction bridging work. Sending from Matrix to Talk is not implemented yet — see [Status](#status).

## Requirements

- Nextcloud 27.1 or newer with Talk 17.1 or newer (the `bots-v1` capability).
- Shell access to the Nextcloud server, to run `occ talk:bot:install` once.
- A Matrix homeserver you can register an appservice on.
- Go 1.24+ to build.

## Building

```sh
make build
```

The build uses the `goolm` tag to select mautrix's pure-Go Olm implementation. Without it, the build requires libolm's C headers.

## Setup

### 1. Generate the config and registration

```sh
./matrix-nctalk -e -c config.yaml     # write an example config
$EDITOR config.yaml                    # fill in homeserver, appservice, network
./matrix-nctalk -g -c config.yaml -r registration.yaml
```

Register `registration.yaml` with your homeserver and restart it.

### 2. Install the bot on Nextcloud

Generate a shared secret and install the bot, pointing it at the bridge's public address:

```sh
occ talk:bot:install "Matrix Bridge" "$SECRET" \
    "https://bridge.example.com/_nctalk/webhook" \
    --feature webhook --feature response --feature reaction
```

Two things matter here:

- **Do not pass `--no-setup`.** That installs the bot in Talk's "no setup" state, where only an admin can add it to conversations. The bridge then cannot enable itself and every conversation needs a manual `occ talk:bot:setup`.
- **`bot_name` in the config must match the name above.** The bridge finds its own bot ID by looking itself up by name in a conversation's bot list, which is the only route that does not require admin API access.

Put the same secret in `network.bot_secret` in `config.yaml`.

### 3. Log in

Start the bridge, then message the bridge bot on Matrix:

```
login
```

Pick **Browser** and approve the request in your Nextcloud session. Nextcloud mints a dedicated app password; the bridge never sees your account password. The **App password** flow is available for headless setups.

## How it works

| Direction | Mechanism |
|---|---|
| Talk → Matrix | Talk POSTs Activity Streams 2.0 events to `/_nctalk/webhook`, signed with HMAC-SHA256. The bridge verifies, enqueues, and acks immediately — Talk allows only 5 seconds and disables bots that fail repeatedly. |
| Matrix → Talk | OCS chat API using the sender's own app password, so messages are attributed to the real Nextcloud user. Matrix users with no linked account are rejected, or relayed via the bot if `relay_unlinked_users` is on. |

Conversations become **shared portals**: a Talk conversation token is global to the server and both sides of a one-to-one see the same token, so every bridged user of a conversation lands in the same Matrix room.

The bridge enables its own bot per conversation via `POST /ocs/v2.php/apps/spreed/api/v1/bot/{token}/{botId}`, which needs the logged-in user to be a **moderator** of that conversation. Where they are not, a moderator must enable "Matrix Bridge" in the conversation's settings, or an admin can run `occ talk:bot:setup <botId> <token>`.

### Signing, in both directions

Talk uses the same HMAC primitive each way but signs different data, which is easy to get wrong:

- **Inbound webhooks** sign the **raw JSON request body**. The handler must read the exact bytes before decoding.
- **Outbound bot calls** sign only the **message text** or **reaction emoji** — never the encoded form body.

`pkg/nctalk/bot_test.go` pins both.

## Status

| Milestone | State |
|---|---|
| M0 — scaffold, OCS client, login flows | Done |
| M1 — webhook ingress, portals, ghosts | Done |
| M2 — egress as the real Nextcloud user | Not started |
| M3 — reactions, edits, redactions, receipts | Not started |
| M4 — files, rich objects, system messages | Not started |
| M5 — backfill and metadata sync | Not started |
| M6 — Docker packaging | Not started |

Out of scope for v1: voice/video calls (bridged only as notices), Talk Federation interop, and breakout rooms.

## Layout

```
cmd/matrix-nctalk/    entry point
pkg/connector/        bridgev2 network connector
pkg/nctalk/           standalone Nextcloud OCS client, no bridge dependencies
```

## Licence

Not yet chosen. mautrix-go is AGPL-3.0, which constrains the options for a derived work.
