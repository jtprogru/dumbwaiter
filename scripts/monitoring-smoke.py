#!/usr/bin/env python3
"""Check the running local stack, including real datasource query execution.

Only sends a handful of requests to the demo site. Does not stop containers,
change dashboards or insert synthetic samples into metric storage.
"""
import base64
import json
import math
import os
from pathlib import Path
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
# Honor the same exported port/credential overrides as Compose.
APP = "http://127.0.0.1:" + os.getenv("APP_PORT", "8080")
VM = "http://127.0.0.1:" + os.getenv("VM_PORT", "8428")
VL = "http://127.0.0.1:" + os.getenv("VL_PORT", "9428")
GF = "http://127.0.0.1:" + os.getenv("GRAFANA_PORT", "3333")
AUTH = base64.b64encode((os.getenv("GRAFANA_ADMIN_USER", "admin") + ":" +
                        os.getenv("GRAFANA_ADMIN_PASSWORD", "admin")).encode()).decode()


def request(url, params=None, payload=None, headers=None):
    if params:
        url += "?" + urllib.parse.urlencode(params)
    hdr = dict(headers or {})
    if url.startswith(GF + "/"):
        hdr["Authorization"] = "Basic " + AUTH
    data = None
    if payload is not None:
        hdr["Content-Type"] = "application/json"
        data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, headers=hdr)
    with urllib.request.urlopen(req, timeout=15) as response:
        return response.read().decode()


def query(expr):
    data = json.loads(request(VM + "/api/v1/query", {"query": expr}))
    assert data["status"] == "success", data
    return data["data"]["result"]


def wait(label, check, timeout=120):
    deadline = time.monotonic() + timeout
    last = "not ready"
    while time.monotonic() < deadline:
        try:
            if check():
                print("PASS", label, flush=True)
                return
        except (urllib.error.URLError, TimeoutError, AssertionError, ValueError) as exc:
            last = str(exc)
        time.sleep(3)
    raise AssertionError(f"{label}: timed out ({last})")


def sample(path, status=200, host="localhost", method="GET"):
    req = urllib.request.Request(APP + path, headers={"Host": host}, method=method)
    try:
        response = urllib.request.urlopen(req, timeout=10)
    except urllib.error.HTTPError as exc:
        response = exc
    with response:
        body = response.read().decode()
        assert response.status == status, (path, response.status, body)
        return body


def interpolate(expr):
    for key, value in {"$__rate_interval": "1m", "$instance": ".*", "$host": ".*",
                       "$listener": ".*", "$bucket": ".*", "$profile": ".*", "$testid": ".*"}.items():
        expr = expr.replace(key, value)
    assert "$" not in expr, expr
    return expr


def all_targets_up():
    samples = query("up")
    expected = {"dumbwaiter", "known-object", "victoriametrics", "victorialogs",
                "vmagent", "vmalert", "vector", "blackbox", "grafana"}
    return ({s["metric"]["job"] for s in samples} == expected and
            all(s["value"][1] == "1" for s in samples))


def run():
    wait("Grafana ready", lambda: json.loads(request(GF + "/api/health"))["database"] == "ok")
    wait("all nine scrape targets up", all_targets_up)
    wait("known-object status AND body", lambda: any(s["value"][1] == "1" for s in query('probe_success{job="known-object"}')))
    for uid in ("victoriametrics", "victorialogs"):
        wait(uid + " datasource health", lambda uid=uid: json.loads(request(GF + "/api/datasources/uid/" + uid + "/health"))["status"] == "OK")

    token = uuid.uuid4().hex
    assert sample("/probe.txt?smoke=" + token) == "dumbwaiter-monitoring-ok\n"
    sample("/missing-" + token, 404)
    sample("/upstream-error.txt", 502)
    sample("/probe.txt", 405, method="POST")
    sample("/probe.txt", 404, host="scanner-" + token + ".invalid")
    print("PASS demo responses: 200 / 404 / 502 / 405 / unknown host")

    def log_arrived():
        lines = request(VL + "/select/logsql/query", {
            "query": '{service="dumbwaiter"} path:="/missing-' + token + '"', "limit": 5})
        return any(json.loads(line).get("host") == "localhost" and
                   json.loads(line).get("status") == "404" for line in lines.splitlines())
    wait("structured application log ingested with host and status", log_arrived)
    wait("error counter scraped", lambda: bool(query('dumbwaiter_http_requests_total{job="dumbwaiter",code="502",host="localhost"}')))
    wait("unknown hosts remain bounded", lambda: bool(query('dumbwaiter_http_requests_total{job="dumbwaiter",host="unknown"}')))
    assert not query('dumbwaiter_http_requests_total{job="dumbwaiter",host=~"scanner-.*"}')

    for name in ("process_open_fds", "process_max_fds"):
        wait(name + " exported by app", lambda name=name: any(
            float(s["value"][1]) > 0 for s in query(name + '{job="dumbwaiter"}')))

    count = 0
    for path in sorted((ROOT / "deploy/monitoring/grafana/dashboards").glob("*.json")):
        d = json.loads(path.read_text())
        wait("provisioned " + d["uid"], lambda d=d: json.loads(request(GF + "/api/dashboards/uid/" + d["uid"]))["dashboard"]["title"] == d["title"])
        for panel in d["panels"]:
            for target in panel["targets"]:
                expr = interpolate(target["expr"])
                if target["datasource"]["uid"] == "victoriametrics":
                    query(expr)  # Empty is valid for TLS and lazy event counters.
                else:
                    # Exercise the actual installed Grafana plugin, not just LogsQL parsing.
                    t = dict(target, expr=expr, intervalMs=15000, maxDataPoints=120)
                    response = json.loads(request(GF + "/api/ds/query", payload={
                        "from": str(int((time.time() - 600) * 1000)),
                        "to": str(int(time.time() * 1000)), "queries": [t]}))
                    result = response["results"][target["refId"]]
                    assert not result.get("error"), (panel["title"], result)
                    assert result.get("status", 200) == 200, result
                    assert result.get("frames"), (panel["title"], "plugin returned no frames")
                count += 1
    print(f"PASS {count} dashboard queries (including Grafana logs plugin)")

    # Probe host has healthy traffic and no 5xx series; a lazy numerator must
    # produce zero rather than an empty panel. No traffic must stay undefined.
    overview = json.loads((ROOT / "deploy/monitoring/grafana/dashboards/overview.json").read_text())
    expr = next(p for p in overview["panels"] if p["title"] == "HTTP 5xx ratio")["targets"][0]["expr"]
    def healthy_ratio_is_zero():
        healthy = query(interpolate(expr.replace("$host", "dumbwaiter")))
        return bool(healthy) and all(float(s["value"][1]) == 0 for s in healthy)
    wait("healthy traffic with lazy error series has zero error ratio", healthy_ratio_is_zero)
    missing = query(interpolate(expr.replace("$host", "never-seen-host")))
    assert not missing or all(math.isnan(float(s["value"][1])) for s in missing), missing
    print("PASS lazy error counter = zero with traffic; missing traffic != healthy")


if __name__ == "__main__":
    try:
        run()
    except (AssertionError, urllib.error.URLError, TimeoutError, ValueError, KeyError) as exc:
        raise SystemExit(f"FAIL: {exc}") from exc
