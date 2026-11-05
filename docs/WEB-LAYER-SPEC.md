# Web Layer — the agent's hands on the web

Status: DRAFT for PO red-line. No code until sign-off.
Author: Hermes · 2026-11-05

## 1. The problem, in the PO's words

> "What if the user asks the agent to find something? Or check status?
> There's plenty of things it can do, so we need to be able to handle logins.
> People don't use products because they are difficult."

Per-service connectors cannot cover the web: most sites have no API, and a
form that asks users for transports and headers will never feel simple. The
connector inventory is not the product — **a browser the agent can drive,
with logins solved once and reused everywhere, is.**

## 2. Product principles

1. **Capability, not configuration.** The user types a goal ("check whether
   my order shipped"); the agent picks fetch vs. browse vs. a saved session.
2. **The user owns the password, always.** The agent never sees or stores a
   password. It holds *sessions* (cookies/tokens the site itself issued) in
   the keystore — same law-#7 envelope the device tokens use.
3. **Read freely, act with consent.** Reading pages is ordinary tool use.
   Submitting forms, posting, purchasing, deleting — each requires an
   approval card, same custody pattern already shipped.
4. **Never a flat no.** Login wall ≠ failure; it is a prompt. "I can't reach
   that — want to sign me in?" is one tap from yes.
5. **Don't reinvent.** Headless Chromium + persistent profiles (Playwright
   storage_state pattern), prior art: changedetection.io (watch), browser-use
   (agent-driving), Claude-in-Chrome / ChatGPT Agent (saved-logins UX).

## 3. Capability model (daemon side)

Three new tools available to the rails agent loop; all are ordinary
observations-producing tools, so the fasti engine and cards need no changes.

| Tool | What it does | Gate |
|---|---|---|
| `web_fetch(url)` | Static GET → text/markdown. No JS, no session. | none (public web = public read) |
| `web_browse(url, goal)` | Drives headless Chromium in the origin's profile; reads DOM, may click navigational links; returns extracted answer + screenshot | none for read; **approval card** for any act (submit/post/purchase/delete/toggle) |
| `session_login(origin)` | Raises a custody card: "Sign me in to ⟨site⟩?" Opens a guided sign-in window the user completes in their own hand; on success the browser profile for that origin is encrypted into the session wallet | user-initiated (card is the consent) |

**Session wallet.** One encrypted browser profile per origin
(`~/.lararium/sessions/<origin>/`, AES-GCM, keystore key). Loaded on browse,
written back after use (cookies refreshed). Expiry is detected — hitting the
login wall again re-raises the sign-in card, nothing else breaks.

**Watch/status tasks reuse the same layer.** "Every morning check X" =
scheduled `web_browse` + diff-against-previous + only-notify-on-change —
changedetection.io's model, built on rails that already exist (fasti
observations → decide → card). No separate watcher engine.

## 4. The three user stories, end to end

1. **"Find me the cheapest 1-night rate at the Marriott downtown."**
   → `web_fetch` fails (JS site) → `web_browse` (no login needed) → answer +
   link. Zero setup.
2. **"Did my order ship?"** (UPS, saved session)
   → `web_browse` in UPS profile → reads status → answers. First time: card
   "Sign me in to ups.com?" once; silent forever after (until expiry).
3. **"Every day at 7, tell me if the HOA portal posts anything new."**
   → schedule → `web_browse` in portal profile → diff → card only on change.

## 5. Security & boundaries

- **Per-origin isolation.** Sessions never shared across origins; no
  cross-site navigation with a session (agent stays on-origin, else card).
- **Acts are custody.** Form-submit/post/purchase/delete/publish → approval
  card with screenshot of the exact pending action (Review-only, mirrors
  T6 notification hardening).
- **Boundaries stay data, not personality.** Optional per-site allowlist +
  per-site "read-only" flag, enforced in the daemon, off by default.
- **Full activity log** (url, time, verdict, screenshot ref) — user-queryable
  ("what did you do on my account?").
- **Bot walls** (Cloudflare etc.): headed mode on a desktop browser node is
  the escalation path; honestly unsolvable cases get a clear "this site is
  fighting automation" answer, not a silent loop.
- **ToS posture:** user's own account, user-directed, user-consented — same
  as a browser extension acting for its user. Read-heavy by default; acts
  gated; no scraping at scale; respect robots meta for scheduled jobs.

## 6. Where the browser runs

- **Default: the daemon host** (headless Chromium, ARM64 fine, ~400–600 MB
  RSS per profile; single-board-class hardware is adequate for 1 concurrent
  profile).
- **Escalation: browser node.** Any LAN machine (a laptop, a desktop) can
  register as a browser node — headed window for sign-ins and bot-wall sites.
  Same session-wallet sync rules: wallet lives on the daemon, node gets a
  ephemeral copy for the task. (Phase 3+, design noted, not built first.)

## 7. Phasing

- **P1 — hands, no logins (small).** `web_fetch` + headless `web_browse`
  for public pages; answer-with-link in chat. Proves the loop end-to-end.
- **P2 — the wallet (the unlock).** Sign-in card → guided window → encrypted
  per-origin profiles → silent reuse + expiry re-prompt. Act-approval gate.
- **P3 — scheduled web tasks.** Watch/status via fasti + diff; the "every
  morning" class of asks.
- **P4 — convenience.** "Use my browser's login" (cookie import from the
  user's own browser profile / extension), browser nodes, allowlist UI.

## 8. What we deliberately do NOT build

- A crawler, a scraper framework, or bulk extraction of any kind.
- Password capture, autofill of credentials, or 2FA-code interception.
  (User completes sign-ins; session persistence means TOTP is typed once.)
- Per-service bespoke integrations where a browser session suffices —
  the API catalog stays tiny on purpose (clean-API services only).

## 9. Open questions for PO

1. Act-gate default: **every act asks** (safer, P2 default) vs. per-site
   "always allow reads, ask acts" preference set at sign-in time?
2. P1 scope check: ship fetch+browse for public web as its own beta first,
   or hold until the wallet (P2) makes it feel complete?
3. First proof site for the wallet: pick one PO's realest login-wall site
   (email? carrier portal? HOA?) to develop against — not GitHub-toy.
