#!/usr/bin/env python3
"""Fake OpenAI-compatible backend for the surface V-suite.

Modes (first path segment after /v1):
  /v1/plain      — one delta "hello", finish, [DONE]
  /v1/slow       — 10 deltas, 1 s apart, then finish
  /v1/hang       — headers only, then silence until the client goes away
  /v1/runaway    — 256 KiB deltas forever (crosses the 4 MiB cap in ~16)
"""
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def sse(w, obj):
    w.wfile.write(f"data: {obj}\n\n".encode())
    w.wfile.flush()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        self.rfile.read(n)
        parts = self.path.split("/")
        mode = parts[2] if len(parts) > 2 else ""
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        try:
            if mode == "slow":
                for i in range(10):
                    sse(self, '{"choices":[{"delta":{"content":"tick%d "}}]}' % i)
                    time.sleep(1)
            elif mode == "runaway":
                chunk = "x" * (256 * 1024)
                while True:
                    sse(self, '{"choices":[{"delta":{"content":"%s"}}]}' % chunk)
                    time.sleep(0.05)
            elif mode == "hang":
                time.sleep(3600)
                return
            else:
                sse(self, '{"choices":[{"delta":{"content":"hello"}}]}')
            sse(self, '{"choices":[{"delta":{},"finish_reason":"stop"}]}')
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, fmt, *args):
        pass


if __name__ == "__main__":
    port = int(sys.argv[1])
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
