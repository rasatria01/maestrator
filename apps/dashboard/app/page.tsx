const API = process.env.THEORM_API ?? "http://127.0.0.1:8080";

// Phase E builds this out: run DAG, live logs, diff viewer, memory browser.
export default async function Page() {
  let status = "orchestrator unreachable";
  try {
    const res = await fetch(`${API}/healthz`, { cache: "no-store" });
    status = res.ok ? "orchestrator ok" : `orchestrator ${res.status}`;
  } catch {}
  return (
    <main>
      <h1>TheORM</h1>
      <p>{status}</p>
    </main>
  );
}
