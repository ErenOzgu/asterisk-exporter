#!/usr/bin/env python3
"""Checks the Grafana dashboard JSON.

  check-dashboard.py rules DASHBOARD.json OUT.yml   write every query as a recording rule,
                                                    so `promtool check rules` validates the PromQL
  check-dashboard.py live  DASHBOARD.json PROM_URL  run every query against a live Prometheus;
                                                    each must succeed and, except for the
                                                    conference panel, return data
"""
import json
import sys
import urllib.parse
import urllib.request


def queries(path):
    dash = json.load(open(path))
    ids = [p["id"] for p in dash["panels"]]
    assert len(ids) == len(set(ids)), "duplicate panel ids"
    for p in dash["panels"]:
        for t in p.get("targets", []):
            yield p["title"], t["expr"].replace("$instance", ".*")


def main():
    mode, path, arg = sys.argv[1:4]
    if mode == "rules":
        lines = ["groups:", "  - name: dashboard", "    rules:"]
        for i, (_, expr) in enumerate(queries(path)):
            lines.append(f"      - record: dashboard:query{i}")
            lines.append("        expr: " + json.dumps(expr))
        open(arg, "w").write("\n".join(lines) + "\n")
    elif mode == "live":
        bad = 0
        for title, expr in queries(path):
            url = arg.rstrip("/") + "/api/v1/query?" + urllib.parse.urlencode({"query": expr})
            res = json.load(urllib.request.urlopen(url, timeout=10))
            n = len(res["data"]["result"]) if res["status"] == "success" else -1
            optional = "confbridge_parties" in expr
            ok = n >= 0 and (n > 0 or optional)
            print(("OK   " if ok else "FAIL ") + f"[{title}] {n} series: {expr[:80]}")
            bad += not ok
        sys.exit(1 if bad else 0)
    else:
        sys.exit(__doc__)


main()
