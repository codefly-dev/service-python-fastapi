"""Container-local probe. No application imports, credentials, redirects or retries."""
import http.client
import json
import os
import socket
import signal
import ssl
import sys
import time


def check(config):
    deadline = time.monotonic() + config["timeout"]
    connection = socket.create_connection(("127.0.0.1", config["port"]), config["timeout"])
    try:
        if config["secured"]:
            # This is a probe of this container's own listener. Authenticate it
            # against the exact mounted serving certificate before any HTTP
            # bytes are sent; a CA/hostname chosen for external callers is not
            # a loopback identity. Never accept an arbitrary TLS certificate.
            with open(os.environ["UVICORN_SSL_CERTFILE"], encoding="ascii") as source:
                pem = source.read(1048576)
            end = "-----END CERTIFICATE-----"
            leaf = pem[:pem.index(end) + len(end)]
            expected = ssl.PEM_cert_to_DER_cert(leaf)
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
            context.minimum_version = ssl.TLSVersion.TLSv1_2
            context.check_hostname = False
            context.verify_mode = ssl.CERT_NONE
            connection.settimeout(max(0.001, deadline - time.monotonic()))
            connection = context.wrap_socket(connection, server_hostname=None)
            if connection.getpeercert(binary_form=True) != expected:
                raise ValueError("serving certificate does not match mounted certificate")
        if config["kind"] == "transport":
            return
        connection.settimeout(max(0.001, deadline - time.monotonic()))
        request = "GET " + config["path"] + " HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
        connection.sendall(request.encode("ascii"))
        response = http.client.HTTPResponse(connection)
        response.begin()
        if not any(low <= response.status <= high for low, high in config["statuses"]):
            raise ValueError("status predicate failed")
        if config["body"]:
            data = bytearray()
            expected = config["body"].encode("utf-8")
            while len(data) <= 1048576:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError("probe deadline")
                connection.settimeout(remaining)
                chunk = response.read1(min(65536, 1048577 - len(data)))
                if not chunk:
                    break
                data.extend(chunk)
                if expected in data:
                    return
            raise ValueError("body predicate failed or exceeded probe bound")
    finally:
        connection.close()


if __name__ == "__main__":
    try:
        config = json.loads(sys.argv[1])
        def deadline_expired(_signum, _frame):
            raise TimeoutError("probe deadline")
        signal.signal(signal.SIGALRM, deadline_expired)
        signal.setitimer(signal.ITIMER_REAL, config["timeout"])
        check(config)
        signal.setitimer(signal.ITIMER_REAL, 0)
    except Exception:
        # Paths, response contents and TLS material are deliberately not logged.
        sys.exit(1)
