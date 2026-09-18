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

### ⚠️ Concurrent-channel limit — every tenant defaults to 8 (confirmed 2026-09-18)

Every tenant, however created (via GUI or API, with or without explicit
values passed at creation), defaults to an **8 concurrent local/remote
channel cap** — confirmed by hitting a clean, reproducible wall in load
testing (exactly the first 8 calls succeed, everything after fails with
`603 Decline`), then verifying against `tenant.configuration` on multiple
tenants. **Not a license restriction** — PBXware licenses only cap total
system-wide channels (see License section below), not per-tenant.

This is a real, fixable tenant setting — in the admin GUI: System level →
Tenants → `<tenant>` → Advanced settings → Channels → Local/Remote
channels. But the **API has a request/response field-name mismatch** that
makes it very easy to get wrong:
- **Reading** current values (`pbxware.tenant.configuration`) shows them as
  `incominglimit`/`outgoinglimit`.
- **Setting** them via `pbxware.tenant.add` or `pbxware.tenant.edit`
  requires the differently-named `local_channels`/`remote_channels`
  parameters instead. Sending `incominglimit`/`outgoinglimit` as request
  params (an easy mistake — they appear in the docs, in a *different,
  equally-plausible-looking* section covering `conch`/`quech`/`aach`/
  `zapch` for conferences/queues/ring-groups/auto-attendants/DAHDI) is
  **silently ignored** — no error, tenant just stays at 8.

Working example (via `tenant.edit`):
```
action=pbxware.tenant.edit&server=1&id=<tenant_id>&local_channels=600&remote_channels=600
```
Also has the same slow-write-outlasts-HTTP-timeout behavior as
`tenant.add` — retry and verify via a follow-up `tenant.configuration`
read rather than trusting the immediate HTTP response. SwarmDialer's
`pbxware.SetTenantChannelLimits` handles this (retry + verify) and is
called automatically on every provisioning run, for both new and reused
tenants.

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
| `server` | **Required** | Tenant/Server ID |
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
| `codecs` | **Required** | Colon-separated, e.g. `ulaw:alaw` |
| `codecs_ptime` | **Required** | `10`, `20`, `30`... `300` |

**To connect instance A ↔ instance B**: create a trunk on A with
`host`=A's IP, `peer_host`=B's IP, and a shared `username`/`secret` pair
that both sides agree on (generate once, use as both A's `username`/
`secret` and B's `peer_username`/`peer_secret`, and vice versa — needs to
be symmetric). Mirror with a second trunk on B pointing back at A.

**Successful response**: `{ "success": "Trunk ID: 10", "id": 10 }`

**Open question, not yet resolved**: how outbound calls actually get
routed onto a specific trunk isn't fully clear from the docs alone.
Extensions have `primary_trunk`/`secondary_trunk`/`tertiary_trunk` fields
(seen in the Extensions field list) — this may be what "set as default
trunk for the tenant" means in practice (per-extension, set on every
extension in the tenant, rather than one tenant-wide switch). Verify this
empirically against the real system when building this — don't guess
further from docs alone, this system's docs have repeatedly not matched
reality in edge cases (tenant_code range, secret complexity, this exact
channel-limit field mismatch, etc.).

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
| `destination` | **Required** (for dest_type ≠ Phone Callback/Deny Access) | The target — **unclear from docs whether this should be the extension number or its internal ID; verify live when implementing** |
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
