# SERVERware API reference (for SwarmDialer)

Confirmed live against SERVERware 5.2.1 (Mirror edition), 2026-09-28.
The published OpenAPI spec (`openapi.yml`, API version 5.2) covers users,
partitions, hosts, VPSs, flavors, templates, networking and DNS. Two calls
SwarmDialer needs are **not** in it (marked below); they were captured
from the SERVERware GUI and replayed with an API token.

## Authentication

- Header `SW-API-Token: <token>` (an admin user's API token).
- The same token also authenticates Prometheus on the controller
  (`https://<controller>/prometheus/...`); without it Prometheus returns
  401.
- The controller's HTTPS certificate is typically self-signed.
- Success bodies: `{"data": ..., "status": "success"}`. Errors:
  `{"Error": "..."}` with a 4xx/5xx status.

## Calls SwarmDialer uses

| Call | Purpose / notes |
|---|---|
| `GET /api/networks/1/hosts` | Hosts: `id`, `uuid`, `name`, `ip_address`, `purpose`, `mirror_id`, `state`, plus live `cpu_usage`, `io_wait`, `mem_usage`, `mem_total`, `cpu_temp`, and **`platform_details`**: `cpu_model` (e.g. "Intel(R) Xeon(R) Silver 4208 CPU @ 2.10GHz"), `cpu_count`, `motherboard`, `kernel_version`, `storage_ctrls` and `network_cards` (vendor/product), `service_versions`, `swcore_version`. The CPU model is only available here (node exporter doesn't report it). Cheapest call to check a token. `GET /api/networks/1/hosts/{id}` also exists (200) |
| `GET /api/networks/1/vpses` | All VPSs, with `host_id`, `engine` (`lxc`/`kvm`), `state`, `task` and **`ip_addresses`** (used to find VPSs by IP). Its `collect_metrics` values were stale (showed `false` for VPSs that had it on); read the single VPS instead |
| `GET /api/networks/1/vpses/{id}` | One VPS: full record including `collect_metrics`, `cpu_limit`, `cpu_share`, `mem_limit`, `callrec_ram_mb`, `description`, `root_password`. **No `ip_addresses`** in this response |
| `PUT /api/networks/1/vpses/{id}` | Edit VPS (documented). Replaces the whole record: send the current record back with only the changed fields. `root_password: ""` leaves the password unchanged (what the GUI sends). Returns `202 {"status":"modifying"}`; the change is applied as a background task (`task` is `MODIFYING` until done, a few seconds) |
| `GET /api/system-settings/observability` | **Not in the spec.** Returns `{"observability": {"enabled": bool}}` |
| `POST /api/system-settings` | **Not in the spec.** Body `{"observability": {"enabled": true}}` turns observability on (starts the SRW exporter on the controller). `200` with the API token |

`GET /api/system-settings` (without a section) returns 500 with the API
token; read sections individually.

### Collect Metrics (per-VPS metrics)

The Edit VPS form's "Collect Metrics" checkbox is the `collect_metrics`
field of the VPS record (not listed in the spec's `VpsEdit` schema, but
accepted by the documented PUT). Setting it on a running LXC PBXware VPS
changed only that field and did not restart the VPS. Prometheus starts
scraping the VPS about 20 seconds later (a target named after the VPS,
e.g. `DT-CC-Test`, on port 195xx). Not tried on KVM VPSs: the spec says
some KVM properties can only change while the VPS is stopped.

### Recording RAM disk

`callrec_ram_mb` on a PBXware VPS is the call recording RAM disk size
provided to the container (the PBXware setting does not resize it on an
LXC VPS; the container's mount comes from SERVERware). Editable through
the same PUT; the new size applies when the VPS next starts.

Since 2026-10-02, connecting SERVERware raises it to 512 MB on LXC
PBXware VPSs below that, and the wizard offers to restart them with
`POST /api/networks/1/vpses/{id}/restart` (documented; body
`{"daemonize": true}`, returns 202 and runs as a background task). After
the restart SwarmDialer waits for state `RUNNING` with no task, then for
PBXware's API and Asterisk (a SIP OPTIONS answer). **Not yet confirmed
live:** the PUT of `callrec_ram_mb` on a running VPS, and the VPS
state/task values during a restart.

## Prometheus (on the controller)

`https://<controller>/prometheus/api/v1/...` (standard Prometheus HTTP API).

| Job / target | Metrics |
|---|---|
| `host_exporters`, instance per physical node (e.g. `Echo-1`, `Echo-2`), port 9100 | Node exporter: host CPU, memory, disk, network, load. On Mirror, the secondary node's port 9163 target is down by design |
| `host_exporters`, instance = VPS name, port 195xx | Per-VPS process exporter (`namedprocess_namegroup_*`, per `groupname`: `asterisk`, `mysqld`, `php`, `nginx`, `core-api`, ...), once Collect Metrics is on |
| `ctrl_exporters`, `CONTROLLER`, port 9101 | SRW exporter, once observability is on: `srw_info` (version), `srw_host_info`, `srw_partition_info`, `srw_vps_info` (VPS to host UUID), `srw_active_calls` per VPS |

Host filesystems reported by node exporter do not include the PBXware
VPSs' recording RAM disk mounts, so RAM disk usage isn't visible here.

## Security note

VPS records include `description` and `root_password`; on the test site
the descriptions contain passwords. Never copy VPS records into reports
or logs.
