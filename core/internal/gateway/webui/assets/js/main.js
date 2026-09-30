// Boot: tab switching, the connection indicator, and the settings the user can
// change without reloading.

import { settings, saveSettings } from './api.js';
import { initPlayground, refreshModels, clearConversation } from './chat.js';
import { refreshFleet, renderFleetLatency } from './fleet.js';

const el = (id) => document.getElementById(id);

const conn = {
  dot: el('conn-dot'),
  text: el('conn-text'),
  latency: el('conn-latency'),
};

function setConn(state, text, latency = '') {
  conn.dot.dataset.state = state;
  conn.text.textContent = text;
  conn.latency.textContent = latency;
}

function initTabs() {
  const tabs = [...document.querySelectorAll('.tab')];
  const names = tabs.map((t) => t.dataset.tab);

  const show = (name, push = false) => {
    const target = names.includes(name) ? name : names[0];
    for (const tab of tabs) tab.classList.toggle('is-active', tab.dataset.tab === target);
    for (const panel of document.querySelectorAll('.panel')) {
      panel.classList.toggle('is-active', panel.dataset.panel === target);
    }
    if (target === 'fleet') refreshFleet();
    // The hash makes a tab linkable, which is the only way to send someone
    // to the cost view without telling them to click.
    if (push && location.hash.slice(1) !== target) {
      history.replaceState(null, '', '#' + target);
    }
    return target;
  };

  for (const tab of tabs) tab.addEventListener('click', () => show(tab.dataset.tab, true));
  window.addEventListener('hashchange', () => show(location.hash.slice(1)));
  show(location.hash.slice(1) || 'playground');
}

function initSettings() {
  const baseUrl = el('base-url');
  const apiKey = el('api-key');
  baseUrl.value = settings.baseUrl;
  apiKey.value = settings.apiKey;

  baseUrl.addEventListener('change', () => {
    saveSettings({ baseUrl: baseUrl.value.replace(/\/+$/, '') });
    probe();
  });
  apiKey.addEventListener('change', () => saveSettings({ apiKey: apiKey.value }));
}

let probeTimer = null;

/** Probe the gateway and update the indicator. Cheap enough to poll. */
async function probe() {
  clearTimeout(probeTimer);
  const started = performance.now();
  setConn('wait', 'connecting');
  try {
    const models = await refreshModels();
    const ms = Math.round(performance.now() - started);
    if (models.length) {
      setConn('ok', `${models.length} model${models.length === 1 ? '' : 's'}`, `${ms} ms`);
    } else {
      setConn('error', 'no models available', `${ms} ms`);
    }
  } catch {
    setConn('error', 'unreachable');
  }
}

async function initEdition() {
  const badge = el('edition-badge');
  const status = await refreshFleet();
  if (!status) return;
  badge.hidden = false;
  badge.dataset.edition = status.edition;
  badge.textContent = status.edition;
  if (status.customer) badge.title = status.customer;
}

initTabs();
initSettings();
initPlayground(renderFleetLatency);
el('btn-clear').addEventListener('dblclick', clearConversation);

probe();
initEdition();
probeTimer = setInterval(probe, 15000);
