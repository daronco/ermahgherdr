// erma PWA — Terminal tab. Polls GET /api/sessions/<sid>/terminal while the
// tab is visible, parses the ANSI SGR escapes into coloured <span> runs, and
// POSTs one key per button tap through /keys with the allowlisted token.
//
// Scope: SGR 0 (reset), 1 (bold), 22 (bold off), 30-37/40-47 (normal fg/bg),
// 90-97/100-107 (bright fg/bg), 39/49 (default fg/bg). Anything else is
// dropped so a bogus sequence can't inject HTML.

(function () {
  'use strict';

  const params = new URLSearchParams(location.search);
  const SID = params.get('id') || '';
  if (!SID) return;

  const screenEl = document.getElementById('term-screen');
  const emptyEl = document.getElementById('term-empty');
  if (!screenEl) return;

  // Classical 16-colour palette, matching the dark theme already used by the
  // conversation view. Index 0-7 normal, 8-15 bright.
  const PALETTE = [
    '#000000', '#cd3131', '#0dbc79', '#e5e510',
    '#2472c8', '#bc3fbc', '#11a8cd', '#e5e5e5',
    '#666666', '#f14c4c', '#23d18b', '#f5f543',
    '#3b8eea', '#d670d6', '#29b8db', '#ffffff',
  ];

  const REFRESH_MS = 500;
  let timer = null;
  let inFlight = false;
  let visible = false;

  function esc(ch) {
    switch (ch) {
      case '&': return '&amp;';
      case '<': return '&lt;';
      case '>': return '&gt;';
      default: return ch;
    }
  }

  function escapeHtml(s) {
    let out = '';
    for (const c of s) out += esc(c);
    return out;
  }

  // State → inline style. Returns "" when nothing is set; the caller skips
  // the wrapping span in that case.
  function styleFor(state) {
    const parts = [];
    if (state.fg !== null) parts.push('color:' + PALETTE[state.fg]);
    if (state.bg !== null) parts.push('background-color:' + PALETTE[state.bg]);
    if (state.bold) parts.push('font-weight:700');
    return parts.join(';');
  }

  function applySgr(params, state) {
    // Empty params list means "0" (reset).
    if (params.length === 0) params = [0];
    for (let i = 0; i < params.length; i++) {
      const n = params[i];
      if (n === 0) {
        state.fg = null; state.bg = null; state.bold = false;
      } else if (n === 1) {
        state.bold = true;
      } else if (n === 22) {
        state.bold = false;
      } else if (n >= 30 && n <= 37) {
        state.fg = n - 30;
      } else if (n === 39) {
        state.fg = null;
      } else if (n >= 40 && n <= 47) {
        state.bg = n - 40;
      } else if (n === 49) {
        state.bg = null;
      } else if (n >= 90 && n <= 97) {
        state.fg = n - 90 + 8;
      } else if (n >= 100 && n <= 107) {
        state.bg = n - 100 + 8;
      }
      // Anything else (256-colour, truecolour, italics, blink, …) is
      // intentionally dropped — the pane snapshot is good enough without.
    }
  }

  // ansiToHtml walks the capture-pane output and emits coloured <span> runs.
  // Non-SGR CSI sequences (cursor moves, erase line, …) are stripped. Non-CSI
  // escapes are stripped too; capture-pane -e emits SGR, so that's enough.
  function ansiToHtml(raw) {
    let out = '';
    let open = false;
    const state = { fg: null, bg: null, bold: false };

    function openSpan() {
      const style = styleFor(state);
      if (style) {
        out += '<span style="' + style + '">';
        open = true;
      }
    }
    function closeSpan() {
      if (open) {
        out += '</span>';
        open = false;
      }
    }
    function refresh() {
      closeSpan();
      openSpan();
    }

    let i = 0;
    while (i < raw.length) {
      const c = raw.charCodeAt(i);
      if (c === 0x1b && i + 1 < raw.length && raw.charCodeAt(i + 1) === 0x5b) {
        // CSI: ESC [ params final-byte. Params = digits + ';'.
        let j = i + 2;
        let p = '';
        while (j < raw.length) {
          const cc = raw.charCodeAt(j);
          if ((cc >= 0x30 && cc <= 0x3f)) { p += raw[j]; j++; }
          else break;
        }
        if (j >= raw.length) break; // truncated
        const final = raw[j];
        j++;
        if (final === 'm') {
          const params = p.split(';').map((s) => parseInt(s, 10) || 0);
          applySgr(params, state);
          refresh();
        }
        // Non-SGR CSI: drop (cursor moves, clears, etc. — the capture is
        // already in the laid-out form we want).
        i = j;
      } else if (c === 0x1b) {
        // Any other escape: skip the ESC and the following byte (two-byte
        // sequence like RIS, DECALN, or a lone G1 shift). If there is no
        // next byte, we drop just the ESC.
        i += (i + 1 < raw.length) ? 2 : 1;
      } else if (c === 0x0d) {
        // Bare \r — capture-pane uses \n for line breaks, so a stray \r
        // would just clutter the output.
        i++;
      } else if (c === 0x08) {
        // Backspace in a snapshot does nothing visible on its own.
        i++;
      } else {
        out += esc(raw[i]);
        i++;
      }
    }
    closeSpan();
    return out;
  }

  async function fetchScreen() {
    if (inFlight) return;
    inFlight = true;
    try {
      const res = await fetch('/api/sessions/' + encodeURIComponent(SID) + '/terminal', {
        cache: 'no-store',
      });
      if (!res.ok) {
        if (emptyEl) {
          emptyEl.hidden = false;
          emptyEl.textContent = 'terminal unavailable (' + res.status + ')';
        }
        return;
      }
      const raw = await res.text();
      screenEl.innerHTML = ansiToHtml(raw);
      if (emptyEl) emptyEl.hidden = true;
    } catch (e) {
      console.warn('terminal fetch', e);
    } finally {
      inFlight = false;
    }
  }

  function startPolling() {
    if (timer !== null) return;
    fetchScreen();
    timer = setInterval(fetchScreen, REFRESH_MS);
  }

  function stopPolling() {
    if (timer !== null) {
      clearInterval(timer);
      timer = null;
    }
  }

  async function sendKey(token) {
    const csrf = typeof window.ermaCSRF === 'function' ? window.ermaCSRF() : '';
    if (!csrf) {
      console.warn('terminal key: no CSRF yet');
      return;
    }
    const res = await fetch('/api/sessions/' + encodeURIComponent(SID) + '/keys', {
      method: 'POST',
      headers: { 'X-Erma-CSRF': csrf, 'Content-Type': 'application/json' },
      body: JSON.stringify({ key: token }),
    });
    if (!res.ok) console.warn('terminal key', token, res.status);
  }

  // Button bar: one request per tap. Disable while a POST is in flight so a
  // double-tap can't fire two of the same keystroke before the first lands.
  document.querySelectorAll('.term-keybar .tk').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const token = btn.getAttribute('data-tkey');
      if (!token) return;
      btn.disabled = true;
      try {
        await sendKey(token);
        // Refresh sooner than the next tick to make the keystroke feel instant.
        if (visible) fetchScreen();
      } finally {
        btn.disabled = false;
      }
    });
  });

  // Only poll while the Terminal tab is the active tab AND the browser tab
  // itself is visible. Backgrounding the page stops the fetches on the phone.
  document.addEventListener('erma:tab', (ev) => {
    const name = ev.detail && ev.detail.name;
    visible = (name === 'terminal');
    if (visible && document.visibilityState === 'visible') startPolling();
    else stopPolling();
  });
  document.addEventListener('visibilitychange', () => {
    if (visible && document.visibilityState === 'visible') startPolling();
    else stopPolling();
  });
  window.addEventListener('pagehide', stopPolling);
})();
