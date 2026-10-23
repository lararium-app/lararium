# hearthd — Lararium hearth daemon
FROM node:22 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/hearthd ./cmd/hearthd

# debian-slim (not distroless): the entrypoint shim is POSIX sh and needs
# coreutils (cp/grep/sed) for first-run seeding, so the runtime image carries
# a shell + coreutils.
FROM debian:bookworm-slim
COPY --from=build /out/hearthd /usr/local/bin/hearthd
# /hearth = persona home (SOUL.md, MEMORY.md, sessions/); mount a volume there.
# /etc/lararium = config (lararium.yaml); mount the config dir there.
#
# First-run defaults (issue #49): the entrypoint copies these into an empty
# /hearth or /etc/lararium and never touches a populated one. The bundled
# reference config binds 0.0.0.0 INSIDE the container — required for a
# published port to reach it through docker-proxy; host-side exposure stays
# loopback-only via `-p 127.0.0.1:7717:7717` (compose does the same).
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
RUN mkdir -p /defaults/hearth
COPY docker/hearth/SOUL.md docker/hearth/IDENTITY.md docker/hearth/USER.md docker/hearth/MEMORY.md /defaults/hearth/
COPY docker/config/lararium.yaml /defaults/lararium.yaml
# M8: unprivileged by default. Empty /hearth and /etc/lararium are owned by
# the daemon user so first-use named volumes inherit writable ownership;
# bind mounts must be owned by the same uid (compose sets it via
# LARARIUM_UID, default 1000). Nothing in the runtime path needs privileges:
# the daemon binds a high port, writes only under its two mounts, and drops
# every capability via compose (cap_drop ALL + no-new-privileges).
RUN groupadd -g 10001 lararium \
 && useradd -r -u 10001 -g lararium -M -d /hearth lararium \
 && mkdir -p /hearth /etc/lararium \
 && chown -R 10001:10001 /hearth /etc/lararium
WORKDIR /hearth
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
