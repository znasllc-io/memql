# `clients/os` — the MemQL OS shell

The platform's second named front-door site (`os.<domain>`, memql#4705). An
empty desktop and launcher chrome, served as an ordinary `kind: spa` hosted
site by `component/edge`. Not a fork of the portal's pages or nav.

This PR lands the host, the seed, the OAuth redirect, and the empty chrome.
Slot occupants, the Profile module, and the theme store are memql#4706.
