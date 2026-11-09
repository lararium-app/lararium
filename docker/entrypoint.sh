#!/bin/sh
# Lararium container entrypoint (issue #49). Seeds first-run state from the
# defaults baked into the image, then execs the daemon — or whatever command
# you passed to `docker run`. Never blocks on a non-TTY.
set -eu

HEARTH_HOME="${HEARTH_HOME:-/hearth}"
CONFIG="${CONFIG:-/etc/lararium/lararium.yaml}"

# 1. Seed the persona home when it is empty (never clobber an existing hearth:
#    SOUL.md is the marker that the volume already holds someone's hearth).
if [ ! -f "$HEARTH_HOME/SOUL.md" ] && [ -d /defaults/hearth ]; then
	mkdir -p "$HEARTH_HOME"
	cp -R /defaults/hearth/. "$HEARTH_HOME/"
fi

# 2. Seed the config (the named-volume-empty case).
if [ ! -f "$CONFIG" ] && [ -f /defaults/lararium.yaml ]; then
	mkdir -p "$(dirname "$CONFIG")"
	cp /defaults/lararium.yaml "$CONFIG"
fi

# 3. Provider check: any uncommented `- name:` entry counts. With none, the
#    setup wizard runs on an attached TTY; on a detached start we print the
#    guidance and exit 2 rather than hang a headless container on a prompt.
if ! sed 's/#.*//' "$CONFIG" 2>/dev/null | grep -Eq '^[[:space:]]*-[[:space:]]*name:'; then
	if [ -t 0 ]; then
		hearthd -config "$CONFIG" setup
	else
		echo "lararium: no provider configured; run interactively once: docker run -it --rm ghcr.io/lararium-app/hearthd setup"
		echo "lararium: (with -v volumes mounted so the wizard's output persists)"
		exit 2
	fi
fi

# Custos first-run bootstrap: run only when the custos state dir does not exist yet.
# The marker per internal/custos/vault.go (Exists) is vault.age or vault.key.
CUSTOS_DIR="$HEARTH_HOME/custos"
if [ ! -f "$CUSTOS_DIR/vault.age" ] && [ ! -f "$CUSTOS_DIR/vault.key" ]; then
	# (a) If custos.unlock_keyfile is unset or missing, create it.
	KEYFILE="$(sed -n 's/^[[:space:]]*unlock_keyfile:[[:space:]]*//p' "$CONFIG" 2>/dev/null | sed 's/#.*//' | tr -d ' "' || true)"
	KEYFILE="${KEYFILE:-$HEARTH_HOME/unlock.key}"
	if [ ! -f "$KEYFILE" ]; then
		mkdir -p "$(dirname "$KEYFILE")"
		( umask 077 && head -c32 /dev/urandom | base64 > "$KEYFILE" )
	fi

	# (b) Run custos init with the same -config.
	# Docs' law: `custos init` prints nothing on success; verify files on disk after.
	if ! custos -config "$CONFIG" init; then
		echo "lararium: custos init failed" >&2
		exit 1
	fi

	if [ ! -f "$CUSTOS_DIR/vault.key" ] || [ ! -f "$CUSTOS_DIR/vault.age" ]; then
		echo "lararium: custos bootstrap failed: vault files missing on disk" >&2
		exit 1
	fi
fi

# 4. Explicit arguments are hearthd subcommands (`docker run ghcr.io/lararium-app/hearthd
#    token create x`); with none, serve the web chat + API. For a shell,
#    override --entrypoint.
if [ "$#" -gt 0 ] && [ "$1" = "custosd" ]; then
	shift
	exec custosd -config "$CONFIG" "$@"
fi

if [ "$#" -gt 0 ]; then
	exec hearthd -config "$CONFIG" "$@"
fi
exec hearthd -config "$CONFIG" serve
