import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';
import { Counter, Gauge, Rate } from 'k6/metrics';
import { profile, targetRate } from './profiles.js';

const name = __ENV.LOAD_PROFILE || 'smoke';
const scenario = profile(name, Number(__ENV.LOAD_TIME_SCALE || '1'));
const testID = __ENV.TEST_ID;
if (!testID) throw new Error('TEST_ID is required (the container entrypoint generates it)');
const baseURL = (__ENV.BASE_URL || 'http://dumbwaiter:8080').replace(/\/$/, '');
const host = __ENV.HOST_HEADER || 'loadtest.local';
const target = new Gauge('load_target_rps');
const unexpected = new Rate('load_unexpected_response');
// As in scripts/loadtest.js: a balance for the end-of-test threshold, NOT a
// monotonic event counter. Do not use rate() on its remote-write series.
const unfinished = new Counter('load_unfinished_iterations');

export const options = {
  discardResponseBodies: true,
  maxRedirects: 0,
  setupTimeout: '75s',
  tags: { job: 'k6', service: 'dumbwaiter', profile: name, testid: testID },
  // Omit url, IP, VU IDs and iteration IDs: request names are a bounded set.
  systemTags: ['name', 'method', 'status', 'scenario', 'expected_response'],
  scenarios: { traffic: scenario },
  thresholds: {
    iterations: ['count>0'],
    load_unfinished_iterations: ['count==0'],
    load_unexpected_response: ['rate<0.001'],
    'http_req_failed{scenario:traffic}': ['rate<0.001'],
    dropped_iterations: ['count==0'],
    // Test-fixture budgets, not a production SLO. No abortOnFail: the 72h
    // source keeps running during fault experiments, then reports failure.
    // Trend thresholds retain raw samples; only short interactive tests use them.
    ...(name === 'background' ? {} : {
      'http_req_duration{scenario:traffic}': ['p(95)<500', 'p(99)<1000'],
    }),
  },
};

const cases = [
  { method: 'GET', path: '/probe.txt', name: 'known-object', status: 200, body: 'dumbwaiter-monitoring-ok\n' },
  { method: 'HEAD', path: '/probe.txt', name: 'head-object', status: 200 },
  { method: 'GET', path: '/missing-loadtest.txt', name: 'expected-miss', status: 404, body: 'not found\n' },
  { method: 'GET', path: '/upstream-error.txt', name: 'expected-upstream-error', status: 502, body: 'bad gateway\n' },
];
const callbacks = cases.map(c => http.expectedStatuses(c.status));

export function setup() {
  // Compose service_started is not application readiness. Retry only here,
  // before the timed scenario, so each measured iteration is exactly one call.
  for (let i = 0; i < 30; i++) {
    const r = http.get(baseURL + '/probe.txt', { headers: { Host: host }, timeout: '2s',
      responseType: 'text', tags: { name: 'setup-probe' } });
    if (r.status === 200 && r.body === cases[0].body) return;
    sleep(0.25);
  }
  throw new Error('Local fixture did not become ready (status AND body); check BASE_URL/HOST_HEADER');
}

export default function () {
  unfinished.add(1);
  target.add(targetRate(scenario, (Date.now() - exec.scenario.startTime) / 1000));
  // Exact 85/10/4/1 mix per 100 scheduled iterations, deterministic across VUs.
  const n = exec.scenario.iterationInTest % 100;
  const index = n < 85 ? 0 : n < 95 ? 1 : n < 99 ? 2 : 3;
  const c = cases[index];
  const r = http.request(c.method, baseURL + c.path, null, {
    headers: { Host: host }, timeout: '10s', responseType: c.body ? 'text' : 'none',
    tags: { name: c.name }, responseCallback: callbacks[index],
  });
  const statusOK = r.status === c.status;
  const bodyOK = c.body === undefined || r.body === c.body;
  check(r, { 'expected status': () => statusOK, 'complete expected body': () => bodyOK });
  unexpected.add(!statusOK || !bodyOK);
  unfinished.add(-1);
}
