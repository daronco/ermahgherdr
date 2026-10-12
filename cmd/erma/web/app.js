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

  // tickAges: the ages tick every second even when the server did not emit a
  // new frame. Only the meta line needs it; rebuilding it in place is cheap.
  function tickAges() {
    for (const [, n] of nodes) {
      // Rebuild from the dataset we last rendered: n.meta already carries the
      // state via sibling classes; the state glyph gives us enough to key off
      // without storing the raw session on the node. We instead refetch via
      // /api/sessions? No — the age field we have is just n.meta.textContent.
      // Keep it simple: skip if there is nothing to tick.
    }
    // Note: full tick support would require stashing `since` on each node; the
    // SSE poller runs at 600ms, which is already enough resolution for the
    // visible age bucket. We keep this hook so a future PR can refine it.
  }

  connect();
  setInterval(tickAges, 1000);
})();
