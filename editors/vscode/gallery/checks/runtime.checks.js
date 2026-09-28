// The host-to-page half of the page runtime (src/webview/ui/runtime.ts),
// checked against a real DOM in headless Chrome: `npm run gallery:check`.
//
// Each check plays the host with window.postMessage -- exactly the messages
// LiveView sends -- or plays the person with focus, typing and clicks, then
// reads the page. The outcome is written into the page as
// <ol id="checks"><li data-check="pass|fail">...</li></ol>, which
// gallery/check.mjs reads back from Chrome's --dump-dom and turns into an exit
// code. The page it runs in is built by runtimePage.ts; `window.__variants`
// holds the re-renders a check patches in, and `window.__posts` every message
// the page posted to the host.

(function () {
  'use strict';
  var results = [];
  var V = window.__variants;
  var posts = window.__posts;

  function check(name, pass, detail) {
    results.push({ name: name, pass: pass === true, detail: detail === undefined ? '' : JSON.stringify(detail) });
  }
  function sleep(ms) { return new Promise(function (resolve) { setTimeout(resolve, ms); }); }
  function host(msg) { window.postMessage(msg, '*'); return sleep(20); }
  function patchBody(name) { return host({ type: 'patch', regions: { body: V[name] } }); }
  function fire(el, type) { el.dispatchEvent(new Event(type, { bubbles: true })); }
  function type(el, text) { el.value = text; fire(el, 'input'); }
  function lastPost() { return posts[posts.length - 1]; }
  function byId(id) { return document.getElementById(id); }
  function part(name) { return document.querySelector('[data-part="' + name + '"]'); }

  async function run() {
    await sleep(20);
    check('the page posts ready on load', posts.length === 1 && posts[0].type === 'ready', posts);

    // ---- a patch never takes the field being typed in away from the person ----
    var name = byId('f-name');
    name.focus();
    type(name, 'hello world');
    name.setSelectionRange(3, 5);
    await patchBody('nameError');
    check('a patch keeps focus in the field being typed in', document.activeElement === byId('f-name'),
      document.activeElement && document.activeElement.id);
    check('a patch keeps the half-typed value', byId('f-name').value === 'hello world', byId('f-name').value);
    check('a patch keeps the caret', byId('f-name').selectionStart === 3 && byId('f-name').selectionEnd === 5,
      [byId('f-name').selectionStart, byId('f-name').selectionEnd]);
    check('a patch lands the new markup around it', document.querySelector('.mq-field-error') !== null);
    name.blur();

    await patchBody('otherX');
    check('a field nobody is typing in takes the host value', byId('f-other').value === 'X', byId('f-other').value);

    // ---- a value the host wrote is not mistaken for one already posted ----
    var phrase = document.querySelector('[data-field="phrase"]');
    phrase.focus();
    type(phrase, 'delete memql data');
    phrase.blur();
    var other = byId('f-other');
    other.focus();
    type(other, 'abc');
    other.blur();
    await patchBody('otherCleared');
    check('the host can clear a field', byId('f-other').value === '', byId('f-other').value);
    var before = posts.length;
    other.focus();
    type(other, 'abc');
    check('pasting back the value the host cleared still reaches the host',
      posts.length === before + 1 && lastPost().type === 'input' && lastPost().value === 'abc', posts.slice(before));
    other.blur();

    // ---- switches ----
    var sw = byId('s1');
    before = posts.length;
    document.querySelector('.mq-switch-label').click();
    await sleep(10);
    check('a click on the switch words toggles it and posts once',
      sw.checked === true && posts.length === before + 1, posts.slice(before));
    check('the switch posts its act, id, state and data',
      lastPost().type === 'shared' && lastPost().id === 's1' && lastPost().checked === true && lastPost().value === 'k3d', lastPost());
    sw.focus();
    await patchBody('switchOff');
    check('the host can turn a focused switch back off', byId('s1').checked === false, byId('s1').checked);
    sw.blur();

    // ---- progress ----
    await host({ type: 'progress', percent: 60, status: 'Creating the cluster', stepText: 'Step 10 of 16', startedAt: Date.now() - 5000, state: 'running' });
    var bar = part('bar');
    check('progress moves the bar by data-percent', part('fill').getAttribute('data-percent') === '60', part('fill').getAttribute('data-percent'));
    check('progress sets the value for assistive tech',
      bar.getAttribute('aria-valuenow') === '60' && bar.getAttribute('aria-valuetext') === '60%, Creating the cluster',
      [bar.getAttribute('aria-valuenow'), bar.getAttribute('aria-valuetext')]);
    check('progress sets the status line', part('status').textContent === 'Creating the cluster', part('status').textContent);
    check('nothing on the page carries a style attribute', document.querySelectorAll('[style]').length === 0);
    await patchBody('staleStatus');
    check('the latest progress survives a patch rendered from older state',
      part('fill').getAttribute('data-percent') === '60' && part('status').textContent === 'Creating the cluster',
      [part('fill').getAttribute('data-percent'), part('status').textContent]);
    var first = part('elapsed').textContent;
    check('the meta line reads step and elapsed time', part('step').textContent === 'Step 10 of 16' && /^0:0[5-6]$/.test(first),
      [part('step').textContent, first]);
    await sleep(2100);
    var ticked = part('elapsed').textContent;
    check('the clock ticks while running', ticked !== first, [first, ticked]);
    await host({ type: 'progress', status: 'Starting', state: 'running', startedAt: Date.now() });
    check('no percent makes the bar indeterminate and claims no value',
      part('bar').hasAttribute('data-indeterminate') && !part('bar').hasAttribute('aria-valuenow') && !part('fill').hasAttribute('data-percent'));
    await host({ type: 'progress', percent: 100, status: 'MemQL is installed', state: 'done', startedAt: Date.now() - 7000, endedAt: Date.now() });
    var settled = part('elapsed').textContent;
    await sleep(2100);
    check('the clock stops once the run has settled', part('elapsed').textContent === settled && settled === '0:07',
      [settled, part('elapsed').textContent]);

    // ---- the log ----
    var pane = byId('log1');
    var lines = [];
    for (var i = 0; i < 200; i += 1) lines.push({ label: 'Creating the cluster', text: 'line ' + i });
    await host({ type: 'log', lines: lines });
    check('log lines are appended', pane.childElementCount === 200, pane.childElementCount);
    check('the pane follows its tail', pane.scrollHeight - pane.scrollTop - pane.clientHeight < 2,
      [pane.scrollHeight, pane.scrollTop, pane.clientHeight]);
    pane.scrollTop = 10;
    fire(pane, 'scroll');
    await sleep(20);
    check('scrolling up stops the follow', pane.getAttribute('data-follow') === 'false', pane.getAttribute('data-follow'));
    await host({ type: 'log', lines: [{ text: 'a <b>bold</b> line', tone: 'error' }] });
    check('a new line does not move a pane the person scrolled', pane.scrollTop === 10, pane.scrollTop);
    check('line text is text, never markup', pane.querySelector('b') === null && pane.lastElementChild.textContent.indexOf('<b>') >= 0);
    check('a line keeps its tone', pane.lastElementChild.getAttribute('data-tone') === 'error');
    pane.scrollTop = pane.scrollHeight;
    fire(pane, 'scroll');
    await sleep(20);
    check('scrolling back to the bottom resumes the follow', pane.getAttribute('data-follow') === 'true');
    await patchBody('staleStatus');
    check('a patch whose pane is empty keeps the streamed lines', byId('log1').childElementCount === 201, byId('log1').childElementCount);
    await host({ type: 'log', lines: [{ text: 'fresh' }], reset: true });
    check('reset replaces the lines', pane.childElementCount === 1 && pane.textContent === 'fresh', pane.childElementCount);

    // ---- disclosures ----
    var toggle = document.querySelector('[data-disclosure="logs"]');
    toggle.click();
    check('a disclosure closes at once, on the page', byId('logs').hidden === true && toggle.getAttribute('aria-expanded') === 'false');
    check('and tells the host which way', lastPost().type === 'toggleLogs' && lastPost().disclosure === 'logs' && lastPost().open === false, lastPost());
    await host({ type: 'setDisclosure', id: 'logs', open: true });
    check('the host can open it', byId('logs').hidden === false && toggle.getAttribute('aria-expanded') === 'true');

    // ---- focus survives a replaced control; keyed nodes are kept ----
    phrase = document.querySelector('[data-field="phrase"]');
    phrase.focus();
    phrase.value = 'abc';
    phrase.setSelectionRange(1, 1);
    await patchBody('phrasePassword');
    var successor = document.querySelector('[data-field="phrase"]');
    check('a replaced control hands focus to its successor by data-field',
      successor.type === 'password' && document.activeElement === successor, document.activeElement && document.activeElement.outerHTML.slice(0, 80));
    check('and the typed value goes with it', successor.value === 'abc', successor.value);
    successor.blur();
    var kept = byId('i-a');
    await patchBody('reordered');
    var ids = Array.prototype.map.call(byId('list').children, function (c) { return c.id; }).join(',');
    check('a patch reorders by id', ids === 'i-c,i-a,i-b,i-d', ids);
    check('and keeps the node it moved', byId('i-a') === kept && document.querySelectorAll('#i-a').length === 1);
    var pick = document.querySelector('[data-act="pick"]');
    pick.focus();
    await patchBody('relabelled');
    check('a focused button keeps focus through its relabel', document.activeElement === pick && pick.textContent === 'Picked', pick.textContent);

    // ---- keys and clicks ----
    before = posts.length;
    document.body.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    check('Escape posts the page escape act', posts.length === before + 1 && lastPost().type === 'back', lastPost());
    document.querySelector('[data-act="cancel"]').click();
    check('a click posts its act', lastPost().type === 'cancel', lastPost());
  }

  function report(error) {
    var list = document.createElement('ol');
    list.id = 'checks';
    results.forEach(function (r) {
      var item = document.createElement('li');
      item.setAttribute('data-check', r.pass ? 'pass' : 'fail');
      item.textContent = r.name + (r.pass || r.detail === '' ? '' : ' :: ' + r.detail);
      list.appendChild(item);
    });
    if (error) {
      var item = document.createElement('li');
      item.setAttribute('data-check', 'fail');
      item.textContent = 'the checks threw :: ' + String(error && error.stack || error);
      list.appendChild(item);
    }
    document.body.appendChild(list);
  }

  run().then(function () { report(null); }, report);
})();
