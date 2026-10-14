// erma PWA front-end. Read-only: this page observes the bar's state over
// SSE and renders it. No mutation calls today — see the issue roadmap.
//
// The palette and semantics mirror cmd/erma/main.go. If you change colors or
// state tags there, update styles.css here.

(function () {
  'use strict';

  const listEl = document.getElementById('list');
  const emptyEl = document.getElementById('empty');
  const countsEl = document.getElementById('counts');
  const connEl = document.getElementById('conn');

  const otherGroupEl = document.getElementById('other-group');
  const otherToggleEl = document.getElementById('other-toggle');
  const otherListEl = document.getElementById('other-list');
  const otherHintEl = document.getElementById('other-hint');
  const otherCountEl = document.getElementById('other-count');
  const newBtn = document.getElementById('new-session');
  const sheetEl = document.getElementById('new-sheet');
  const recentListEl = document.getElementById('recent-list');
  const recentEmptyEl = document.getElementById('recent-empty');
  const newForm = document.getElementById('new-form');
  const newDirInput = document.getElementById('new-dir');
  const newErrorEl = document.getElementById('new-error');
  const newSubmitBtn = document.getElementById('new-submit');

  // State → glyph, kept in lockstep with the bar.
  const GLYPH = { urgent: '●', done: '✓', proc: '…', idle: '○' };

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

  function metaLine(s) {
    const parts = [];
    if (s.state === 'urgent') parts.push('PERM');
    else if (s.state === 'done') parts.push('DONE');
    if (s.sub > 0) parts.push('⤷' + s.sub);
    const age = fmtAge(s.since);
    if (age) parts.push(age);
    return parts.join(' · ');
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
    const name = document.createElement('span');
    name.className = 'name';
    const meta = document.createElement('span');
    meta.className = 'meta';
    body.appendChild(name);
    body.appendChild(meta);
    const marks = document.createElement('span');
    marks.className = 'marks';
    const badge = document.createElement('span');
    badge.className = 'badge';
    li.appendChild(glyph);
    li.appendChild(body);
    li.appendChild(marks);
    li.appendChild(badge);
    // Row tap opens the conversation view. The whole row is one hit target
    // (the <li>, not an <a>) so taps on marks or badge count too.
    li.addEventListener('click', () => {
      const id = li.dataset.id;
      if (id) location.href = '/conversation.html?id=' + encodeURIComponent(id);
    });
    n = { li, glyph, name, meta, marks, badge, ctx: null };
    nodes.set(s.pane, n);
    return n;
  }

  function updateRow(n, s) {
    const classes = ['row', 'state-' + s.state];
    if (s.dai) classes.push('dai');
    if (s.marks === 'paused') classes.push('paused');
    if (s.marks === 'prio') classes.push('prio');
    n.li.className = classes.join(' ');
    n.li.dataset.id = s.id || s.pane;
    n.glyph.textContent = s.dai ? '📡' : GLYPH[s.state] || '○';
    n.name.textContent = s.name || '(no name)';
    n.meta.textContent = metaLine(s);
    n.marks.innerHTML = '';
    if (s.marks === 'prio') {
      const span = document.createElement('span');
      span.className = 'prio';
      span.textContent = '★ prio';
      n.marks.appendChild(span);
    } else if (s.marks === 'paused') {
      const span = document.createElement('span');
      span.className = 'paused';
      span.textContent = '‖ paused';
      n.marks.appendChild(span);
    }
    n.badge.textContent = s.name ? idShort(s) : s.pane;
    // Context meter: only when the server reports something.
    const ctx = parseInt(s.ctx, 10);
    if (!isNaN(ctx) && ctx >= 0) {
      if (!n.ctx) {
        n.ctx = document.createElement('div');
        n.ctx.className = 'ctx';
        const fill = document.createElement('span');
        fill.className = 'fill';
        n.ctx.appendChild(fill);
        n.li.appendChild(n.ctx);
      }
      n.ctx.classList.toggle('warn', ctx >= 70 && ctx < 85);
      n.ctx.classList.toggle('high', ctx >= 85);
      n.ctx.firstChild.style.width = Math.min(100, Math.max(0, ctx)) + '%';
    } else if (n.ctx) {
      n.ctx.remove();
      n.ctx = null;
    }
  }

  // idShort: a compact badge label. If the pane is "%42" the number alone is
  // what we show; otherwise fall back to the pane id verbatim.
  function idShort(s) {
    const m = /^%(\d+)$/.exec(s.pane);
    return m ? m[1] : s.pane;
  }

  function render(sessions) {
    if (!sessions || sessions.length === 0) {
      emptyEl.hidden = false;
      listEl.innerHTML = '';
      nodes.clear();
      updateCounts([]);
      return;
    }
    emptyEl.hidden = true;
    // Rebuild the list in server order so the dai pin stays first and bands
    // follow. Reusing nodes keeps DOM identity stable across updates.
    const seen = new Set();
    const frag = document.createDocumentFragment();
    for (const s of sessions) {
      const n = ensureRow(s);
      updateRow(n, s);
      frag.appendChild(n.li);
      seen.add(s.pane);
    }
    for (const [pane, n] of nodes) {
      if (!seen.has(pane)) {
        n.li.remove();
        nodes.delete(pane);
      }
    }
    listEl.innerHTML = '';
    listEl.appendChild(frag);
    updateCounts(sessions);
  }

  function updateCounts(sessions) {
    let u = 0, d = 0, p = 0, i = 0, z = 0;
    for (const s of sessions) {
      if (s.marks === 'paused') { z++; continue; }
      if (s.state === 'urgent') u++;
      else if (s.state === 'done') d++;
      else if (s.state === 'proc') p++;
      else i++;
    }
    const parts = [];
    parts.push(`<span class="total">${sessions.length}</span>`);
    if (u > 0) parts.push(`<span class="u">${u}●</span>`);
    if (d > 0) parts.push(`<span class="d">${d}✓</span>`);
    if (p > 0) parts.push(`<span class="p">${p}⠹</span>`);
    if (z > 0) parts.push(`<span class="z">‖${z}</span>`);
    countsEl.innerHTML = parts.join(' ');
  }

  function setConn(state) {
    connEl.className = 'status ' + state;
    connEl.textContent = state === 'live' ? '●' : state === 'lost' ? '✕' : '·';
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
  // The group is collapsed by default and populated from /api/panes/other. We
  // refresh it on open and every 5s while expanded — the main SSE stream only
  // carries Claude rows. The rows are read-only: there is no pane view yet, and
  // nothing here writes to a non-Claude pane.
  let otherTimer = null;
  let otherExpanded = false;

  function renderOtherPanes(panes) {
    otherListEl.innerHTML = '';
    const list = Array.isArray(panes) ? panes : [];
    otherCountEl.textContent = String(list.length);
    if (list.length === 0) {
      // If the group is currently expanded (user clicked it, then every
      // non-Claude pane went away), collapse it and stop the poll before
      // hiding — otherwise aria-expanded stays "true" on a hidden element
      // and otherTimer keeps firing against an empty list.
      if (otherExpanded) {
        otherExpanded = false;
        otherToggleEl.setAttribute('aria-expanded', 'false');
        otherListEl.hidden = true;
        otherHintEl.hidden = true;
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
      const name = document.createElement('span');
      name.className = 'other-name';
      name.textContent = p.title || p.command || p.paneId;
      const meta = document.createElement('span');
      meta.className = 'other-meta';
      meta.textContent = `${p.command || ''} · ${p.location || ''}`;
      const path = document.createElement('span');
      path.className = 'other-path';
      path.textContent = p.path || '';
      li.appendChild(name);
      li.appendChild(meta);
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
    otherExpanded = !otherExpanded;
    otherToggleEl.setAttribute('aria-expanded', String(otherExpanded));
    otherListEl.hidden = !otherExpanded;
    otherHintEl.hidden = !otherExpanded;
    otherToggleEl.classList.toggle('open', otherExpanded);
    if (otherExpanded) {
      fetchOtherPanes();
      otherTimer = setInterval(fetchOtherPanes, 5000);
    } else if (otherTimer) {
      clearInterval(otherTimer);
      otherTimer = null;
    }
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
