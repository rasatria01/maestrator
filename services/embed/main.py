"""CPU embedding + reranking sidecar (design §2.4, A5). No GPU, ever.

POST /embed   {"texts": [...]}              -> {"vectors": [[768 floats], ...]}
POST /rerank  {"query": "...", "docs":[...]}-> {"scores": [float, ...]}
GET  /health                                -> 200

ponytail: stdlib http.server, single-threaded. It sits off the critical path
(§5.6) so throughput does not matter yet; move to uvicorn if it ever does.
"""

import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer

from fastembed import TextEmbedding
from fastembed.rerank.cross_encoder import TextCrossEncoder

# 768 dims, matching ltm_records.embedding in migrations/0001_init.sql.
embedder = TextEmbedding("nomic-ai/nomic-embed-text-v1.5")
reranker = TextCrossEncoder("Xenova/ms-marco-MiniLM-L-6-v2")


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self._send(200, {"ok": True} if self.path == "/health" else {"error": "not found"})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"] or 0)) or b"{}")
        if self.path == "/embed":
            vectors = [v.tolist() for v in embedder.embed(body["texts"])]
            self._send(200, {"vectors": vectors})
        elif self.path == "/rerank":
            scores = list(reranker.rerank(body["query"], body["docs"]))
            self._send(200, {"scores": scores})
        else:
            self._send(404, {"error": "not found"})

    def _send(self, code, payload):
        data = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_):
        pass


if __name__ == "__main__":
    port = int(os.getenv("THEORM_EMBED_PORT", "8090"))
    print(f"embed sidecar on :{port}", flush=True)
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
