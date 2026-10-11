# hearthd — Lararium hearth daemon
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/hearthd ./cmd/hearthd

# debian-slim (not distroless): the compose entrypoint shim runs `sh -c` for
# first-run setup, which needs a shell + coreutils in the runtime image.
FROM debian:bookworm-slim
COPY --from=build /out/hearthd /usr/local/bin/hearthd
# /hearth = persona home (SOUL.md, MEMORY.md, sessions/); mount a volume there.
# /etc/lararium = config (lararium.yaml); mount the config dir there.
WORKDIR /hearth
ENTRYPOINT ["hearthd"]
