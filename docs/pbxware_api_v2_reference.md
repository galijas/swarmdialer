# PBXware API v2 Reference (for SwarmDialer)

Extracted from the Bicom Systems Developer Portal
(https://developers.bicomsystems.com/docs/pbxware/), a Docusaurus site whose
endpoint pages render their request/response schema tables client-side —
a plain fetch only reliably yields the HTTP method/path, high-level
description, response status codes, and sibling-endpoint links from the
sidebar. Full field-level schemas below come from browser-saved copies of
each page (Ctrl+S, "Webpage, Complete", saved to
`~/claude/pages/api v2 pages/`) — the saved DOM has the client-rendered
tables baked in as static markup, which a plain fetch never triggers.
Sections still marked "not yet confirmed" need the same treatment.

This is the sister document to `pbxware_api_reference.md` (v1/legacy) —
see that file for v1's `action=pbxware.*` HTTP GET/POST convention. v1 and
v2 coexist and both stay supported long-term (confirmed with Bicom
2026-09-24); this file exists to work out, endpoint by endpoint, which
API SwarmDialer should actually use for a given operation, not to migrate
wholesale. See the final section for concrete replace/keep recommendations.

## Live findings on 8.2, new hardware (2026-09-25) — read first

Tested against fresh MT (10.1.101.11) and CC (10.1.101.12) instances:

- **Tenant create** (`POST /system/v2/tenants`): 201 in ~8s, with
  `channels_limit` (1000 on all seven pools) and `codecs` applied in the
  same call. Extensions created right after it register and call with
  **no resave**. The >60s `tenant create` daemon timeout seen earlier was
  the old hardware.
- **Batch extension create works, but the docs are wrong in two places:**
  each item's `id` must be an **integer** (a string like `"req-1"` fails
  the whole request with 400 "invalid request body"), and the response is
  a **bare JSON array** of `{id, status, body}`, not `{"responses": [...]}`.
  500 extensions in one request took ~20s (~40ms each).
- `authentication.pin` is required on create (validation
  `required_str`), despite not being listed under `authentication` in
  the docs.
- Per-extension `call_control.incoming_limit`/`outgoing_limit` > 999 is
  rejected by v2 too, same as v1.
- The create response is only `{id, name, number}`. The SIP username is
  derived by PBXware: tenant code + number on MT (`9001001`), just the
  number on CC. Read it back with `GET /extensions/number/{n}`.
- `GET /extensions/default` needs `?uad_id=<id>`.
- Package create rejects `voicemails < extensions`.
- Right after a batch of extensions is created, PBXware answers every
  REGISTER with 403 for a while (~24s after 1000 extensions, also for
  extensions created a minute earlier). SwarmDialer's wizard now waits
  for a real REGISTER to succeed before finishing.
- Tenant delete is synchronous (204, ~2.5s, extensions go with it).
  Extension delete is ~300-500ms each; concurrent deletes largely fail
  with 500 "Unable to delete extension" (77% at 10 at once, ~10% at 2-3).
- Both editions' system record (id 1) default to 246 channels and a
  codec list without opus/g729.
- **Trunks (2026-09-26):** `PATCH /system/v2/trunks/{id}` is a true
  partial update (full config diffed before/after; only `codecs.allowed`
  changed). PBXware writes the change to `pjsip.conf` immediately, but
  Asterisk only reloads PJSIP config on PBXware's own **1-minute cycle**
  (reloads logged exactly 60s apart), so a change takes effect after
  anywhere from a few seconds to ~60s. Neither API can force a reload.
- When PBXware answers a call arriving over a trunk, its SDP answer lists
  codecs in the **trunk's configured order**, not the caller's: with ulaw
  first, a G.729 call was negotiated as ulaw on the trunk and then failed
  with 603 (no G.729 transcoder), and G.722/Opus were transcoded. The
  endpoint's `codec_prefs_*` (already `prefer:pending`) and a
  `keep:first` override via `additional.additional_config` made no
  difference. SwarmDialer works around it by putting the batch's codec
  first on both trunks before each remote batch.
- PBXware has no G.729 transcoder (`core show translation`: no path to or
  from slin), so G.729 calls can't be recorded, and can only cross a trunk
  as G.729 end to end.

## Protocol & Authentication

- REST-ish JSON over HTTPS: `GET`/`POST`/`PATCH`/`PUT`/`DELETE` on resource
  paths, not a single `action=` dispatch endpoint like v1.
