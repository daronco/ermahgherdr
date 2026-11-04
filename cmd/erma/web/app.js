// erma PWA front-end. Read-only: this page observes the bar's state over
// SSE and renders it. No mutation calls today — see the issue roadmap.
//
// The palette and semantics mirror cmd/erma/main.go. If you change colors or
// state tags there, update styles.css here.

(function () {
  'use strict';

  const listEl = document.getElementById('list');
  const emptyEl = document.getElementById('empty');
  const daiPinEl = document.getElementById('dai-pin');
  const liveDotEl = document.getElementById('live-dot');

  const otherGroupEl = document.getElementById('other-group');
  const otherToggleEl = document.getElementById('other-toggle');
  const otherListEl = document.getElementById('other-list');
  const otherCountEl = document.getElementById('other-count');
  const newBtn = document.getElementById('new-session');
  const emptyNewBtn = document.getElementById('empty-new');
  const sheetEl = document.getElementById('new-sheet');
  const recentListEl = document.getElementById('recent-list');
  const recentEmptyEl = document.getElementById('recent-empty');
  const newForm = document.getElementById('new-form');
  const newDirInput = document.getElementById('new-dir');
  const newErrorEl = document.getElementById('new-error');
  const newSubmitBtn = document.getElementById('new-submit');

  // Group containers, in render order. QUIET folds idle + paused.
  const GROUPS = [
    { key: 'you', list: document.getElementById('list-you'), section: document.getElementById('group-you'), count: document.getElementById('count-you') },
    { key: 'done', list: document.getElementById('list-done'), section: document.getElementById('group-done'), count: document.getElementById('count-done') },
    { key: 'work', list: document.getElementById('list-work'), section: document.getElementById('group-work'), count: document.getElementById('count-work') },
    { key: 'quiet', list: document.getElementById('list-quiet'), section: document.getElementById('group-quiet'), count: document.getElementById('count-quiet') },
  ];

  // State → glyph. The purple ring, green check, amber spinner, idle circle
  // and paused bars mirror the bar's state semantics.
  const GLYPH = { urgent: '◉', done: '✓', proc: '◠', idle: '○', paused: '‖' };

  // nodes: pane → {li, parts} cache so we never recreate a row for a no-op
  // update. The server emits full snapshots, so the diff is "which rows live
  // where" and "which fields moved".
  const nodes = new Map();

  function fmtAge(sinceSec) {
    if (!sinceSec) return '';
    const d = Math.max(0, Math.floor(Date.now() / 1000) - sinceSec);
    if (d < 60) return d + 's';
    if (d < 3600) return Math.floor(d / 60) + 'm';
    const h = Math.floor(d / 3600);
    const m = Math.floor((d % 3600) / 60);
    return d < 36000 ? `${h}h${String(m).padStart(2, '0')}` : h + 'h';
  }

  // paneLabel: "%42" → "pane 42". Anything that does not match "%NN" falls
  // back to the raw value so a misshapen pane id still renders readably.
  function paneLabel(pane) {
    if (!pane) return '';
    const m = /^%(\d+)$/.exec(pane);
    return m ? 'pane ' + m[1] : pane;
  }

  // groupKey: partition one session into exactly one of the four buckets.
  // Paused folds into QUIET regardless of the underlying state.
  function groupKey(s) {
    if (s.marks === 'paused') return 'quiet';
    if (s.state === 'urgent') return 'you';
    if (s.state === 'done') return 'done';
    if (s.state === 'proc') return 'work';
    return 'quiet';
  }

  function ensureRow(s) {
    let n = nodes.get(s.pane);
    if (n) return n;
    const li = document.createElement('li');
    li.className = 'row';
    const glyph = document.createElement('span');
    glyph.className = 'glyph';
    const body = document.createElement('div');
    body.className = 'body';
    const head = document.createElement('div');
    head.className = 'row-head';
    const name = document.createElement('span');
    name.className = 'name';
    const age = document.createElement('span');
    age.className = 'age';
    head.appendChild(name);
    head.appendChild(age);
    const sub = document.createElement('div');
    sub.className = 'sub';
    const meta = document.createElement('div');
    meta.className = 'meta';
    const metaPane = document.createElement('span');
    metaPane.className = 'meta-pane';
    const ctxBar = document.createElement('span');
    ctxBar.className = 'ctx';
    const ctxFill = document.createElement('span');
    ctxFill.className = 'fill';
    ctxBar.appendChild(ctxFill);
    const metaCtx = document.createElement('span');
    metaCtx.className = 'meta-ctx';
    meta.appendChild(metaPane);
    meta.appendChild(ctxBar);
    meta.appendChild(metaCtx);
    body.appendChild(head);
    body.appendChild(sub);
    body.appendChild(meta);
    const actions = document.createElement('div');
    actions.className = 'row-actions';
    body.appendChild(actions);
    li.appendChild(glyph);
    li.appendChild(body);
    // Row tap opens the conversation view. The whole row is one hit target
    // so taps on name or meta count too.
    li.addEventListener('click', (ev) => {
      // Let the Review button handle its own navigation.
      if (ev.target && ev.target.closest && ev.target.closest('.row-review')) return;
      const id = li.dataset.id;
      if (id) location.href = '/conversation.html?id=' + encodeURIComponent(id);
    });
    n = { li, glyph, name, age, sub, metaPane, ctxBar, ctxFill, metaCtx, actions };
    nodes.set(s.pane, n);
    return n;
  }

  // subLine: build the state-specific second line as a sequence of
  // {text, cls?} spans. Returns null if nothing worth showing.
  function subLine(s) {
    if (s.dai) return null;
    if (s.state === 'urgent') {
      const what = (s.prompt || '').trim();
      if (!what) return [{ text: 'Permission', cls: 'sub-you' }];
      return [
        { text: 'Permission', cls: 'sub-you' },
        { text: ' · ' + what },
      ];
    }
    if (s.state === 'proc') {
      // apiSession has no tool/args field today, so we render the plain state
      // word. The spec allows this fallback until the stream carries live
      // tool-call info.
      return [{ text: 'Working', cls: 'sub-work' }];
    }
    if (s.state === 'done') {
      // The last-reply preview is deferred — the stream doesn't carry it yet.
      return [{ text: 'Done' }];
    }
    // Quiet: idle or paused.
    if (s.marks === 'paused') {
      const parts = ['Paused'];
      const last = plainState(s.state);
      if (last && last !== 'Paused') parts.push(last.toLowerCase());
      const pane = paneLabel(s.pane);
      if (pane) parts.push(pane);
      const ctx = parseInt(s.ctx, 10);
      if (!isNaN(ctx) && ctx >= 0) parts.push('ctx ' + ctx + '%');
      return [{ text: parts.join(' · ') }];
    }
    // Plain idle.
    const parts = ['Idle', 'no turns yet'];
    const pane = paneLabel(s.pane);
    if (pane) parts.push(pane);
    return [{ text: parts.join(' · ') }];
  }

  function plainState(st) {
    switch (st) {
      case 'urgent': return 'Needs you';
      case 'done': return 'Done';
      case 'proc': return 'Working';
      default: return 'Idle';
    }
  }

  function renderSub(n, spans) {
    n.sub.innerHTML = '';
    if (!spans || spans.length === 0) {
      n.sub.hidden = true;
      return;
    }
    n.sub.hidden = false;
    for (const sp of spans) {
      const el = document.createElement('span');
      if (sp.cls) el.className = sp.cls;
      el.textContent = sp.text;
      n.sub.appendChild(el);
    }
  }

  function updateRow(n, s) {
    const classes = ['row', 'state-' + s.state];
    if (s.dai) classes.push('dai');
    if (s.marks === 'paused') classes.push('paused');
    if (s.marks === 'prio') classes.push('prio');
    if (s.state === 'urgent' && !s.dai) classes.push('you-tint');
    n.li.className = classes.join(' ');
    n.li.dataset.id = s.id || s.pane;
    n.glyph.textContent = s.dai ? '📡' : (s.marks === 'paused' ? GLYPH.paused : (GLYPH[s.state] || '○'));
    n.name.textContent = s.name || '(no name)';
    n.age.textContent = fmtAge(s.since);
    renderSub(n, subLine(s));

    // Meta line: pane + optional neutral context bar + optional ctx NN%. Only
    // done and working rows carry it; needs-you, idle and paused fold the
    // pane/ctx info into their sub-line.
    const pane = paneLabel(s.pane);
    const ctx = parseInt(s.ctx, 10);
    const hasCtx = !isNaN(ctx) && ctx >= 0;
    const showMeta = !s.dai && !!pane && (s.state === 'done' || s.state === 'proc') && s.marks !== 'paused';
    n.metaPane.parentElement.hidden = !showMeta;
    if (showMeta) {
      n.metaPane.textContent = pane;
      n.ctxBar.hidden = !hasCtx;
      n.metaCtx.hidden = !hasCtx;
      if (hasCtx) {
        n.ctxFill.style.width = Math.min(100, Math.max(0, ctx)) + '%';
        n.metaCtx.textContent = 'ctx ' + ctx + '%';
      }
    }

    // Actions: Review button on needs-you rows, nothing otherwise.
    n.actions.innerHTML = '';
    if (s.state === 'urgent' && !s.dai) {
      const review = document.createElement('button');
      review.type = 'button';
      review.className = 'row-review';
      review.textContent = 'Review';
      review.addEventListener('click', (ev) => {
        ev.stopPropagation();
        const id = n.li.dataset.id;
        if (id) location.href = '/conversation.html?id=' + encodeURIComponent(id);
      });
      n.actions.appendChild(review);
    }
  }

  function render(sessions) {
    const list = Array.isArray(sessions) ? sessions : [];

    // Pull the dai-live pinned session out of the array; there is at most one.
    let daiSession = null;
    const rest = [];
    for (const s of list) {
      if (!daiSession && s.dai) {
        daiSession = s;
      } else {
        rest.push(s);
      }
    }

    // Render the pin slot above the group bands.
    if (daiSession) {
      const n = ensureRow(daiSession);
      updateRow(n, daiSession);
      daiPinEl.innerHTML = '';
      daiPinEl.appendChild(n.li);
      daiPinEl.hidden = false;
    } else {
      daiPinEl.innerHTML = '';
      daiPinEl.hidden = true;
    }

    // Partition the rest into the four groups, preserving server order.
    const buckets = { you: [], done: [], work: [], quiet: [] };
    for (const s of rest) buckets[groupKey(s)].push(s);

    // Rebuild each group, reusing cached nodes so DOM identity holds.
    const seen = new Set();
    if (daiSession) seen.add(daiSession.pane);
    for (const g of GROUPS) {
      const arr = buckets[g.key];
      if (arr.length === 0) {
        g.section.hidden = true;
        g.list.innerHTML = '';
        continue;
      }
      g.section.hidden = false;
      g.count.textContent = String(arr.length);
      const frag = document.createDocumentFragment();
      for (const s of arr) {
        const n = ensureRow(s);
        updateRow(n, s);
        frag.appendChild(n.li);
        seen.add(s.pane);
      }
      g.list.innerHTML = '';
      g.list.appendChild(frag);
    }

    // Drop cached nodes whose session disappeared from the stream.
    for (const [pane, n] of nodes) {
      if (!seen.has(pane)) {
        if (n.li.parentNode) n.li.parentNode.removeChild(n.li);
        nodes.delete(pane);
      }
    }

    // Empty state: no Claude sessions anywhere (pin included). The other-panes
    // group defaults to expanded in this case.
    const empty = !daiSession && rest.length === 0;
    emptyEl.hidden = !empty;
    listEl.hidden = empty;
    setOtherDefault(empty);
  }

  function setConn(state) {
    const live = state === 'live';
    liveDotEl.classList.toggle('on', live);
    liveDotEl.classList.toggle('off', !live);
  }

  // Primary channel: SSE. The browser handles reconnects, so a dropped
  // connection reads as "lost" until the next `open` event.
  function connect() {
    setConn('wait');
    const es = new EventSource('/api/stream');
    es.addEventListener('open', () => setConn('live'));
    es.addEventListener('error', () => setConn('lost'));
    es.addEventListener('sessions', (ev) => {
      try {
        render(JSON.parse(ev.data));
      } catch (e) {
        console.error('erma: bad sessions event', e);
      }
    });
  }

  connect();

  // ---------- Other panes group ----------
  //
  // Default: collapsed when at least one Claude session is on screen, expanded
  // when the empty state is up. We refresh the list on open and every 5s while
  // expanded; the main SSE stream only carries Claude rows.
  let otherTimer = null;
  let otherExpanded = false;
  let otherDefaultApplied = false;

  function setOtherDefault(emptyList) {
    // Only snap to the default once per page load. Once the user clicks the
    // toggle we respect their choice.
    if (otherDefaultApplied) return;
    if (emptyList) {
      if (!otherExpanded) setOtherExpanded(true);
    } else {
      if (otherExpanded) setOtherExpanded(false);
    }
    otherDefaultApplied = true;
  }

  function setOtherExpanded(open) {
    otherExpanded = open;
    otherToggleEl.setAttribute('aria-expanded', String(open));
    otherListEl.hidden = !open;
    otherToggleEl.classList.toggle('open', open);
    if (open) {
      fetchOtherPanes();
      if (!otherTimer) otherTimer = setInterval(fetchOtherPanes, 5000);
    } else if (otherTimer) {
      clearInterval(otherTimer);
      otherTimer = null;
    }
  }

  function renderOtherPanes(panes) {
    otherListEl.innerHTML = '';
    const list = Array.isArray(panes) ? panes : [];
    otherCountEl.textContent = String(list.length);
    if (list.length === 0) {
      // If currently expanded (user clicked it, then every non-Claude pane
      // went away), collapse before hiding so aria stays coherent.
      if (otherExpanded) {
        otherExpanded = false;
        otherToggleEl.setAttribute('aria-expanded', 'false');
        otherListEl.hidden = true;
        otherToggleEl.classList.remove('open');
        if (otherTimer) {
          clearInterval(otherTimer);
          otherTimer = null;
        }
      }
      otherGroupEl.hidden = true;
      return;
    }
    otherGroupEl.hidden = false;
    const frag = document.createDocumentFragment();
    for (const p of list) {
      const li = document.createElement('li');
      li.className = 'other-row';
      const title = document.createElement('span');
      title.className = 'other-title';
      title.textContent = p.title || p.command || p.paneId;
      const tag = document.createElement('span');
      tag.className = 'other-tag';
      tag.textContent = `${p.command || ''} · ${p.location || ''}`;
      const path = document.createElement('span');
      path.className = 'other-path';
      path.textContent = p.path || '';
      li.appendChild(title);
      li.appendChild(tag);
      li.appendChild(path);
      frag.appendChild(li);
    }
    otherListEl.appendChild(frag);
  }

  async function fetchOtherPanes() {
    try {
      const res = await fetch('/api/panes/other', { credentials: 'same-origin' });
      if (!res.ok) return;
      const body = await res.json();
      renderOtherPanes(body.panes || []);
    } catch (e) {
      console.warn('other panes', e);
    }
  }

  otherToggleEl.addEventListener('click', () => {
    // User interaction wins over the auto-default from render().
    otherDefaultApplied = true;
    setOtherExpanded(!otherExpanded);
  });

  // Always keep the count fresh so the group reveals itself when something
  // non-Claude shows up, even while collapsed.
  fetchOtherPanes();
  setInterval(() => { if (!otherExpanded) fetchOtherPanes(); }, 15000);

  // ---------- New session sheet ----------

  let lastFocusBeforeSheet = null;

  function openSheet() {
    lastFocusBeforeSheet = document.activeElement;
    sheetEl.hidden = false;
    sheetEl.setAttribute('aria-hidden', 'false');
    newErrorEl.hidden = true;
    newDirInput.value = '';
    loadRecentDirs();
    // hidden=false applies synchronously; focus on the next frame once layout
    // has settled, rather than an arbitrary setTimeout delay.
    requestAnimationFrame(() => newDirInput.focus());
  }

  function closeSheet() {
    sheetEl.hidden = true;
    sheetEl.setAttribute('aria-hidden', 'true');
    if (lastFocusBeforeSheet && typeof lastFocusBeforeSheet.focus === 'function') {
      lastFocusBeforeSheet.focus();
    }
    lastFocusBeforeSheet = null;
  }

  // Focus trap: keep Tab/Shift-Tab inside the sheet while it's open.
  function sheetFocusables() {
    const sel = 'a[href], button:not([disabled]), input:not([disabled]), ' +
      'select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
    const body = sheetEl.querySelector('.sheet-body');
    if (!body) return [];
    return Array.from(body.querySelectorAll(sel)).filter((el) => !el.hidden);
  }

  // The CSRF token POST /api/sessions requires; GET /api/dirs/recent hands it
  // out with the list.
  let newSessionCsrf = '';

  async function loadRecentDirs() {
    recentListEl.innerHTML = '';
    try {
      const res = await fetch('/api/dirs/recent', { credentials: 'same-origin' });
      if (!res.ok) return;
      const body = await res.json();
      newSessionCsrf = res.headers.get('X-Erma-CSRF') || body.csrf || newSessionCsrf;
      // Start from the server's default directory; the user can still edit it.
      if (!newDirInput.value && body.default) newDirInput.value = body.default;
      const dirs = Array.isArray(body.dirs) ? body.dirs : [];
      recentEmptyEl.hidden = dirs.length > 0;
      const frag = document.createDocumentFragment();
      for (const d of dirs) {
        const li = document.createElement('li');
        li.className = 'recent-row';
        li.textContent = d.path;
        // A tap on a list row is easy to land by accident: ask before launching.
        li.addEventListener('click', () => {
          newDirInput.value = d.path;
          if (window.confirm('Start a new Claude session in ' + d.path + '?')) submitNewSession();
        });
        frag.appendChild(li);
      }
      recentListEl.appendChild(frag);
    } catch (e) {
      console.warn('recent dirs', e);
    }
  }

  async function submitNewSession() {
    const dir = (newDirInput.value || '').trim();
    if (!dir) return;
    newSubmitBtn.disabled = true;
    newErrorEl.hidden = true;
    try {
      if (!newSessionCsrf) await loadRecentDirs();
      const res = await fetch('/api/sessions', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Erma-CSRF': newSessionCsrf },
        body: JSON.stringify({ dir }),
      });
      if (!res.ok) {
        const text = await res.text();
        newErrorEl.hidden = false;
        newErrorEl.textContent = text || ('error ' + res.status);
        return;
      }
      closeSheet();
    } catch (e) {
      newErrorEl.hidden = false;
      newErrorEl.textContent = String(e && e.message || e);
    } finally {
      newSubmitBtn.disabled = false;
    }
  }

  if (newBtn) newBtn.addEventListener('click', openSheet);
  if (emptyNewBtn) emptyNewBtn.addEventListener('click', openSheet);
  if (newForm) newForm.addEventListener('submit', (ev) => {
    ev.preventDefault();
    submitNewSession();
  });
  if (sheetEl) {
    sheetEl.addEventListener('click', (ev) => {
      const t = ev.target;
      if (t && t.getAttribute && t.getAttribute('data-close') === '1') closeSheet();
    });
  }
  document.addEventListener('keydown', (ev) => {
    if (sheetEl.hidden) return;
    if (ev.key === 'Escape') {
      closeSheet();
      return;
    }
    if (ev.key !== 'Tab') return;
    const nodes = sheetFocusables();
    if (nodes.length === 0) return;
    const first = nodes[0];
    const last = nodes[nodes.length - 1];
    const active = document.activeElement;
    if (ev.shiftKey && (active === first || !sheetEl.contains(active))) {
      ev.preventDefault();
      last.focus();
    } else if (!ev.shiftKey && active === last) {
      ev.preventDefault();
      first.focus();
    }
  });
})();
