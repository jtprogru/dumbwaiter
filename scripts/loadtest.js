// One request per iteration. Run only against a dedicated test environment.
import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate } from 'k6/metrics';

const baseURL = __ENV.BASE_URL;
if (!baseURL) throw new Error('BASE_URL is required');
const path = __ENV.OBJECT_PATH || '/';
if (!path.startsWith('/') || path.startsWith('//')) {
  throw new Error('OBJECT_PATH must start with a single slash');
}

function positiveInt(name, fallback) {
  const value = Number(__ENV[name] || fallback);
  if (!Number.isInteger(value) || value < 1) throw new Error(`${name} must be a positive integer`);
  return value;
}

const expectedStatus = positiveInt('EXPECTED_STATUS', 200);
const expectedProtocol = __ENV.EXPECTED_PROTOCOL;
const method = __ENV.METHOD || 'GET';
const requestTimeout = __ENV.REQUEST_TIMEOUT || '35s';
if (!['GET', 'HEAD'].includes(method)) throw new Error('METHOD must be GET or HEAD');
const unexpected = new Rate('unexpected_response');
// A balance, not a monotonic counter: +1 at start, -1 after all checks.
// Interrupted iterations leave a positive balance in the final thresholds.
const unfinished = new Counter('unfinished_iterations');
const headers = {};
if (__ENV.HOST_HEADER) headers.Host = __ENV.HOST_HEADER;
if (__ENV.RANGE) headers.Range = __ENV.RANGE;
if (__ENV.IF_NONE_MATCH) headers['If-None-Match'] = __ENV.IF_NONE_MATCH;

export const options = {
  discardResponseBodies: true,
  maxRedirects: 0,
  scenarios: {
    objects: {
      executor: 'constant-arrival-rate',
      rate: positiveInt('RATE', 10),
      timeUnit: '1s',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: positiveInt('PREALLOCATED_VUS', 20),
      maxVUs: positiveInt('MAX_VUS', 200),
      gracefulStop: __ENV.GRACEFUL_STOP || requestTimeout,
    },
  },
  // Illustrative thresholds: replace with the SLO for the chosen object class.
  thresholds: {
    iterations: ['count>0'],
    unfinished_iterations: ['count==0'],
    unexpected_response: ['rate<0.001'],
    http_req_failed: ['rate<0.001'],
    http_req_duration: [
      `p(95)<${positiveInt('P95_MS', 200)}`,
      `p(99)<${positiveInt('P99_MS', 500)}`,
    ],
    dropped_iterations: ['count==0'],
  },
};

http.setResponseCallback(http.expectedStatuses(expectedStatus));

export default function () {
  unfinished.add(1);
  const response = http.request(method, baseURL.replace(/\/$/, '') + path, null, {
    headers,
    timeout: requestTimeout,
    tags: { name: __ENV.CASE || 'object' },
  });
  const statusOK = response.status === expectedStatus;
  const protocolOK = !expectedProtocol || response.proto === expectedProtocol;
  check(response, {
    'expected status': () => statusOK,
    'expected protocol': () => protocolOK,
  });
  unexpected.add(!statusOK || !protocolOK);
  unfinished.add(-1);
}
