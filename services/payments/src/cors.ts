const ORIGINS = (process.env.SPADE_ALLOWED_ORIGINS ?? "http://localhost:5173").split(",").map(o => o.trim()).filter(Boolean);

/** Browser access is limited to configured client origins; webhooks are server-to-server and need no CORS. */
export function corsHeaders(origin: string | undefined): Record<string, string> {
  if (!origin || !ORIGINS.includes(origin)) return {};
  return { "Access-Control-Allow-Origin": origin, Vary: "Origin", "Access-Control-Allow-Headers": "Authorization, Content-Type", "Access-Control-Allow-Methods": "GET, POST, OPTIONS" };
}
