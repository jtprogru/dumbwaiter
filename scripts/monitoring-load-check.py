#!/usr/bin/env python3
"""Short E2E checks of all manual k6 profiles and their native-histogram output.

Requires make obs-up. Never starts the 72-hour source; it inspects its options.
"""
import json
import os
from pathlib import Path
import shlex
import subprocess
import time
import urllib.parse
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
COMPOSE = shlex.split(os.getenv("DOCKER", "docker")) + ["compose", "-f", str(ROOT / "deploy/monitoring/compose.yaml")]
VM = "http://127.0.0.1:" + os.getenv("VM_PORT", "8428")


def query(expr):
    url = VM + "/api/v1/query?" + urllib.parse.urlencode({"query": expr})
    with urllib.request.urlopen(url, timeout=10) as response:
        data = json.load(response)
    assert data["status"] == "success", data
    return data["data"]["result"]


def wait_samples(expr, predicate=bool):
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        samples = query(expr)
        if predicate(samples):
            return samples
        time.sleep(2)
    raise AssertionError("No remote-write data: " + expr)


def run():
    inspected = subprocess.run(COMPOSE + ["run", "--rm", "--no-deps", "-e", "LOAD_PROFILE=background",
        "-e", "LOAD_TIME_SCALE=1", "k6-run", "inspect", "--include-system-env-vars", "/scripts/load.js"],
        check=True, capture_output=True, text=True)
    options = json.loads(inspected.stdout)
    assert not any("duration" in key for key in options["thresholds"]), "72h Trend thresholds retain raw samples"
    stages = options["scenarios"]["traffic"]["stages"]
    assert len(stages) == 432, "background must be 72 full waves"
    print("PASS background options: 72h wave, no raw-sample Trend thresholds", flush=True)

    for profile, scale in [("smoke", "1"), ("steady", "0.1"), ("spike", "0.1")]:
        run_id = "check-" + uuid.uuid4().hex
        command = COMPOSE + ["run", "--rm", "--no-deps", "-e", "LOAD_PROFILE=" + profile,
            "-e", "LOAD_TIME_SCALE=" + scale, "-e", "TEST_ID=" + run_id, "k6-run"]
        result = subprocess.run(command, capture_output=True, text=True, timeout=150)
        if result.returncode or "level=error" in result.stderr:
            raise AssertionError(f"{profile} failed:\n{result.stdout}\n{result.stderr}")
        labels = '{job="k6",testid="' + run_id + '"}'
        # last_over_time sees the final pushed batch of a finite run.
        expected_requests = {"smoke": 300, "steady": 3000, "spike": 1890}[profile]
        traffic_labels = '{job="k6",testid="' + run_id + '",scenario="traffic"}'
        wait_samples('sum(last_over_time(k6_http_reqs_total' + traffic_labels + '[5m]))',
                     lambda samples: bool(samples) and abs(float(samples[0]["value"][1]) - expected_requests) <= 2)
        for metric in ["k6_load_unexpected_response_rate", "k6_load_unfinished_iterations_total"]:
            samples = wait_samples("last_over_time(" + metric + labels + "[5m])")
            assert all(float(s["value"][1]) == 0 for s in samples), (metric, samples)
        targets = wait_samples("max_over_time(k6_load_target_rps" + labels + "[5m])")
        expected = {"smoke": 10, "steady": 100, "spike": 200}[profile]
        assert max(float(s["value"][1]) for s in targets) == expected, targets
        buckets = wait_samples("last_over_time(k6_http_req_duration_seconds_bucket" + labels + "[5m])")
        assert all("vmrange" in s["metric"] for s in buckets), "VM did not convert native histogram"
        # Nonempty, finite quantile: correct metric name, unit and bucket grouping.
        latency = wait_samples('histogram_quantile(0.99, sum by (vmrange)(rate(k6_http_req_duration_seconds_bucket' + labels + '[1m])))')
        assert all(0 <= float(s["value"][1]) < 1 for s in latency), latency
        statuses = wait_samples('last_over_time(k6_http_reqs_total{job="k6",testid="' + run_id + '",scenario="traffic"}[5m])')
        assert {s["metric"]["status"] for s in statuses} == {"200", "404", "502"}, statuses
        requests = sum(float(s["value"][1]) for s in statuses)
        assert abs(requests - expected_requests) <= 2, (profile, requests, expected_requests)
        print(f"PASS {profile}: thresholds, request mix, target RPS, native histogram, remote write ({run_id})", flush=True)


if __name__ == "__main__":
    try:
        run()
    except (AssertionError, subprocess.SubprocessError) as exc:
        raise SystemExit(f"FAIL: {exc}") from exc
