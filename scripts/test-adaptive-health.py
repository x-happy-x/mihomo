"""Isolated end-to-end adaptive health test; all traffic stays on loopback."""
import argparse
import http.client
import http.server
import json
from pathlib import Path
import select
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request

state = {"mode": "normal"}

class QuietHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

class Origin(QuietHandler):
    protocol_version = "HTTP/1.1"
    def do_HEAD(self):
        self.respond(False)
    def do_GET(self):
        self.respond(True)
    def respond(self, body):
        code = 200
        content = b"x" * 4096
        if self.path == "/allowed":
            code, content = 204, b""
        if self.path == "/global":
            code = 204 if state["mode"] == "normal" else 503
            content = b""
        self.send_response(code)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(content)))
        self.end_headers()
        if body:
            self.wfile.write(content)

class Proxy(QuietHandler):
    protocol_version = "HTTP/1.1"
    def do_CONNECT(self):
        if state["mode"] != self.server.works_in:
            self.send_error(502)
            return
        host, port = self.path.rsplit(":", 1)
        if host != "127.0.0.1" or int(port) != self.server.origin_port:
            self.send_error(403)
            return
        with socket.create_connection((host, int(port)), timeout=2) as upstream:
            self.send_response(200)
            self.end_headers()
            self.wfile.flush()
            peers = [self.connection, upstream]
            while True:
                readable, _, _ = select.select(peers, [], [], 3)
                if not readable:
                    break
                for source in readable:
                    data = source.recv(65536)
                    if not data:
                        return
                    target = upstream if source is self.connection else self.connection
                    target.sendall(data)


def server(handler):
    result = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    result.daemon_threads = True
    threading.Thread(target=result.serve_forever, daemon=True).start()
    return result


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def run(binary):
    origin = server(Origin)
    normal, whitelist = server(Proxy), server(Proxy)
    for proxy, mode in [(normal, "normal"), (whitelist, "whitelist")]:
        proxy.works_in = mode
        proxy.origin_port = origin.server_port
    controller, mixed = free_port(), free_port()
    base = f"http://127.0.0.1:{origin.server_port}"
    config = {
        "mixed-port": mixed, "bind-address": "127.0.0.1", "allow-lan": False,
        "external-controller": f"127.0.0.1:{controller}", "log-level": "silent",
        "proxy-providers": {"lab": {
            "type": "inline", "payload": [
                {"name": "normal-node", "type": "http", "server": "127.0.0.1", "port": normal.server_port},
                {"name": "whitelist-node", "type": "http", "server": "127.0.0.1", "port": whitelist.server_port},
            ],
            "health-check": {
                "enable": True, "url": base + "/payload", "expected-status": "200",
                "interval": 1, "timeout": 500, "lazy": False,
                "adaptive": {
                    "enable": True, "confirmations": 2, "concurrency": 1,
                    "failure-threshold": 3, "recovery-threshold": 2,
                    "direct-allowed": [{"url": base + "/allowed", "expected-status": "204"}],
                    "direct-global": [{"url": base + "/global", "expected-status": "204"}],
                    "targets": [{"url": base + "/payload", "expected-status": "200", "min-bytes": 1024,
                        "timeout": 750, "content-type": "text/plain", "body-regex": "^x+$",
                        "body-not-regex": "(?i)unsupported_country|challenge"}],
                },
            },
        }},
        "proxy-groups": [{"name": "TEST", "type": "fallback", "use": ["lab"], "url": base + "/payload"}],
        "rules": ["MATCH,TEST"],
    }
    def api(path):
        with urllib.request.urlopen(f"http://127.0.0.1:{controller}" + path, timeout=2) as response:
            return json.load(response)
    def wait_for(predicate, label):
        deadline = time.monotonic() + 35
        last = None
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise AssertionError("core stopped unexpectedly")
            try:
                last = api("/providers/proxies/lab")["adaptive"]
                if predicate(last):
                    return last
            except (OSError, KeyError):
                pass
            time.sleep(0.25)
        raise AssertionError(f"timeout: {label}: {last}")
    with tempfile.TemporaryDirectory(prefix="mihomo-adaptive-") as directory:
        root = Path(directory)
        config_path = root / "config.yaml"
        config_path.write_text(json.dumps(config), encoding="utf-8")
        with (root / "core.log").open("w", encoding="utf-8") as output:
            process = subprocess.Popen([str(binary), "-d", str(root), "-f", str(config_path)],
                stdout=output, stderr=subprocess.STDOUT,
                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
            try:
                for mode, node in [("normal", "normal-node"), ("whitelist", "whitelist-node"), ("normal", "normal-node")]:
                    state["mode"] = mode
                    snapshot = wait_for(lambda s: s["mode"] == mode and s["observed"] == mode
                        and s["rankings"][mode][0]["name"] == node
                        and s["rankings"][mode][0]["stable"]
                        and s["results"].get(node, {}).get("mode") == mode
                        and s["results"][node].get("available", False), mode)
                    assert api("/proxies/TEST")["now"] == node
                    conn = http.client.HTTPConnection("127.0.0.1", mixed, timeout=3)
                    conn.request("GET", base + "/payload")
                    response = conn.getresponse()
                    assert response.status == 200 and len(response.read()) == 4096
                    conn.close()
                    print(json.dumps({"mode": mode, "selected": node,
                        "normalStable": [r["name"] for r in snapshot["rankings"]["normal"] if r["stable"]],
                        "whitelistStable": [r["name"] for r in snapshot["rankings"]["whitelist"] if r["stable"]],
                        "proxiedGetBytes": 4096}), flush=True)
                assert (root / "cache.db").exists(), "statistics cache missing"
                old_whitelist_checks = next(r["record"]["checks"] for r in snapshot["rankings"]["whitelist"] if r["name"] == "whitelist-node")
                # The final normal round has already persisted the whitelist
                # history; restarting must not erase that inactive-mode list.
                process.terminate()
                process.wait(timeout=5)
                process = subprocess.Popen([str(binary), "-d", str(root), "-f", str(config_path)],
                    stdout=output, stderr=subprocess.STDOUT,
                    creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
                restored = wait_for(lambda s: s["mode"] == "normal"
                    and s["results"].get("normal-node", {}).get("available", False)
                    and any(r["name"] == "whitelist-node" and r["record"]["checks"] >= old_whitelist_checks
                        for r in s["rankings"]["whitelist"]), "persistent inactive-mode history")
                assert api("/proxies/TEST")["now"] == "normal-node"
                print(json.dumps({"restart": "ok", "whitelistHistoryRetained": True}), flush=True)
            except Exception:
                output.flush()
                print((root / "core.log").read_text(encoding="utf-8", errors="replace"))
                raise
            finally:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
    for s in [normal, whitelist, origin]:
        s.shutdown()
        s.server_close()

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True, type=Path)
    args = parser.parse_args()
    run(args.binary.resolve(strict=True))
