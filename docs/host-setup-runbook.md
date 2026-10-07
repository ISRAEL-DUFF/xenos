# Host setup runbook: from an empty server to a launch-ready Xenos host

This is the order to do things in. Every step ends with a **Check** that must pass before you go on. Where the commands come from a longer document, the step links to it instead of repeating it.

**Assumptions** (taken from `VPS V1 Weekend Build Plan.md`): one dedicated server at **Hetzner** with two NVMe disks, Debian 12 then Proxmox VE 8, a **routed** network (Hetzner drops unknown MAC addresses, so VMs are routed, not bridged), one extra IPv4 subnet, an IPv6 /64, and a Hetzner Storage Box for backups. At another provider steps 1, 2 and 4 change; the rest is the same. Everything in `<angle brackets>` is yours to fill in. **Nothing in this runbook has been run on a real host yet**: treat the first run as the test, do each Check, and send back the exact output of anything that fails.

Time: about one day if nothing goes wrong. Do it from a laptop with a stable connection, and keep the provider's **rescue system / remote console** open in another tab: several steps (network, firewall) can lock you out, and that is how you get back in.

---

## 0. Collect these first

| Item | Where it comes from | Used in |
|---|---|---|
| Server's main IPv4, its gateway and netmask | provider panel | step 4 |
| Extra IPv4 subnet, e.g. `203.0.113.8/29` | provider panel (routed to the main IP) | steps 4, 12 |
| IPv6 /64, e.g. `2001:db8:1::/64` | provider panel | steps 4, 12 |
| Your own public IP(s) (office, home, VPN) | `curl ifconfig.me` | steps 6, 7 |
| Control-plane VM's public IP | the cloud VM you create for `deploy/README.md` | steps 6, 8 |
| Region name, e.g. `eu-de-1` | your choice, goes in `XENOS_REGION` | step 12 |
| Storage Box host, user, password | provider panel | step 11 |
| A subnet plan for private networks | nothing to order: VLANs on `vmbr1` | step 4 |

Decide now: **node name** (`pve1`, never change it later) and the **VLAN range** you keep for yourself (Xenos uses 1000-3999 by default, so keep management VLANs below 1000).

---

## 1. Order the server

Server Auction box with at least 6 cores, 64 GB RAM, 2 x NVMe. Order the extra IPv4 subnet (a /29 gives 6 usable addresses: start there, you can add more) and note the routing: the subnet must be **routed to the server's main IP**, which is the default for Hetzner additional subnets.

**Check:** you have the rescue-system login and the numbers from step 0.

## 2. Install Debian 12 with RAID1

Boot the rescue system, run `installimage`, choose Debian 12, and set software RAID level 1 across both NVMe disks. Leave most of the space **unallocated** or in one large partition for LVM-thin (step 5):

```
SWRAID 1
SWRAIDLEVEL 1
HOSTNAME pve1.example.com
PART /boot ext4 1G
PART /     ext4 60G
PART lvm   vg0  all
```

(`PART lvm vg0 all` gives the rest to a volume group; step 5 builds the thin pool in it.) Reboot into the new system and log in over SSH with your key.

**Check:** `cat /proc/mdstat` shows both disks active `[UU]`; `lsblk` shows the large `vg0` area.

## 3. Install Proxmox VE 8

```sh
echo "deb [arch=amd64] http://download.proxmox.com/debian/pve bookworm pve-no-subscription" > /etc/apt/sources.list.d/pve.list
wget -qO /etc/apt/trusted.gpg.d/proxmox-release-bookworm.gpg https://enterprise.proxmox.com/debian/proxmox-release-bookworm.gpg
apt update && apt full-upgrade -y
apt install -y proxmox-default-kernel
reboot
```

After the reboot (now on the Proxmox kernel):

```sh
apt install -y proxmox-ve postfix open-iscsi chrony libguestfs-tools
apt remove -y os-prober
rm -f /etc/apt/sources.list.d/pve-enterprise.list     # no subscription: the enterprise repo returns 401
```

`/etc/hosts` must resolve the hostname to the **main IPv4** (not 127.0.1.1): `hostname --ip-address` must print it.

**Check:** `pveversion` prints `pve-manager/8.x`; `https://<main-ip>:8006` loads (accept the self-signed certificate for now; step 6 will lock the port down).

## 4. Network: routed `vmbr0`, private `vmbr1`

Edit `/etc/network/interfaces`. Keep your existing physical-interface lines as the installer wrote them (name, address, gateway); the parts to add or change are the two bridges. Example (replace the interface name, addresses and subnets):

