# Lararium Android

The phone is a door, not a home. This app connects **only** to the user's
own [hearthd](../../README.md) endpoint — over LAN, a tailnet/VPN, or the
user's own public https API. There is no Lararium cloud: no analytics SDKs,
no ad IDs, no Firebase, no accounts.

## Build (v0.1 scaffold)

```bash
cd android
./bootstrap-env.sh          # one-time: JDK17 + Android SDK 35 on the CI box
~/gradle-8.11.1/bin/gradle testDebugUnitTest assembleDebug
# APK: app/build/outputs/apk/debug/app-debug.apk
```

## What's here (Slice 1)

- `data/Models.kt` — Endpoint + the **cleartext trust policy**: bearer
  tokens only travel over TLS unless the host is RFC1918 / tailnet (CGNAT
  100.64/10) / loopback. Widening it must break the pinned unit tests.
- `data/HearthClient.kt` — minimal `/v1/health` client (stdlib).
- `MainActivity.kt` — pair screen (URL + token → health check) and the
  `lararium://pair?host=…&token=…` deep link emitted by `hearthd pair`.
- `Storage.kt` — endpoint persistence (plain prefs; **TODO slice 2**:
  EncryptedSharedPreferences — the token unlocks a whole home).

## Roadmap (signed off 2026-10-11, see briefs/android-parity doc)

1. ✅ scaffold + pair + health — **this PR**
2. chat over SSE, session list, markdown
3. reverse device channel: phone-as-MCP-tool-belt with on-device approvals
4. voice (on-device VAD → hearthd), UnifiedPush notifications (no FCM)
5. connectors UI; memory editor; agent-computer viewer

## Distribution

Debug APKs sideload. A signed release channel (Play via the
`larariumio` service account, later F-Droid) lands when signing-key
management is decided; keystores live in CI secrets, never in the repo.
