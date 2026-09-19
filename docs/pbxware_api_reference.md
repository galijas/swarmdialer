# PBXware API Reference (for SwarmDialer)

Extracted from `PBXware API.pdf` (Bicom Systems, dated "April 2026", 238 pages).
This file covers what SwarmDialer needs: auth, protocol, Extensions
(add/edit/delete/list), Tenants (add/edit/delete/list), License info, Trunks
(add/edit/delete/list/providers), and DIDs (add/edit). For anything else,
re-open the source PDF (`~/claude/PBXware API.pdf`; re-extract text with
`pdftotext -layout` if you need to search it again — see PROJECT_STATE.md
for that workflow).

## Protocol & Authentication

- All requests are plain `HTTP GET` or `HTTP POST`.
- Auth is via a single `apikey` query/POST argument (unique string, min 10 random
  characters, set in PBXware Admin Settings, or generated in the UI).
- The main argument controlling behavior is `action`, always in the form
  `application.object.method` — application is always `pbxware`.
  Examples: `pbxware.ext.add`, `pbxware.tenant.list`, `pbxware.ext.delete`.
- Response format defaults to JSON; can be overridden with `apiformat` (`json` or `php`
  serialized).
- **Error handling**: if a response body contains an `error` key, the caller MUST abort
  further operations. Example:
  ```json
  { "error": "Invalid API key." }
  ```

Example raw request:
```
GET /?apikey=my.secret.apikey&action=pbxware.did.list HTTP/1.0
Host: pbxware.local
User-Agent: Mozilla/5.0
```

Example with httpie (recommended for manual testing):
```
http -b "http://pbx.local/?apikey=my.secret.key&action=pbxware.ext.list"
```

**Security note from the docs**: keep the API key secret — it exposes fairly
critical functionality to 3rd parties (e.g. delete operations).

---

## Extensions (`action=pbxware.ext.*`)

### List — `pbxware.ext.list`

**Arguments**
| Field | Description |
|---|---|
| `server` | Filter extensions by tenant/server |

**Response** — object keyed by Extension ID:
```json
{
  "1": {
    "name": "User 530",
    "email": "name@provider.com",
    "protocol": "sip",
    "ext": "530",
    "location": "local",
    "ua_id": "50",
    "ua_name": "generic_sip",
    "ua_fullname": "Generic SIP",
    "status": "enabled",
    "macaddress": "",
    "sn": "",
    "linenum": "10",
    "user_location": "Location",
    "department": "4,6"
  }
}
```

### Configuration (get one) — `pbxware.ext.configuration`

**Arguments**: `server` (required, Tenant/Server ID), `id` (required, Extension ID) —
**or** `ext` (extension number) instead of `id`. Cannot supply both `id` and `ext`.

**Response** includes full config, notably (this is what you need to build a SIP UA
that can register as this extension and place/receive calls):
```json
{
  "1": {
    "name": "User 530",
    "pin": "1234",
    "protocol": "sip",
    "ext": "530",
    "location": "local",
    "user_language": "en",
    "ua_id": "50",
    "status": "enabled",
    "options": {
      "type": "friend",
      "dtmfmode": "rfc2833",
      "context": "t-344",
      "canreinvite": "0",
      "qualify": "8000",
      "host": "dynamic",
      "cid_enable": "yes",
      "callerid": "User 530 <530>",
      "voicemail": "1",
      "incominglimit": "1",
      "outgoinglimit": "2",
      "username": "344530",
      "secret": "530",
      "disallow": "all",
      "allow": ["ulaw", "alaw"],
      "recordcalls": "0"
    }
  }
}
```
**`options.secret` is the SIP password.** `options.username` combined with the
extension (`ext`) and the tenant's SIP domain/host is what the Go SIP client needs to
register. `context` (e.g. `t-344`) shows the tenant code is embedded (tenant 344).

### Add — `pbxware.ext.add`

**Arguments** (only the fields relevant to a load-test extension; full list in PDF
pages 33–40 covers ~90 optional fields for voicemail, provisioning, WebRTC, etc. — all
safely omittable for our purposes):