```
auto lo
iface lo inet loopback
iface lo inet6 loopback

# physical NIC: keep the installer's stanza (static main IP, gateway, IPv6 with gateway fe80::1)
auto <nic>
iface <nic> inet static
    address <main-ip>/<mask>
    gateway <gateway>
iface <nic> inet6 static
    address <ipv6-host-address>/64
    gateway fe80::1

# VM bridge: routed. The bridge has an address from the extra subnet, which VMs use as their gateway.
auto vmbr0
iface vmbr0 inet static
    address <subnet-first-usable>/29          # e.g. 203.0.113.9/29
    bridge-ports none
    bridge-stp off
    bridge-fd 0
iface vmbr0 inet6 static
    address <ipv6-subnet>::1/64               # a VM-side /64 from your block, or the same /64, per provider docs

# Private networks: VLAN-aware, no uplink. Nothing outside the host may ever see this traffic.
auto vmbr1
iface vmbr1 inet manual
    bridge-ports none
    bridge-stp off
    bridge-fd 0
    bridge-vlan-aware yes
    bridge-vids 2-4094
```

Then forwarding, and apply:

```sh
cat > /etc/sysctl.d/99-xenos.conf <<'EOF'
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1
EOF
sysctl --system
ifreload -a        # if you lose SSH, use the provider's remote console to undo the change
```

**Why the pool and the gateway look like this:** with `vmbr0` holding `203.0.113.9/29`, VMs `203.0.113.10-14` are on-link to it. In Xenos you will load the pool as `ip add 203.0.113.10-203.0.113.14 203.0.113.9` and set **`XENOS_IPV4_PREFIX_LEN=29`** (the default 32 is for setups whose gateway is outside the VM's own /32; step 10's dry run settles which one your provider needs). If your provider's routed docs say otherwise, follow the provider and let step 10 decide.

**Check:** `ip -br a` shows `vmbr0` and `vmbr1` UP with the addresses above; you can still SSH in; `ping -c1 1.1.1.1` works from the host.

## 5. Storage: the `vmdata` thin pool

```sh
lvcreate -l 95%FREE -T vg0/vmdata_pool            # a thin pool using ~95% of what is left in vg0
pvesm add lvmthin vmdata --vgname vg0 --thinpool vmdata_pool --content rootdir,images
```

(If `vg0` does not exist because you used other partitioning, create a PV/VG on the free space first: `pvcreate`, `vgcreate vg0`.)

**Check:** `pvesm status` lists `vmdata` active with the expected size; `lvs` shows the pool. Snapshots work only on this kind of storage, so do not use a plain directory.

## 6. Proxmox firewall (required by Xenos) and port lockdown

Xenos needs the Proxmox firewall **on at datacenter and node level**, otherwise its per-VM anti-spoofing does nothing (`xenosctl preflight` fails without it). Turning it on blocks everything not allowed, so write the allow rules **first** and keep the provider console open.

`/etc/pve/firewall/cluster.fw`:

```
[OPTIONS]
enable: 1
policy_in: DROP
policy_out: ACCEPT

[IPSET admin]
<your-ip-1>
<your-ip-2>

[IPSET control]
<control-plane-vm-ip>

[RULES]
IN SSH(ACCEPT) -source +admin -log nolog
IN ACCEPT -source +admin -p tcp -dport 8006 -log nolog
IN ACCEPT -source +control -p tcp -dport 8006 -log nolog
IN ACCEPT -p icmp -log nolog
```

`/etc/pve/nodes/<node>/host.fw`:

```
[OPTIONS]
enable: 1
```

(VM traffic is not affected by these host rules: guests have their own firewall policy, which Xenos sets to ACCEPT with MAC and IP filtering. Guests are forwarded traffic, not input to the host.)

**Check:** from an address **not** in `admin` or `control`, `curl -m5 -k https://<main-ip>:8006` times out and SSH is refused; from your address both work. In the UI: Datacenter → Firewall → Options says Firewall: Yes, and the same on the node.

## 7. SSH and brute-force protection

```sh
adduser --disabled-password --gecos "" admin
usermod -aG sudo admin && mkdir -p /home/admin/.ssh && cp ~/.ssh/authorized_keys /home/admin/.ssh/ && chown -R admin:admin /home/admin/.ssh
sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/; s/^#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
systemctl reload ssh
apt install -y fail2ban
```

**Check:** open a **second** terminal and log in as `admin` with your key before closing the first; `ssh -o PubkeyAuthentication=no root@<host>` is refused.

## 8. The API user, role and token for Xenos

