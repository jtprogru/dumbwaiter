// Open-model profiles: one iteration = one HTTP request (no redirects/retries).
export const profileNames = ['smoke', 'steady', 'spike', 'background'];

export function profile(name, scale = 1) {
  if (!profileNames.includes(name)) throw new Error(`Unknown LOAD_PROFILE: ${name}`);
  if (!Number.isFinite(scale) || scale <= 0 || scale > 1) {
    throw new Error('LOAD_TIME_SCALE must be > 0 and <= 1');
  }
  if (name === 'background' && scale !== 1) {
    throw new Error('The background profile always runs for 72 hours; LOAD_TIME_SCALE must be 1');
  }
  const stage = (seconds, target) => ({ duration: `${Math.max(1, Math.round(seconds * scale * 1000))}ms`, target });
  const common = { executor: 'ramping-arrival-rate', timeUnit: '1s', startRate: 10,
    preAllocatedVUs: 100, maxVUs: 400, gracefulStop: '12s' };
  switch (name) {
    case 'smoke':
      return { ...common, stages: [stage(30, 10)] };
    case 'steady':
      return { ...common, startRate: 100, stages: [stage(300, 100)] };
    case 'spike':
      return { ...common, stages: [stage(30, 10), stage(30, 200), stage(60, 200),
        stage(30, 10), stage(30, 10)] };
    default: {
      // 72 x 1h. Linear ramps, plus explicit low/high plateaus.
      const stages = [];
      for (let hour = 0; hour < 72; hour++) {
        for (const target of [50, 200, 200, 80, 10, 10]) stages.push(stage(600, target));
      }
      return { ...common, stages };
    }
  }
}

// Same piecewise-linear schedule as k6, in requests/s. Exported as a Gauge
// alongside achieved RPS; dropped iterations must never look like lower demand.
export function targetRate(scenario, elapsedSeconds) {
  let previous = scenario.startRate;
  let elapsed = Math.max(0, elapsedSeconds);
  for (const stage of scenario.stages) {
    const seconds = Number(stage.duration.replace(/ms$/, '')) / 1000;
    if (elapsed <= seconds) return previous + (stage.target - previous) * elapsed / seconds;
    elapsed -= seconds;
    previous = stage.target;
  }
  return previous;
}
