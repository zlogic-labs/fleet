// The Fleet tab: what the gateway is connected to, what this build is
// licensed for, and what the recent requests actually cost in latency.

import { fleetStatus } from './api.js';
import { samples } from './chat.js';

const el = (id) => document.getElementById(id);

const CAP_LABELS = {
  sso:          'SSO / directory login',
  audit:        'Immutable audit trail',
  rbac:         'Per-role permissions',
  policy:       'Request policy engine',
  multicluster: 'Multi-cluster control',
  ha:           'Replicated control plane',
  cost_export:  'Scheduled cost export',
};

function row(name, value) {
  const wrap = document.createElement('div');
  wrap.className = 'row';
  const grow = document.createElement('span');
  grow.className = 'grow name';
  grow.textContent = name;
  const val = document.createElement('span');
  val.className = 'val';
  val.textContent = value;
  wrap.append(grow, val);
  return wrap;
}

function pill(text, on) {
  const span = document.createElement('span');
  span.className = 'pill ' + (on ? 'on' : 'off');
  span.textContent = text;
  return span;
}

function meter(fraction) {
  const bar = document.createElement('span');
  bar.className = 'bar';
  const fill = document.createElement('i');
  fill.style.width = Math.min(100, Math.round(fraction * 100)) + '%';
  bar.append(fill);
  return bar;
}

export function renderEndpoints(status) {
  const host = el('fleet-endpoints');
  host.replaceChildren();

  if (!status.endpoints?.length) {
    host.append(row('no endpoints registered', ''));
    return;
  }

  for (const ep of status.endpoints) {
    const wrap = document.createElement('div');
    wrap.className = 'row';

    const name = document.createElement('span');
    name.className = 'grow name';
    name.textContent = ep.model;
    name.title = ep.base_url;

    const healthy = ep.healthy !== false;
    wrap.append(name, meter(ep.kv_cache_used ?? 0), pill(healthy ? 'healthy' : 'down', healthy));
    host.append(wrap);

    const detail = document.createElement('div');
    detail.className = 'row';
    detail.style.paddingLeft = '0';
    detail.style.fontSize = '11px';
    detail.append(row('  ' + ep.base_url, `${ep.replicas ?? 1}× · queue ${ep.queue_depth ?? 0}`));
    host.append(detail);
  }
}

export function renderCapabilities(status) {
  const host = el('fleet-caps');
  host.replaceChildren();

  const granted = new Set(status.capabilities ?? []);
  for (const key of Object.keys(CAP_LABELS)) {
    const wrap = document.createElement('div');
    wrap.className = 'row';
    const label = document.createElement('span');
    label.className = 'grow';
    label.textContent = CAP_LABELS[key];
    wrap.append(label, pill(granted.has(key) ? 'granted' : 'community', granted.has(key)));
    host.append(wrap);
  }

  // Show the upsell only when there is something to upsell.
  const card = el('card-upgrade');
  const missing = Object.keys(CAP_LABELS).filter((k) => !granted.has(k));
  card.hidden = missing.length === 0 || status.edition === 'enterprise';
  if (!card.hidden) {
    el('upgrade-copy').textContent =
      `${missing.length} of ${Object.keys(CAP_LABELS).length} capabilities are not in this ` +
      `edition. They are gated in the gateway, not hidden in the browser, so removing the ` +
      `badge does not grant them.`;
  }
}

/**
 * The latency table is deliberately about TTFT versus decode time. A wide gap
 * between them on short completions is the signature of KV-cache misses, which
 * is the thing prefix-affinity routing exists to prevent.
 *
 * The browser's own samples are preferred because they carry the timing the
 * user actually experienced. A freshly opened tab has none, so it falls back
 * to what the gateway recorded, and the card says which source it used rather
 * than presenting the second as if it were the first.
 */
export function renderFleetLatency(local, recent = []) {
  const host = el('fleet-latency');
  host.replaceChildren();

  const source = el('latency-source');
  if (local.length) {
    source.textContent = 'From requests made in this browser.';
  } else if (recent.length) {
    source.textContent = 'From this browser\'s first request onward — the rows below are the gateway\'s own record.';
  } else {
    source.textContent = 'No requests yet.';
  }

  if (local.length) {
    for (const s of local.slice(-8).reverse()) {
      const ttft = s.ttft ? Math.round(s.ttft) : 0;
      const total = Math.round(s.total);
      host.append(row(s.model,
        `${ttft} ms ttft · ${Math.max(0, total - ttft)} ms decode · ${s.usage ? 'billed' : 'estimated'}`));
    }
    return;
  }

  for (const r of recent.slice(0, 8)) {
    const ttft = r.ttft_ms || 0;
    host.append(row(r.model,
      `${ttft} ms ttft · ${Math.max(0, r.duration_ms - ttft)} ms decode · ${r.usage_known ? 'billed' : 'estimated'}`));
  }
}

export async function refreshFleet() {
  try {
    const status = await fleetStatus();
    renderEndpoints(status);
    renderCapabilities(status);
    renderFleetLatency(samples, status.recent ?? []);
    return status;
  } catch {
    el('fleet-endpoints').replaceChildren(row('gateway did not answer /fleet/status', ''));
    return null;
  }
}
