// erma PWA — conversation view. Opens a transcript live over SSE, lets the
// user send text / quick keys / the read ack. Everything is sid-scoped: the
// id comes from ?id=<sid> and is sent back to the server unmodified.
//
// Security: the CSRF token travels in a response header AND the JSON body of
// GET /conversation. We store the latest one and attach it to every POST.

(function () {
  'use strict';

  const params = new URLSearchParams(location.search);
  const SID = params.get('id') || '';
  if (!SID) {
    document.body.innerHTML = '<p class="empty">missing id</p>';
    return;
  }

  const titleEl = document.getElementById('conv-title');
  const badgeEl = document.getElementById('conv-badge');
  const connEl = document.getElementById('conv-conn');
  const turnsEl = document.getElementById('turns');
  const emptyEl = document.getElementById('conv-empty');
  const permEl = document.getElementById('perm-banner');
  const permBodyEl = document.getElementById('perm-body');
  const replyEl = document.getElementById('reply');
  const sendBtn = document.getElementById('send');
  const voiceBtn = document.getElementById('voice');

  // The CSRF token issued by GET /conversation. Needed on every POST. The
  // conversation GET refreshes it; if we have none yet, state-changing POSTs
  // short-circuit.
  let csrf = '';
  let lastKnownState = ''; // for the permission banner
  let latestScreenPrompt = '';
  // Set once the HTTP conversation has rendered. The SSE stream also fires a
  // `conversation` event on connect; if it lands within the grace window below,
  // we skip the second resetTurns to avoid a visible flash on slow links.
  let httpRenderedAt = 0;
  const BOOT_SSE_GRACE_MS = 1500;

  function setConn(state) {
    connEl.className = 'status ' + state;
    connEl.textContent = state === 'live' ? '●' : state === 'lost' ? '✕' : '·';
  }

  function fmtTime(ts) {
    if (!ts) return '';
    const d = new Date(ts);
    if (isNaN(d)) return '';
    const hh = String(d.getHours()).padStart(2, '0');
    const mm = String(d.getMinutes()).padStart(2, '0');
    return `${hh}:${mm}`;
  }

  function isAtBottom() {
    // Auto-scroll only when the user hasn't scrolled up to read older turns.
    const slop = 80;
    return window.innerHeight + window.scrollY >= document.body.scrollHeight - slop;
  }

  function renderTurn(t) {
    const li = document.createElement('li');
    li.className = 'turn turn-' + (t.role || 'user');
    const head = document.createElement('div');
    head.className = 'turn-head';
    const role = document.createElement('span');
    role.className = 'turn-role';
    role.textContent = t.role === 'assistant' ? 'claude' : t.role === 'tool_result' ? 'result' : 'you';
    head.appendChild(role);
    const ts = document.createElement('span');
    ts.className = 'turn-ts';
    ts.textContent = fmtTime(t.ts);
    head.appendChild(ts);
    li.appendChild(head);

    if (t.tool) {
      const row = document.createElement('div');
      row.className = 'turn-tool';
      const name = document.createElement('span');
      name.className = 'tool-name';
      name.textContent = t.tool.name || 'tool';
      row.appendChild(name);
      const args = document.createElement('span');
      args.className = 'tool-args';
      args.textContent = t.tool.args || '';
      row.appendChild(args);
      li.appendChild(row);
      if (t.text) {
        const detail = document.createElement('details');
        detail.className = 'tool-detail';
        const summary = document.createElement('summary');
        summary.textContent = 'thinking / preface';
        detail.appendChild(summary);
        const pre = document.createElement('pre');
        pre.textContent = t.text;
        detail.appendChild(pre);
        li.appendChild(detail);
      }
    } else if (t.text) {
      const body = document.createElement('div');
      body.className = 'turn-body';
      body.textContent = t.text;
      li.appendChild(body);
    }
    return li;
  }

  function appendTurns(turns) {
    if (!turns || turns.length === 0) return;
    const wasBottom = isAtBottom();
    const frag = document.createDocumentFragment();
    for (const t of turns) frag.appendChild(renderTurn(t));
    turnsEl.appendChild(frag);
    emptyEl.hidden = turnsEl.children.length > 0;
    if (wasBottom) window.scrollTo({ top: document.body.scrollHeight });
  }

  function resetTurns(turns) {
    turnsEl.innerHTML = '';
    appendTurns(turns);
    window.scrollTo({ top: document.body.scrollHeight });
  }

  function applyState(row) {
    if (!row) return;
    lastKnownState = row.state || '';
    // row.prompt is set only when a dialog is up; keep the last known value when
    // the server omits it mid-stream, so the banner does not blank between ticks.
    if (typeof row.prompt === 'string' && row.prompt) {
      latestScreenPrompt = row.prompt;
    } else if (row.state !== 'urgent') {
      latestScreenPrompt = '';
    }
    document.body.classList.remove('state-idle', 'state-done', 'state-proc', 'state-urgent');
    if (row.state) document.body.classList.add('state-' + row.state);
    titleEl.textContent = row.name || row.pane || SID;
    const parts = [];
    if (row.state === 'urgent') parts.push('PERM');
    else if (row.state === 'done') parts.push('DONE');
    else if (row.state === 'proc') parts.push('…');
    if (row.sub > 0) parts.push('⤷' + row.sub);
    badgeEl.textContent = parts.join(' · ');
    renderPermBanner();
  }

  // The permission banner shows whenever the row is `urgent`. The server parses
  // the pane's Screen and sends the question text as `row.prompt`; if that is
  // missing (older server, or the Screen hasn't been read yet), fall back to a
  // generic nudge so the keys are still prominent.
  function renderPermBanner() {
    if (lastKnownState === 'urgent') {
      permEl.hidden = false;
      permBodyEl.textContent = latestScreenPrompt || 'Claude is waiting on an answer. Pick a choice or press Esc.';
    } else {
      permEl.hidden = true;
    }
  }

  // Fetch helpers.
  async function callConversation() {
    const res = await fetch('/api/sessions/' + encodeURIComponent(SID) + '/conversation');
    if (!res.ok) throw new Error('conversation ' + res.status);
    const body = await res.json();
    const token = res.headers.get('X-Erma-CSRF') || body.csrf || '';
    if (token) csrf = token;
    return body;
  }

  async function callRead() {
    if (!csrf) return;
    await fetch('/api/sessions/' + encodeURIComponent(SID) + '/read', {
      method: 'POST',
      headers: { 'X-Erma-CSRF': csrf },
    });
  }

  async function callKey(key) {
    if (!csrf) return;
    const res = await fetch('/api/sessions/' + encodeURIComponent(SID) + '/key', {
      method: 'POST',
      headers: { 'X-Erma-CSRF': csrf, 'Content-Type': 'application/json' },
      body: JSON.stringify({ key }),
    });
    if (!res.ok) console.warn('key', key, res.status);
  }

  async function callText(text) {
    if (!csrf || !text) return;
    sendBtn.disabled = true;
    try {
      const res = await fetch('/api/sessions/' + encodeURIComponent(SID) + '/text', {
        method: 'POST',
        headers: { 'X-Erma-CSRF': csrf, 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      });
      if (!res.ok) {
        console.warn('text', res.status);
      } else {
        replyEl.value = '';
      }
    } finally {
      sendBtn.disabled = false;
      replyEl.focus();
    }
  }

  // SSE connect: tails /stream, routes `conversation` | `delta` | `state`.
  function connect() {
    setConn('wait');
    const es = new EventSource('/api/sessions/' + encodeURIComponent(SID) + '/stream');
    es.addEventListener('open', () => setConn('live'));
    es.addEventListener('error', () => setConn('lost'));
    es.addEventListener('conversation', (ev) => {
      try {
        const payload = JSON.parse(ev.data);
        // On boot the HTTP conversation already rendered; the stream's first
        // frame carries the same transcript, so re-rendering it just causes a
        // flicker. Skip within the grace window; honor it on reconnect.
        if (httpRenderedAt && Date.now() - httpRenderedAt < BOOT_SSE_GRACE_MS) {
          httpRenderedAt = 0;
          return;
        }
        resetTurns(payload.turns);
      } catch (e) {
        console.error('conversation', e);
      }
    });
    es.addEventListener('delta', (ev) => {
      try {
        const payload = JSON.parse(ev.data);
        appendTurns(payload.turns);
      } catch (e) {
        console.error('delta', e);
      }
    });
    es.addEventListener('state', (ev) => {
      try {
        applyState(JSON.parse(ev.data));
      } catch (e) {
        console.error('state', e);
      }
    });
  }

  // Wiring.
  sendBtn.addEventListener('click', () => callText(replyEl.value));
  replyEl.addEventListener('keydown', (ev) => {
    // Enter sends; Shift+Enter inserts a newline. The textarea stays editable
    // even while a POST is in flight — the agent working does not block typing.
    if (ev.key === 'Enter' && !ev.shiftKey) {
      ev.preventDefault();
      callText(replyEl.value);
    }
  });
  document.querySelectorAll('button.qk').forEach((b) => {
    b.addEventListener('click', () => {
      const k = b.getAttribute('data-key');
      if (k) callKey(k);
    });
  });

  // Chrome / Edge voice-to-textarea. Spec: no server-side STT.
  const SR = window.SpeechRecognition || window.webkitSpeechRecognition;
  if (SR) {
    const rec = new SR();
    rec.continuous = false;
    rec.interimResults = false;
    rec.lang = navigator.language || 'en-US';
    let listening = false;
    rec.addEventListener('result', (ev) => {
      let text = '';
      for (const r of ev.results) text += r[0].transcript;
      if (replyEl.value && !replyEl.value.endsWith(' ')) replyEl.value += ' ';
      replyEl.value += text;
      replyEl.focus();
    });
    rec.addEventListener('end', () => {
      listening = false;
      voiceBtn.classList.remove('listening');
    });
    voiceBtn.hidden = false;
    voiceBtn.addEventListener('click', () => {
      if (listening) {
        rec.stop();
        return;
      }
      try {
        rec.start();
        listening = true;
        voiceBtn.classList.add('listening');
      } catch (_) {
        // start() throws if already running — ignore.
      }
    });
  }

  // Boot.
  (async function boot() {
    try {
      const body = await callConversation();
      resetTurns(body.turns);
      httpRenderedAt = Date.now();
      await callRead();
    } catch (e) {
      console.error('boot', e);
    }
    connect();
  })();
})();
