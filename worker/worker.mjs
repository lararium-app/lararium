/**
 * Lararium waitlist API — Cloudflare Worker
 * POST /subscribe { email }  ->  200 { ok: true }
 * GET  /count               ->  200 { count: N }  (light, public)
 * Storage: KV binding WAITLIST, key = email (lowercased), value = ISO timestamp.
 * Export anytime:  npx wrangler kv key list --namespace-id <id>
 */
export default {
  async fetch(request, env) {
    const cors = {
      "Access-Control-Allow-Origin": "https://lararium.io",
      "Access-Control-Allow-Methods": "POST, GET, OPTIONS",
      "Access-Control-Allow-Headers": "Content-Type",
    };
    if (request.method === "OPTIONS") return new Response(null, { headers: cors });

    const url = new URL(request.url);
    const json = (body, status = 200) =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json", ...cors },
      });

    if (url.pathname === "/count" && request.method === "GET") {
      // count is approximate-cheap: KV has no counter; keep a running key.
      const n = (await env.WAITLIST.get("_count")) ?? "0";
      return json({ count: Number(n) });
    }

    if (url.pathname === "/subscribe" && request.method === "POST") {
      let email;
      try {
        ({ email } = await request.json());
      } catch {
        return json({ ok: false, error: "bad_json" }, 400);
      }
      email = String(email || "").trim().toLowerCase();

      // Honest, cheap validation — no regex hairball, just the invariants.
      if (
        email.length < 3 || email.length > 254 ||
        !email.includes("@") || email.startsWith("@") ||
        email.includes(" ") || email.includes("..")
      ) {
        return json({ ok: false, error: "bad_email" }, 400);
      }

      // Basic abuse brake: one write per IP per 10s.
      const ip = request.headers.get("CF-Connecting-IP") || "unknown";
      const rlKey = `_rl:${ip}`;
      if (await env.WAITLIST.get(rlKey)) return json({ ok: false, error: "slow_down" }, 429);
      await env.WAITLIST.put(rlKey, "1", { expirationTtl: 60 });

      const existing = await env.WAITLIST.get(email);
      if (!existing) {
        await env.WAITLIST.put(email, new Date().toISOString());
        const n = Number((await env.WAITLIST.get("_count")) ?? "0") + 1;
        await env.WAITLIST.put("_count", String(n));
      }
      return json({ ok: true, already: Boolean(existing) });
    }

    return json({ ok: false, error: "not_found" }, 404);
  },
};
