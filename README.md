# SwarmDialer

SwarmDialer is an internal load-testing tool for PBXware. It provisions
tenants and extensions directly through PBXware's HTTP API, then places up
to hundreds of simultaneous real SIP calls (full INVITE/ACK/BYE signaling,
with real RTP media) between them to see how a PBXware instance behaves
under concurrent call load. Everything is driven from a web GUI: a Setup
Wizard walks you through connecting to one or two PBXware instances and
provisioning test extensions, and a Dashboard lets you ramp up local and
cross-server call volume on demand while watching a live log and call
graph of what's happening.

It's built to be deployed fresh wherever it's needed, not tied to any
one environment — the wizard collects all connection details (API keys,
IPs) at runtime, so nothing about a specific PBXware instance is baked
into the tool itself.

Tested on Ubuntu 24.04. The PBXware instance(s) you connect it to should
be fresh, dedicated test systems — not production — since this is how it
was tested and validated: SwarmDialer provisions (and later deletes)
real tenants, extensions, trunks, and DIDs on whatever instance you point
it at.

## Deploying on a fresh Ubuntu server

1. **Clone the repo** onto the server that will run the load test:

   ```
   mkdir /root/swarmdialer
   cd /root/swarmdialer
   git clone git@github.com:galijas/swarmdialer.git .
   ```

2. **Run the install script.** It installs `ca-certificates` and Go if
   either is missing, builds the binaries into `bin/`, and sets up a
   systemd service so the GUI starts on boot and restarts automatically
   if it ever dies:

   ```
   sudo ./install.sh
   ```

3. **Open `http://<server-ip>/`** in a browser and complete the Setup
   Wizard: connect to your PBXware instance (base URL + API key),
   provision a tenant and test extensions, and optionally connect a
   second PBXware instance for cross-server (trunk/DID) testing. Once
   configured, the page opens straight to the Dashboard on every later
   visit — reconfigure at any time from there.

Configuration (which servers are connected, what was provisioned) is
persisted to `swarmdialer_config.json` in the working directory, and is
specific to this deployment — nothing here is meant to be copied between
environments. Set `-config <path>` to change where it's stored.
