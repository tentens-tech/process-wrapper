"""Fake external services for the e2e tests.

HTTP on 127.0.0.1:18080:
  GET  /preempted        GCE metadata "instance/preempted" (long poll, answers in 1s): FALSE or TRUE
  POST /preempt          switch /preempted to TRUE
  POST /drain            envoy admin drain_listeners
  GET  /drains           number of /drain calls
  GET  /events           datagrams received by the metrics-server, one per line
UDP on 127.0.0.1:18125: metrics-server events (JSON datagrams)
"""
import http.server
import socket
import threading
import time

state = {"preempted": "FALSE", "drains": 0, "events": []}
lock = threading.Lock()


def udp_collector():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.bind(("127.0.0.1", 18125))
    while True:
        data, _ = s.recvfrom(65535)
        with lock:
            state["events"].append(data.decode(errors="replace"))


class Handler(http.server.BaseHTTPRequestHandler):
    def reply(self, body, code=200):
        data = body.encode()
        self.send_response(code)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path.startswith("/preempted"):
            if self.headers.get("Metadata-Flavor") != "Google":
                return self.reply("missing Metadata-Flavor header", 403)
            time.sleep(1)
            return self.reply(state["preempted"])
        if self.path == "/drains":
            return self.reply(str(state["drains"]))
        if self.path == "/events":
            with lock:
                return self.reply("\n".join(state["events"]) + "\n")
        self.reply("not found", 404)

    def do_POST(self):
        if self.path == "/preempt":
            state["preempted"] = "TRUE"
            return self.reply("ok")
        if self.path == "/drain":
            with lock:
                state["drains"] += 1
            return self.reply("OK")
        self.reply("not found", 404)

    def log_message(self, fmt, *args):
        print(fmt % args, flush=True)


threading.Thread(target=udp_collector, daemon=True).start()
http.server.ThreadingHTTPServer(("127.0.0.1", 18080), Handler).serve_forever()
