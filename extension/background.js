// The helper is reached from here, not from the content script, so page CSP
// and the browser's local-network prompts never get involved.
const BASE = "http://127.0.0.1:7420";
const api = globalThis.chrome ?? globalThis.browser;

api.runtime.onMessage.addListener((m, _sender, reply) => {
  const headers = { "X-Tuiostv": "1" };
  const id = encodeURIComponent(m.id);
  let req;
  if (m.op === "poll") {
    req = fetch(`${BASE}/poll?role=source&id=${id}`, { headers });
  } else {
    headers["Content-Type"] = "application/json";
    req = fetch(`${BASE}/msg?from=source&id=${id}`, {
      method: "POST",
      headers,
      body: JSON.stringify(m.msg),
    });
  }
  req
    .then(async (r) => {
      if (!r.ok) return reply({ ok: false, status: r.status });
      reply({ ok: true, data: m.op === "poll" ? await r.json() : null });
    })
    .catch((e) => reply({ ok: false, error: String(e) }));
  return true;
});
