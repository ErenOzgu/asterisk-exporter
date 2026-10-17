#!/usr/bin/env python3
"""Minimal SIP user agent for integration tests (Python standard library only).

It REGISTERs one endpoint at an Asterisk PJSIP server using MD5 digest
authentication and then answers the server's OPTIONS "qualify" pings with
200 OK, so the contact shows up as Reachable with a round-trip time.

    sipstub.py --user 100 --password pw100 --server 127.0.0.1:5060 --port 5099
"""
import argparse
import hashlib
import random
import re
import socket
import string
import sys
import time


def md5(s):
    return hashlib.md5(s.encode()).hexdigest()


def rand(n=10):
    return "".join(random.choice(string.ascii_lowercase + string.digits) for _ in range(n))


def build_register(args, cseq, call_id, from_tag, auth=None):
    uri = f"sip:{args.server}"
    lines = [
        f"REGISTER {uri} SIP/2.0",
        f"Via: SIP/2.0/UDP 127.0.0.1:{args.port};branch=z9hG4bK{rand()};rport",
        "Max-Forwards: 70",
        f"From: <sip:{args.user}@{args.server}>;tag={from_tag}",
        f"To: <sip:{args.user}@{args.server}>",
        f"Call-ID: {call_id}",
        f"CSeq: {cseq} REGISTER",
        f"Contact: <sip:{args.user}@127.0.0.1:{args.port}>",
        f"Expires: {args.expires}",
        "User-Agent: sipstub/1.0",
    ]
    if auth:
        lines.append(auth)
    lines += ["Content-Length: 0", "", ""]
    return "\r\n".join(lines).encode()


def digest(args, challenge):
    realm = re.search(r'realm="([^"]+)"', challenge).group(1)
    nonce = re.search(r'nonce="([^"]+)"', challenge).group(1)
    uri = f"sip:{args.server}"
    ha1 = md5(f"{args.user}:{realm}:{args.password}")
    ha2 = md5(f"REGISTER:{uri}")
    resp = md5(f"{ha1}:{nonce}:{ha2}")
    return (f'Authorization: Digest username="{args.user}", realm="{realm}", nonce="{nonce}", '
            f'uri="{uri}", response="{resp}", algorithm=MD5')


def parse(data):
    text = data.decode(errors="replace")
    head = text.split("\r\n\r\n", 1)[0].split("\r\n")
    headers = {}
    for line in head[1:]:
        if ":" in line:
            k, v = line.split(":", 1)
            headers.setdefault(k.strip().lower(), []).append(v.strip())
    return head[0], headers


def register(sock, args, server):
    call_id, from_tag = f"{rand(16)}@sipstub", rand(8)
    sock.sendto(build_register(args, 1, call_id, from_tag), server)
    first, headers = parse(sock.recvfrom(65535)[0])
    if " 401 " in first or " 407 " in first:
        challenges = headers.get("www-authenticate", []) + headers.get("proxy-authenticate", [])
        md5_challenge = next((c for c in challenges if "sha" not in c.lower()), challenges[0])
        sock.sendto(build_register(args, 2, call_id, from_tag, digest(args, md5_challenge)), server)
        first, headers = parse(sock.recvfrom(65535)[0])
    if " 200 " not in first:
        sys.exit(f"registration failed: {first}")
    print(f"registered {args.user}: {first}", flush=True)


def reply_ok(sock, data, addr):
    first, h = parse(data)
    to = h.get("to", [""])[0]
    if "tag=" not in to:
        to += f";tag={rand(8)}"
    resp = "\r\n".join([
        "SIP/2.0 200 OK",
        *[f"Via: {v}" for v in h.get("via", [])],
        f"From: {h.get('from', [''])[0]}",
        f"To: {to}",
        f"Call-ID: {h.get('call-id', [''])[0]}",
        f"CSeq: {h.get('cseq', [''])[0]}",
        "Allow: OPTIONS, INVITE, ACK, BYE, CANCEL",
        "Content-Length: 0", "", "",
    ]).encode()
    sock.sendto(resp, addr)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--user", required=True)
    ap.add_argument("--password", required=True)
    ap.add_argument("--server", default="127.0.0.1:5060")
    ap.add_argument("--port", type=int, required=True, help="local UDP port to listen on")
    ap.add_argument("--expires", type=int, default=120)
    args = ap.parse_args()

    host, port = args.server.rsplit(":", 1)
    server = (host, int(port))
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.bind(("127.0.0.1", args.port))
    sock.settimeout(5)
    register(sock, args, server)

    next_refresh = time.time() + args.expires / 2
    while True:
        try:
            data, addr = sock.recvfrom(65535)
        except socket.timeout:
            data = None
        if data and data.startswith(b"OPTIONS "):
            reply_ok(sock, data, addr)
        if time.time() >= next_refresh:
            register(sock, args, server)
            next_refresh = time.time() + args.expires / 2


if __name__ == "__main__":
    main()
