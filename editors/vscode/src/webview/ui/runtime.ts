// The one page script every MemQL webview document runs.
//
// WHY ONE. Each panel used to carry its own script: a click handler here, a
// field recorder there, a log-follow snippet copied into two places. They
// drifted, and none of them could update a page in place -- a change meant a
// new `webview.html`, which replaced the whole document and took scroll,
// focus, a caret, an open disclosure and a running animation with it. This
// script is the other half of liveView.ts: the document is assigned once per
// screen, and everything after that arrives as a message this script applies
// to the page that is already there. The message shapes are in protocol.ts.
//
// WHAT IT DOES
//   - acquires the VS Code API once, posts `{ type: "ready" }` when loaded and
//     restores the window's scroll for the same screen from getState();
//   - a click on the nearest `[data-act]` posts `{ type: data-act, value:
//     data-value, ...every other data-* attribute camelCased }`, ignoring
//     disabled and aria-disabled elements;
//   - a `[data-field]` control posts `{ type: "input", field, value }` on input
//     and change ("true"/"false" for a checkbox), and never repaints;
//     `data-enter-act` on it also posts that act on Enter;
//   - a `[role=switch]` without `data-field` posts `{ type: data-switch-act or
//     "switch", id, checked, ...its other data-* }` on change;
//   - Escape posts the `data-escape-act` of the body (or of the first element
//     carrying one);
//   - a `[data-disclosure]` toggle opens or closes its body HERE, at once, and
//     posts its act with `open` so the host remembers;
//   - a log pane (`[data-region=log]`) follows its tail while scrolled to the
//     bottom, stops when the person scrolls up, and resumes at the bottom;
//     a line marked `data-anchor` (a failure's first line) is scrolled to
//     once, the first time its pane is visible, and stops the follow;
//   - host messages: `patch` (region HTML), `progress`, `log`, `setDisclosure`.
//
// A PATCH IS A MORPH, NOT A REPLACEMENT. The new region HTML is parsed into a
// template and the live region is walked into its shape node by node: an
// element that is still there is KEPT and has its attributes and text
// brought up to date. That is what lets a switch's thumb slide, the bar's
// width transition, a log pane keep its scroll, and a focused field keep its
// half-typed value and caret -- and, when the focused element really was
// replaced, focus is restored to its successor by id, then `data-field`, then
// `data-act` + `data-value`. A log pane the new HTML leaves empty keeps the
// lines it streamed.
//
// NO BACKTICK AND NO DOLLAR-BRACE ANYWHERE IN THE SCRIPT, and no backslash: it
// is embedded in template literals, where the first two would be consumed
// before the browser saw them and the third would silently change meaning.
// test/pageRuntime.test.ts holds that line.
//
// TESTED IN TWO PLACES. test/pageRuntime.test.ts runs the page-to-host half
// under `node --test`; the host-to-page half needs a real DOM and runs in
// headless Chrome with `npm run gallery:check` (gallery/checks/). Run both
// after changing this file.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

/** The most lines a log pane keeps; older lines are dropped from the top. Matches LiveView's buffer. */
export const PAGE_LOG_LIMIT = 5000;

