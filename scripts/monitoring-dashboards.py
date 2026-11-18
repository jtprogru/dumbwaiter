#!/usr/bin/env python3
"""Generate deterministic Grafana JSON. --check rejects stale committed dashboards."""
import argparse
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "deploy/monitoring/grafana/dashboards"
VM = {"type": "prometheus", "uid": "victoriametrics"}
VL = {"type": "victoriametrics-logs-datasource", "uid": "victorialogs"}
APP = 'job="dumbwaiter",instance=~"$instance"'
HTTP = APP + ',host=~"$host",listener=~"$listener"'
S3 = APP + ',bucket=~"$bucket"'
WINDOW = "$__rate_interval"


def metric(name, labels=APP):
    return name + "{" + labels + "}"


def rate(name, labels=APP, by=""):
    grouping = " by (" + by + ")" if by else ""
    return "sum" + grouping + "(rate(" + metric(name, labels) + "[" + WINDOW + "]))"


def ratio(name, labels, bad, by="host"):
    # Lazy counters: zero only when the matching total exists. Idle is 0/0,
    # NOT 100% availability. Missing telemetry is never replaced with vector(0).
    total = rate(name, labels, by)
    errors = rate(name, labels + "," + bad, by)
    return "((" + errors + ") or (0 * " + total + ")) / (" + total + " > 0)"


def quantile(name, labels, q, by=""):
    return f'histogram_quantile({q}, {rate(name + "_bucket", labels, "le" + ("," + by if by else ""))})'


def variable(name, query):
    return {"name": name, "type": "query", "datasource": VM,
            "query": {"query": query, "refId": name}, "definition": query,
            "refresh": 2, "sort": 1, "multi": True, "includeAll": True,
            "allValue": ".*", "current": {"text": "All", "value": "$__all"}, "options": []}


def dashboard(slug, title, description, filters=()):
    variables = [variable("instance", 'label_values(up{job="dumbwaiter"}, instance)')]
    for field in filters:
        name = "dumbwaiter_s3_requests_total" if field == "bucket" else "dumbwaiter_http_requests_total"
        variables.append(variable(field, f'label_values({metric(name)}, {field})'))
    return {"uid": "dumbwaiter-" + slug, "title": "dumbwaiter · " + title,
            "description": description, "schemaVersion": 41, "version": 1,
            "editable": False, "tags": ["dumbwaiter"], "timezone": "browser",
            "refresh": "15s", "time": {"from": "now-30m", "to": "now"}, "graphTooltip": 1,
            "links": [{"title": "dumbwaiter", "type": "dashboards", "tags": ["dumbwaiter"],
                       "keepTime": True, "includeVars": True}],
            "templating": {"list": variables}, "panels": []}