- Auth: `Authorization: Bearer <key>` — confirmed live 2026-09-24, the API
  key itself is usable directly as the bearer token, no separate
  login/token-exchange step. A "user token" mode also exists (resolves an
  "acting extension" automatically) but isn't relevant to SwarmDialer,
  which always acts as an admin API key. Per-endpoint OpenAPI security
  scopes are named things like `system:tenants:create`,
  `system:tenants:update` — not yet confirmed whether a single admin key
  carries all scopes or whether different keys are scoped narrower.
- Path shape: two families —
  - **System-level**, no tenant context: `/system/v2/tenants`,
    `/system/v2/tenants/{id}`, `/system/v2/trunks`,
    `/system/v2/trunks/{id}`, `/system/v2/tenant-packages`,
    `/system/v2/tenant-packages/{id}`, `/system/v2/server-settings`,
    `/system/v2/calls` (list across all tenants).
  - **Org-scoped**, under a tenant: `/org/{tenant}/v2/extensions/...`,
    `/org/{tenant}/v2/calls/...`, `/org/{tenant}/v2/trunks/allowed`.
    `{tenant}` here is a path segment documented as
    `^(default|\d{3})$` — literally the string `default` for a
    single-tenant (non-Multi-Tenant) system, or the tenant's 3-digit
    **code** (e.g. `"800"`) for Multi-Tenant — confirmed from the
    Extensions/Calls endpoint schemas. This resolves the v1 ambiguity
    noted in an earlier draft of this doc: v2 has an explicit `default`
    literal for the non-Multi-Tenant case, unlike v1's implicit
    `server=1` convention.
- Error shape: `{"code": <int>, "description": "<string>", "details": [...], "trace_id": "..."}`
  — confirmed across multiple endpoints' documented error schema (e.g.
  tenant packages). `details` is a string array of per-field validation
  messages; `trace_id` is present "only when available." Cleaner and
  more structured than v1's ad hoc `{"error": "..."}`.
- Rate limit: response headers include `ratelimit-limit`/
  `ratelimit-policy`/`ratelimit-remaining` — confirmed `60;w=12` (60
  requests per 12s window, ~5 req/s sustained) against the tenant config
  endpoint live. Not yet confirmed whether this is per-key, per-IP, or
  system-wide, or shared across endpoint families.

---

## Tenants — `/system/v2/tenants`

**Fully documented.** Two distinct endpoints share almost the entire
field schema:

### Create — `POST /system/v2/tenants`

Saved page: `Create a tenant _ Bicom Systems Developer Portal.html`.
"Creates a new tenant with the provided configuration. Returns the full
tenant object on success." Responses: `201`, `400`, `401`, `403`, `409`
(code or SSO identifier already exists), `500`.

