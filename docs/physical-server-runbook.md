# Rehearsal on a physical server you do not own (a friend's or ISP's machine)

A real server with real KVM gives the closest test to the paid host: boot times, disk behaviour, snapshots, resize and every network feature are for real. Use this runbook when the server is on someone else's premises and you reach it remotely. For a rented dedicated server see `host-setup-runbook.md`; for a cheap VPS see `test-host-runbook.md`. The steps after the install are the same as those two, so this document points to them instead of repeating them.

Nothing here has been run yet. Do each Check and send me the exact output of anything that fails.

**The shape of it:** your friend does the physical parts once (BIOS, install, put it on the network, join it to a private VPN). Everything after that you do over the VPN. He never needs to be around again, except for a power cycle.

---

## 0. Is the machine suitable? (check before you spend a day)

| Need | Why |
|---|---|
| 64-bit CPU with VT-x or AMD-V, 4+ cores | KVM; Xenos refuses to pretend |
| 16 GB RAM or more (32 GB is comfortable) | Proxmox takes about 2 GB; each test VM 1-2 GB |
| 2 disks, SSD or NVMe, 200 GB+ total (1 disk works, but then a failure loses everything) | snapshots and resize need real storage |
| Wired Ethernet | Wi-Fi cannot be bridged |
| Remote console: IPMI/iDRAC/iLO/AMT, or he can sit at it for the install | a bad network change needs a way back |
| A UPS or a stable supply, and a BIOS option "power on after AC loss" | so a power cut does not need a visit |

Ask him for: CPU model, RAM, disk sizes and types, and whether it has IPMI. A desktop-class machine is fine for a rehearsal.

## 1. What your friend does (send him this section)

