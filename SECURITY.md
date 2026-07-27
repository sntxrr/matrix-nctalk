# Security policy

## Reporting a vulnerability

Please report security issues privately through GitHub's
[**Report a vulnerability**](https://github.com/sntxrr/matrix-nctalk/security/advisories/new)
form, not as a public issue.

This is a small project maintained by one person in their own time, so the only
promise worth making is an honest one: reports are read, and you will get a
reply acknowledging the report. Fixes for anything that lets someone read or
send messages they should not, or take over a login, will be prioritised over
everything else. Anything less severe gets fixed when it gets fixed.

If you would like credit in the advisory and the release notes, say so and how
you would like to be named. If you would rather not be named, that is fine too.

## What is in scope

The bridge sits between two trust domains and holds credentials for both, so
the interesting parts are:

- **The webhook endpoint** (`/_nctalk/webhook`). It is unauthenticated by
  design — the HMAC signature is the authentication. Anything that gets an
  event accepted without a valid signature, or gets one server's event
  attributed to another, is in scope. See
  [The header naming the sender is not signed](README.md#the-header-naming-the-sender-is-not-signed)
  for the design and its reasoning.
- **Credential handling.** App passwords are encrypted at rest under
  `network.credential_key`; anything that exposes one, or that gets the bridge
  to act with a credential belonging to somebody else, is in scope.
- **Cross-user or cross-portal leakage.** A message, file, reaction or receipt
  reaching a Matrix room or Talk conversation whose members should not see it.
- **Request forgery through user input.** The login flow makes the bridge fetch
  a URL the user supplies; the guards on that are described in
  [Security notes](README.md#security-notes).
- **Anything that makes the bridge act as a Matrix user or Nextcloud user
  without their authorisation.**

## Known limitations, already documented

These are understood and written down rather than overlooked. Reports are still
welcome if you can show the impact is worse than described, but they are not
news on their own:

- **The credential key sits beside the database by default.** Encryption at rest
  protects the database leaking on its own — a backup, a dump, a replica — not
  an attacker who already has the host. Separating them is documented under
  [Credential storage](README.md#credential-storage).
- **The address checks on login do not survive DNS rebinding.** Names are
  resolved when checked and again when connected to; closing that needs address
  checking at connect time.
- **The bridge does not rate-limit its own webhook.** Talk counts any non-200
  against a bot's error budget and disables bots that accumulate them, so
  shedding load would end with Nextcloud switching the bridge off. Rate limiting
  belongs on the reverse proxy in front of it.
- **Talk chat is not end-to-end encrypted**, so a bridged room cannot offer more
  confidentiality than Talk does, whatever the Matrix side is configured with.

## Supported versions

Only the latest release. This is 0.x software: fixes go into a new release
rather than being backported.

## Scope of this policy

This covers the bridge in this repository. Vulnerabilities in Nextcloud, the
Talk app, Synapse, or mautrix-go belong with those projects — though if you
find one while looking at this bridge and are not sure where it belongs, report
it here and it will be passed on.
