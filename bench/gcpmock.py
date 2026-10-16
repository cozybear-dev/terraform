#!/usr/bin/env python3
"""Minimal in-memory mock of a few Google Cloud REST APIs, for benchmarking
Terraform with the real google provider without a GCP project.

Supports Pub/Sub topics and subscriptions, Secret Manager secrets and IAM
service accounts. usage: gcpmock.py PORT
"""
import json, re, sys, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

store = {}
lock = threading.Lock()
counter = [100000000000000000000]

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a):
        pass
    def reply(self, code, obj):
        data = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def notfound(self, path):
        self.reply(404, {"error": {"code": 404, "message": "not found: " + path, "status": "NOT_FOUND"}})
    def body(self):
        n = int(self.headers.get("Content-Length", "0") or 0)
        raw = self.rfile.read(n) if n else b""
        return json.loads(raw) if raw else {}
    def handle_any(self, method):
        u = urlparse(self.path)
        path = re.sub(r"^/v1/", "", u.path).strip("/")
        q = parse_qs(u.query)
        body = self.body() if method in ("PUT", "POST", "PATCH") else {}
        with lock:
            # Secret Manager: POST projects/P/secrets?secretId=X
            m = re.fullmatch(r"(projects/[^/]+)/secrets", path)
            if m and method == "POST":
                name = "%s/secrets/%s" % (m.group(1), q["secretId"][0])
                body["name"] = name
                body.setdefault("createTime", "2026-01-01T00:00:00Z")
                store[name] = body
                return self.reply(200, body)
            # IAM: POST projects/P/serviceAccounts
            m = re.fullmatch(r"projects/([^/]+)/serviceAccounts", path)
            if m and method == "POST":
                project = m.group(1)
                email = "%s@%s.iam.gserviceaccount.com" % (body["accountId"], project)
                counter[0] += 1
                sa = dict(body.get("serviceAccount", {}))
                sa.update({"name": "projects/%s/serviceAccounts/%s" % (project, email), "projectId": project,
                           "uniqueId": str(counter[0]), "email": email, "oauth2ClientId": str(counter[0])})
                store["projects/%s/serviceAccounts/%s" % (project, email)] = sa
                return self.reply(200, sa)
            if method == "PUT":
                body["name"] = path
                store[path] = body
                return self.reply(200, body)
            if method == "GET":
                key = re.sub(r"^projects/-/", "projects/tfperf/", path)
                if key in store:
                    return self.reply(200, store[key])
                return self.notfound(path)
            if method == "PATCH":
                if path not in store:
                    return self.notfound(path)
                for k, v in body.items():
                    if isinstance(v, dict) and k in ("topic", "subscription", "serviceAccount", "secret"):
                        store[path].update(v)
                    elif k not in ("updateMask",):
                        store[path][k] = v
                return self.reply(200, store[path])
            if method == "DELETE":
                store.pop(path, None)
                return self.reply(200, {})
        self.reply(400, {"error": {"code": 400, "message": "unsupported %s %s" % (method, path)}})
    def do_GET(self): self.handle_any("GET")
    def do_PUT(self): self.handle_any("PUT")
    def do_POST(self): self.handle_any("POST")
    def do_PATCH(self): self.handle_any("PATCH")
    def do_DELETE(self): self.handle_any("DELETE")

ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
