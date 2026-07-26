FROM golang:1.26-alpine AS builder

# CGO is not optional here. mautrix's mxmain imports github.com/mattn/go-sqlite3
# unconditionally — it reads sqlite3.ErrCorrupt while handling database errors —
# so the C sqlite driver is compiled in even for deployments using Postgres.
# There is no CGO_ENABLED=0 build of this bridge to be had.
#
# libolm is deliberately absent: the goolm build tag selects mautrix's pure-Go
# Olm implementation, so neither stage needs olm-dev or olm.
RUN apk add --no-cache build-base git

WORKDIR /build

# Dependencies first, so editing source does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# These feed mautrix's version formatter, which is picky in two ways worth
# knowing: it discards a commit of 8 characters or fewer, so COMMIT must be the
# full SHA rather than a short one, and it only calls a build a release when
# VERSION matches the `version` constant in main.go exactly. Get either wrong
# and the bridge reports itself as vX.Y.Z+dev.unknown.
ARG VERSION=unknown
ARG COMMIT=unknown
# RFC 3339, or it is ignored.
ARG BUILD_TIME=unknown
RUN go build -tags goolm \
    -ldflags "-X main.Tag=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -o /build/matrix-nctalk ./cmd/matrix-nctalk

FROM alpine:3.22

# ca-certificates to reach Nextcloud over HTTPS; su-exec to drop privileges
# after fixing the ownership of a freshly mounted volume.
RUN apk add --no-cache ca-certificates su-exec

COPY --from=builder /build/matrix-nctalk /usr/local/bin/matrix-nctalk
COPY docker-run.sh /usr/local/bin/docker-run.sh

# Everything that survives a restart lives here: config, registration, and the
# sqlite database when Postgres is not used.
VOLUME /data
WORKDIR /data

# Nextcloud posts bot webhooks to this port, and the homeserver sends appservice
# transactions to it. See the README on why that is two inbound paths on one
# listener.
EXPOSE 29337

ENTRYPOINT ["/usr/local/bin/docker-run.sh"]