| Field | Required? | Notes |
|---|---|---|
| `server` | **Required** | Tenant/Server ID |
| `name` | **Required** | Full Name |
| `email` | **Required** | E-mail |
| `ext` | optional | Extension number (auto-assigned if omitted?) |
| `location` | **Required** | `1` = Local, `2` = Remote |
| `ua` | **Required** | User Agent Device ID (numeric). `50` = generic SIP per the list example above |
| `status` | **Required** | `1` = Active, `0` = Not Active |
| `pin` | **Required** | PIN |
| `incominglimit` | **Required** | Incoming limit |
| `outgoinglimit` | **Required** | Outgoing limit |
| `voicemail` | **Required** | `1`=Yes, `0`=No |
| `prot` | **Required** | Protocol — **SIP and IAX only!** Use `sip` |
| `secret` | **Required** | **SIP password** — set this explicitly so the Go SIP client knows it |
| `nat` | optional | `1`=Yes, `0`=No, `2`=Never |
| `canreinvite` | optional | SIP re-INVITE support, `1`/`0` |
| `qualify` | optional | Qualify (max 4 digits) |
| `acodecs` | optional | Allowed codecs, colon-separated, e.g. `ulaw:alaw` |
| `dtmfmode` | optional | `auto`, `inband`, `rfc2833`, `info` |

**Successful response**:
```json
{ "success": "Extension ID: 10", "id": "10", "ext": 100 }
```
Note: response gives back the assigned `id` and `ext` number — capture both when
provisioning in bulk (the `ext` may be auto-assigned by PBXware if not supplied).

### Edit — `pbxware.ext.edit`
Same arguments as Add, all optional except `server` and `id` (or `ext`, not both).

### Delete — `pbxware.ext.delete`
**Arguments**: `id` (Extension ID), `server` (Server/Tenant ID).
**Response**:
```json
{ "success": "Deleted Extension ID 10 successfully." }
```

### ⚠️ Non-Multi-Tenant extension number length isn't a free choice — read it from `pbxware.server.configuration` (confirmed 2026-09-18)

For Multi-Tenant editions, extension number length is a free per-tenant
choice we make ourselves via `tenant.add`'s `ext_length` field (e.g. `3`
digits, `100`-`999`). **Non-Multi-Tenant editions (e.g. Call Centre) have
one system-wide length instead, and it's not something we choose — it has
to be detected and matched.** Guessing wrong doesn't fail cleanly: every
`ext.add` attempt is rejected with the same permanent validation error,
e.g.
```
Required field 'ext=100' contains invalid data (Regex: /^\d{4}$/)
```
on every single attempt, indistinguishable by `AddExtensionWithRetry`
from a transient "system still settling" error — so it burns the full
retry window (up to 10 minutes) before finally surfacing.

The digit length is the `numbering` field on `pbxware.server.configuration`
(a system-level analog to `tenant.configuration`, used instead of it on
non-tenant-mode instances — confirmed live: calling `tenant.configuration`
on a Multi-Tenant instance's system scope, or `server.configuration` on a
Multi-Tenant instance at all, both return `"Tenant mode enabled, please
use action \"tenant\" object instead of \"server\""`, so the two action
families are mutually exclusive based on whether Tenant Mode is on):
```
action=pbxware.server.configuration
```
```json
{"server_name":"PBXware", ..., "numbering":"4", ...}
```
`pbxware.EnsureSwarmDialerPackage`'s sibling, `Client.GetSystemExtensionLength`,
reads this and `wizard.Provision` uses it to compute the actual starting
extension number for non-Multi-Tenant editions, instead of the
Multi-Tenant-only assumption of starting at `100`.

### ⚠️ The chosen starting extension number can collide with a reserved one (confirmed 2026-09-18)

