# matrix-nctalk

A Matrix ↔ Nextcloud Talk bridge built on [mautrix-go bridgev2](https://pkg.go.dev/maunium.net/go/mautrix/bridgev2).

matrix.org's [bridge ecosystem page](https://matrix.org/ecosystem/bridges/) lists Nextcloud Talk as having no bridge. The only existing option, [talk_matterbridge](https://github.com/nextcloud/talk_matterbridge), is a plain relay: one shared bot account, `<username> message` prefixes, and no ghost users, reactions, edits or attachments.

This is a double-puppeting bridge instead:

- Nextcloud users appear in Matrix as **ghost users** with their real names and avatars.
- Matrix users post into Talk **as their own Nextcloud account**, not as a relay bot.
- Ingress uses Talk's **bot webhook API**, so messages are pushed rather than polled.

> **Status: early development.** Messages, files, reactions, edits, deletions and read receipts bridge both ways, with replies, mentions and formatting. History is backfilled into new rooms and after downtime. There is no Docker image yet — see [Status](#status).

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

**`appservice.public_address` must be set to a real URL.** The Matrix connector treats the placeholder value as unset and then exposes no HTTP server at all, which is what the webhook endpoint is mounted on — so the bridge refuses to start. Set it to the address Nextcloud can reach the bridge at.

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

## Local development stack

`docker-compose.dev.yaml` runs Nextcloud + Talk, Postgres and Synapse locally. The bridge itself runs on the host so it can be rebuilt and debugged without a container round trip.

```sh
make dev-up       # start everything and configure it (safe to re-run)
make dev-bridge   # run the bridge against it, in the foreground
```

`dev-up` starts Nextcloud, installs Talk, creates test users, registers the bridge bot, generates the bridge config and appservice registration, and starts Synapse. The ordering matters and is why it is a script: Synapse refuses to start when its config names an appservice registration that does not exist, and only the bridge can generate that file.

| | |
|---|---|
| Nextcloud | <http://localhost:8081> — `admin` / `adminpassword`, plus `alice` and `bob` |
| Synapse | <http://localhost:8008> — server name `dev.local`, registration open |
| Bridge | <http://localhost:29337> |

Talk reaches the bridge at `host.docker.internal`. No tunnel is needed: Talk's `BotService` passes `allow_local_address`, so it will post webhooks to private addresses, and it accepts `http://` bot URLs.

To exercise it: log in as `alice` through the bridge bot, create a conversation in Talk that alice moderates, and send a message. The bridge enables its own bot in the conversation, and messages flow both ways.

```sh
make dev-logs     # follow container logs
make dev-down     # stop, keeping all data
make dev-reset    # destroy the stack and all its data (prompts first)
```

Everything in the dev stack uses fixed throwaway credentials and is bound to localhost. `dev/config.yaml` and `dev/synapse/` are gitignored because generating the registration writes live tokens and a signing key into them.

## How it works

| Direction | Mechanism |
|---|---|
| Talk → Matrix | Talk POSTs Activity Streams 2.0 events to `/_nctalk/webhook`, signed with HMAC-SHA256. The bridge verifies, enqueues, and acks immediately — Talk allows only 5 seconds and disables bots that fail repeatedly. |
| Matrix → Talk | OCS chat API using the sender's own app password, so messages are attributed to the real Nextcloud user. Matrix users with no linked account are rejected, or relayed via the bot if `relay_unlinked_users` is on. |
| Files | WebDAV against the user's own files, then an OCS share into the conversation. Both directions move the bytes through the bridge; nothing is hotlinked, so Matrix clients need no Nextcloud credentials. |
| History | `GET /chat/{token}` read in whichever direction bridgev2 asks for, paging on `lastKnownMessageId` and the `X-Chat-Last-Given` header. A new room is filled with recent history; a bridge that has been down catches up on what it missed. |

Conversations become **shared portals**: a Talk conversation token is global to the server and both sides of a one-to-one see the same token, so every bridged user of a conversation lands in the same Matrix room.

The bridge enables its own bot per conversation via `POST /ocs/v2.php/apps/spreed/api/v1/bot/{token}/{botId}`, which needs the logged-in user to be a **moderator** of that conversation. Where they are not, a moderator must enable "Matrix Bridge" in the conversation's settings, or an admin can run `occ talk:bot:setup <botId> <token>`.

### Signing, in both directions

Talk uses the same HMAC primitive each way but signs different data, which is easy to get wrong:

- **Inbound webhooks** sign the **raw JSON request body**. The handler must read the exact bytes before decoding.
- **Outbound bot calls** sign only the **message text** or **reaction emoji** — never the encoded form body.

`pkg/nctalk/bot_test.go` pins both.

### Reactions do not say who reacted

Talk's `Like` and `Undo` activities carry the **author of the message being reacted to** in `actor`, not the person reacting, and nothing else in the payload identifies them. Taking `actor` at face value credits every reaction to whoever wrote the message.

So a reaction activity is treated only as a signal that something changed: the bridge fetches `GET /ocs/v2.php/apps/spreed/api/v1/reaction/{token}/{messageId}` and syncs the whole set. That also covers removals, which Talk reports the same way, and it means the bridge's own reactions reconcile against the rows it already wrote instead of echoing back as duplicates.

### Files arrive as something other than a message

A file shared into a conversation reaches the bot as an **`Activity`**, not a `Create`, with an **empty** system message name — a real chat message whose whole body is a `{file}` rich object. Genuine system messages are also `Activity`, but always name their type, so the empty name is what tells the two apart. Route `Activity` to the system-message handler alone and every file silently disappears.

The `file` object in that payload cannot be used to fetch the file. Its `path` is resolved for nobody in particular — a file the sender keeps in their own Talk folder arrives as a bare `note.txt` — whereas the chat API resolves it per requester (`Talk/note.txt` for everyone who can see it). The bridge therefore re-reads the message with `GET /chat/{token}/{messageId}/context?limit=1` as the logged-in user and downloads over WebDAV using that path.

Going the other way, a file is a WebDAV `PUT` into the user's attachment folder followed by an OCS share with `shareType=10`. The share reports its own ID but not the chat message's, so the bridge passes a `referenceId` — which Talk does store on the message — and looks the message up by it afterwards. Without that, every file sent from Matrix would come back over the webhook and be bridged a second time.

Also note that `PUT` overwrites silently, so a name already in use has to be found with `HEAD` and stepped around before uploading, not discovered afterwards.

### Reading history is full of small traps

`GET /chat/{token}` serves one page in one direction and reports the cursor for the next in the **`X-Chat-Last-Given`** header. Four things about it are easy to get wrong:

- **An empty result is a `304` with no body at all** — no OCS envelope to parse. That is how the end of the history announces itself, in both directions, so it has to be read as "nothing left" rather than as a failure.
- **`limit` above 200 is a bodyless `400`.** spreed's own controller clamps the value to 200, but the route rejects a larger one before that code runs, so the caller has to clamp too.
- **The cursor counts messages the server withheld**, not just the ones it returned. A page can come back with a cursor and no messages — every entry hidden by expiry or visibility — so paging follows the cursor and does not stop at the first empty page.
- **Reading history must not look like reading the chat.** `setReadMarker=0`, `noStatusUpdate=1` and `markNotificationsAsRead=0` keep a backfill from moving the user's read marker, clearing their notifications, or making them appear online in Talk.

Two things fall out of reading history as the logged-in user. File paths come back already resolved against that user's own files, so a backfilled share needs none of the per-message `/context` lookup the webhook path does. Reactions, though, are reported only as a count per emoji, so the people who reacted are a separate request per message — charged against a per-batch budget, and logged when it runs out.

Talk's history is also dense with system messages that narrate things the bridge already sends as themselves — every reaction, edit and deletion. Those are dropped, along with deleted messages, whose remaining "message deleted" placeholder is meaningless as history.

Paging *further* back on demand — bridgev2's backwards backfill queue — is implemented, but only runs on a homeserver that supports batch sending. Synapse does not, so there the bridge fills a new room with `backfill.max_initial_messages` and goes no deeper.

### Nothing retries a missed webhook

Talk sends each bot event once. A bridge that is down misses those messages permanently, and no later event refers back to them. So each login resyncs its bridged conversations on a timer (`sync_interval`, default hourly) and immediately on connect: room name, topic, avatar and members, plus the conversation's last activity time, which is what tells bridgev2 to pull in anything newer than the last bridged message. Recovering missed messages also needs `backfill.enabled: true`, which is off in the default config.

Only conversations that already have a portal are resynced — a timer is not a reason to pull every conversation on the server into Matrix — and when several logins share a conversation, the one that owns the portal does the work.

## Status

| Milestone | State |
|---|---|
| M0 — scaffold, OCS client, login flows | Done |
| M1 — webhook ingress, portals, ghosts | Done |
| M2 — egress as the real Nextcloud user | Done |
| M3 — reactions, edits, redactions, receipts | Done |
| M4 — files, rich objects, system messages | Done |
| M5 — backfill and metadata sync | Done |
| M6 — Docker packaging | Not started |

Out of scope for v1: voice/video calls (bridged only as notices), Talk Federation interop, and breakout rooms.

## Layout

```
cmd/matrix-nctalk/    entry point
pkg/connector/        bridgev2 network connector
pkg/nctalk/           standalone Nextcloud OCS client, no bridge dependencies
```

## Licence

[GNU AGPL-3.0-or-later](LICENSE), the same licence as [mautrix-go](https://github.com/mautrix/go), which this is built on.

Note what that means for a bridge in particular: AGPL section 13 says that if you run a **modified** version and let other people use it over a network, those users must be offered the source of your version. Running the stock bridge for your own users carries no such obligation.
