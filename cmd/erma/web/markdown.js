// erma PWA — safe Markdown renderer for assistant turns.
//
// Builds DOM nodes directly (document.createElement + createTextNode); never
// touches innerHTML/outerHTML/document.write/eval, so an attacker-controlled
// transcript cannot inject executable markup. Links go through an allow-list:
// only http:// and https:// hrefs are kept; anything else falls back to the
// link label as plain text. Every emitted <a> gets rel="noopener noreferrer"
// and target="_blank".
//
// Exposes window.ermaRenderMarkdown(text, parentEl) — the caller passes the
// raw assistant text and the container <div> to append rendered children to.
// Any construct the parser does not recognise falls through as plain text via
// createTextNode so the original characters survive.
//
// To run the test harness: `python3 -m http.server` from cmd/erma/web/ and
// open http://127.0.0.1:8000/markdown.test.html — results print in-page.

(function () {
  'use strict';

  // Allow-list of URL schemes for rendered links. Only http:// and https://
  // are safe: javascript:, data:, vbscript: can execute attacker-supplied
  // code; file: can exfiltrate local files; mailto: and tel: open native
  // handlers the user did not consent to; relative/path-only hrefs like
  // /api/... and fragment anchors like #section are navigations inside this
  // origin that an attacker-controlled transcript must not be able to trigger.
  // Anything outside http(s) returns null and the caller falls back to
  // rendering the label as plain text.
  function safeHref(url) {
    const u = String(url == null ? '' : url).trim();
    if (/^https?:\/\//i.test(u)) return u;
    return null;
  }

  function makeLink(href, label) {
    const a = document.createElement('a');
    a.href = href;
    a.rel = 'noopener noreferrer';
    a.target = '_blank';
    renderInline(label, a);
    return a;
  }

  // Inline parser: single pass over `text`, emitting into `parent`. Order of
  // delimiters: inline code, link, bold (**/__), italic (*/_). Unrecognised
  // delimiters fall through as literal characters.
  function renderInline(text, parent) {
    const n = text.length;
    let i = 0;
    let buf = '';
    const flush = function () {
      if (buf) {
        parent.appendChild(document.createTextNode(buf));
        buf = '';
      }
    };
    while (i < n) {
      const c = text.charAt(i);

      // inline code — no markdown parsed inside
      if (c === '`') {
        const end = text.indexOf('`', i + 1);
        if (end > i) {
          flush();
          const code = document.createElement('code');
          code.textContent = text.slice(i + 1, end);
          parent.appendChild(code);
          i = end + 1;
          continue;
        }
      }

      // link [label](url)
      if (c === '[') {
        const close = findLinkClose(text, i + 1);
        if (close > i && text.charAt(close + 1) === '(') {
          const paren = text.indexOf(')', close + 2);
          if (paren > close) {
            const label = text.slice(i + 1, close);
            const rawUrl = text.slice(close + 2, paren);
            const href = safeHref(rawUrl);
            flush();
            if (href) {
              parent.appendChild(makeLink(href, label));
            } else {
              parent.appendChild(document.createTextNode(label));
            }
            i = paren + 1;
            continue;
          }
        }
      }

      // bold ** or __
      // Left-flanking guard: the char right after the opening delimiter must
      // not be a space, so `** word**` falls through as literal characters.
      if ((c === '*' || c === '_') && text.charAt(i + 1) === c) {
        const after = text.charAt(i + 2);
        if (after && after !== ' ') {
          const delim = c + c;
          const end = findDelimClose(text, i + 2, delim);
          if (end > i + 2) {
            flush();
            const strong = document.createElement('strong');
            renderInline(text.slice(i + 2, end), strong);
            parent.appendChild(strong);
            i = end + 2;
            continue;
          }
        }
      }

      // italic * or _ (with word-boundary guard on _ to avoid snake_case breakage)
      if (c === '*' || c === '_') {
        const next = text.charAt(i + 1);
        if (next && next !== c && next !== ' ') {
          if (c === '_' && isWordChar(text.charAt(i - 1))) {
            // intra-word underscore: literal
          } else {
            const end = findSingleDelimClose(text, i + 1, c);
            if (end > i + 1) {
              const after = text.charAt(end + 1);
              const allow = !(c === '_' && isWordChar(after));
              if (allow) {
                flush();
                const em = document.createElement('em');
                renderInline(text.slice(i + 1, end), em);
                parent.appendChild(em);
                i = end + 1;
                continue;
              }
            }
          }
        }
      }

      buf += c;
      i++;
    }
    flush();
  }

  function isWordChar(c) {
    return !!c && /[A-Za-z0-9]/.test(c);
  }

  // Find the matching `]` for a link label, skipping nested `[...]` so that
  // labels like `[a [nested] b]` are respected. Returns -1 if unbalanced.
  function findLinkClose(text, start) {
    let depth = 1;
    for (let j = start; j < text.length; j++) {
      const ch = text.charAt(j);
      if (ch === '[') depth++;
      else if (ch === ']') {
        depth--;
        if (depth === 0) return j;
      }
    }
    return -1;
  }

  function findDelimClose(text, start, delim) {
    for (let j = start; j <= text.length - delim.length; j++) {
      if (text.slice(j, j + delim.length) === delim && text.charAt(j - 1) !== ' ') {
        return j;
      }
    }
    return -1;
  }

  function findSingleDelimClose(text, start, delim) {
    for (let j = start; j < text.length; j++) {
      if (text.charAt(j) === delim && text.charAt(j - 1) !== ' ') {
        // Both sides are checked: `j+1 === delim` rejects an italic close that
        // is really the first half of a `**` bold open (`*foo **b** bar*`),
        // and `j-1 === delim` rejects the second half of a `**` bold close
        // (`**foo** bar`) from being swallowed as an italic terminator.
        if (text.charAt(j + 1) === delim) continue;
        if (text.charAt(j - 1) === delim) continue;
        return j;
      }
    }
    return -1;
  }

  // Block tokenizer: splits input lines into a flat list of blocks. One level
  // of list nesting is supported — deeper indents collapse into the parent
  // <li>'s continuation text, matching the spec.
  function parseBlocks(text) {
    const lines = text.split('\n');
    const blocks = [];
    let i = 0;
    while (i < lines.length) {
      const line = lines[i];

      // fenced code block ```...```
      const fenceMatch = line.match(/^(\s*)(`{3,})(.*)$/);
      if (fenceMatch) {
        const fence = fenceMatch[2];
        const bodyLines = [];
        i++;
        while (i < lines.length) {
          const close = lines[i].match(/^\s*(`{3,})\s*$/);
          if (close && close[1].length >= fence.length) {
            i++;
            break;
          }
          bodyLines.push(lines[i]);
          i++;
        }
        blocks.push({ type: 'code', text: bodyLines.join('\n') });
        continue;
      }

      // ATX heading ##..###
      const headMatch = line.match(/^(#{1,3})\s+(.+?)\s*#*\s*$/);
      if (headMatch) {
        blocks.push({ type: 'heading', level: headMatch[1].length, text: headMatch[2] });
        i++;
        continue;
      }

      // table — header + separator row
      if (line.indexOf('|') >= 0 && i + 1 < lines.length && isTableSeparator(lines[i + 1])) {
        const header = parseTableRow(line);
        const rows = [];
        i += 2;
        while (i < lines.length && lines[i].indexOf('|') >= 0 && lines[i].trim()) {
          rows.push(parseTableRow(lines[i]));
          i++;
        }
        blocks.push({ type: 'table', header: header, rows: rows });
        continue;
      }

      // list — ordered or unordered
      if (isListLine(line)) {
        const parsed = parseList(lines, i);
        blocks.push(parsed.block);
        i = parsed.next;
        continue;
      }

      // blank line
      if (!line.trim()) {
        i++;
        continue;
      }

      // paragraph — collect consecutive non-blank, non-block lines
      const paraLines = [line];
      i++;
      while (i < lines.length && lines[i].trim() && !isBlockStart(lines, i)) {
        paraLines.push(lines[i]);
        i++;
      }
      blocks.push({ type: 'paragraph', text: paraLines.join('\n') });
    }
    return blocks;
  }

  function isBlockStart(lines, i) {
    const line = lines[i];
    if (/^(\s*)(`{3,})/.test(line)) return true;
    if (/^(#{1,3})\s+/.test(line)) return true;
    if (isListLine(line)) return true;
    if (line.indexOf('|') >= 0 && i + 1 < lines.length && isTableSeparator(lines[i + 1])) return true;
    return false;
  }

  function isListLine(line) {
    return /^(\s*)([-*+]|\d+\.)\s+/.test(line);
  }

  function isTableSeparator(line) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.indexOf('|') < 0 && trimmed.indexOf('-') < 0) return false;
    // Cells of only -, :, spaces, pipes. The `\-` is escaped so the class is
    // read as four discrete characters (whitespace / `|` / `:` / `-`), not as
    // a `:` to `-` range.
    return /^[\s|:\-]+$/.test(trimmed) && trimmed.indexOf('-') >= 0;
  }

  function parseTableRow(line) {
    let s = line.trim();
    if (s.charAt(0) === '|') s = s.slice(1);
    if (s.charAt(s.length - 1) === '|') s = s.slice(0, -1);
    return s.split('|').map(function (c) { return c.trim(); });
  }

  // parseList — collects consecutive items at the top indent, folds continuation
  // lines (indent >= 2) into each item's content, promotes a nested list once.
  function parseList(lines, start) {
    const firstMatch = lines[start].match(/^(\s*)([-*+]|\d+\.)\s+(.*)$/);
    const baseIndent = firstMatch[1].length;
    const ordered = /\d+\./.test(firstMatch[2]);
    const startNum = ordered ? parseInt(firstMatch[2], 10) : null;
    const items = [];
    let i = start;
    while (i < lines.length) {
      const m = lines[i].match(/^(\s*)([-*+]|\d+\.)\s+(.*)$/);
      if (!m || m[1].length !== baseIndent) {
        // A deeper item here means nested list continuation handled below; a
        // non-list line means the list is over.
        if (!m) break;
        if (m[1].length < baseIndent) break;
        // Deeper start without an open parent item (shouldn't happen since we
        // process continuation inside the loop below); break to be safe.
        break;
      }
      const item = { text: m[3], children: [] };
      i++;
      // Collect continuation and nested blocks until next sibling or dedent.
      const contLines = [];
      while (i < lines.length) {
        const cl = lines[i];
        if (!cl.trim()) {
          // blank line: peek ahead for a sibling at same indent to keep the list going
          const next = lines[i + 1];
          if (next && new RegExp('^\\s{' + baseIndent + '}([-*+]|\\d+\\.)\\s+').test(next)) {
            i++;
            continue;
          }
          break;
        }
        const sibling = cl.match(/^(\s*)([-*+]|\d+\.)\s+/);
        if (sibling && sibling[1].length === baseIndent) break;
        if (sibling && sibling[1].length > baseIndent) {
          // nested list — grab a one-level child list
          const nested = parseList(lines, i);
          item.children.push({ type: 'list', list: nested.block });
          i = nested.next;
          continue;
        }
        // plain continuation line — append to item's text
        contLines.push(cl.replace(/^\s+/, ''));
        i++;
      }
      if (contLines.length) item.text += '\n' + contLines.join('\n');
      items.push(item);
    }
    return { block: { type: 'list', ordered: ordered, start: startNum, items: items }, next: i };
  }

  function renderBlock(block, parent) {
    switch (block.type) {
      case 'heading': {
        const h = document.createElement('h' + block.level);
        renderInline(block.text, h);
        parent.appendChild(h);
        return;
      }
      case 'paragraph': {
        const p = document.createElement('p');
        // preserve explicit newlines inside a paragraph as <br>
        const parts = block.text.split('\n');
        for (let k = 0; k < parts.length; k++) {
          if (k > 0) p.appendChild(document.createElement('br'));
          renderInline(parts[k], p);
        }
        parent.appendChild(p);
        return;
      }
      case 'code': {
        const pre = document.createElement('pre');
        const code = document.createElement('code');
        code.textContent = block.text;
        pre.appendChild(code);
        parent.appendChild(pre);
        return;
      }
      case 'list': {
        const el = document.createElement(block.ordered ? 'ol' : 'ul');
        if (block.ordered && Number.isFinite(block.start) && block.start !== 1) {
          el.setAttribute('start', String(block.start));
        }
        for (const it of block.items) {
          const li = document.createElement('li');
          renderInline(it.text, li);
          for (const child of (it.children || [])) {
            if (child.type === 'list') renderBlock(child.list, li);
          }
          el.appendChild(li);
        }
        parent.appendChild(el);
        return;
      }
      case 'table': {
        const table = document.createElement('table');
        const thead = document.createElement('thead');
        const trh = document.createElement('tr');
        for (const cell of block.header) {
          const th = document.createElement('th');
          renderInline(cell, th);
          trh.appendChild(th);
        }
        thead.appendChild(trh);
        table.appendChild(thead);
        const tbody = document.createElement('tbody');
        for (const row of block.rows) {
          const tr = document.createElement('tr');
          for (const cell of row) {
            const td = document.createElement('td');
            renderInline(cell, td);
            tr.appendChild(td);
          }
          tbody.appendChild(tr);
        }
        table.appendChild(tbody);
        // tables scroll inside their own box to avoid page-level overflow
        const wrap = document.createElement('div');
        wrap.className = 'md-table-wrap';
        wrap.appendChild(table);
        parent.appendChild(wrap);
        return;
      }
      default: {
        // Unknown block: dump the raw text so no character is lost.
        if (block.text) {
          parent.appendChild(document.createTextNode(block.text));
        }
      }
    }
  }

  function render(text, parent) {
    if (text == null) return;
    try {
      const blocks = parseBlocks(String(text));
      for (const b of blocks) renderBlock(b, parent);
    } catch (e) {
      // Any parse error falls back to plain text so a malformed input never
      // crashes the turn render.
      parent.appendChild(document.createTextNode(String(text)));
    }
  }

  window.ermaRenderMarkdown = render;
})();
