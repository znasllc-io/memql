const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const vm = require('node:vm');
const source = readFileSync(__dirname + '/site_refresh.js', 'utf8');
const A = 'a'.repeat(32), B = 'b'.repeat(32), C = 'c'.repeat(32);
async function browser({ stored = 0, blocked = false, url = 'https://shop.example.test/cart?buyer=one#items' } = {}) {
    let now = 100000, id = 0, latest = A, fail = false, requests = 0;
    const timers = new Map(), events = {}, reloads = [];
    const location = { href: url, get pathname() { return new URL(this.href).pathname; }, replace(value) { reloads.push(value); } };
    const history = { state: null, replaceState(state, unused, url) { this.state = state; location.href = new URL(url, location.href).href; return 'kept'; }, pushState(state, unused, url) { this.state = state; location.href = new URL(url, location.href).href; return 'kept'; } };
    const on = (name, fn) => (events[name] ??= []).push(fn);
    const document = { currentScript: { getAttribute: () => A }, visibilityState: 'visible', addEventListener: on };
    const window = { addEventListener: on };
    function timer(fn, ms, repeat = 0) { timers.set(++id, { fn, at: now + ms, repeat }); return id; }
    const context = vm.createContext({ window, document, location, history, URL, AbortController,
        Date: { now: () => now }, sessionStorage: { getItem() { if (blocked)
                throw Error('blocked'); return stored; }, setItem(k, v) { if (blocked)
                throw Error('blocked'); stored = Number(v); } },
        setTimeout: (fn, ms) => timer(fn, ms), clearTimeout: id => timers.delete(id), setInterval: (fn, ms) => timer(fn, ms, ms),
        fetch: async (url, options) => { requests++; assert.equal(url, '/runtime-config.json'); assert.equal(options.method, 'HEAD'); assert.equal(options.cache, 'no-store'); if (fail)
            throw Error('offline'); return { ok: true, headers: { get: () => latest } }; }
    });
    const settle = async () => { for (let i = 0; i < 8; i++)
        await Promise.resolve(); };
    vm.runInContext(source, context);
    await settle();
    async function tick(ms) { const end = now + ms; for (;;) {
        let entry = [...timers].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
        if (!entry)
            break;
        const [key, t] = entry;
        now = t.at;
        timers.delete(key);
        if (t.repeat)
            timers.set(key, { ...t, at: now + t.repeat });
        t.fn();
        await settle();
    } now = end; await settle(); }
    return { document, history, location, reloads, tick, get requests() { return requests; }, set latest(v) { latest = v; }, set fail(v) { fail = v; }, emit: async (name, event = {}) => { for (const fn of events[name] ?? [])
            fn(event); await settle(); }, rerun: () => vm.runInContext(source, context) };
}
test('unchanged pages and duplicate installation do not reload', async () => { const b = await browser(); b.rerun(); await b.tick(30000); await b.emit('pageshow', { persisted: true }); assert.deepEqual(b.reloads, []); assert.equal(b.requests, 2); });
test('a restored page confirms a changed release and preserves its route', async () => { const b = await browser(); b.latest = B; await b.tick(2100); await b.emit('pageshow', { persisted: true }); assert.equal(b.reloads.length, 0); await b.tick(2500); assert.equal(b.reloads.length, 1); const u = new URL(b.reloads[0]); assert.equal(u.pathname, '/cart'); assert.equal(u.searchParams.get('buyer'), 'one'); assert.equal(u.hash, '#items'); assert.ok(u.searchParams.get('__memql_reload')); });
test('visible idle pages update automatically', async () => { const b = await browser(); b.latest = B; await b.tick(32500); assert.equal(b.reloads.length, 1); });
test('hidden tabs pause and check again when visible', async () => { const b = await browser(); b.document.visibilityState = 'hidden'; b.latest = B; await b.tick(60000); assert.equal(b.requests, 1); b.document.visibilityState = 'visible'; await b.emit('visibilitychange'); await b.tick(2500); assert.equal(b.reloads.length, 1); });
test('unsaved edits survive; leaving the route lets an SPA update', async () => { const b = await browser(); await b.emit('input', { target: { closest: () => true } }); b.latest = B; await b.tick(32500); assert.equal(b.reloads.length, 0); await b.tick(2100); assert.equal(b.history.pushState({ value: 1 }, '', '/next'), 'kept'); await b.tick(2500); assert.equal(b.reloads.length, 1); assert.equal(new URL(b.reloads[0]).pathname, '/next'); });
test('offline failure keeps the working page and online recovery retries', async () => { const b = await browser(); b.fail = true; b.latest = B; await b.tick(60000); assert.equal(b.reloads.length, 0); b.fail = false; await b.tick(2100); await b.emit('online'); await b.tick(2500); assert.equal(b.reloads.length, 1); });
test('alternating replicas cannot cause an immediate reload loop', async () => { const b = await browser(); b.latest = B; await b.tick(2100); await b.emit('focus'); b.latest = C; await b.tick(2500); b.latest = A; await b.tick(2500); assert.equal(b.reloads.length, 0); });
test('cooldown survives page loads and blocked browser storage', async () => { for (const blocked of [false, true]) {
    const b = await browser({ blocked, url: 'https://shop.example.test/?__memql_reload=99000' });
    b.latest = B;
    await b.tick(32500);
    assert.equal(b.reloads.length, 0);
    await b.tick(30000);
    assert.equal(b.reloads.length, 1);
} });
test('missing version headers never reload a page', async () => { const b = await browser(); b.latest = null; await b.tick(60000); assert.equal(b.reloads.length, 0); });
test('an invalid or future loop marker cannot disable updates', async () => { for (const stamp of ['Infinity', '999999999999999', '-1']) {
    const b = await browser({ stored: stamp, url: 'https://shop.example.test/?__memql_reload=' + stamp });
    b.latest = B;
    await b.tick(32500);
    assert.equal(b.reloads.length, 1);
} });
test('hash and query routers release deferred updates after navigation', async () => {
    for (const route of ['/cart?step=two#items', '/cart?buyer=one#checkout']) {
        const b = await browser();
        await b.emit('input', { target: { closest: () => true } });
        b.latest = B;
        await b.tick(32500);
        assert.equal(b.reloads.length, 0);
        await b.tick(2100);
        b.history.pushState({}, '', route);
        await b.tick(2500);
        assert.equal(b.reloads.length, 1);
        assert.equal(new URL(b.reloads[0]).hash, new URL(route, b.location.href).hash);
    }
});