**Confirmed: this accepts the full same field set as the PATCH-by-id
endpoint below**, including `channels_limit` (required object),
`codecs`, and `call_recordings` (including `use_ram_disk`/
`ram_disk_size`/`stereo_recording_enabled`/`stereo_recording_format` —
the same fields that are otherwise system-level-only on the PATCH
endpoint's `id=1` record, see below). Required top-level fields: `name`,
`package_id`. Required nested objects: `locality`, `channels_limit`,
`numbering_defaults` (with `ext_length` required inside it, mirroring
v1's `TenantParams.ExtLength`).

**This is the single most actionable finding in this document**: a fresh
tenant can be created *and* fully configured (channel limits, codec
allowlist, recording/RAM-disk/stereo settings) in **one** v2 call,
instead of today's v1 `AddTenant` (bare identity fields only) → v1
`ResaveTenant` (full-form resend to fix registration/dialplan brokenness,
called *twice* for the documented settling-time reason) → a separate v2
`PatchTenant` call for recording. See the comparison section.

### Read/Update by ID — `/system/v2/tenants/{id}`

Saved pages: `Update a tenant's configuration...html`,
`Get an existing tenant's configuration...html` (redundant with the
update page's schema — same fields, no new information).

- `GET /system/v2/tenants/{id}` — read.
- `PATCH /system/v2/tenants/{id}` — **true partial update**: only fields
  present in the JSON body change; everything else is left alone
  (confirmed live: PATCHing only `channels_limit` leaves
  `call_recordings`, `codecs`, etc. completely untouched). This is the
  standout advantage over v1's `tenant.edit`, which requires resending
  the tenant's whole form or risking breakage (see v1's `ResaveTenant`
  doc comment in `internal/pbxware/client.go`).
- `id=1` always means "system-wide" config — on Multi-Tenant this is a
  distinct "System" record separate from any real tenant's own ID; on
  non-Multi-Tenant (no separate tenants exist) it IS the whole system's
  config. Two fields (`call_recordings.use_ram_disk`/`ram_disk_size` and
  `.stereo_recording_enabled`/`.stereo_recording_format`) **only ever
  appear on the `id=1` record**, never on a real Multi-Tenant tenant's own
  record — confirmed live 2026-09-24.
- Key sub-objects SwarmDialer uses: `channels_limit` (`local`/`remote`
  ints, `0`-`99999`), `codecs` (`local`/`remote`/`network` string arrays,
  enum `[ulaw, alaw, g722, opus, g723, g726, g726aal2, g729, gsm, ilbc,
  speex, speex16, speex32, lpc10, h261, h263, h263p, h264]` — real JSON
  arrays, not v1's colon-separated string), `call_recordings` (`enabled`,
  `format`, `use_ram_disk`, `ram_disk_size`, `stereo_recording_enabled`,
  `stereo_recording_format`).
- **List** — `GET /system/v2/tenants` (all tenants). **Delete** —
  `DELETE /system/v2/tenants/{id}`.
- Validation caveat: the per-extension `incoming_limit`/`outgoing_limit`
  is capped at 999 by v1 **and v2** (confirmed live 2026-09-25: v2 create
  returns "'incoming_limit' must not be greater than 999"), while this
  tenant/system endpoint accepts `channels_limit` values of 1000+.

---

## Tenant Packages — `/system/v2/tenant-packages`

**Fully documented.** Saved pages: `Create a new tenant package`,
`Update an existing tenant package`, `Delete a tenant package`,
`Get a tenant package by ID`, `Get tenant packages list`.

- `POST /system/v2/tenant-packages` — create. Fields: `name` (required),
  `extensions`, `voicemails`, `queues`, `ivrs`, `conferences`,
  `ring_groups`, `hot_desking` (all plain integers — no v1-style
  request/response name mismatch), `restrict_service_plans` (yes/no),
  `allowed_service_plans` (int array), `default_service_plan`,
  `call_recordings` (yes/no, **required**), `call_monitoring` (yes/no),
  `call_screening` (yes/no). Response: `{"id": <int>}`. Status codes:
  `201`, `400`.
- `PUT /system/v2/tenant-packages/{id}` — update. **Not a partial
  update** — `name`, `restrict_service_plans`, `call_recordings`,
  `call_monitoring`, `call_screening` are all still marked `required` in
  the update schema, same as create. This is a `PUT` (full replace), not
  a `PATCH`, and behaves like one — same "resend the whole thing" shape
  as v1's `package.edit`, just with cleaner/consistent field names (no
  more `extensions`-request vs `ext`-response style mismatch that v1's
  `packageParams` doc comment complains about).
- `GET /system/v2/tenant-packages` — list. `GET .../tenant-packages/{id}`
  — get one. `DELETE .../tenant-packages/{id}` — delete.

---

## Extensions — `/org/{tenant}/v2/extensions`

**Fully documented, including both batch endpoints.** Saved pages cover
create, update, delete, get-by-id, get-by-number, get-default-config,
get-DID-numbers, get-list, batch-create, batch-update.

### Create — `POST /org/{tenant}/v2/extensions`

Responses: `201`, `400`, `500`. Required top-level fields: `number`
(int), `name`, `email`, `status` (`active`/`not_active`/`suspended`),
`dtmf_mode`, `user_type` (`friend`/`user`/`peer`), `pin`. Required nested
objects: `authentication` (with `secret` and `user_password`, both
required, 8-128 chars, must match
`(?=.*[a-z])(?=.*[A-Z])(?=.*\d)(?=.*[!%*_-])` — same complexity rule as
v1's undocumented-but-empirically-found secret rule, now explicit in the
schema), `uad` (device/location config), `codecs` (`allowed` array,
required).

Fields directly relevant to what SwarmDialer sets today:
- `call_control.incoming_limit` / `.outgoing_limit` — plain integers,
  capped at 999 (confirmed live 2026-09-25, same as v1's
  `incominglimit`/`outgoinglimit`).
- `codecs.allowed` — array of `{name: <enum>, ptime: <int>}` objects (the
  enum matches the tenant's codec list) — a real structured list per
  codec including per-codec `ptime`, replacing v1's flat colon-separated
  `acodecs` string. `codecs.force_trunk_codec` also exists (forces a
  specific codec for outbound trunk calls — no v1 equivalent found).
- `default_trunks` — `primary`/`secondary`/`tertiary` (+ 3 emergency
  variants) trunk IDs, each defaulting to `-1`. v1 has no per-extension
  trunk assignment field SwarmDialer sets — extensions inherit the
  tenant's default trunk via `SetTenantDefaultTrunk` instead. Not
  something SwarmDialer would need to touch even on v2.
- `recording.enabled` (yes/no) + `beep_interval` — **per-extension**
  recording toggle, distinct from the tenant-level `call_recordings`
  SwarmDialer's dashboard toggle already controls. Not currently used.

Response schema: full extension object echoed back (`id`, `tenant_id`,
`user_id`, `ref`, `context`, `number`, ... — same shape as the request,
plus server-assigned IDs).

### Update — `PATCH /org/{tenant}/v2/extensions/{id}`

Responses: `200`, `400`, `404`, `500`. Far fewer fields marked `required`
than Create (12 vs. 23, and the remainder are nested/array-item-level,
e.g. an SMS array entry's own `number`/`is_default`) — consistent with a
**true partial update**, same pattern as the Tenants PATCH endpoint. Not
explicitly confirmed with "only provided fields are updated" wording the
way the Tenants page has it, but the required-field pattern strongly
implies the same semantics.

### Others

- `GET /org/{tenant}/v2/extensions` — list.
- `GET /org/{tenant}/v2/extensions/{id}` — get by internal ID.
- `GET /org/{tenant}/v2/extensions/number/{number}` — get by extension
  number (mirrors v1's `ext`-instead-of-`id` alternative on
  `pbxware.ext.configuration`).
- `GET /org/{tenant}/v2/extensions/default` — default field values for a
  new extension.
- `GET /org/{tenant}/v2/extensions/number/{number}/dids` — DIDs mapped to
  this extension.
- `DELETE /org/{tenant}/v2/extensions/{id}` — delete.

### Batch create — `POST /org/{tenant}/v2/extensions/batch`

Saved page: `Execute batch extension creation`. **This is a genuine
single-request bulk operation, not a client-side loop wrapper**: the body
is `{"requests": [{"id": 1, "data": {...full extension object...}},
{"id": 2, "data": {...}}, ...]}` (`id` must be an **integer** — the docs'
string example `"req-1"` fails the whole request with 400 "invalid request
body", confirmed live 2026-09-25) — one HTTP call carries arbitrarily
many extensions (schema says `requests` array must have `>= 1` items, no
documented upper bound). The response is a **bare JSON array** (not the documented `{"responses":
[...]}` wrapper) of `{"id": <request id>, "status": 201, "body": {"id":
12345, "name": ..., "number": ...}}` / `{"id": ..., "status": 400, "body":
{"code": ..., "details": [...]}}` items — **each item gets its own HTTP-style
status code**, so one bad extension in a batch of 1000 doesn't fail the
other 999. This directly replaces the *purpose* of `wizard.Provision`'s
current one-at-a-time loop with its 20ms stagger and 20-collision retry
budget (`internal/wizard/wizard.go`) — a reserved-number collision would
just show up as one `400` entry in the response array instead of aborting
client-side retry logic.

Per-item `data` is the **full single-extension create schema** (see
Create above) — every required field (`number`, `name`, `email`, `status`,
`dtmf_mode`, `user_type`, `pin`, `authentication.secret` +
`authentication.user_password`, `uad.id` + `uad.location`,
`codecs.allowed`) must be present per extension in the batch, same as a
standalone create. Two things worth flagging for a SwarmDialer migration:
- `authentication.user_password` (portal login password) is **required**
  alongside `secret` (SIP registration password) — v1 only ever sets a SIP
  secret, since SwarmDialer's virtual phones never log into a client
  portal. This would need a second generated value per extension (reusing
  `pbxware.GenerateSecret()`'s complexity rule is fine, since the schema
  requires the identical character-class regex for both fields).
- `call_control.incoming_limit`/`outgoing_limit` are capped at 999, same
  as single create and v1.
- `authentication.pin` is required (validation `required_str`), although
  not listed under `authentication` in the docs.
- Measured live 2026-09-25: 500 extensions in one request in ~20s.

Auth scope: `extensions:create`.

### Batch update — `PATCH /org/{tenant}/v2/extensions/batch`

Saved page: `Execute batch extension update`. Same `requests`/`responses`
array shape as batch create, except each request item has **both** an
`id` (correlation ID, echoed back) **and** a separate `resource_id`
(integer — which existing extension to update) alongside `data` (partial
fields, true PATCH semantics per-item, same as the single-extension
update endpoint).

---

## Trunks — `/system/v2/trunks`

**Fully documented.** Saved pages: `Create a trunk`,
`Update an existing trunk`, `Delete a trunk`, `Get trunks list`,
`Get allowed trunks for the specified tenant`.

### Create — `POST /system/v2/trunks`

Responses: `201`, `400`, `409` (already exists/in use), `500`. Required
fields: `name` (1-30 chars, `[a-zA-Z0-9-_.]`, unique — "some providers
require this field to be equal to the DID number... but if connecting two
systems, the IP address may be used as well"), `provider_id`, `channels`
object (`incoming_limit`/`outgoing_limit`, both required — no documented
max found), `trunk_type` (`friend`/`user`/`peer`, default `peer`),
`dtmf_mode`, `status`, `country_id`, `national_code`, `international_code`,
and `authentication` object (required).

**Directly relevant to the 2026-09-25 trunk self-loop finding**: inside
`authentication`, the schema has **both** `host` and `peer_host` as
distinct, clearly-scoped fields:
- `host` — "SIP host address... **Required when trunk_type is 'friend' or
  'user'**. Cannot be 'dynamic'."
- `peer_host` — "Peer host address... **Required when trunk_type is
  'peer'**. Cannot be 'dynamic'."

SwarmDialer's trunks are `trunk_type: peer` (v1's `AddTrunk` sends
`"type": "peer"`), so `peer_host` — not `host` — is the field v2 says
governs a peer trunk. **This matches what v1's own `AddTrunk` already
does**: it sends `host` = this instance's own address AND separately
`peer_host` = the far side's address (`internal/pbxware/trunk.go`), which
is the semantically correct split per this v2 schema too — v1 isn't
mixing up host/peer_host, and v2 doesn't expose some third,
unambiguous "the one true outbound contact" field that v1 was missing.
**This means the self-loop bug (each trunk's live PJSIP AOR contact
resolving to its own IP instead of its peer's) is very unlikely to be a
field-mapping/API-shape issue that switching to v2 would fix** — the
field semantics are identical between v1 and v2 for a peer trunk. If the
bug reproduces on a fresh v8.2 instance, it's more likely a genuine
PBXware-side bug in how it derives the runtime AOR contact from
`peer_host` for `trunk_type: peer` — something to report to Bicom, not
route around via v2.

Other notable fields: `channels.ringtime`, `authentication.insecure`,
`authentication.register` (enum incl. `not_required` — v1's
`register: "0"`), `authentication.from_ipaddr`-equivalent is
`incoming_ip_addresses` (an actual IP/CIDR array, not overloading
`peer_host` as v1's `from_ipaddr` does — a real improvement: v1 reuses
`PeerHost` for both the outbound dial target *and* the inbound ACL, which
is confusing but was never actually the source of the self-loop bug per
above), `codecs.allowed` (same `{name, ptime}` array shape as
extensions), `network.transport`/`.direct_media`/`.qualify`.

### Update — `PATCH /system/v2/trunks/{id}`

14 required-field mentions vs. Create's 24 (mostly nested/array-item
requireds remaining) — behaves like a partial update, consistent with the
Tenants/Extensions PATCH endpoints. Same `host`/`peer_host` field split
as Create.

### Others

- `GET /system/v2/trunks` — list. `DELETE /system/v2/trunks/{id}` —
  delete. `GET /org/{tenant}/v2/trunks/allowed` — trunks a specific
  tenant is allowed to use (Multi-Tenant only, presumably — no v1
  equivalent read exists; v1 only has `SetTenantDefaultTrunk`'s write
  side via the oddly-named three-part `pbxware.tenant.trunks.set`
  action).

---

## Server Settings — `/system/v2/server-settings`

**Read-only page saved** (`Get server settings`); `Update server
settings` also saved but not yet cross-checked field-by-field.

`GET /system/v2/server-settings` returns the **same tenant-shaped
object** (`id`, `name`, `code`, `channels_limit`, `call_recordings`,
`codecs`, etc.) as `GET /system/v2/tenants/{id}` — this looks like a
convenience alias that always resolves to the system-level record,
equivalent to `GET /system/v2/tenants/1`. **Not a new capability** —
SwarmDialer's existing `ClientV2.GetTenant(SystemTenantID)` already
covers this; no reason to add a second code path for the same data.

---

## Calls — `/org/{tenant}/v2/calls` and `/system/v2/calls`

**Paths confirmed for all 5 endpoints, full schema only for
`initiate-a-call`.**

- `POST /org/{tenant}/v2/calls` — **"Initiate a call."** Click-to-call /
  callback-style: the PBX itself originates a call from `extension` (or
  the authenticated user's own extension, if using a user token) to a
  destination, optionally as a callback (`from_number` set — PBX calls
  that number first, then bridges to the destination on answer,
  presenting the extension's caller ID; requires `device: gsm` with a GSM
  number assigned to the extension). Response `200` returns `uid`/
  `linked_id`; `404` if the extension isn't found; `422` if callback
  calling is disabled system-wide; `504` if the HTTP request times out
  but the call may still be proceeding PBX-side.
- `GET /org/{tenant}/v2/calls` — list calls for a tenant.
- `GET /org/{tenant}/v2/calls/{uid}` — fetch a single call.
- `DELETE /org/{tenant}/v2/calls/{uid}` — hang up a call.
- `GET /system/v2/calls` — list calls across all tenants (system-level,
  no tenant scoping).

**Important distinction from what SwarmDialer does today**: `initiate-a-
call` has the *PBX* originate and manage the call — SwarmDialer has no
real SIP-level control over the resulting call's SDP/codec offer, RTP
stream, or exact INVITE timing, since it never sends a SIP message
itself; it just asks PBXware to place a call and gets back an identifier.
That's fine for click-to-call use cases, but it would undermine
SwarmDialer's actual purpose (measuring real client-observed setup
latency, offering a specific codec per call, exchanging real RTP for
MOS) — those all depend on SwarmDialer's own `sipua` package acting as a
genuine SIP endpoint, not delegating call placement to the PBX. **None of
the Calls endpoints are a fit for replacing SwarmDialer's core dialing
mechanism**, whatever else migrates to v2. The list/fetch/hangup
endpoints could theoretically be used to *observe* or forcibly end a
real SwarmDialer-placed call from the admin side, but SwarmDialer already
has its own BYE-based hangup via `sipua`/`orchestrator`, so there's no
gap these would fill either.

---

## AI Providers — `/org/{tenant}/v2/ai/providers`

Not relevant to SwarmDialer (text-to-speech/speech-to-text/transcription
provider config for voicemail/IVR features). Not investigated further.

---

## Sidebar categories seen, not yet explored

Not directly relevant to SwarmDialer's current feature set, or not yet
looked at beyond the sidebar category name: AI Services (Text-to-Speech,
Transcriptions, Voice Agents, Voicemail), SMS, Webhooks, Departments,
Dial Groups, Contacts, Routes, SIP Providers (used by v1's
`GenericSIPProviderID` lookup — worth checking if v2 exposes an
equivalent, since trunk creation needs a `provider_id`), Protocols,
Queues, Enhanced Ring Groups, IVRs, Greetings, Music On Hold, Service
Plans, Countries (referenced by `country_id` fields throughout — v1
hardcodes `"869"`, would need the v2 list endpoint to do this
dynamically), Emergency Button, Enhanced Services, Reports, Event
Publisher, UADs, DNO.

**DIDs specifically**: checked directly (2026-09-25) — the only DID-
related v2 page under the sidebar's "DIDs" category is extension
operation-times (unrelated scheduling, not DID CRUD); confirmed with
Bicom that **API v2 is still under active development and DID management
isn't implemented yet** (not a documentation gap, a genuine feature gap).
No standalone `/system/v2/dids` resource or DID batch-creation endpoint
exists to migrate to right now. This rules out DIDs entirely for the
current "speed up provisioning" push — `internal/wizard/connect.go`'s
`addDIDsFor` (fully sequential, one `pbxware.did.add` call per extension,
no stagger at all — likely the single slowest step in the whole wizard
for a large batch) has to stay on v1 until Bicom adds v2 DID support.
Worth checking back periodically.

---

## v1 vs v2 comparison notes — concrete recommendations

- **Tenant creation + configuration** (`AddTenant` + `ResaveTenant` +
  the already-migrated v2 recording PATCH): **worth prototyping now,
  highest-value candidate.** `POST /system/v2/tenants` accepts
  `channels_limit`, `codecs`, and `call_recordings` (RAM disk, stereo,
  everything) in the *same* create call — confirmed from its saved
  schema, which is structurally identical to the PATCH-by-id endpoint
  already in production use. This could collapse today's sequence (v1
  create → v1 resave #1 → extension creation loop → v1 resave #2 →
  separate v2 system-config PATCH) down to one v2 create call up front,
  eliminating the entire class of bug `ResaveTenant`'s doc comment
  describes (a narrow edit leaving the tenant able to read back correct
  limits while still being unable to place/receive calls). Caveat: not
  yet live-tested, so the "eventually consistent backend" timing issue
  `ResaveTenant` also works around (a second resave needed once real
  wall-clock time has passed) might still apply regardless of which API
  writes the initial config — that would need to be verified live before
  trusting a single v2 create call to fully replace the two-resave
  pattern.
- **Extension batch create/update**: **the clearest actual speed win in
  this whole document — prototype this first.** `POST
  /org/{tenant}/v2/extensions/batch` takes arbitrarily many extensions in
  *one* HTTP request and returns per-item status codes (no documented
  batch-size cap), directly replacing `wizard.Provision`'s current
  one-at-a-time loop (20ms stagger, up to 20 reserved-number-collision
  retries, one HTTP round-trip per extension — several minutes for 1000
  extensions). A reserved-number collision on one item would just be a
  `400` entry in the response array rather than aborting or needing
  client-side skip-and-retry logic at all. Two things to confirm with a
  live test before committing to this: (1) whether it actually executes
  as one fast bulk operation server-side or is internally still N
  sequential inserts wrapped in one HTTP response (the docs don't say
  either way, and this is the whole ballgame for whether it's actually
  faster, not just fewer round-trips); (2) whether `call_control.
  incoming_limit`/`outgoing_limit` really has no upper bound in v2 or
  still silently caps at 999 like v1. Also note the extra required
  `authentication.user_password` field (v1 never sets one, since
  SwarmDialer's virtual phones don't do portal logins) — trivial to add
  (reuse `pbxware.GenerateSecret()`), just a new field to populate.
- **Trunk creation**: **not worth switching for the self-loop bug** —
  v2's schema confirms v1's `host`/`peer_host` split was already
  semantically correct for a `peer`-type trunk (see the Trunks section
  above); the 2026-09-25 self-loop finding is more likely a genuine
  PBXware-side bug in deriving the live PJSIP AOR contact from
  `peer_host`, not something either API's request shape would fix.
  Worth revisiting only if the bug doesn't reproduce on the fresh v8.2
  redeploy (in which case it may already be fixed upstream, independent
  of API version) — or reporting to Bicom directly if it does reproduce.
  v2's `incoming_ip_addresses` (a real array) is a minor improvement over
  v1 overloading `peer_host`/`from_ipaddr` for two different purposes,
  but not related to the self-loop symptom.
- **Tenant packages**: **not worth switching** — the update endpoint is
  `PUT`, not `PATCH`, and requires resending `name` +
  `restrict_service_plans`/`call_recordings`/`call_monitoring`/
  `call_screening` every time, i.e. the same "whole form" shape as v1's
  `package.edit`. The only real improvement is cosmetic (consistent field
  naming, no more request/response name mismatch), not worth a migration
  by itself.
- **Call placement**: **do not migrate** — `initiate-a-call` (and the
  other Calls endpoints) delegate call origination to the PBX itself,
  incompatible with SwarmDialer's need to control SDP/codec/RTP/timing
  directly via its own `sipua` package. Keep this on real SIP signaling
  regardless of what else moves to v2.
- **Server Settings**: **skip** — `GET /system/v2/server-settings` is
  equivalent to `GET /system/v2/tenants/1`, already covered by the
  existing `ClientV2.GetTenant(SystemTenantID)`.
