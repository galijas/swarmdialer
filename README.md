# SwarmDialer

SwarmDialer is an internal load-testing tool for PBXware. It provisions
tenants and extensions directly through PBXware's HTTP API, then places up
to hundreds of simultaneous real SIP calls (full INVITE/ACK/BYE signaling,
with real RTP media carrying speech-like audio) between them to see how a
PBXware instance behaves under concurrent call load. Everything is driven
from a web GUI: a Setup Wizard walks you through connecting to one or two
PBXware instances and provisioning test extensions, and a Dashboard lets
you ramp up local and cross-server call volume on demand while watching a
live log and call graph of what's happening.

It's built to be deployed fresh wherever it's needed, not tied to any
one environment - the wizard collects all connection details (API keys,
IPs) at runtime, so nothing about a specific PBXware instance is baked
into the tool itself.

Tested on Ubuntu 24.04. The PBXware instance(s) you connect it to should
be fresh, dedicated test systems - **not production** - since this is 
how it was tested and validated: SwarmDialer provisions real tenants, 
extensions, trunks, and DIDs on the instance you point it to. You can 
optionally have it delete the resources it created later.

## Deploying on a Fresh Ubuntu (24.04) VPS/Server

1. **Clone the repo** onto the freshly deployed Ubuntu (24.04) VPS/server that will run the load test:

   ```
   #You may need to install git before running the 'git clone' command:
   #sudo apt update
   #sudo apt install git

   cd /root/
   git clone https://github.com/galijas/swarmdialer.git
   cd /root/swarmdialer/
   ```

2. **Run the install script.** It installs `ca-certificates` and Go if
   either is missing, builds the binaries into `bin/`, and sets up a
   systemd service so the GUI starts on boot and restarts automatically
   if it ever dies:

   ```
   sudo ./install.sh
   ```

3. **Save the admin password** the install script prints at the end
   (username `admin`). It's shown only once and stored as a hash. If it's
   lost, see [Troubleshooting](#troubleshooting).

4. **Open `https://<server-ip>/`** in a browser (plain `http://`
   redirects there) and log in. The certificate is self-signed, so the
   browser shows a one-time warning. Then complete the Setup Wizard:
   connect your PBXware instances (base URL, legacy API key and API v2
   key for each), provision a tenant and test extensions, connect the two
   instances with a trunk and DIDs, and connect the SERVERware site they
   run on (controller address and an admin API key).

## SERVERware host testing

The **SW Host Benchmark** tab runs a fixed, versioned test script
(profile "standard") of call-load tests on remote calls between the two
PBXware instances, while monitoring the SERVERware host and the PBXware
VPSs through SERVERware's Prometheus. SwarmDialer and both PBXware VPSs
must run on the same SERVERware host; the wizard checks this and turns on
SERVERware observability and per-VPS metrics itself. It also raises each
PBXware VPS's call recording RAM disk to 512 MB in SERVERware and offers
to restart the VPSs so it takes effect (one at a time, waiting until
PBXware is back up). If the SERVERware step is skipped, the wizard instead
tells you to raise the RAM disk and restart the VPSs manually.

When a run completes, a report (hardware details and results only, no IPs,
host names or secrets) is saved under `reports/` and uploaded to DT
Collector, the central report server (`https://dtcollector.dtbicom.xyz`
by default; the URL and the upload key are set in Settings). A "smoke"
profile of a few minutes is available for checking the setup; its
reports are never uploaded.

While a run is going, the tab shows its progress test by test, live
figures and whole-run charts of host and PBXware VPS load; **Present**
shows that view full screen. **Previous Reports** lists every saved
report (view, download, delete), and **Upload Hardware Info Only** sends
just the host's hardware and environment to DT Collector without running
tests.

Once the Setup Wizard is finished its tab is hidden; connected instances
are then reconfigured (name, address, API keys) under **Settings ->
PBXware Instances**. The admin password can be changed under **Settings ->
Account Security**. A light/dark theme toggle is at the top right.

## Configuration and security

Configuration (which servers are connected, what was provisioned, API
keys) is persisted to `swarmdialer_config.json` in the working directory,
readable only by root, and is specific to this deployment - nothing here
is meant to be copied between environments. Set `-config <path>` to
change where it's stored. The admin password hash
(`swarmdialer_auth.json`) and the TLS certificate (`tls/`) live next to
it. After a test is finished and uploaded, the SW Host Benchmark tab offers
"Finish and wipe", which deletes the configuration and every stored key.

## Troubleshooting

**Check that the service is running, and read its log:**

```
systemctl status swarmdialer
journalctl -u swarmdialer -f
```

**Reset the admin password.** This prints a new random password for
`admin`; restart the service to apply it:

```
cd /root/swarmdialer
./bin/gui -reset-password -config /root/swarmdialer/swarmdialer_config.json
systemctl restart swarmdialer
```

**The trunk or DID step of the wizard fails because they already exist.**
An earlier SwarmDialer that was connected to the same PBXware instances,
and wasn't reset (Settings -> Reset Instances), leaves its trunks
(`SwarmDialer-to-<instance name>`), tenant, extensions and DIDs behind.
Delete them in PBXware, then run the step again.

**"Trunk name contains invalid characters".** PBXware only allows letters,
digits, `-`, `_` and `.` in trunk names, and SwarmDialer names each trunk
after the other instance's display name. SwarmDialer replaces other
characters with `-` (e.g. `MT Test` becomes `SwarmDialer-to-MT-Test`), so
this only appears on versions before 1.6; update, or rename the instance
in Settings -> PBXware Instances.

**The GUI looks outdated after an update.** Reload the page with Ctrl+F5
so the browser doesn't use its cached copy.
