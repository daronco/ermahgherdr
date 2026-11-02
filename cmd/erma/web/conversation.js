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
  const earlierBtn = document.getElementById('earlier');

  // The CSRF token issued by GET /conversation. Needed on every POST. The
  // conversation GET refreshes it; if we have none yet, state-changing POSTs
  // short-circuit.
  let csrf = '';
  let lastKnownState = ''; // for the permission banner
  let latestScreenPrompt = '';
  // The page holds the transcript from byte `start` to byte `cursor`: the
  // stream resumes at `cursor`, "load earlier" pages back from `start`.
  let cursor = 0;
  let start = 0;

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

  // Build a <li> for a tool-use step inside a folded group.
  function renderToolStep(t) {
    const row = document.createElement('li');
    row.className = 'tool-step';
    const name = document.createElement('span');
    name.className = 'tool-name';
    name.textContent = (t.tool && t.tool.name) || 'tool';
    row.appendChild(name);
    const args = document.createElement('span');
    args.className = 'tool-args';
    args.textContent = (t.tool && t.tool.args) || '';
    row.appendChild(args);
    return row;
  }

  // Build the folded group chip for a run of consecutive tool-call turns.
  // Expansion state is kept across live updates so a user who opened the chip
  // keeps seeing the steps as new ones get appended via SSE.
  function renderToolsGroup(steps, open) {
    const li = document.createElement('li');
    li.className = 'turn turn-tools';
    const details = document.createElement('details');
    details.className = 'tools-group';
    if (open) details.open = true;
    const summary = document.createElement('summary');
    summary.textContent = '🔧 ' + steps.length + (steps.length === 1 ? ' step' : ' steps');
    details.appendChild(summary);
    const list = document.createElement('ul');
    list.className = 'tool-steps';
    for (const s of steps) list.appendChild(renderToolStep(s));
    details.appendChild(list);
    li.appendChild(details);
    return li;
  }

  // Update an existing <li.turn.turn-tools> in place with a new step count and
  // extra step rows. Called when an SSE delta extends the trailing group so the
  // user's open/closed choice survives the update.
  function extendToolsGroup(li, newSteps) {
    const details = li.querySelector('details.tools-group');
    if (!details) return;
    const list = details.querySelector('ul.tool-steps');
    if (!list) return;
    const summary = details.querySelector('summary');
    const total = list.children.length + newSteps.length;
    if (summary) summary.textContent = '🔧 ' + total + (total === 1 ? ' step' : ' steps');
    const frag = document.createDocumentFragment();
    for (const s of newSteps) frag.appendChild(renderToolStep(s));
    list.appendChild(frag);
  }

  function renderTurn(t) {
    if (t.role === 'system') return renderSystemTurn(t);
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

    if (t.text) {
      const body = document.createElement('div');
      body.className = 'turn-body';
      if (t.role === 'assistant' && window.ermaRenderMarkdown) {
        window.ermaRenderMarkdown(t.text, body);
      } else {
        body.textContent = t.text;
      }
      li.appendChild(body);
    }
    // An assistant turn carrying prose can still have a tool call attached;
    // render it as a one-row tool line so the reader sees what Claude went on
    // to do after the prose.
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
    }
    return li;
  }

  // The system turn is a compact grey chip: the one-line summary is visible,
  // the full raw body sits inside a <details> and expands on tap.
  function renderSystemTurn(t) {
    const li = document.createElement('li');
    li.className = 'turn turn-system';
    const details = document.createElement('details');
    details.className = 'system-detail';
    const summary = document.createElement('summary');
    summary.className = 'system-summary';
    summary.textContent = t.summary || 'system message';
    details.appendChild(summary);
    if (t.text) {
      const pre = document.createElement('pre');
      pre.className = 'system-body';
      pre.textContent = t.text;
      details.appendChild(pre);
    }
    li.appendChild(details);
    return li;
  }

  // isToolTurn: an assistant turn whose body is nothing but a tool call. A
  // tool call carrying prose breaks the folding group so the user still sees
  // the text that explains the call.
  function isToolTurn(t) {
    return t && t.role === 'assistant' && t.tool && !(t.text && t.text.length > 0);
  }

  // FOLD_MIN: runs of this many consecutive tool-only assistant turns fold
  // into a single chip. A lone tool call stays as its own row — a one-step
  // "🔧 1 steps" chip would hide one line to save none.
  const FOLD_MIN = 2;

  // Walk a list of turns and build a fragment that folds runs of >=FOLD_MIN
  // consecutive tool-only assistant turns into a single collapsed group chip.
  // The FOLD_MIN check lives here so append/prepend share the same invariant.
  function foldTurnsIntoFragment(turns) {
    const frag = document.createDocumentFragment();
    let i = 0;
    while (i < turns.length) {
      const t = turns[i];
      if (isToolTurn(t)) {
        const run = [];
        while (i < turns.length && isToolTurn(turns[i])) {
          run.push(turns[i]);
          i++;
        }
        if (run.length >= FOLD_MIN) {
          frag.appendChild(renderToolsGroup(run, false));
        } else {
          for (const r of run) frag.appendChild(renderTurn(r));
        }
      } else {
        frag.appendChild(renderTurn(t));
        i++;
      }
    }
    return frag;
  }

  // Append a list of turns. When the incoming run starts with tool turns and
  // the DOM already ends with a tool group, extend that existing group instead
  // of starting a new one so the user's open/closed choice is kept.
  function appendTurns(turns) {
    if (!turns || turns.length === 0) return;
    const wasBottom = isAtBottom();
    let i = 0;
    const lastLi = turnsEl.lastElementChild;
    if (lastLi && lastLi.classList.contains('turn-tools') && isToolTurn(turns[0])) {
      const run = [];
      while (i < turns.length && isToolTurn(turns[i])) {
        run.push(turns[i]);
        i++;
      }
      extendToolsGroup(lastLi, run);
    }
    turnsEl.appendChild(foldTurnsIntoFragment(turns.slice(i)));
    emptyEl.hidden = turnsEl.children.length > 0;
    if (wasBottom) window.scrollTo({ top: document.body.scrollHeight });
  }

  function resetTurns(turns) {
    turnsEl.replaceChildren();
    appendTurns(turns);
    window.scrollTo({ top: document.body.scrollHeight });
  }

  // Older turns go above without moving what the reader is looking at. A run
  // that touches the top of the visible list is NOT merged with it, because
  // the caller is paging back and we want each page's groups to stay distinct.
  function prependTurns(turns) {
    if (!turns || turns.length === 0) return;
    const before = document.body.scrollHeight;
    turnsEl.insertBefore(foldTurnsIntoFragment(turns), turnsEl.firstChild);
    emptyEl.hidden = turnsEl.children.length > 0;
    window.scrollBy(0, document.body.scrollHeight - before);
  }

  function setStart(s) {
    start = s || 0;
    earlierBtn.hidden = start <= 0;
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
  async function callConversation(query) {
    const res = await fetch('/api/sessions/' + encodeURIComponent(SID) + '/conversation' + (query || ''));
    if (!res.ok) throw new Error('conversation ' + res.status);
    const body = await res.json();
    const token = res.headers.get('X-Erma-CSRF') || body.csrf || '';
    if (token) csrf = token;
    return body;
  }

  async function loadEarlier() {
    if (start <= 0) return;
    earlierBtn.disabled = true;
    try {
      const body = await callConversation('?before=' + start);
      prependTurns(body.turns);
      setStart(body.start);
    } catch (e) {
      console.error('earlier', e);
    } finally {
      earlierBtn.disabled = false;
    }
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

  // SSE connect: tails /stream from the cursor the page already has, routes
  // `conversation` | `delta` | `state`. A reconnect resumes by Last-Event-ID.
  function connect() {
    setConn('wait');
    const es = new EventSource('/api/sessions/' + encodeURIComponent(SID) + '/stream?after=' + cursor);
    es.addEventListener('open', () => setConn('live'));
    es.addEventListener('error', () => setConn('lost'));
    es.addEventListener('conversation', (ev) => {
      try {
        // The transcript was rotated or truncated: a fresh tail replaces the list.
        const payload = JSON.parse(ev.data);
        resetTurns(payload.turns);
        setStart(payload.start);
        cursor = payload.cursor;
      } catch (e) {
        console.error('conversation', e);
      }
    });
    es.addEventListener('delta', (ev) => {
      try {
        const payload = JSON.parse(ev.data);
        appendTurns(payload.turns);
        cursor = payload.cursor;
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
  earlierBtn.addEventListener('click', loadEarlier);
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

  // Chrome / Edge voice-to-textarea. Spec: no server-side STT, pt-BR default.
  // Dictation lands in the textarea; nothing is sent until the user taps Send.
  const SR = window.SpeechRecognition || window.webkitSpeechRecognition;
  if (SR) {
    const rec = new SR();
    rec.continuous = false;
    rec.interimResults = false;
    rec.lang = 'pt-BR'; // hardcoded for this project
    let listening = false;
    const setListening = (on) => {
      listening = on;
      voiceBtn.classList.toggle('listening', on);
      voiceBtn.setAttribute('aria-pressed', on ? 'true' : 'false');
    };
    rec.addEventListener('result', (ev) => {
      let text = '';
      for (const r of ev.results) text += r[0].transcript;
      if (!text) return;
      if (replyEl.value && !replyEl.value.endsWith(' ')) replyEl.value += ' ';
      replyEl.value += text;
      replyEl.focus();
    });
    rec.addEventListener('end', () => setListening(false));
    rec.addEventListener('error', (ev) => {
      // Common: 'not-allowed' (mic denied), 'no-speech', 'aborted'. Some browsers
      // skip `end` after `error`, so reset state here too.
      console.warn('voice', ev.error);
      setListening(false);
    });
    voiceBtn.hidden = false;
    voiceBtn.addEventListener('click', () => {
      if (listening) {
        rec.stop();
        return;
      }
      try {
        rec.start();
        setListening(true);
      } catch (_) {
        // start() throws InvalidStateError if already running — ignore.
        if (_.name !== 'InvalidStateError') throw _;
      }
    });
  }

  // Boot.
  (async function boot() {
    try {
      const body = await callConversation();
      resetTurns(body.turns);
      setStart(body.start);
      cursor = body.cursor;
      await callRead();
    } catch (e) {
      console.error('boot', e);
    }
    connect();
  })();

  // Tab bar wiring. The Terminal tab panel owns its own polling loop in
  // terminal.js; we only toggle visibility and emit a custom event so the
  // other script can start/stop its interval.
  const panels = {
    conversation: document.getElementById('tab-conversation'),
    terminal: document.getElementById('tab-terminal'),
  };
  const tabs = document.querySelectorAll('.tabs .tab');
  function activateTab(name) {
    for (const b of tabs) {
      const active = b.getAttribute('data-tab') === name;
      b.classList.toggle('active', active);
      b.setAttribute('aria-selected', active ? 'true' : 'false');
    }
    for (const [k, el] of Object.entries(panels)) {
      el.hidden = k !== name;
    }
    document.dispatchEvent(new CustomEvent('erma:tab', {
      detail: { name, sid: SID, csrf: () => csrf },
    }));
  }
  tabs.forEach((b) => b.addEventListener('click', () => activateTab(b.getAttribute('data-tab'))));
  // Expose a getter so terminal.js can grab the current CSRF without a
  // module import — the serve binary ships both scripts off the same root.
  window.ermaCSRF = () => csrf;
})();
