#!/usr/bin/env python3
# AWS Lambda MicroVMs 冒烟测试用最小 HTTP 服务。
# 监听 0.0.0.0:8080,对每个 GET 返回一小段 JSON。绑 0.0.0.0(而非 127.0.0.1)以便 ingress 连通。
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 8080


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"status": "ok", "path": self.path}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        # 打到 stdout,方便在 CloudWatch 构建日志里看到访问记录
        print("%s - %s" % (self.address_string(), fmt % args), flush=True)


if __name__ == "__main__":
    httpd = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"Listening on port {PORT}", flush=True)
    httpd.serve_forever()
