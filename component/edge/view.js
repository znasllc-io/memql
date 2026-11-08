(() => {
  if (window.__memqlView) return;
  window.__memqlView = true;
  let last = location.pathname;
  const report = () => {
    const p = location.pathname;
    if (p === last) return;
    last = p;
    try { navigator.sendBeacon("/_memql/view", p); } catch {}
  };
  const push = history.pushState;
  history.pushState = function (...args) { const out = push.apply(this, args); report(); return out; };
  addEventListener("popstate", report);
})();