export const PAGE_RUNTIME = `
(function () {
  'use strict';
  var vscode = acquireVsCodeApi();
  var LOG_LIMIT = ${PAGE_LOG_LIMIT};
  var FOLLOW_SLACK = 8;
  var lastProgress = null;
  var ticker = 0;

  function post(msg) { vscode.postMessage(msg); }
  window.memqlPage = { post: post };

  // ---------- saved view state ----------

  function readState() {
    var state = vscode.getState();
    return state && typeof state === 'object' ? state : {};
  }
  function writeState(key, value) {
    var state = readState();
    state[key] = value;
    vscode.setState(state);
  }
  function screenKey() {
    var meta = document.querySelector('meta[name="memql-screen"]');
    return meta ? meta.getAttribute('content') || '' : '';
  }

  // ---------- small helpers ----------

  function dataOf(el) {
    var out = {};
    var ds = el.dataset;
    for (var key in ds) out[key] = ds[key];
    return out;
  }
  function attrSelector(name, value) {
    return '[' + name + '="' + CSS.escape(String(value)) + '"]';
  }
  function isDisabled(el) {
    return el.disabled === true || el.getAttribute('aria-disabled') === 'true';
  }
  function fieldValue(el) {
    if (el.type === 'checkbox' || el.type === 'radio') return el.checked ? 'true' : 'false';
    return String(el.value);
  }

  // ---------- disclosures ----------

  // A line marked as the place to open the log (a failure's first line) is
  // scrolled to once, the first time its pane is visible, and the pane stops
  // following the tail so later lines do not carry the reason away.
  function scrollToAnchor(pane) {
    if (pane.clientHeight === 0) return false;
    var anchor = pane.querySelector('[data-anchor="true"]');
    if (!anchor) return false;
    anchor.setAttribute('data-anchor', 'shown');
    pane.scrollTop += anchor.getBoundingClientRect().top - pane.getBoundingClientRect().top - 6;
    pane.setAttribute('data-follow', 'false');
    return true;
  }
  function followLogsIn(root) {
    var panes = root.matches && root.matches('[data-region="log"]') ? [root] : [];
    root.querySelectorAll('[data-region="log"]').forEach(function (p) { panes.push(p); });
    panes.forEach(function (pane) {
      if (scrollToAnchor(pane)) return;
      if (pane.getAttribute('data-follow') !== 'false') pane.scrollTop = pane.scrollHeight;
    });
  }
  function setDisclosure(toggle, open) {
    var id = toggle.getAttribute('aria-controls') || toggle.getAttribute('data-disclosure');
    var body = id ? document.getElementById(id) : null;
    toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (body) {
      body.hidden = !open;
      if (open) followLogsIn(body);
    }
  }
  function disclosureToggle(id) {
    return document.querySelector('[data-disclosure]' + attrSelector('aria-controls', id)) ||
      document.querySelector(attrSelector('data-disclosure', id));
  }

  // ---------- clicks ----------

  document.addEventListener('click', function (event) {
    var target = event.target;
    if (!(target instanceof Element)) return;
    var el = target.closest('[data-act]');
    if (!el || isDisabled(el)) return;
    if (el.tagName === 'A') event.preventDefault();
    var msg = dataOf(el);
    msg.type = msg.act;
    delete msg.act;
    if (el.hasAttribute('data-disclosure')) {
      var open = el.getAttribute('aria-expanded') !== 'true';
      setDisclosure(el, open);
      msg.open = open;
    }
    post(msg);
  });

  // ---------- fields and switches ----------

  // The value each field last posted, so the change event that follows the
  // last input event does not post it twice. A value the HOST writes (a patch,
  // a focus restore) is recorded here too: the host already knows it, and a
  // person who then types or pastes the value they had before must still be
  // heard -- otherwise a cleared confirmation phrase, pasted back in one go,
  // would never reach the host.
  var lastPosted = new WeakMap();
  function noteHostValue(el) {
    if (el.hasAttribute && el.hasAttribute('data-field')) lastPosted.set(el, fieldValue(el));
  }
  function onFieldEvent(event) {
    var el = event.target;
    if (!(el instanceof Element)) return;
    if (el.hasAttribute('data-field')) {
      var value = fieldValue(el);
      if (lastPosted.get(el) === value) return;
      lastPosted.set(el, value);
      post({ type: 'input', field: el.getAttribute('data-field'), value: value });
      return;
    }
    if (event.type === 'change' && el.getAttribute('role') === 'switch') {
      var msg = dataOf(el);
      msg.type = msg.switchAct || 'switch';
      delete msg.switchAct;
      msg.id = el.id;
      msg.checked = el.checked === true;
      post(msg);
    }
  }
  document.addEventListener('input', onFieldEvent);
  document.addEventListener('change', onFieldEvent);

  document.addEventListener('keydown', function (event) {
    if (event.defaultPrevented || event.isComposing) return;
    var el = event.target;
    if (event.key === 'Enter' && el instanceof Element && el.hasAttribute('data-enter-act')) {
      event.preventDefault();
      post({ type: el.getAttribute('data-enter-act'), field: el.getAttribute('data-field') || '', value: fieldValue(el) });
      return;
    }
    if (event.key === 'Escape') {
      var holder = document.body.hasAttribute('data-escape-act') ? document.body : document.querySelector('[data-escape-act]');
      if (holder) post({ type: holder.getAttribute('data-escape-act') });
    }
  });

  // ---------- scroll: the window, and each log pane's follow ----------

  var scrollTimer = 0;
  window.addEventListener('scroll', function () {
    if (scrollTimer) return;
    scrollTimer = setTimeout(function () {
      scrollTimer = 0;
      writeState('scroll', { screen: screenKey(), y: window.scrollY });
    }, 150);
  }, { passive: true });

  document.addEventListener('scroll', function (event) {
    var pane = event.target;
    if (!(pane instanceof Element) || !pane.matches('[data-region="log"]')) return;
    if (pane.clientHeight === 0) return;
    var atBottom = pane.scrollHeight - pane.scrollTop - pane.clientHeight < FOLLOW_SLACK;
    pane.setAttribute('data-follow', atBottom ? 'true' : 'false');
  }, true);

  // ---------- patch: morph a region into its new HTML ----------

  function sameNode(a, b) {
    if (a.nodeType !== b.nodeType) return false;
    if (a.nodeType !== 1) return true;
    if (a.nodeName !== b.nodeName) return false;
    if ((a.id || '') !== (b.id || '')) return false;
    if (a.nodeName === 'INPUT' && a.type !== (b.getAttribute('type') || 'text').toLowerCase()) return false;
    return true;
  }
  function isLog(el) {
    return el.nodeType === 1 && el.getAttribute('data-region') === 'log';
  }
  function syncAttributes(from, to) {
    var keepFollow = isLog(from);
    Array.prototype.slice.call(from.attributes).forEach(function (attr) {
      if (keepFollow && attr.name === 'data-follow') return;
      if (!to.hasAttribute(attr.name)) from.removeAttribute(attr.name);
    });
    Array.prototype.slice.call(to.attributes).forEach(function (attr) {
      if (keepFollow && attr.name === 'data-follow') return;
      if (from.getAttribute(attr.name) !== attr.value) from.setAttribute(attr.name, attr.value);
    });
  }
  // A control's live state follows the host's HTML, with one exception: the
  // text of the field the person is typing in, which is theirs until they
  // leave it. A checkbox or switch has no half-typed state to protect, so it
  // follows the host even while focused -- a host that refuses or resets a
  // choice must be able to show it, or the page would claim a choice the host
  // does not hold.
  function syncFormState(from, to) {
    if (from.nodeName === 'INPUT' && (from.type === 'checkbox' || from.type === 'radio')) {
      var checked = to.hasAttribute('checked');
      if (from.checked !== checked) {
        from.checked = checked;
        noteHostValue(from);
      }
      return;
    }
    if (from === document.activeElement) return;
    var value = from.nodeName === 'INPUT' ? to.getAttribute('value') || '' : from.nodeName === 'TEXTAREA' ? to.textContent : null;
    if (value !== null && from.value !== value) {
      from.value = value;
      noteHostValue(from);
    }
  }
  function morphNode(from, to) {
    if (from.nodeType !== 1) {
      if (from.nodeValue !== to.nodeValue) from.nodeValue = to.nodeValue;
      return;
    }
    syncAttributes(from, to);
    syncFormState(from, to);
    if (isLog(from) && !to.firstChild) return;
    morphChildren(from, to);
  }
  function morphChildren(fromParent, toParent) {
    var incoming = Array.prototype.slice.call(toParent.childNodes);
    var cursor = fromParent.firstChild;
    incoming.forEach(function (next) {
      var match = null;
      if (cursor && sameNode(cursor, next)) {
        match = cursor;
      } else if (next.nodeType === 1 && next.id) {
        var found = document.getElementById(next.id);
        if (found && found.parentNode === fromParent && sameNode(found, next)) match = found;
      }
      if (match === null) {
        fromParent.insertBefore(next, cursor);
        return;
      }
      if (match === cursor) cursor = cursor.nextSibling;
      else fromParent.insertBefore(match, cursor);
      morphNode(match, next);
    });
    while (cursor) {
      var after = cursor.nextSibling;
      fromParent.removeChild(cursor);
      cursor = after;
    }
  }
  function focusKeyOf(el) {
    if (!el || el === document.body || !(el instanceof HTMLElement)) return null;
    var key = { el: el, id: el.id || '', field: el.getAttribute('data-field') || '',
      act: el.getAttribute('data-act') || '', value: el.getAttribute('data-value') || '',
      text: null, start: null, end: null };
    if (el.nodeName === 'INPUT' || el.nodeName === 'TEXTAREA') {
      key.text = el.value;
      try { key.start = el.selectionStart; key.end = el.selectionEnd; } catch (err) { key.start = null; }
    }
    return key.id || key.field || key.act ? key : null;
  }
  function findSuccessor(key) {
    if (key.id) return document.getElementById(key.id);
    if (key.field) return document.querySelector(attrSelector('data-field', key.field));
    var acts = document.querySelectorAll(attrSelector('data-act', key.act));
    for (var i = 0; i < acts.length; i += 1) {
      if ((acts[i].getAttribute('data-value') || '') === key.value) return acts[i];
    }
    return null;
  }
  function restoreFocus(key) {
    if (!key || (key.el.isConnected && document.activeElement === key.el)) return;
    var next = findSuccessor(key);
    if (!next || typeof next.focus !== 'function') return;
    if (key.text !== null && (next.nodeName === 'INPUT' || next.nodeName === 'TEXTAREA') &&
        next.type !== 'checkbox' && next.type !== 'radio') {
      next.value = key.text;
      noteHostValue(next);
    }
    next.focus({ preventScroll: true });
    if (key.start !== null && typeof next.setSelectionRange === 'function') {
      try { next.setSelectionRange(key.start, key.end); } catch (err) { /* not a text control */ }
    }
  }
  function applyPatch(regions) {
    var key = focusKeyOf(document.activeElement);
    Object.keys(regions).forEach(function (name) {
      var html = String(regions[name]);
      document.querySelectorAll(attrSelector('data-region', name)).forEach(function (region) {
        var template = document.createElement('template');
        template.innerHTML = html;
        try {
          morphChildren(region, template.content);
        } catch (err) {
          region.innerHTML = html;
        }
      });
    });
    restoreFocus(key);
    if (lastProgress) applyProgress(lastProgress);
    else syncTicker();
  }

  // ---------- progress ----------

  function part(region, name) {
    return region.querySelector(attrSelector('data-part', name));
  }
  function pad2(n) { return n < 10 ? '0' + n : String(n); }
  function formatElapsed(ms) {
    var total = Math.max(0, Math.floor(ms / 1000));
    var h = Math.floor(total / 3600);
    var m = Math.floor((total % 3600) / 60);
    var s = pad2(total % 60);
    return h > 0 ? h + ':' + pad2(m) + ':' + s : m + ':' + s;
  }
  function paintMeta(region) {
    var stepText = region.getAttribute('data-step-text') || '';
    var started = Number(region.getAttribute('data-started-at'));
    var ended = Number(region.getAttribute('data-ended-at'));
    var elapsed = '';
    if (region.hasAttribute('data-started-at') && isFinite(started)) {
      var end = region.getAttribute('data-state') === 'running' || !region.hasAttribute('data-ended-at') || !isFinite(ended)
        ? Date.now() : ended;
      elapsed = formatElapsed(end - started);
    }
    var step = part(region, 'step');
    var sep = part(region, 'sep');
    var clock = part(region, 'elapsed');
    if (step && step.textContent !== stepText) step.textContent = stepText;
    if (clock && clock.textContent !== elapsed) clock.textContent = elapsed;
    if (sep) sep.hidden = !(stepText && elapsed);
  }
  function paintProgress(region, p) {
    var state = String(p.state || 'running');
    region.setAttribute('data-state', state);
    if (typeof p.startedAt === 'number') region.setAttribute('data-started-at', String(Math.round(p.startedAt)));
    if (typeof p.endedAt === 'number') region.setAttribute('data-ended-at', String(Math.round(p.endedAt)));
    else if (state === 'running') region.removeAttribute('data-ended-at');
    else if (!region.hasAttribute('data-ended-at')) region.setAttribute('data-ended-at', String(Date.now()));
    region.setAttribute('data-step-text', typeof p.stepText === 'string' ? p.stepText : '');
    var title = part(region, 'title');
    if (title && typeof p.title === 'string' && title.textContent !== p.title) title.textContent = p.title;
    var status = part(region, 'status');
    var statusText = typeof p.status === 'string' ? p.status : '';
    if (status && status.textContent !== statusText) status.textContent = statusText;
    var bar = part(region, 'bar');
    var fill = part(region, 'fill');
    if (bar) {
      bar.setAttribute('data-state', state);
      if (typeof p.percent === 'number' && isFinite(p.percent)) {
        var pct = Math.min(100, Math.max(0, Math.round(p.percent)));
        bar.removeAttribute('data-indeterminate');
        bar.setAttribute('aria-valuenow', String(pct));
        bar.setAttribute('aria-valuetext', pct + '%, ' + statusText);
        if (fill) fill.setAttribute('data-percent', String(pct));
      } else {
        bar.setAttribute('data-indeterminate', 'true');
        bar.removeAttribute('aria-valuenow');
        bar.setAttribute('aria-valuetext', statusText);
        if (fill) fill.removeAttribute('data-percent');
      }
    }
    paintMeta(region);
  }
  function applyProgress(p) {
    lastProgress = p;
    document.querySelectorAll('[data-region="progress"]').forEach(function (region) { paintProgress(region, p); });
    syncTicker();
  }
  function syncTicker() {
    var running = document.querySelector('[data-region="progress"][data-state="running"][data-started-at]');
    if (running && !ticker) {
      ticker = setInterval(function () {
        document.querySelectorAll('[data-region="progress"]').forEach(paintMeta);
        syncTicker();
      }, 1000);
    } else if (!running && ticker) {
      clearInterval(ticker);
      ticker = 0;
    }
  }

  // ---------- log ----------

  function lineNode(line) {
    var row = document.createElement('div');
    row.className = 'mq-log-line';
    if (line && (line.tone === 'error' || line.tone === 'muted')) row.setAttribute('data-tone', line.tone);
    if (line && line.anchor === true) row.setAttribute('data-anchor', 'true');
    if (line && line.label) {
      var label = document.createElement('span');
      label.className = 'mq-log-label';
      label.textContent = String(line.label);
      row.appendChild(label);
    }
    var text = document.createElement('span');
    text.className = 'mq-log-text';
    text.textContent = line && line.text != null ? String(line.text) : '';
    row.appendChild(text);
    return row;
  }
  function applyLog(msg) {
    var lines = Array.isArray(msg.lines) ? msg.lines : [];
    document.querySelectorAll('[data-region="log"]').forEach(function (pane) {
      var follow = pane.getAttribute('data-follow') !== 'false';
      if (msg.reset) pane.textContent = '';
      var batch = document.createDocumentFragment();
      lines.forEach(function (line) { batch.appendChild(lineNode(line)); });
      pane.appendChild(batch);
      while (pane.childElementCount > LOG_LIMIT) pane.removeChild(pane.firstElementChild);
      if (scrollToAnchor(pane)) return;
      if (follow) pane.scrollTop = pane.scrollHeight;
    });
  }

  // ---------- host messages ----------

  window.addEventListener('message', function (event) {
    var msg = event.data;
    if (!msg || typeof msg !== 'object') return;
    if (msg.type === 'patch' && msg.regions && typeof msg.regions === 'object') applyPatch(msg.regions);
    else if (msg.type === 'progress') applyProgress(msg);
    else if (msg.type === 'log') applyLog(msg);
    else if (msg.type === 'setDisclosure' && typeof msg.id === 'string') {
      var toggle = disclosureToggle(msg.id);
      if (toggle) setDisclosure(toggle, msg.open === true);
    }
  });

  // ---------- load ----------

  var saved = readState().scroll;
  if (saved && saved.screen === screenKey() && typeof saved.y === 'number') window.scrollTo(0, saved.y);
  document.querySelectorAll('[data-region="progress"]').forEach(paintMeta);
  followLogsIn(document);
  syncTicker();
  post({ type: 'ready' });
})();
`;