Even with the digit length right, the literal number chosen can still be
taken — e.g. a system's default Operator extension sitting at the digit
base (`1000` on a 4-digit system):
```
Error creating extension: ua_id: extension is reserved - number 1000 is reserved by (Extension) Operator
```
This is also a **permanent** error (retrying it can never succeed), and
also indistinguishable from a transient one without checking the message.
`pbxware.IsReservedExtensionError` matches it specifically; `wizard.Provision`
skips just that number and tries the next one rather than aborting the
whole run (a handful of skips is expected/harmless; more than ~20 in a row
means something is more broadly wrong, e.g. the whole range is already in
use, and it gives up rather than looping forever). Separately,
`AddExtensionWithRetry` now also fails fast (no retry) on any error
substring it recognizes as permanent (`is reserved`, `already exist`,
`contains invalid data`) rather than burning the full retry window on
something that can never resolve itself.

---

## Tenants (`action=pbxware.tenant.*`)

### List — `pbxware.tenant.list`

**Response** — object keyed by Tenant ID:
```json
{
  "2": {
    "name": "t1.dot.com",
    "tenantcode": "344",
    "package_id": "1",
    "package": "Package 1",
    "ext_length": 3,
    "country_id": "869",
    "country_code": "1"
  }
}
```

### Configuration (get one) — `pbxware.tenant.configuration`
**Arguments**: `id` (required, Tenant ID). Returns a large config object (dozens of
fields — recording defaults, call limits, LDAP, etc.) Only fields relevant to
provisioning are summarized below under Add.

### Add — `pbxware.tenant.add`

| Field | Required? | Notes |
|---|---|---|
| `tenant_name` | **Required** | Should be a valid FQDN, e.g. `loadtest1.local` |
| `tenant_code` | **Required** | Unique **3-digit** tenant code |
| `package` | **Required** | Tenant Package ID (get via `pbxware.package.list`) |
| `ext_length` | **Required** | Extension number length, **range 2–16 only** |
| `country` | **Required** | Country ID (get via `pbxware.route.list` per the docs) |
| `national` | **Required** | National dialing code |
| `international` | **Required** | International dialing code |
| `area_code` | optional | Area code |
| `enabletcalls` | optional | Enable tenant-to-tenant calls (`1`/`0`) — worth enabling if extensions across tenants need to call each other |

**Successful response**:
```json
{ "success": "Tenant ID: 10", "id": 10 }
```

### Edit — `pbxware.tenant.edit`
Same arguments as Add, all optional except `server` (Server ID, must be `1`) and `id`
(Tenant ID). Note: `ext_length` **cannot be changed** after creation.
Also has a `status` field: `0`=Not Active, `1`=Active, `2`=Suspended.

### Delete — `pbxware.tenant.delete`
**Arguments**: `server` (must be `1`), `id` (Tenant ID).
**Response**:
```json
{ "success": "Deleted Tenant ID 19 successfully." }
```
**Note (confirmed 2026-09-17)**: this call returns success but does **not**
actually remove the tenant — verified by checking `tenant.list` afterward.
Don't rely on it for cleanup.

### ⚠️ Concurrent-channel limit — every tenant defaults to 8, on FIVE separate settings, not just Local/Remote (confirmed 2026-09-18)

