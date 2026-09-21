# The browser QA harness

`clients/os/DESIGN.md`: **"the acceptance for any surface change under these
rules is rendered screenshots, both modes, empty and populated -- not the
diff."** jsdom performs no layout, resolves no custom property and never puts a
value beside its own label, so the suite can be entirely green over a surface
whose columns collide, whose action bar sits below the fold, or whose sentence
disagrees with the number in it.

This is that pass, for the storefront's Store surface (epic memql#5530).

## Running it

```bash
cd clients/os
npx vite --config qa/vite.config.ts --port 5199 --strictPort
```

Then capture. A one-shot headless screenshot is enough for any view that needs
no click, and every view here is reachable by URL for that reason:

```bash
google-chrome --headless=new --disable-gpu --no-sandbox --hide-scrollbars \
  --window-size=1400,900 --virtual-time-budget=12000 \
  --screenshot=store.png "http://localhost:5199/?view=store&mode=dark"
```

`view` is one of `overview`, `store`, `quiet`, `picker`, `readonly`, `hidden`;
`mode` is `dark` or `light`. **Take at least one narrow capture** (`820,760`):
two of the three real defects this harness found were invisible at 1400x900.

## What it is, and what it is not

It mounts the **real components** over the **suite's own** fixture connection
(`test/deployables/harness.tsx`), so a screenshot cannot disagree with what the
tests assert. `vitest`'s `vi` is shimmed for the same reason: a second copy of
the fake is a second thing to keep in step.

**It does not click.** The Store pane is reached in the product by clicking the
workspace's Store slot, which needs React's synthetic event system and so a
real driver. Each view is rendered directly here, in the wrapper the page gives
it. These captures judge LAYOUT, COLOUR, COPY and DENSITY; the navigation that
reaches them is `test/deployables/store.test.tsx`'s.

## Three things it gets wrong by default, all silent

1. **The connection swap needs `enforce: "pre"`, on the RESOLVED path.** Vite's
   own resolver is a `pre` plugin, so a plain `resolveId` never sees the id; and
   `src/live/connection` is imported both as `"../../../live/connection"` and as
   `"./connection"`, so any `endsWith` match on the specifier catches one and
   misses the other. Missing one means the app holds a second, empty seam and
   every live list says "Not connected to the cluster".

2. **`test/seededAccess.ts` reads `dsl/rbac/seeds.memql` with `node:fs`, at
   module load.** In a browser that is an unresolved import rather than a thrown
   error, so the page is blank and `window.onerror` is silent. The config
   inlines the seeds text on the Node side and leaves the module's own parse
   alone -- a browser copy of "which role holds what" would be a second copy of
   the policy in a second language.

3. **The providers are the suite's.** `withSession` installs the session, the
   shell provider and the role's seeded capability set together; `useOs` throws
   outside its provider, and without the role ladder the whole app renders
   read-only with nothing saying why.

## What it found

Three defects, all invisible to 3,344 green cases: a pane that painted 800px of
dead space beside a 600px stripe (DESIGN.md rule 9), a sentence that disagreed
with its own number ("1 of the 3 scopes ... are not granted"), and a choice
label with nested parentheses.
