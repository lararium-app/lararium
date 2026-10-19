# Remote access

By default `hearthd serve` binds `127.0.0.1:7717` and refuses to answer any
request whose `Host` header is not localhost. That is the safe posture; this
guide covers the three supported ways to reach a hearth from elsewhere, in
recommended order.

The rule that drives everything: **a non-loopback bind refuses to start
without `serve.allowed_hosts`**, and every `Host` header that should be
served must appear in that list. The allow-list is checked on every request,
so a reverse DNS rebinding attack gets a 403, not your agent.

## 1. Tailnet (recommended)

Put the daemon on a Tailscale interface and never expose it publicly.

```yaml
serve:
  listen: "100.100.100.100:7717"   # the machine's tailnet address
  allowed_hosts:
    - "100.100.100.100:7717"
    - "hearth.your-tailnet.ts.net:7717"   # if you use ts Serve's hostname
```

Notes:

- Match what actually arrives in `Host:` — with plain Tailscale that is the
  tailnet IP (plus port); with `tailscale serve` in front, whatever hostname
  the client used reaches the daemon depending on your Serve config, so
  start with the client-visible host and tighten.
- Tokens cross the tailnet in headers; tailnet traffic is end-to-end
  encrypted, which is what makes the plaintext HTTP bind acceptable. If you
  would not send the token over the network you are on, terminate TLS.

## 2. TLS reverse proxy (public or LAN)

Terminate TLS in a proxy and keep the daemon loopback-bound. The proxy must
forward the original `Host` (or `preserve_host`) and SSE must not be
buffered.

Caddy (`http://` hostnames get automatic Let's Encrypt):

```caddy
lararium.example.com {
    reverse_proxy 127.0.0.1:7717 {
        flush_interval -1          # don't buffer SSE streams
    }
}
```

nginx:

```nginx
location / {
    proxy_pass http://127.0.0.1:7717;
    proxy_set_header Host $host;
    proxy_http_version 1.1;
    proxy_buffering off;           # SSE
    proxy_read_timeout 1h;
}
```

With the loopback bind, `allowed_hosts` stays empty — the Host check still
applies, so requests through the proxy must present a `Host` the daemon
accepts; when you proxy by IP, add that IP (with port) to `allowed_hosts`.

Docker note: inside the container Lararium listens on `0.0.0.0:7717` (the
container has its own network namespace — that is the bind, not an
exposure); publish it loopback (`127.0.0.1:7717:7717`) and let your host
proxy or tailnet decide who gets through.

## 3. SSH tunnel (ad-hoc, zero config)

From any machine with SSH access to the hearth host:

```sh
ssh -N -L 7717:127.0.0.1:7717 user@hearth-host
```

Then use `http://localhost:7717` locally. No config change at all — the
daemon only ever sees loopback with `Host: localhost`.

## Hardening checklist

- Prefer 1 or 2 with TLS over anything plaintext on a shared network.
- Keep `allowed_hosts` minimal: exact hosts, and remove the port when the
  proxy normalizes it away.
- Door tokens are bearer credentials: rotating one is `hearthd token
  revoke` + mint a replacement; do it whenever a client is lost.
- The web UI is single-operator; there is no authentication layer to hide
  behind at beta. Network posture *is* the authentication.
