// The playground: model selection, transcript rendering, and the send/stop
// lifecycle for one in-flight completion.

import { settings, saveSettings, listModels, streamChat, chatBody, GatewayError } from './api.js';
import { consumeStream } from './sse.js';

const el = (id) => document.getElementById(id);

const ui = {
  transcript: el('transcript'),
  empty: el('transcript-empty'),
  model: el('model-select'),
  facts: el('model-facts'),
  prompt: el('prompt'),
  send: el('btn-send'),
  stop: el('btn-stop'),
  clear: el('btn-clear'),
  maxTokens: el('max-tokens'),
  temperature: el('temperature'),
};

let messages = [];
let inflight = null;

/** Recent request outcomes, so the Fleet tab has something to show. */
export const samples = [];

/* ── transcript ──────────────────────────────────────────────── */

function addMessage(role, text = '') {
  const wrap = document.createElement('div');
  wrap.className = 'msg ' + role;

  const who = document.createElement('div');
  who.className = 'who';
  who.textContent = role === 'user' ? 'you' : 'assistant';

  const body = document.createElement('div');
  body.className = 'text';
  body.textContent = text;

  wrap.append(who, body);
  ui.transcript.append(wrap);
  ui.empty.hidden = true;

  const cursor = document.createElement('span');
  cursor.className = 'cursor';
  return { wrap, body, cursor };
}

function addMeta(target, parts) {
  const meta = document.createElement('div');
  meta.className = 'meta';
  for (const [text, tone] of parts) {
    const span = document.createElement('span');
    span.textContent = text;
    if (tone) span.classList.add(tone);
    meta.append(span);
  }
  target.append(meta);
}

function toast(message) {
  const box = el('toast');
  box.textContent = message;
  box.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { box.hidden = true; }, 4000);
}

export function clearConversation() {
  messages = [];
  ui.transcript.replaceChildren(ui.empty);
  ui.empty.hidden = false;
  samples.length = 0;
  renderFleetLatency(samples);
}

/* ── models ──────────────────────────────────────────────────── */

export async function refreshModels() {
  try {
    const models = await listModels();
    if (!models.length) {
      ui.model.replaceChildren(new Option('no models registered', ''));
      return [];
    }
    const previous = ui.model.value;
    ui.model.replaceChildren(...models.map((m) => new Option(m.id, m.id)));
    if (models.some((m) => m.id === previous)) ui.model.value = previous;
    renderModelFacts(models.find((m) => m.id === ui.model.value));
    return models;
  } catch (err) {
    ui.model.replaceChildren(new Option('— unavailable —', ''));
    toast(err instanceof GatewayError ? err.message : String(err));
    return [];
  }
}

function renderModelFacts(model) {
  ui.facts.replaceChildren();
  if (!model) return;
  const rows = [
    ['id', model.id],
    ['owned by', model.owned_by || '—'],
  ];
  for (const [label, value] of rows) {
    const row = document.createElement('div');
    const dt = document.createElement('dt');
    dt.textContent = label;
    const dd = document.createElement('dd');
    dd.textContent = value;
    row.append(dt, dd);
    ui.facts.append(row);
  }
}

/* ── send ────────────────────────────────────────────────────── */

export function isBusy() { return inflight !== null; }

async function send() {
  const text = ui.prompt.value.trim();
  if (!text || inflight) return;

  const model = ui.model.value;
  if (!model) {
    toast('no model selected');
    return;
  }

  messages.push({ role: 'user', content: text });
  addMessage('user', text);
  ui.prompt.value = '';
  ui.prompt.style.height = 'auto';

  const view = addMessage('assistant');
  view.body.append(view.cursor);

  inflight = new AbortController();
  ui.send.hidden = true;
  ui.stop.hidden = false;
  ui.prompt.disabled = true;

  const started = performance.now();
  let firstByteAt = 0;

  try {
    const body = streamChat(
      chatBody({
        model,
        messages,
        maxTokens: Number(ui.maxTokens.value),
        temperature: Number(ui.temperature.value),
        stream: true,
      }),
      inflight.signal,
    );

    const res = await body;
    const { text: reply, usage, finishReason } = await consumeStream(res, (delta) => {
      if (!firstByteAt) firstByteAt = performance.now();
      view.body.append(document.createTextNode(delta));
      view.body.append(view.cursor);
      ui.transcript.scrollTop = ui.transcript.scrollHeight;
    });

    view.body.textContent = reply;
    messages.push({ role: 'assistant', content: reply });

    const total = performance.now() - started;
    samples.push({ model, ttft: firstByteAt - started, total, usage: !!usage });
    if (samples.length > 20) samples.shift();

    // Usage is what makes the request billable. Its absence is worth showing
    // rather than hiding: the gateway charged max_tokens for this one.
    const parts = [
      [`${Math.round(total)} ms`, null],
      [finishReason || 'stop', null],
    ];
    if (usage) {
      parts.push([`${usage.prompt_tokens}→${usage.completion_tokens} tok`, 'ok']);
      const cached = usage.prompt_tokens_details?.cached_tokens;
      if (cached) parts.push([`${cached} cached`, null]);
    } else {
      parts.push(['no usage reported · charged max_tokens', 'warn']);
    }
    addMeta(view.wrap, parts);
    renderFleetLatency(samples);
  } catch (err) {
    view.body.textContent = '';
    view.cursor.remove();

    if (err.name === 'AbortError') {
      view.body.textContent = '(stopped)';
      addMeta(view.wrap, [['aborted by user', 'warn']]);
    } else {
      const message = err instanceof GatewayError ? err.message : String(err);
      view.body.textContent = message;
      addMeta(view.wrap, [[message, 'err']]);
    }
  } finally {
    inflight = null;
    ui.send.hidden = false;
    ui.stop.hidden = true;
    ui.prompt.disabled = false;
    ui.prompt.focus();
  }
}

export function stop() { inflight?.abort(); }

/* ── wiring ─────────────────────────────────────────────────── */

export function initPlayground(renderFleetLatency) {
  ui.send.addEventListener('click', send);
  ui.stop.addEventListener('click', stop);
  ui.clear.addEventListener('click', clearConversation);
  ui.model.addEventListener('change', () => renderModelFacts({ id: ui.model.value, owned_by: '—' }));

  el('composer').addEventListener('submit', (e) => {
    e.preventDefault();
    send();
  });

  ui.prompt.addEventListener('input', () => {
    ui.prompt.style.height = 'auto';
    ui.prompt.style.height = Math.min(ui.prompt.scrollHeight, 180) + 'px';
  });

  ui.prompt.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  });

  for (const [input, out, fmt] of [
    [ui.maxTokens, el('max-tokens-out'), (v) => v],
    [ui.temperature, el('temperature-out'), (v) => Number(v).toFixed(1)],
  ]) {
    input.addEventListener('input', () => { out.textContent = fmt(input.value); });
  }
}

export { toast, saveSettings };