def panel(d, title, exprs, unit="short", description="", kind="timeseries", datasource=VM, health=False):
    n = len(d["panels"])
    if isinstance(exprs, str):
        exprs = [(exprs, "")]
    targets = []
    for i, (expr, legend) in enumerate(exprs):
        target = {"refId": chr(65 + i), "expr": expr, "legendFormat": legend,
                  "datasource": datasource, "editorMode": "code"}
        if datasource == VL:
            target["queryType"] = "instant" if kind == "logs" else "statsRange"
            target["maxLines"] = 200
        elif kind in ("stat", "table"):
            target["instant"] = True
            target["format"] = "table" if kind == "table" else "time_series"
        targets.append(target)
    options = {"legend": {"displayMode": "table", "placement": "bottom", "calcs": ["lastNotNull"]},
               "tooltip": {"mode": "multi"}}
    if kind == "stat":
        options = {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value", "graphMode": "none"}
    if kind == "logs":
        options = {"showTime": True, "showLabels": False, "enableLogDetails": True,
                   "sortOrder": "Descending", "wrapLogMessage": True}
    d["panels"].append({"id": n + 1, "title": title, "description": description,
                        "type": kind, "datasource": datasource, "targets": targets,
                        "gridPos": {"x": (n % 2) * 12, "y": (n // 2) * 8, "w": 12, "h": 8},
                        "fieldConfig": {"defaults": {"unit": unit, "noValue": "No data",
                                         "custom": {"spanNulls": False}}, "overrides": []},
                        "options": options})
    if health:
        d["panels"][-1]["fieldConfig"]["defaults"].update({
            "color": {"mode": "thresholds"},
            "thresholds": {"mode": "absolute", "steps": [
                {"color": "red", "value": None}, {"color": "green", "value": 1}]},
            "mappings": [{"type": "value", "options": {
                "0": {"text": "FAILED", "color": "red"},
                "1": {"text": "OK", "color": "green"}}},
                {"type": "special", "options": {"match": "null", "result": {
                    "text": "No data", "color": "gray"}}}]
        })


def build():
    dashboards = {}
    d = dashboard("overview", "Overview / RED", "Start with scrape health and the known-object probe. HTTP status and transfer outcomes are independent; this is not an SLO dashboard.", ("host", "listener"))
    panel(d, "Application scrape health (1 = up)", metric("up"), kind="stat", health=True)
    panel(d, "Known object (1 = expected status AND body)", 'probe_success{job="known-object"}', kind="stat", health=True,
          description="Local synthetic through public HTTP, independent from /healthz. Not external Internet availability. Probe traffic is included in request metrics.")
    panel(d, "Traffic by host and status", rate("dumbwaiter_http_requests_total", HTTP, "host,code"), "reqps")
    panel(d, "HTTP 5xx ratio", ratio("dumbwaiter_http_requests_total", HTTP, 'code=~"5.."'), "percentunit",
          "Not download success: a committed 200 can abort. Idle and missing data remain No data; no preexisting error series is treated as zero only alongside a request total.")
    panel(d, "Full response latency", [(quantile("dumbwaiter_http_request_duration_seconds", HTTP, q), label) for q, label in [(0.5, "p50"), (0.95, "p95"), (0.99, "p99")]], "s",
          "Includes body and slow clients, all statuses/outcomes. Aggregate buckets before quantiles. Highest finite bucket is 30s; a clipped p99 is not a measured tail bound.")
    panel(d, "Locally failed transfers (excluding cancellations)", ratio("dumbwaiter_http_response_outcomes_total", HTTP, 'outcome=~"aborted|write_error|panic"'), "percentunit",
          "Separate from 5xx; do NOT add the two ratios (overlapping requests). Cancellations are shown below, not classified as server failures.")
    panel(d, "Response outcomes", rate("dumbwaiter_http_response_outcomes_total", HTTP, "outcome"), "reqps")
    panel(d, "In-flight per listener (all hosts)", metric("dumbwaiter_http_requests_in_flight", APP + ',listener=~"$listener"'), description="Not admission-slot occupancy. Includes requests rejected before admission; no host label or capacity metric exists.")
    panel(d, "Response body throughput", rate("dumbwaiter_http_response_bytes_total", HTTP, "host"), "Bps")
    panel(d, "Admission rejections (all listeners)", rate("dumbwaiter_admission_rejected_total", APP + ',host=~"$host"', "host,scope"), "reqps", "Lazy counter; absent before first rejection. Scope identifies site/global limit; no listener label exists.")
    panel(d, "Lookup budget exceeded (all listeners)", rate("dumbwaiter_lookup_timeouts_total", APP + ',host=~"$host"', "host"), "reqps", "Lazy counter. Can include multiple fast lookups rather than one slow S3 call.")
    panel(d, "HTTP protocol and method", rate("dumbwaiter_http_requests_total", HTTP, "proto,method"), "reqps")
    panel(d, "Firing operational alerts (whole stack)", 'ALERTS{alertstate="firing"}', kind="table", description="No data normally means no firing alerts; check monitoring health before interpreting it. Local notifications are disabled.")
    panel(d, "Responses beyond histogram's finite tail", '(' + rate("dumbwaiter_http_request_duration_seconds_count", HTTP) + ' - ' + rate("dumbwaiter_http_request_duration_seconds_bucket", HTTP + ',le="30"') + ') / (' + rate("dumbwaiter_http_request_duration_seconds_count", HTTP) + ' > 0)', "percentunit", "Fraction of full responses >30s. Useful when p99 sticks to 30s.")
    dashboards["overview"] = d

    d = dashboard("s3", "S3 dependency", "A request may perform multiple S3 lookups. S3 timing stops at headers, not at body completion.", ("bucket",))
    panel(d, "S3 traffic", rate("dumbwaiter_s3_requests_total", S3, "bucket,method,code"), "reqps")
    panel(d, "S3 failures: 5xx / transport / timeout", ratio("dumbwaiter_s3_requests_total", S3, 'code=~"5..|error|timeout"', "bucket"), "percentunit", "Denominator includes all S3 attempts including client cancellations; this is diagnostic, not an SLO.")
    panel(d, "Time to S3 response headers", [(quantile("dumbwaiter_s3_request_duration_seconds", S3, q, "bucket"), label + " {{bucket}}") for q, label in [(0.5, "p50"), (0.95, "p95"), (0.99, "p99")]], "s", "Includes failed attempts except canceled. Streaming body read failures are not in this metric.")
    panel(d, "Cancellations and timeouts", rate("dumbwaiter_s3_requests_total", S3 + ',code=~"canceled|timeout"', "bucket,code"), "reqps")
    panel(d, "S3 misses / forbidden", rate("dumbwaiter_s3_requests_total", S3 + ',code=~"403|404"', "bucket,code"), "reqps", "404 and 403 may be expected during directory/SPA lookup. Not all are incidents.")
    panel(d, "AccessDenied masked as 404", rate("dumbwaiter_s3_access_denied_masked_total", S3, "bucket"), "reqps", "Cannot distinguish missing keys without ListBucket permission from an IAM regression. Use known-object checks. Absent until the first event.")
    panel(d, "Known-object probe latency", 'probe_duration_seconds{job="known-object"}', "s")
    panel(d, "Known-object HTTP status", 'probe_http_status_code{job="known-object"}', kind="stat", description="Local fixture only; probes are not filtered by the bucket variable. Add a known object for every real backend.")
    dashboards["s3"] = d

    d = dashboard("runtime", "Runtime / TLS", "USE-style resource signals. CPU is cores, not percent of a container quota. TLS panels are empty in the default plain-HTTP stack.")
    panel(d, "Build", metric("dumbwaiter_build_info"), kind="table")
    panel(d, "Uptime", 'time() - ' + metric("process_start_time_seconds"), "s", kind="stat")
    panel(d, "CPU used", rate("process_cpu_seconds_total", APP, "instance"), "short", "CPU seconds per second = cores. No quota is exported by this metric.")
    panel(d, "Memory", [(metric("process_resident_memory_bytes"), "RSS {{instance}}"), (metric("go_memstats_heap_alloc_bytes"), "Go heap {{instance}}")], "bytes")
    panel(d, "Goroutines / threads", [(metric("go_goroutines"), "goroutines {{instance}}"), (metric("go_threads"), "threads {{instance}}")])
    panel(d, "File descriptor utilization", metric("process_open_fds") + ' / ' + metric("process_max_fds"), "percentunit", "OS-dependent collector, not necessarily available on all hosts.")
    panel(d, "GC pause p99", quantile("go_gc_pauses_seconds", APP, 0.99, "instance"), "s", "Uses histogram, not an average of per-process summary quantiles. No GC events => no quantile.")
    panel(d, "Allocation rate", rate("go_memstats_alloc_bytes_total", APP, "instance"), "Bps")
    panel(d, "Certificate time remaining", '(' + metric("dumbwaiter_tls_certificate_expiration_timestamp_seconds") + ' - time()) / 86400', "d", "Empty when TLS terminates elsewhere. Monitor edge certificates at that termination point.")
    panel(d, "TLS reloads", [(rate("dumbwaiter_tls_certificate_reloads_total", APP, "instance"), "success {{instance}}"), (rate("dumbwaiter_tls_certificate_reload_errors_total", APP, "instance"), "failed {{instance}}")], "ops")
    dashboards["runtime"] = d

    d = dashboard("logs", "Logs", "Structured access and application logs. Expand log details for host/path/status/outcome/duration_ms. Raw paths and remote IPs stay log fields, never metric labels.")
    d["templating"]["list"] = []
    base = '{service="dumbwaiter",environment="local"}'
    panel(d, "Log rate by level", base + ' | stats by (level) rate()', "ops", datasource=VL)
    panel(d, "Access log outcomes", base + ' _msg:="request" | stats by (outcome) rate()', "reqps", datasource=VL)
    panel(d, "Errors / warnings / failed transfers", base + ' (level:in("ERROR", "WARN") OR outcome:in("aborted", "write_error", "panic"))', kind="logs", datasource=VL, description="Access lines are INFO even on HTTP errors. Filter by status/outcome rather than level alone.")
    panel(d, "HTTP errors (including INFO access lines)", base + ' _msg:="request" status:~"^[45]"', kind="logs", datasource=VL)
    panel(d, "All application logs", base, kind="logs", datasource=VL)
    panel(d, "Vector read vs delivered", [(rate("vector_component_received_events_total", 'job="vector",component_id="dumbwaiter"'), "read"), (rate("vector_component_sent_events_total", 'job="vector",component_id="victorialogs"'), "delivered")], "ops", "Different windows/batching can briefly diverge; not a durable exactly-once audit trail.")
    dashboards["logs"] = d

    d = dashboard("monitoring", "Monitoring health", "Monitoring must monitor itself. No data is not green. Persistent queues are bounded; 7-day retention is not a disk size cap.")
    d["templating"]["list"] = []
    panel(d, "Scrape health (1 = up)", 'up', kind="table")
    panel(d, "Scrape duration", 'scrape_duration_seconds', "s")
    panel(d, "Scraped series", 'scrape_samples_scraped')
    panel(d, "Monitoring process RSS", 'process_resident_memory_bytes{job=~"victoriametrics|victorialogs|vmagent|vmalert"}', "bytes")
    panel(d, "vmagent pending remote-write bytes", 'sum(vmagent_remotewrite_pending_data_bytes)', "bytes")
    panel(d, "Vector pipeline errors / drops", [(rate("vector_component_errors_total", 'job="vector"', "component_id,error_type"), "error {{component_id}} {{error_type}}"), (rate("vector_component_discarded_events_total", 'job="vector"', "component_id"), "discarded {{component_id}}")], "ops", "Counters may be absent before first event. Check scrape health.")
    panel(d, "VictoriaLogs ingestion", rate("vl_rows_ingested_total", 'job="victorialogs"'), "ops")
    panel(d, "VictoriaLogs dropped rows", rate("vl_rows_dropped_total", 'job="victorialogs"', "reason"), "ops")
    panel(d, "Rule evaluation errors", rate("vmalert_execution_errors_total", 'job="vmalert"'), "ops")
    panel(d, "Firing alerts", 'ALERTS{alertstate="firing"}', kind="table")
    dashboards["monitoring"] = d

    return dashboards


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    OUT.mkdir(parents=True, exist_ok=True)
    for name, d in build().items():
        path = OUT / (name + ".json")
        content = json.dumps(d, indent=2, ensure_ascii=False) + "\n"
        if args.check:
            if not path.exists() or path.read_text() != content:
                raise SystemExit(f"Stale dashboard: {path}; run python3 scripts/monitoring-dashboards.py")
        else:
            path.write_text(content)
    print("Dashboard JSON is up to date")


if __name__ == "__main__":
    main()