1. **BIOS/UEFI:** enable **Intel VT-x / AMD-V** (called "Virtualization Technology" or "SVM Mode"). Leave VT-d/IOMMU off unless he knows it. Set **Restore on AC power loss: Power On**. Boot from USB first.
2. **USB installer:** download the Proxmox VE 8 ISO from `https://www.proxmox.com/en/downloads`, write it to a USB stick (Rufus, balenaEtcher, or `dd`), boot the machine from it, and choose **Install Proxmox VE (Graphical)**.
3. **Installer answers:**
   - Target disk: **if two disks**, Options → filesystem **ZFS (RAID1)** with both disks; **if one disk**, `ext4` (default).
   - Country/time zone/keyboard: yours.
   - Password: a long one **you** choose; email: yours.
   - Hostname: `pvelab.local` (any single word plus a domain; the first label is the node name).
   - Network: the installer shows the NIC and an address. Use a **static** address on his LAN (he picks a free one outside the router's DHCP range, for example `192.168.1.50/24`, gateway = the router, DNS = the router or `1.1.1.1`). Write the address down.
4. **After the install, from his laptop** open `https://<that address>:8006` (accept the warning) and log in as `root` to confirm it works.
5. **Join the VPN** so you can reach it without opening anything on his router. On the server (Shell button in the web UI, or SSH):

   ```sh
   curl -fsSL https://tailscale.com/install.sh | sh
   tailscale up --authkey <key you send him> --hostname pvelab
   ```

   (You create a reusable auth key at `https://login.tailscale.com/admin/settings/keys`, set to expire in a day.) He does **not** forward any port on his router.
6. **Tell you** the LAN address and that it joined Tailscale. Done on his side.

**What he must not do:** expose port 8006 or 22 to the internet, or change the root password.

## 2. Connect and take over

Install Tailscale on your own computer and on the machine that will run the Xenos control plane (if they differ). Then:

```sh
ssh root@pvelab                    # over the tailnet; MagicDNS resolves the name
```

(If SSH does not accept the password, enable it once in `/etc/ssh/sshd_config` for the first login, then switch to a key and turn password login off as in step 7 of `host-setup-runbook.md`.)

```sh
pveversion                          # pve-manager/8.x
egrep -c '(vmx|svm)' /proc/cpuinfo ; ls /dev/kvm       # > 0 and the file exists: real KVM
lsblk ; pvesm status                # disks and the default storages
```

Remove the enterprise repository and update:

```sh
rm -f /etc/apt/sources.list.d/pve-enterprise.list /etc/apt/sources.list.d/ceph.list
echo "deb http://download.proxmox.com/debian/pve bookworm pve-no-subscription" > /etc/apt/sources.list.d/pve-no-sub.list
apt update && apt full-upgrade -y && apt install -y libguestfs-tools fail2ban
```

**Check:** you are in over Tailscale, KVM is present, `pvesm status` lists `local` and either `local-lvm` (ext4 install) or `local-zfs` (ZFS install). **The storage name you see is what Xenos is told in step 6** (`XENOS_PVE_STORAGE`): you do not need to create or rename anything.

## 3. Network: pick the case that matches his network

**Case A: LAN only (the usual one).** The VMs get private addresses behind the server and reach the internet through NAT. This is the same layout as the VPS rehearsal. Keep the installer's `vmbr0` for the **LAN side** (it already holds `192.168.1.50`), and add two new bridges to `/etc/network/interfaces`:

```
# VMs: private, NATed out through vmbr0
auto vmbr2
iface vmbr2 inet static
    address 10.77.0.1/24
    bridge-ports none
    bridge-stp off
    bridge-fd 0
    post-up   echo 1 > /proc/sys/net/ipv4/ip_forward
    post-up   iptables -t nat -A POSTROUTING -s 10.77.0.0/24 -o vmbr0 -j MASQUERADE
    post-down iptables -t nat -D POSTROUTING -s 10.77.0.0/24 -o vmbr0 -j MASQUERADE

# Customer private networks (VLANs): no uplink
auto vmbr1
iface vmbr1 inet manual
    bridge-ports none
    bridge-stp off
    bridge-fd 0
    bridge-vlan-aware yes
    bridge-vids 2-4094
```

Apply with `ifreload -a` (do it over the iDRAC/IPMI or with him standing by the first time; a mistake on `vmbr0` cuts you off). In Xenos set `XENOS_PVE_BRIDGE=vmbr2` and `XENOS_IPV4_PREFIX_LEN=24`; pool `10.77.0.10-10.77.0.60`, gateway `10.77.0.1`, floating pool `10.77.0.100-10.77.0.110`.

**Case B: the ISP gives you routed public addresses** (a subnet routed to the server's LAN address, or real public IPs on the LAN). Then follow step 4 of `host-setup-runbook.md` (a routed `vmbr0` with an address from the subnet as the VMs' gateway, `ip_forward` on). Set the prefix and gateway from the dry run in step 5 below.

**Check:** `ip -br a` shows the bridges UP; the server is still reachable over Tailscale and its LAN address.

## 4. Firewall (required)

Same as step 6 of `host-setup-runbook.md`, with the allowed sources being the **tailnet**, not a public IP:

`/etc/pve/firewall/cluster.fw`:

```
[OPTIONS]
enable: 1
policy_in: DROP
policy_out: ACCEPT

[IPSET admin]
100.64.0.0/10
192.168.1.0/24

[RULES]
IN SSH(ACCEPT) -source +admin -log nolog
IN ACCEPT -source +admin -p tcp -dport 8006 -log nolog
IN ACCEPT -p udp -dport 41641 -log nolog
IN ACCEPT -p icmp -log nolog
```

(Keep `192.168.1.0/24` so he can still reach it from the LAN if Tailscale breaks; the UDP rule is Tailscale's direct-connection port.) `/etc/pve/nodes/pvelab/host.fw`: `[OPTIONS]` then `enable: 1`.

**Check:** over Tailscale both SSH and `:8006` work; Datacenter and node Firewall show **Yes**. From his LAN laptop the UI still loads.

## 5. API token, templates, dry run

- Role, user and token: step 8 of `host-setup-runbook.md`.
- Templates 9000 and 9001: step 9 of the same file, with these storage differences: on ext4 (`local-lvm`) use `qm importdisk 9001 <image> local-lvm` then `qm set 9001 --scsi0 local-lvm:vm-9001-disk-0 ...`; on ZFS (`local-zfs`) the same with `local-zfs`. Use the storage name from step 2 everywhere you see `vmdata`, and `--net0 virtio,bridge=vmbr2` (Case A) or `vmbr0` (Case B).
- Manual dry run (step 10 of that file): clone, `ip=10.77.0.10/24,gw=10.77.0.1`, SSH from the server, `ping 1.1.1.1`, resize to 20 GB, destroy. **Time each step**: on real KVM and real disks these timings are meaningful (provisioning must come in under 2 minutes).

**Check:** the dry-run VM boots, has network, SSH works from the server, grown disk.

## 6. The Xenos control plane

Run it on your computer or any Linux box on the tailnet, in development mode with the fake wallet, exactly as in step 8 of `test-host-runbook.md`. The environment differs only here:

```
XENOS_PVE_URL=https://pvelab:8006          # the tailnet name
XENOS_PVE_NODE=pvelab
XENOS_PVE_STORAGE=local-lvm                # or local-zfs: whatever step 2 showed
XENOS_PVE_BRIDGE=vmbr2                     # Case A; vmbr0 in Case B
XENOS_PVE_PRIVATE_BRIDGE=vmbr1
XENOS_IPV4_PREFIX_LEN=24                   # from the dry run
# no DISABLE_KVM: this machine has real KVM
```

Then `xenosctl ip add 10.77.0.10-10.77.0.60 10.77.0.1`, `xenosctl ip add-floating 10.77.0.100-10.77.0.110`, and `xenosctl preflight`.

**Check:** the whole PROXMOX group is `ok` and **KVM is `ok`** (a failure there means a BIOS or kernel problem, not something to ignore). The environment/IPv6/SMTP/iswallet lines may fail on a rehearsal; Proxmox lines must not.

## 7. The tests

Run the nine tests in step 10 of `test-host-runbook.md` in order (create, power, snapshots, resize, rebuild, console, boot script, anti-spoofing, floating IP, private networks, port 25, optionally a second host). Differences from the VPS rehearsal, to your advantage:

- Timings are real: **a created VM must reach `running` in under 2 minutes**, resize and rebuild must not hang.
- You can reach the VMs from the server (`ssh root@10.77.0.10`) and from your computer if you add a route over Tailscale (`tailscale up --advertise-routes=10.77.0.0/24` on the server, approve it in the Tailscale admin page, then `ssh root@10.77.0.10` works from your laptop).
- **Two disks give you something the VPS could not:** pull one disk's cable while a VM runs (ask him; only with the ZFS mirror) and check the VM keeps running and Xenos alerts. Do not do this on a machine with data you care about.
- **Power loss:** ask him to cut the power once while VMs run, then power it on: Proxmox must come back, and within a few minutes the worker's monitor must notice the guests Xenos believes are running but the host reports stopped and start them again (you get an alert saying so; a VM you stopped yourself minutes before the cut is left alone). Check each VM comes back with its IP, floating IP and private network.

## 8. Backups and the 8 hour check

Add a second disk or a USB disk as a directory storage and run the nightly backup and restore test from `deploy/proxmox/README.md` §2 against it (the Storage Box part does not apply). Then leave the lab running overnight with a couple of VMs and check in the morning: no failed jobs (`xenosctl` admin jobs page), no alerts, metering rows for every hour (`/wallet` → Statements).

## What this proves, and what it does not

**Proves:** the whole host side with real KVM and real disks: timings, snapshots, resize, rebuild, console, boot scripts, anti-spoofing, floating IPs, private networks, firewall and port-25 rules, backups, power-cut recovery, and (with a second machine) placement.

**Does not prove:** how a data-centre provider routes you a subnet or filters MAC addresses, IPv6 routing from a provider, outbound mail reputation, and noisy-neighbour network conditions. Those are the short session on the paid server.

## When something fails

Send me the step number, the command, its full output, and for Xenos problems the last 80 lines of the control plane log. Typical: `403 Permission check failed (…)` means a privilege is missing from `XenosControl`; "waiting for the guest agent" means the agent is missing from the template or the VM has not booted; no VM network means `ip_forward` is off, the MASQUERADE rule is missing, or the VM's bridge in the template does not match `XENOS_PVE_BRIDGE`; you are locked out after a network or firewall change means use the IPMI console or ask him to attach a keyboard and revert `/etc/network/interfaces` or empty `/etc/pve/firewall/cluster.fw`.
