#!/usr/bin/env python3
# pathfix: 18081 → 127.0.0.1:18085, 将老客户端路径 /messages 重写为 /v1/messages
import http.server, urllib.request, sys

UP = "http://127.0.0.1:18085"
HOP = {"connection","keep-alive","proxy-authenticate","proxy-authorization","te","trailers","transfer-encoding","upgrade"}

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def _forward(self):
        n = int(self.headers.get("content-length", "0") or 0)
        body = self.rfile.read(n) if n else None
        path = self.path
        if path == "/messages" or path.startswith("/messages?"):
            path = "/v1/messages" + (path.partition("?")[2] and "?" + path.partition("?")[2] or "")
        req = urllib.request.Request(UP + path, data=body, method=self.command)
        for k, v in self.headers.items():
            if k.lower() not in HOP and k.lower() != "host":
                req.add_header(k, v)
        try:
            with urllib.request.urlopen(req, timeout=1900) as r:
                data = r.read()
                self.send_response(r.status)
                ct = r.headers.get("content-type", "application/json")
                self.send_header("content-type", ct)
                self.send_header("content-length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
        except urllib.error.HTTPError as e:
            data = e.read()
            self.send_response(e.code)
            self.send_header("content-type", e.headers.get("content-type", "application/json"))
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        except Exception as e:
            data = ('{"error":{"message":"pathfix: %s","type":"api_error"}}' % e).encode()
            self.send_response(502)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
    do_POST = _forward
    do_GET = _forward
    def log_message(self, fmt, *a):
        sys.stderr.write("[pathfix] %s\n" % (fmt % a))
        sys.stderr.flush()

http.server.ThreadingHTTPServer(("0.0.0.0", 18081), H).serve_forever()
