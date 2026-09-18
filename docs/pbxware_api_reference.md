# PBXware API Reference (for SwarmDialer)

Extracted from `PBXware API.pdf` (Bicom Systems, dated "April 2026", 238 pages).
This file only covers what SwarmDialer's provisioning module needs: auth, protocol,
Extensions (add/edit/delete/list), and Tenants (add/edit/delete/list). For anything
else, re-open the source PDF.

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