```sh
pveum role add XenosControl --privs "VM.Allocate VM.Clone VM.Audit VM.PowerMgmt VM.Console VM.Monitor \
  VM.Snapshot VM.Snapshot.Rollback VM.Config.CPU VM.Config.Memory VM.Config.Disk VM.Config.Network \
  VM.Config.Cloudinit VM.Config.Options VM.Config.HWType Datastore.AllocateSpace Datastore.Audit Sys.Audit SDN.Use"
pveum user add xenos@pve --comment "Xenos control plane"
pveum aclmod / --users xenos@pve --roles XenosControl
pveum user token add xenos@pve control --privsep 0
```

The last command prints the token **once**: this is `XENOS_PVE_TOKEN_SECRET`; the id is `xenos@pve!control`. `VM.Monitor` is the guest-agent privilege on Proxmox 8; on 9 it is `VM.GuestAgent.Unrestricted` (add it if your version has it). If a later step says `403 Permission check failed (…)`, the message names the missing privilege: add it to the role with `pveum role modify XenosControl --privs "<all privileges>" ` and retry. Once everything works you may narrow `/` to `/vms /storage/vmdata /sdn/zones/localnetwork /nodes` (see `deploy/proxmox/README.md` §3).

**Check** (from the control-plane VM's IP, which step 6 allows):

```sh
curl -sk -H "Authorization: PVEAPIToken=xenos@pve!control=<secret>" https://<main-ip>:8006/api2/json/nodes
```

prints your node. From any other address it must not connect.

## 9. Templates: Ubuntu 24.04 (9000) and Debian 12 (9001)

Run on the host. Debian first, the same pattern for Ubuntu (change the URL, name and ID):

```sh
cd /var/lib/vz/template
wget https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
# bake the guest agent in (also required for Xenos's boot scripts and floating IPs)
virt-customize -a debian-12-genericcloud-amd64.qcow2 --install qemu-guest-agent,iproute2,arping,cloud-guest-utils --run-command 'systemctl enable qemu-guest-agent'

qm create 9001 --name debian-12 --memory 1024 --cores 1 --net0 virtio,bridge=vmbr0 --ostype l26 \
   --scsihw virtio-scsi-pci --agent enabled=1 --vga std --serial0 socket
qm importdisk 9001 debian-12-genericcloud-amd64.qcow2 vmdata
qm set 9001 --scsi0 vmdata:vm-9001-disk-0 --boot order=scsi0 --ide2 vmdata:cloudinit
qm template 9001
```

Ubuntu: image `https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img`, VMID 9000, name `ubuntu-24.04`, same commands. Three details that matter to Xenos:

- `--vga std` (not `serial0`): the browser console needs a graphical display.
- The **guest agent** must be installed and `agent enabled=1`: provisioning waits for it.
- `ciuser`: Xenos creates VMs with the login user stored on the template (`root` for both seeded templates). Cloud images usually accept `ciuser=root` with the injected key, but some refuse direct root login. Step 10 tests exactly this; if it is refused, tell me which image: the fix is a template login user other than root (`templates.ci_user`, which has no CLI command yet, so I would add one) or a change in the image.

**Check:** `qm list` shows 9000 and 9001 as templates; `qm config 9001` shows `agent: enabled=1`, `vga: std` and `template: 1`.

## 10. The manual dry run (this decides the network settings)

Do by hand exactly what Xenos will automate, and time each step:

```sh
qm clone 9001 100 --name dry-run --full 1 --storage vmdata
qm set 100 --ciuser root --sshkeys /root/.ssh/authorized_keys \
   --ipconfig0 ip=203.0.113.10/29,gw=203.0.113.9,ip6=<ipv6-vm-address>/64,gw6=<ipv6-gateway> --nameserver "1.1.1.1 1.0.0.1"
qm resize 100 scsi0 20G
qm start 100
```

Then, from your laptop: `ssh root@203.0.113.10`, and inside: `ping -c2 1.1.1.1`, `ping -6 -c2 2606:4700:4700::1111`, `lsblk` (disk is 20G, filesystem grown).

- If this works with `/29` and gateway `203.0.113.9`: use `XENOS_IPV4_PREFIX_LEN=29` and load the pool with that gateway.
- If the VM has no route with that layout, try `ip=203.0.113.10/32,gw=<main-ip>` (gateway outside the /32; needs the image to accept an on-link gateway) and then `XENOS_IPV4_PREFIX_LEN=32`. Whichever works is your setting.

Clean up: `qm stop 100 && qm destroy 100 --purge`.

**Check:** SSH over IPv4 and IPv6 (as root with your key: the dry run sets `--ciuser root`, which is what Xenos uses), outbound ping both families, grown disk. Write down the step timings: provisioning must come in under 2 minutes in Xenos.

## 11. Outbound port 25 block and VM backups

Follow `deploy/proxmox/README.md` §1 (block TCP 25) and §2 (nightly backups to the Storage Box, plus the one-time restore test). The port-25 rules are generated by Xenos, so do §1 **after** step 12 (the control plane must exist to run `xenosctl firewall nft`).

## 12. Point the control plane at the host

On the control-plane VM (build and install per `deploy/README.md` §1-6), in `/etc/xenos/xenos.env`:

```
XENOS_REGION=<region>
XENOS_PVE_URL=https://<main-ip>:8006
XENOS_PVE_NODE=<node name>
XENOS_PVE_TOKEN_ID=xenos@pve!control
XENOS_PVE_TOKEN_SECRET=<secret from step 8>
XENOS_PVE_STORAGE=vmdata
XENOS_PVE_DISK=scsi0
XENOS_PVE_BRIDGE=vmbr0
XENOS_PVE_PRIVATE_BRIDGE=vmbr1
XENOS_PVE_INSECURE_TLS=true        # only until the host has a real certificate; port 8006 is firewalled to this VM
XENOS_IPV4_PREFIX_LEN=29           # or 32: whatever step 10 proved
XENOS_IPV6_PREFIX=<your /64>       # VM addresses are derived from this
XENOS_IPV6_GATEWAY=<ipv6 gateway>
XENOS_NAMESERVERS="1.1.1.1 1.0.0.1"
```

Then load addresses and check the templates:

```sh
xenosctl ip add 203.0.113.10-203.0.113.14 203.0.113.9
xenosctl ip add-floating <separate-extra-addresses>      # needs their own routed addresses, not the pool above
xenosctl template list                                    # 9000 and 9001 should show as ubuntu-24.04 and debian-12
```

**Check:** `xenosctl preflight` — every line in the PROXMOX group is `ok` (connection, storage, **firewall**, private bridge, both templates). Fix every FAIL before continuing; each message says what to change. Then do step 11's port-25 part.

## 13. First real VMs through Xenos, then the host checks

In the dashboard, sign up, verify the email, add an SSH key, create a VM. Then walk the **host** items in `docs/launch-checklist.md` in this order (each is a few minutes):

1. VM reaches `running` in under 2 minutes; SSH over IPv4 and IPv6 as the template's user.
2. Stop/start, snapshot twice and restore the first, resize, rebuild onto the other template, console shows the login prompt and accepts typing, boot script runs.
3. **Anti-spoofing** (the important one): create two VMs A and B. From A run `ip addr add <B's address>/29 dev eth0` and `arping -U -I eth0 -c3 <B's address>`: B must keep receiving its traffic, and A's packets as B's address must not leave (check with `tcpdump -ni vmbr0 host <B's address>` on the host while A pings out with `ping -I <B's address> 1.1.1.1`).
4. **Floating IP:** allocate one, attach to A, ping it from outside, move to B: it answers from B within seconds; detach and release.
5. **Private network:** put A and B on one network, ping across the 10.64.x addresses after the join restart. This proves cloud-init applied the address (if not, tell me what `ip -br a` shows inside the guest). From a VM of a **different account** on another network, try the same addresses: no reply.
6. **Empty-filter check:** detach A from its network and confirm nothing from A's old private NIC reaches B's VLAN (the checklist item on `ipfilter-net1`).
7. Port 25 blocked, port 587 reaches out (`deploy/proxmox/README.md` §1).
8. Backup restore tests (`deploy/proxmox/README.md` §2 and `deploy/README.md` §7).

Tick each item in `docs/launch-checklist.md` as it passes.

---

## When something fails

Send me: the step number, the exact command, its full output, and for Xenos problems `journalctl -u xenos-worker -n 80 -o cat` from the control plane. The usual suspects:

| Symptom | Likely cause |
|---|---|
| Locked out after step 4 or 6 | use the provider console; revert `/etc/network/interfaces` or empty `/etc/pve/firewall/cluster.fw` and `systemctl restart pve-firewall` |
| `403 Permission check failed (/…, VM.X)` | privilege missing from `XenosControl` (step 8) |
| Provisioning stalls at "waiting for the guest agent" | agent not installed in the template, or `agent enabled=1` missing (step 9) |
| VM boots but has no network | prefix length or gateway wrong (step 10), or `ip_forward` off |
| Console is black | template has `--vga serial0` (step 9) |
| Preflight says firewall is off | step 6: datacenter *and* node |
| Disk does not grow | image lacks `cloud-guest-utils`/growpart (step 9) |

## Later: a second host

Same steps 1-9 on the new server (give it its own node name and its own subnet), then follow **More than one host** in `deploy/proxmox/README.md` (hosts file, `ip add --host`, `template host`, preflight).
