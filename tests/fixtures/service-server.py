import socket
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

marker = sys.argv[1].encode()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Length", str(len(marker)))
        self.end_headers()
        self.wfile.write(marker)

    def log_message(self, *_):
        pass


def echo():
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as server:
        server.bind(("0.0.0.0", 8081))
        while True:
            data, peer = server.recvfrom(65535)
            server.sendto(marker + b":" + data, peer)


threading.Thread(target=echo, daemon=True).start()
ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
