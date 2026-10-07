# Proxmox host: mail block, VM backups, API access

These steps run **on the Proxmox host**. They complete Phase 4 for the host; the control plane is covered in [../README.md](../README.md).

## 1. Block outbound SMTP (port 25)

Customer VMs must not send mail directly: it is the first thing spammers try, and it ruins the host IP's reputation.

On the control-plane VM, render the rules (this reads which VMs a human has exempted):

```sh
xenosctl firewall nft vmbr0 > xenos_mail.nft
scp xenos_mail.nft root@<proxmox-host>:/etc/nftables.d/xenos_mail.nft
```

On the host, make sure `/etc/nftables.conf` ends with `include "/etc/nftables.d/*.nft"`, then:

```sh
nft -c -f /etc/nftables.d/xenos_mail.nft     # syntax check first
nft -f /etc/nftables.d/xenos_mail.nft        # load (replaces the xenos_mail table; other rules are untouched)
systemctl enable nftables
```

The rules live in the `forward` hook because routed VM traffic passes through the host. They drop TCP 25 from `vmbr0` for every VM except addresses in the exempt sets.

**Exempting a VM after review:** `xenosctl port25 allow <vm-id>`, then re-run the three commands above. `xenosctl port25 block <vm-id>` reverses it.

**Verify (part of the Phase 1 done-when):** from inside a test VM, `timeout 5 bash -c '</dev/tcp/smtp.gmail.com/25' ; echo $?` must time out (non-zero), while `…/587` connects. `nft list ruleset | grep -A2 'dport 25'` shows the drop counter increasing.

## 2. Nightly VM backups to the Storage Box

Add the Storage Box as Proxmox storage (SMB/CIFS is simplest; the host needs `cifs-utils`):

```sh
pvesm add cifs storagebox --server u123456.your-storagebox.de --share backup \
    --username u123456 --password '<password>' --content backup --subdir /xenos-vms
```

Create the job (all VMs, snapshot mode, 02:00, keep the last 3):

```sh
pvesh create /cluster/backup --id xenos-nightly --storage storagebox --all 1 \
    --mode snapshot --compress zstd --schedule '02:00' --prune-backups keep-last=3 \
    --mailnotification failure --mailto you@example.com --enabled 1
```

Snapshot mode needs the QEMU guest agent in the guest (already in the templates). Tell customers V1 backups are best-effort.

**Restore test (do once before launch):**

```sh
pvesm list storagebox | tail                       # find a volid
qmrestore <volid> 9999 --storage vmdata --unique 1
qm start 9999 && qm agent 9999 ping                # boots and the agent answers
qm stop 9999 && qm destroy 9999 --purge
```

## 3. Lock down API access

The control plane reaches the host only through the Proxmox API (port 8006) with an API token, so allow 8006 **only** from the control-plane VM's IP and your own IP, and port 22 only from your IP (Proxmox firewall or the provider's firewall). Create the token with the narrowest role that can clone, configure, resize, power and destroy VMs on `vmdata` and the bridge, plus read access to node and storage status (used by the monitor). Do not use `root@pam`. The role needs, besides clone/config/power/destroy: `VM.Snapshot` and `VM.Snapshot.Rollback` (customer snapshots), `VM.Config.CPU`, `VM.Config.Memory` and `VM.Config.Disk` (resize), `VM.Console` (browser console) and `VM.GuestAgent.Unrestricted` (the boot script runs through the guest agent; on Proxmox 8 this is `VM.Monitor`). The console needs the guest to keep a **VGA display** (`qm set <template> --vga std`): a template that sets `--vga serial0` shows a blank console. Snapshots need snapshot-capable storage (LVM-thin is); they use pool space, so keep an eye on the pool alert.

**Private networks.** Customers can create private networks (`/24`s from 10.64.0.0/10, one VLAN id each) and give their VMs a second NIC on one. On every host create a bridge for them, VLAN-aware and **without an uplink** (nothing outside should ever see the traffic):

```
auto vmbr1
iface vmbr1 inet manual
    bridge-ports none
    bridge-stp off
    bridge-fd 0
    bridge-vlan-aware yes
    bridge-vids 2-4094
```

