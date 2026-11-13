# Profile and app relocation visual acceptance

Run `npx vite --config qa/vite.config.ts --host 127.0.0.1 --port 5199` from clients/os. Open `/accounts-shell-profile.html?mode=dark` and repeat in light mode, a narrow phone viewport with `&layout=phone`, and reduced motion. `&role=viewer` supplies the ordinary-user door set; `&empty=true` supplies the existing quiet Profile fixture. Fixtures are local and intercept HTTP; they do not grant access, call providers or simulate account switching.

- The picture opens one Profile directly. Profile is absent from Launcher, dock pin options and app selector choices.
- Review populated and quiet Profile, Appearance, About, passkeys/sessions, access tokens and personal Settings; sign out ends only the fixture.
- Review owner navigation: Identity contains policy/requests/keys/service tokens/audit; Cluster has facts/benchmarks, Concepts has Language, Users has Access, Settings has Data and installation services.
- Review viewer navigation: Profile remains reachable, administrative Identity and Settings do not appear. No placeholder account switch is offered; server account-bound sessions remain separate program work.
- Review `/accounts-shell-shared.html` for paging/retry, entry transition and ranked-list More arrow cases described by the adjacent shared QA guide.

Source unit tests cover exact custom grant conjunctions, legacy deep entries and revoked destinations; this fixture is visual evidence, not backend permission or provider acceptance proof.

For page-local reduced motion append `&motion=reduced` (also supported on the shared entry fixture). It overrides only that fixture's `matchMedia` and applies the actual production CSS rules from its reduced-motion blocks; it does not alter macOS or browser preferences. The QA build label explicitly reads `local-accounts-shell-qa`.
