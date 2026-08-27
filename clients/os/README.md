# `clients/os` — the MemQL OS shell

The platform's second named front-door site (`os.<domain>`, memql#4705).
Slots, research, Profile, and a coming-soon tile occupy the empty chrome
(memql#4706). Not a fork of the portal's pages or nav.

- Slots (`data-os-slot="a"|"b"`) mount one module React root each. Cap 2
  on desktop/iPad. Phone has no slots.
- Research is chrome (strip on desktop/iPad, sheet on phone). Not a module.
- Profile is the one module. MyAccess data only. Sign out stays in chrome.
- Coming-soon tile is visible on desktop/iPad and does not open a store.
