#!/usr/bin/env python3
"""Serve hls-player.html against a running OpenCloud, on one origin.

    ./serve-hls-player.py --upstream https://localhost:9200 --user admin:admin
    open http://localhost:8099/

Everything that is not the page itself is forwarded to the upstream OpenCloud
with the given credentials attached, so the player and the DAV endpoints share
an origin. That is worth the twenty lines: opening the page from the filesystem
and pointing it at a server means cross-origin requests, which drags in CORS
configuration and credentialed-request rules, and then a failure tells you
something about CORS rather than about video.

Range requests are forwarded as they come, so the upstream's own 206 handling is
what gets exercised.

For development only. It attaches credentials to whatever asks and verifies no
TLS certificates.
"""

import argparse
import http.server
import ssl
import sys
import urllib.error
import urllib.request
from base64 import b64encode
from pathlib import Path

PAGE = Path(__file__).with_name("hls-player.html")

# Hop-by-hop headers are meaningless to forward, and a mismatched
# Content-Encoding or Transfer-Encoding turns a working stream into a corrupt one.
SKIP_RESPONSE_HEADERS = {
    "transfer-encoding",
    "content-encoding",
    "connection",
    "keep-alive",
    "upgrade",
}
SKIP_REQUEST_HEADERS = {"host", "connection", "keep-alive", "accept-encoding", "upgrade"}


def build_handler(upstream: str, authorization: str) -> type:
    context = ssl.create_default_context()
    context.check_hostname = False
    context.verify_mode = ssl.CERT_NONE
    opener = urllib.request.build_opener(urllib.request.HTTPSHandler(context=context))

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, fmt: str, *args) -> None:
            sys.stderr.write("  %s\n" % (fmt % args))

        def do_GET(self) -> None:  # noqa: N802
            if self.path in ("/", "/index.html") or self.path.startswith("/?"):
                self.serve_page()
                return
            self.proxy("GET")

        def do_HEAD(self) -> None:  # noqa: N802
            self.proxy("HEAD")

        def do_PUT(self) -> None:  # noqa: N802
            self.proxy("PUT")

        def do_PROPFIND(self) -> None:  # noqa: N802
            self.proxy("PROPFIND")

        def serve_page(self) -> None:
            body = PAGE.read_bytes()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def proxy(self, method: str) -> None:
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else None

            request = urllib.request.Request(upstream + self.path, data=body, method=method)
            for name, value in self.headers.items():
                if name.lower() not in SKIP_REQUEST_HEADERS:
                    request.add_header(name, value)
            request.add_header("Authorization", authorization)

            try:
                with opener.open(request) as response:
                    payload = response.read()
                    status, headers = response.status, response.headers
            except urllib.error.HTTPError as err:
                payload = err.read()
                status, headers = err.code, err.headers
            except OSError as err:
                self.send_error(502, str(err))
                return

            self.send_response(status)
            for name, value in headers.items():
                if name.lower() not in SKIP_RESPONSE_HEADERS and name.lower() != "content-length":
                    self.send_header(name, value)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            if method != "HEAD":
                self.wfile.write(payload)

    return Handler


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--upstream", default="https://localhost:9200")
    parser.add_argument("--user", default="admin:admin", help="user:password")
    parser.add_argument("--port", type=int, default=8099)
    args = parser.parse_args()

    authorization = "Basic " + b64encode(args.user.encode()).decode()
    handler = build_handler(args.upstream.rstrip("/"), authorization)

    server = http.server.ThreadingHTTPServer(("127.0.0.1", args.port), handler)
    print(f"player:   http://localhost:{args.port}/")
    print(f"upstream: {args.upstream} as {args.user.split(':')[0]}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