Every tenant, however created (via GUI or API, with or without explicit
values passed at creation), defaults to an **8 concurrent cap on each of
five separate resource pools**, independently: Local/Remote SIP channels,
Conferences, Queues, Enhanced Ring Groups, and DAHDI (Auto Attendants also
defaults low and should be raised for the same reason, though its specific
effect wasn't isolated as precisely as the other four). **Not a license
restriction** — PBXware licenses only cap total system-wide channels (see
License section below), not per-tenant.

**The trap**: raising only Local/Remote channels (`incominglimit`/
`outgoinglimit`, see below) looks like it fixes concurrency, but a plain
same-tenant extension-to-extension call still hits a hard wall at exactly
8 concurrent calls (confirmed live via Asterisk's own `core show channels
count`: sustained at 16 channels = 8 calls, immovable, until the other
settings were also raised — then it climbed straight through, no ceiling).
Root cause traced to `agi_dial_local.php` (PBXware's local-dialing AGI,
launched via the FastAGI daemon at `127.0.0.1:4573`) issuing a near-instant
hangup (`AST hangup cause 0`) on declined calls — the file is encoded
(Bicomsystems' own commercial PHP loader), so the exact internal check
couldn't be read directly, only inferred from this exhaustive elimination:
tenant Local/Remote channels, system-level (`id=1`) channels, per-extension
`incominglimit`/`outgoinglimit`, the full `license.info` response, and
Asterisk's own global settings (`core show settings`: Maxcalls 512) were
all checked and ruled out one at a time before landing on Conferences/
Queues/Enhanced Ring Groups/DAHDI as the actual fix. Even though the call
being tested doesn't use conferences, queues, ring groups, or DAHDI at
all — this AGI is apparently gated by (or shares some resource-check code
path with) all of them regardless.

This is a real, fixable tenant setting — in the admin GUI: System level →
Tenants → `<tenant>` → Advanced settings → Channels (all five settings live
together there). But the **API has a request/response field-name
mismatch** on every one of them, and it's a *different* mismatch per field
(most maddening: sending back the same name that was just read is silently
ignored — no error, the tenant just stays at 8):

| Response field (`tenant.configuration`) | Request param (`tenant.add`/`tenant.edit`) | Meaning |
|---|---|---|
| `incominglimit` | `local_channels` | Local SIP channels |
| `outgoinglimit` | `remote_channels` | Remote SIP channels |
| `conch` | `conferences` | Conference channels |
| `quech` | `queues` | Queue channels |
| `ergch` | `enhanced_ring_groups` | Enhanced Ring Group channels |
| `aach` | `aach` | Auto Attendant channels (only one where request = response name) |
| `zapch` | `dahdi` | DAHDI channels (legacy "Zap" naming survives only in the response field) |

These request-param names were found empirically (systematic guess-and-
verify against a live tenant, since the actual admin-GUI form and API
handler source are both encoded) — they aren't documented anywhere else
we've found.

Working example (via `tenant.edit`, raising every one of the five/six to
the same value):
```
action=pbxware.tenant.edit&server=1&id=<tenant_id>&local_channels=600&remote_channels=600&conferences=600&queues=600&enhanced_ring_groups=600&aach=600&dahdi=600
```
Also has the same slow-write-outlasts-HTTP-timeout behavior as
`tenant.add` — retry and verify via a follow-up `tenant.configuration`
read rather than trusting the immediate HTTP response. SwarmDialer's
`pbxware.SetTenantChannelLimits` handles this (retry + verify all seven
fields) and is called automatically on every provisioning run, for both
new and reused tenants — **fixed 2026-09-18** to cover all of these, not
just Local/Remote (the original version of this function only raised
Local/Remote channels, which is exactly what let this 8-call ceiling slip
through undetected until real load testing at ~25 concurrent calls
surfaced it).

---

## Packages (`action=pbxware.package.*`)

Multi-Tenant editions require an existing tenant **package** to reference
when creating a tenant (`tenant.add`'s `package` field, an ID) — there's
no "no package" option. SwarmDialer creates its own
(`pbxware.EnsureSwarmDialerPackage`, name `SwarmDialerPackage`) rather than
relying on one already existing on the target instance, since a fresh
PBXware deployment may not have one set up yet.

### List — `pbxware.package.list`
**Response**: object keyed by package ID, value is the package name:
```json
{"1": "1000"}
```

### Configuration (get one) — `pbxware.package.configuration`
**Arguments**: `id` (required).
**Response**:
```json
{
  "1": {
    "name": "1000", "service_plan": "", "allowed_service_plans": null,
    "ext": "1000", "voicemail": "1000", "queues": "1000", "ivr": "1000",
    "cf": "1000", "rgroups": "1000", "hot_desking": "1000",
    "restrict_splans": "0", "call_recordings": "0", "monitoring": "0",
    "call_screening": "0"
  }
}
```

### Add — `pbxware.package.add`
**⚠️ Request field names don't match the response field names above** —
found empirically by iterating on the API's own "Required field 'X' is
missing" errors one at a time (undocumented anywhere we've found):

| Response field | Request field | Meaning |
|---|---|---|
| `ext` | `extensions` | Extension limit |
| `voicemail` | `voicemails` | Voicemail limit |
| `queues` | `queues` | Queue limit (matches) |
| `ivr` | `ivrs` | IVR limit |
| `cf` | `cfs` | Conference limit |
| `rgroups` | `rgroups` | Ring group limit (matches) |
| `hot_desking` | `hot_desking` | Hot desking limit (matches) |
| `restrict_splans` | `restrict_splans` | Restrict service plans (matches) |
| `call_recordings` | `call_recordings` | Call recording feature (matches) |
| `monitoring` | `monitoring` | Monitoring feature (matches) |
| `call_screening` | `call_screening` | Call screening feature (matches) |

Also requires `name`. Working example (SwarmDialer's actual profile — see
`pbxware.packageParams`): generous limits everywhere since none of it
matters for load testing, and every optional feature off since it's all
noise. **Raised from 1000 to 2000** (2026-09-19) alongside the switch to
4-digit Multi-Tenant extensions and the dashboard's +1000 dialer button:
```
action=pbxware.package.add&name=SwarmDialerPackage&extensions=2000&voicemails=2000&queues=2000&ivrs=2000&cfs=2000&rgroups=2000&hot_desking=2000&restrict_splans=0&call_recordings=0&monitoring=0&call_screening=0
```
**Response**: `{"success": "Tenant package: 2.", "id": 2}`.

### Edit — `pbxware.package.edit`
Same fields as `package.add` (see table above), plus `id` — no `server`
param needed, unlike `tenant.edit`. Confirmed working correctly (unlike
some other `*.edit` actions in this API, it took effect immediately, no
retry-and-verify dance needed). `pbxware.UpdatePackage` uses this so a
package created by an older version of this tool (e.g. still at the
previous 1000 limit) gets corrected automatically the next time
`EnsureSwarmDialerPackage` runs, rather than silently staying stale.

### Delete — `pbxware.package.delete`
**Arguments**: `id`. Works cleanly (unlike `tenant.delete`, which doesn't)
— confirmed by creating and immediately deleting a test package.

---

## License

### Info — `pbxware.license.info`
**Arguments**: none.
**Response**: system edition, version, and every licensed limit — use this
to detect whether a connected PBXware instance is Multi-Tenant (tenant
creation applies) or a different edition (skip tenant creation, provision
extensions directly at the system level) and to validate wizard inputs
against real license limits instead of just warning generically.
```json
{
  "Edition": "Multi-Tenant",
  "Version": "8.1 (a35a9165)",
  "Channels": "512",
  "DIDs": "9999",
  "Extensions": "1999",
  "Tenants": "999",
  "VOIP Trunks": "20",
  "PSTN Trunks": "20",
  ...
}
```
**Gap**: only the `"Multi-Tenant"` edition string is confirmed (this is
what our test instance returns). The exact strings for "Business"/"Contact
Centre" editions the user mentioned aren't documented anywhere we've
found — treat anything other than `"Multi-Tenant"` (case-insensitive) as
"skip tenant creation" as a safe default, rather than trying to match
specific other edition names.

---

## Trunks

### List — `pbxware.trunk.list`
**Arguments**: `server` (filter by tenant; "does not apply in Tenant Mode"
per the docs — behavior in Tenant Mode not yet verified live).
**Response** — keyed by Trunk ID: `name`, `protocol`, `provider_id`,
`provider_name`, `status` (`enabled`/`disabled`).

### List Providers — `pbxware.trunk.providers`
**Arguments**: `server`.
**Response** — keyed by provider name, value `[provider_id, "pstn"|"voip"]`.
**Generic SIP = provider ID `20`** (confirmed live against our instance,
matches docs exactly — unlike UAD IDs, no reason to expect this one to
drift per-install, but re-verify per target instance anyway since it's
cheap).
```json
{ "Generic SIP": ["20", "voip"], "Generic Analog": ["12", "pstn"], ... }
```

### Add — `pbxware.trunk.add`
**Arguments** (only fields relevant to a SwarmDialer-created SIP trunk
between two PBXware instances — full list has ~50 fields for callerID/
privacy/RPID/recording etc., all safely omittable):

| Field | Required? | Notes |
|---|---|---|
| `server` | **Required** | **Must be `1` (System level) even in Tenant Mode** — confirmed live: passing the tenant ID gets `"In tenant mode, make sure server=1."`. Trunks are system-wide objects even on multi-tenant installs; tenants get *pointed at* a trunk (see the "default trunk" open question above), they don't own one. |
| `name` | **Required** | Trunk name |
| `provider_id` | **Required** | `20` for Generic SIP |
| `type` | **Required** | `user`, `friend`, or `peer` |
| `dtmfmode` | **Required** | `auto`, `inband`, `rfc2833`, `info`, `shortinfo` |
| `status` | **Required** | `active` / `not active` |
| `country`/`national`/`international` | **Required** | Same convention as tenants |
| `emerg_trunk` | **Required** | Emergency trunk (yes/no) |
| `host` | **Required** | **This side's** host |
| `username` | **Required** | **This side's** auth username |
| `secret` | **Required** | **This side's** auth secret |
| `peer_host` | **Required** | **Other side's** host — the far PBXware instance's IP |
| `peer_username` | **Required** | **Other side's** auth username |
| `peer_secret` | **Required** | **Other side's** auth secret |
| `insecure` | **Required** | `port`, `invite`, `port,invite`, or `very` |
| `looserouting` | **Required** | `yes`/`no`/`1`/`0` |
| `incominglimit`/`outgoinglimit` | **Required** | Trunk-level (not tenant-level — different from the tenant channel-limit gotcha above) |
| `codecs` | **Required** | **Comma-separated** (confirmed live via the field's validation regex — docs text was ambiguous/misleading here), e.g. `ulaw,alaw`. Note this differs from extensions' `acodecs` field, which genuinely is colon-separated. |
| `codecs_ptime` | **Required** | **Must have the same number of comma-separated values as `codecs`** (confirmed live: `"Fields codecs and codecs_ptime must match in size"`), e.g. `codecs=ulaw,alaw` needs `codecs_ptime=20,20`, not a single shared value. Each value one of `10`, `20`, `30`... `300`. |

**To connect instance A ↔ instance B**: create a trunk on A with
`host`=A's IP, `peer_host`=B's IP, and a shared `username`/`secret` pair
that both sides agree on (generate once, use as both A's `username`/
`secret` and B's `peer_username`/`peer_secret`, and vice versa — needs to
be symmetric). Mirror with a second trunk on B pointing back at A.

**Successful response**: `{ "success": "Trunk ID: 10", "id": 10 }`

**Resolved (2026-09-18), simpler than expected**: no "default trunk"
configuration is needed at all for SwarmDialer's use case. Tested live:
created a trunk (system-level, `server=1`) and one DID on tenant 700
mapped to its own extension 100, then dialed that DID's exact number
(`5551000`) from a *different* tenant's extension (999400) with no other
config — **it worked immediately**, ringing and answering on tenant 700's
extension 100. PBXware's dialplan apparently falls through to a
system-level DID lookup automatically once a matching DID exists, for any
dialed number that isn't a local extension. So: create the trunk + one DID
per extension, and dialing the exact DID number is enough — no need to
touch extensions' `primary_trunk`/`secondary_trunk` fields.
(Tested with both "tenants" on the same physical PBXware instance, which
proves the DID/trunk *mechanics* — trunk routing to a real different
instance's IP is standard Asterisk SIP-peer behavior and should work
identically, but hasn't been verified end-to-end across two physically
separate PBXware boxes yet — do that once a second real instance is
available.)

**New finding from testing with two trunks simultaneously**: trunks need a
**settling period after creation**, same as tenants/extensions. With two
system-level trunks created back-to-back (one per direction, for
`wizard.ConnectServers`), dialing a DID through the trunk created *most
recently* failed immediately with:
```
NOTICE core_local.c: No such extension/context 9700100@SwarmDialer-to-ServerA while calling Local channel
```
Retried the identical call ~90s later with no other change — it worked.
The *other* direction's trunk (created slightly earlier, given a bit more
incidental time before first use) worked on the first try. There's no
read-back API call that confirms "this trunk is ready" the way
`tenant.configuration` does for channel limits, so `ConnectServers` just
does a blind `time.Sleep(90 * time.Second)` after creating both trunks and
all DIDs, rather than retry+verify. Empirically tuned, not derived from
any documented guarantee — may need adjustment on a different/faster
PBXware instance.

### Edit / Delete — `pbxware.trunk.edit` / `pbxware.trunk.delete`
Same shape as tenant edit/delete (edit: same args as add, optional except
`server`+`id`; delete: `server`+`id`). Assume delete may have the same
"returns success but doesn't actually delete" issue seen with tenants —
verify before relying on it.

---

## DIDs

### Add — `pbxware.did.add`
**Arguments** (relevant subset — full list has ~25 optional fields for
billing/CRM/recording etc.):

| Field | Required? | Notes |
|---|---|---|
| `server` | **Required** | Tenant/Server ID |
| `trunk` | **Required** | Trunk ID this DID is mapped to (inbound route) |
| `did` | **Required** | The actual DID number |
| `name` | optional | Display name |
| `dest_type` | **Required** | `0` = Extension (what SwarmDialer needs); other values: Ring Group, IVR, Queue, External Number, etc. — see full enum in source PDF if ever needed |
| `destination` | **Required** (for dest_type ≠ Phone Callback/Deny Access) | The target. For `dest_type=0` (Extension): **the plain extension number** (confirmed live via `did.list` — not the extension's internal ID) |
| `disabled` | **Required** | `0` = enabled, `1` = disabled |

**Successful response**: `{ "success": "DID ID: 1.", "id": 1 }`

For SwarmDialer's remote-calling feature: create one DID per extension on
the *receiving* instance, `trunk` = the trunk connecting back to the
*calling* instance, `dest_type=0`, `destination`=that extension — giving a
one-to-one DID↔extension mapping so a call placed to the DID over the
trunk lands directly on the matching extension.

---

## Gaps / things to verify before implementation

1. **No bulk-create endpoint.** Both `pbxware.tenant.add` and `pbxware.ext.add` create
   one object per call — there is no batch/array variant and no documented rate limit.
   For 500 extensions you will be making 500 sequential (or lightly-parallelized) HTTP
   requests. Recommend adding a small concurrency limit (e.g. 5-10 parallel requests)
   and retry-on-error logic rather than firing all 500 at once, to avoid overloading
   PBXware's own API handler — the docs give no guidance on safe concurrency.
2. **`ua` (User-Agent Device ID) is required for Add but its valid values aren't
   listed** in the sections read — the example data shows `50` = "generic_sip" and
   `52` = "custom_generic_iax". You'll need to call `pbxware.uads.list` (not yet read)
   against your actual PBXware instance to get the correct ID for "Generic SIP" before
   writing the provisioning code, or just try `50` and confirm via
   `pbxware.ext.configuration` after creating a test extension.
3. **Auto-assigned extension numbers**: unclear from the docs whether omitting `ext`
   on Add lets PBXware auto-assign the next free number, or whether it's required in
   practice. Test this against your instance early — if required, you'll need to
   generate non-colliding numbers yourselves (e.g. sequential starting from a base per
   tenant).
4. **Tenant-to-tenant calling**: since SwarmDialer needs extensions to call each other,
   decide whether all test extensions go under a **single tenant** (simplest, avoids
   `enabletcalls`/cross-tenant CID complications) or spread across multiple tenants
   (more realistic, but needs `enabletcalls=1` and possibly CLI routing). Recommend
   starting with a single tenant for simplicity.
5. **SIP domain/host for registration**: the API doesn't return the PBXware SIP
   registration domain/IP directly in these responses — that's just your PBXware
   server's own hostname/IP, which you already know since it's your own system.
