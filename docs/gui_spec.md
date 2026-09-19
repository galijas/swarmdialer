# SwarmDialer GUI Spec & Implementation Plan

User-authored spec (2026-09-18), plus the decisions made when clarifying
it and the resulting implementation plan. This is the source of truth for
building the web GUI — read this alongside `PROJECT_STATE.md` (overall
project state) and `pbxware_api_reference.md` (API details this spec
depends on: License, Trunks, DIDs sections).

**The user said this spec is big and may be refined later** as they
remember missing pieces — treat it as the current best version, not final.

## Original spec (as given)

> there will be configurable sections of the page, and there will be
> another section where it will display live status of the calls some
> kind of graphic tracking calls which can be switched to log style of
> call tracking.
>
> before we get to the dashboard of the gui, create another tab on the
> page called setup wizard. setup wizard will be shown initially and will
> be used to connect pbxware instances (give api keys, ip addresses etc).
> once the setup wizard is completed, then dashboard is shown.
>
> as mentioned, there will be setup wizard tab and dashboard tab. once the
> setup wizard is completed, user can switch tab to change setup wizard
> configuration (api keys and other).
>
> setup wizard will have the following steps:
>
> - connect first pbxware (api key, server ip/dns) — write access
>   confirmation when the next button is pressed, decline moving to the
>   next step if the api key or ip is not correct
> - create a tenant and extensions on the first server — specify number
>   of extensions (max: 500 → **revised to 1000**, see Decisions);
>   depending on the edition of pbxware connected there is a possibility
>   we need to skip tenant creation (if the edition is Contact Centre or
>   Business) and just continue with extension creation; also write a
>   note notifying the user that they need to ensure the license allows
>   creating this number of extensions; show a log for extension creation
>   progress before moving to the next step
> - next step: offer connecting the next pbxware instance — a "complete"
>   button or "add another instance" button
> - if "complete": finish the wizard, go to dashboard
> - if "add another instance": repeat the connection/provisioning steps
>   for instance 2, then **add a step to create trunks between the
>   instances and unique DIDs mapped one-to-one to each extension
>   created** (originally "select existing trunk," revised to "actually
>   create it," see Decisions)
> - after trunk+DID creation, offer "complete" again. No third instance
>   for now (could be added later).
>
> dashboard:
>
> - bottom: live status section, toggle log style / graphical style
>   (no exact spec for the graphic — "put here what you think is
>   appropriate"). Include stats: total calls, total RTP calls, etc.
> - above that, main section:
>   - status of connected servers (names, IPs, extension counts, DID
>     counts, etc.)
>   - local dialer section (calls within one instance)
>   - remote dialer section (calls between the two added instances) —
>     greyed out if no second instance
>   - both sections: increment buttons +25/+50/+100/+250/+500 to
>     start/add calls, plus call-length config and an RTP on/off toggle
>     (silence audio if on)
>   - clicking an increment button asks for confirmation, then starts
>     that many calls

## Decisions made when clarifying (2026-09-18)

1. **Trunk creation, not selection.** Originally planned as "select an
   already-existing trunk" (2026-09-17 future-scope note) — the user
   changed their mind: SwarmDialer should actually *create* the trunk on
   both instances via the API (confirmed feasible — see
   `pbxware_api_reference.md`'s Trunks section), and set it as the
   default trunk for the created tenant on each side. (Exact mechanism
   for "default trunk" not yet confirmed — see that doc's open question
   about `primary_trunk` on extensions vs. a tenant-wide setting.)
2. **Extension max raised to 1000** (was 500) — 500 only supports 250
   concurrent calls at 2 extensions/call, which doesn't cover the
   dashboard's own "+500" button. 1000 extensions supports up to 500
   concurrent calls, matching the largest increment button.
3. **Increment buttons are additive.** Clicking +50 while 100 calls are
   already running results in 150 total, not a fresh batch of 50. This
   drives the orchestrator rework (see Implementation plan) — it can no
   longer be a single one-shot blocking `Run()` call; it needs to support
   adding more pairs to an already-running pool.
4. **Edition/Tenant-Mode detection**: solved via `pbxware.license.info`,
   which returns `"Edition": "Multi-Tenant"` (or presumably something
   else for other editions — exact other-edition strings not documented,
   see gap noted in `pbxware_api_reference.md`). Treat non-Multi-Tenant
   as "skip tenant creation, provision extensions at system level
   directly." No manual edition-picker needed in the wizard unless this
   turns out to be wrong in practice for a non-Multi-Tenant instance we
   test against later.

## 🎯 Portability requirement (2026-09-18) — read this before building anything

**SwarmDialer is a portable, redeployable tool, not a fixture of this one
VPS/network.** The intended workflow going forward: deploy a fresh Ubuntu
VPS on whatever network needs testing, `git clone` this repo, install/build
it, then use the GUI's Setup Wizard to enter that environment's own PBXware
access details from scratch. **The current dev/test PBXware instances
(10.1.100.208, 10.1.100.215, and all the credentials in `PROJECT_STATE.md`)
are purely our own build/test scaffolding — none of that is part of the
"product," and none of it should end up hardcoded anywhere a fresh
deployment would inherit it.** `PROJECT_STATE.md` and `extensions*.json` are
already correctly gitignored for exactly this reason; that's confirmed
correct, not something to loosen.

Concrete implications for what we're about to build:
- **No hardcoded IPs/keys in the real product's code path.** The scratch
  CLIs (`cmd/sip-test`, `cmd/call-test`, `cmd/loadtest`) have flag
  *defaults* pointing at our test IPs (`10.1.100.208`/`.215`) — that's fine
  for those dev tools, but the GUI/wizard must never inherit or default to
  them. Every PBXware connection detail comes from what the user types into
  the wizard on that specific deployment, every time.
- **Local IP handling needs to be automatic, not manual entry.** Our test
  tools take `-local-ip` as an explicit flag because *we* know our own VPS's
  IP. A fresh deployment shouldn't require the user to look up and type
  their new VPS's IP address as a setup step. Auto-detect it instead (e.g.,
  determine the local interface IP that would route to the PBXware server
  being configured — a well-known trick: open a UDP socket "connected" to
  that server's IP and read back the local address the OS picked, no
  packets need to actually be sent). Only fall back to asking the user if
  auto-detection is ambiguous (multiple interfaces, unusual NAT setup).
- **Needs a real install path**, not "read PROJECT_STATE.md and run go
  build by hand" — that was fine for our own dev loop, but "quickly deploy
  when needed" implies something closer to a single setup script:
  install Go if missing, build all binaries, maybe set up a systemd service
  for the GUI server. Worth adding an `install.sh` (or a `Makefile`) as a
  deliverable — not built yet, add to the implementation plan below.
- **Config persistence is per-deployment.** The JSON config file (Decisions
  section below) lives on *that* VPS, generated fresh by *that* wizard run
  — never shipped in the repo, never assumed to pre-exist.

## Decisions made re: hosting/security (2026-09-18)

- **Plain HTTP**, no TLS. Internal-only tool on the private network,
  matches how everything else in this project has been handled (API keys
  already sit in plaintext in `PROJECT_STATE.md` by design).
- **No authentication/access control** on the GUI page itself. Same
  internal-only trust model.
- **Engine**: plain HTML/CSS/vanilla JS served by the existing Go
  `net/http` server — no separate frontend build pipeline/Node toolchain.
  Matches the project's "single Go binary, simple deployment" philosophy
  throughout.
- **Live updates**: WebSocket push (not polling) for the live-status
  section, to handle up to 500 concurrent pairs' worth of updates
  smoothly.
- **Config persistence**: a JSON file on the SwarmDialer VPS holding
  connected-server info (API keys, provisioned extensions, DIDs) so the
  dashboard survives a process restart.

## Things to solve while building (not blocking, but real gaps)

- **Extension pool bookkeeping across local/remote dialers**: an
  extension not currently on a call should be usable by either the local
  or remote dialer — needs a shared "available extensions" pool per
  instance with reservation/release around each call's lifecycle, rather
  than the local and remote dialers each assuming exclusive access.
- ~~**DID `destination` field**~~ — resolved: it's the plain extension
  number, confirmed live via `did.list`.
- ~~**Outbound trunk routing mechanism**~~ — resolved, and simpler than
  expected: no default-trunk config needed at all. Create the trunk + one
  DID per extension; dialing the DID's exact number just works via
  PBXware's own dialplan fallthrough. Verified end-to-end (dialed a DID
  from a different tenant's extension, it rang and answered on the target
  tenant's extension) — though only tested with both "tenants" on the same
  physical instance so far; re-verify across two real separate PBXware
  boxes once available. See `pbxware_api_reference.md`'s Trunks section
  for the full trace.
- **"Write access confirmation" in wizard step 1**: PBXware API keys
  appear to be all-or-nothing admin keys (no read/write scoping we've
  seen), so in practice this probably just means "confirm the key/IP work
  at all" via a lightweight read call (e.g. `tenant.list` or
  `license.info`) — true write access isn't really separately testable
  without actually creating something. Treat "read call succeeds" as
  sufficient confirmation unless this proves wrong in practice.

## Implementation plan (phased) — ALL PHASES BUILT 2026-09-18

1. ~~**Backend config/session store**~~ — done: `internal/store` (JSON
   file, `swarmdialer_config.json` by default, gitignored — see its
   pattern already established for `PROJECT_STATE.md`/`extensions*.json`).
2. ~~**Wizard backend endpoints**~~ — done: `internal/wizard`
   (`TestConnection`, `Provision` with live progress, `ConnectServers` for
   trunk+DIDs) plus `internal/netutil.DetectLocalIP` for the
   no-manual-entry local IP requirement.
3. ~~**Orchestrator rework**~~ — done: `internal/orchestrator/session.go`.
   `Session` (local, one pool) and `NewRemoteSession` (two pools + DID
   lookup) both support `AddCalls` growing an already-running pool.
4. ~~**Frontend**~~ — done: `cmd/gui/web/` (plain HTML/CSS/vanilla JS,
   embedded into the binary via `go:embed`). Setup Wizard tab + Dashboard
   tab per spec.
5. ~~**Install/deploy script**~~ — done: `install.sh` (installs
   ca-certificates + Go if missing, builds all binaries, optional
   `--systemd` flag).

**Validation performed** (see PROJECT_STATE.md's 2026-09-18 progress log
for the full trail): every backend piece was tested against the real
PBXware instance, including the full HTTP+WebSocket API surface (not just
internal Go calls) — test-connection, provision-with-progress, the
dial/status/websocket loop, and RTP flowing for the full correct call
duration. Trunk+DID creation and cross-tenant dialing (local mode and
`NewRemoteSession`) were both validated against real tenants on the one
physical PBXware instance available.

**Not yet validated — genuinely needs the user**:
- **The actual frontend has never been opened in a browser.** All of its
  API calls were verified correct via direct HTTP/WebSocket testing, and
  the JS was written carefully against that verified contract, but visual
  layout, click-through flow, and any JS typos that only manifest in a
  real browser (vs. `go vet`, which doesn't check JS at all) are
  unverified. This is the most important thing to test first.
- Remote calling (`NewRemoteSession`) was validated using two tenants on
  the *same* physical PBXware instance (no second real instance available
  to us) — the mechanics are proven, but true cross-machine trunk/DID
  behavior isn't yet confirmed.
- `install.sh` was written carefully against the same steps done manually
  throughout this project, but hasn't been run against a genuinely fresh
  VPS end-to-end (only the pieces it automates were tested individually).

Not yet started as of 2026-09-18 — see `PROJECT_STATE.md`'s roadmap for
current status.
