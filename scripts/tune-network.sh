#!/bin/sh
# Run by the swarmdialer service before it starts (ExecStartPre).
#
# Gives the outgoing network interface(s) a longer transmit queue. At 512
# calls SwarmDialer sends about 50,000 RTP packets a second; the default
# 1000-packet queue holds about 20ms of that, and overflowed (dropping
# packets before they left the VPS) whenever the busy SERVERware host
# briefly delayed the VPS's network. 10000 packets is about 200ms.
for dev in $(ip -o route show default | sed -n 's/.* dev \([^ ]*\).*/\1/p' | sort -u); do
  ip link set dev "$dev" txqueuelen 10000
done