Set `XENOS_PVE_PRIVATE_BRIDGE` (default `vmbr1`; per host `private_bridge` in the hosts file) and the VLAN range Xenos may use with `XENOS_PRIVATE_VLAN_MIN`/`MAX` (default 1000-3999; keep any VLANs you use yourself outside it). `xenosctl preflight` warns when the bridge is missing. The guest firewall (macfilter and an `ipfilter-net1`/`net2` set holding only the VM's private address) is applied to the second NIC as it is to the first. Private addresses are applied by cloud-init: a join stops the VM, changes `net1`/`ipconfig1` and starts it, and the guest must re-read its network config at that start (the templates' cloud-init does, since the instance configuration changed); check this once on a real host. A network is **pinned to one host**: later VMs of the network are placed there. To let one network span hosts you must connect the hosts' `vmbr1` bridges yourself, then set `XENOS_PRIVATE_NETWORK_TUNNEL=true`. A VXLAN recipe, run on each host (replace the peer addresses; use the hosts' private or WireGuard addresses, never public ones unencrypted):

```
# /etc/network/interfaces.d/vxlan1 on host A (peer = host B), mirrored on B
auto vxlan1
iface vxlan1 inet manual
    pre-up ip link add vxlan1 type vxlan id 4242 dstport 4789 local <A-underlay-ip> remote <B-underlay-ip> nolearning || true
    up ip link set vxlan1 up mtu 1450
    post-up ip link set vxlan1 master vmbr1
    post-down ip link del vxlan1 || true
```

VXLAN and WireGuard cost MTU: private NICs should use an MTU of 1400 or less (1450 on VXLAN over a clean 1500 link, less over WireGuard); set it in the guest or have the template's cloud-init do so. Xenos cannot configure this tunnel through the Proxmox API, does not check that it works, and with more than two hosts you need a full mesh or a hub.

**More than one host.** Hosts are standalone Proxmox nodes (no cluster, no shared storage), each with its own routed subnet. Describe them in a YAML file on the control plane (mode 0600, it holds the API secrets) and point `XENOS_HOSTS_FILE` at it:

```yaml
hosts:
  - name: pve1               # lowercase letters, digits, hyphens; never change it once VMs exist
    region: lagos-1          # default XENOS_REGION; only hosts in the platform region receive VMs
    url: https://10.0.0.1:8006
    node: pve1
    token_id: xenos@pve!control
    token_secret: "…"
    insecure_tls: false
    storage: vmdata          # defaults come from XENOS_PVE_STORAGE / XENOS_PVE_DISK / XENOS_PVE_BRIDGE / XENOS_NAMESERVERS
    disk: scsi0
    bridge: vmbr0
    ipv6_prefix: 2001:db8:1::/64
    ipv6_gateway: 2001:db8:1::1
    nameservers: 1.1.1.1 1.0.0.1
  - name: pve2
    url: https://10.0.0.2:8006
    node: pve2
    token_id: xenos@pve!control
    token_secret: "…"
```

Unknown keys are refused. Without the file the `XENOS_PVE_*` variables describe one host named `default`, so an upgrade changes nothing. After adding a host: start the API or worker once (it registers the host), then load its addresses with `xenosctl ip add <range> <gateway> --host pve2` (and `ip add-floating … --host pve2`), tell Xenos where each template lives with `xenosctl template host <slug> pve2 <vmid>` (VMIDs may differ per host but must be unique on a host), and run `xenosctl preflight`, which checks every host and every template on it. A host with a template or addresses missing is skipped by placement. VMIDs come from one global sequence, so a VMID identifies its host: do not create VMs by hand on a Xenos host with VMIDs from Xenos's range. `xenosctl host drain <name>` stops new VMs landing on a host (existing ones keep running); `enable` undoes it. Placement picks the host with the lowest committed-RAM fraction (`XENOS_RAM_COMMIT_LIMIT`, default 1.0 = 100%, bounds how much RAM may be promised) and VMs of one `spread_group` go on different hosts. A VM stays on its host for life.

**Anti-spoofing (required).** Every guest is created with the Proxmox VM firewall on, with `macfilter` and `ipfilter` and an `ipfilter-net0` set holding exactly its own IPv4 and IPv6 (and later its floating IPs), so one VM cannot answer ARP for, or send from, another VM's address. This only works when the firewall is **enabled at both datacenter and node level** (Datacenter → Firewall → Options → Firewall: Yes; the same on the node); `xenosctl preflight` fails if it is not. The guest's own input/output policy is set to ACCEPT, so what reaches guests is still decided by your host rules. The API token's role also needs `VM.Config.Network` (set NIC firewall, edit the VM firewall and its IP set) and `Sys.Audit` (preflight reads the firewall state). Link-local IPv6 (`fe80::`) derived from the guest's MAC is allowed automatically by Proxmox. **Host check:** from one test VM try `ip addr add <another VM's address>/32 dev eth0` and ping out, or answer ARP for it with `arping -U -I eth0 <address>`: the traffic must be dropped.
