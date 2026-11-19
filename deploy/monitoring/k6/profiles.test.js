// Pure contract tests inside the same pinned k6 runtime; no HTTP traffic.
import { profile, profileNames, targetRate } from './profiles.js';

export const options = { vus: 1, iterations: 1 };
function assert(value, message) { if (!value) throw new Error(message); }
function duration(scenario) {
  return scenario.stages.reduce((sum, s) => sum + Number(s.duration.replace('ms', '')), 0) / 1000;
}
function rejects(fn, message) {
  let rejected = false;
  try { fn(); } catch (_) { rejected = true; }
  assert(rejected, message);
}

export default function () {
  const seconds = { smoke: 30, steady: 300, spike: 180, background: 72 * 3600 };
  for (const name of profileNames) {
    const s = profile(name);
    assert(s.executor === 'ramping-arrival-rate' && s.timeUnit === '1s', name + ': open RPS model');
    assert(duration(s) === seconds[name], name + ': duration');
    assert(s.preAllocatedVUs > 0 && s.maxVUs >= s.preAllocatedVUs, name + ': VU bounds');
    assert(s.stages.every(stage => stage.target >= 10 && stage.target <= 200), name + ': rate bounds');
    if (name !== 'background') assert(duration(profile(name, 0.1)) === seconds[name] / 10, name + ': time scale');
  }
  const bg = profile('background');
  assert(bg.stages.length === 432, '72 complete six-stage waves');
  for (let hour = 0; hour < 72; hour++) {
    const t = hour * 3600;
    assert(targetRate(bg, t) === 10, 'low at hour boundary');
    assert(targetRate(bg, t + 300) === 30, 'linear first ramp');
    assert(targetRate(bg, t + 1500) === 200, 'high plateau');
    assert(targetRate(bg, t + 3300) === 10, 'low plateau');
  }
  rejects(() => profile('typo'), 'unknown profile rejected');
  for (const scale of [0, -1, NaN, 2]) rejects(() => profile('smoke', scale), 'invalid time scale rejected');
  rejects(() => profile('background', 0.1), '72-hour duration cannot be shortened');
  console.log('PASS profile durations, 72h wave, rate bounds, VU bounds, interpolation, invalid input');
}
