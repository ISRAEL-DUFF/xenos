# Rehearsal on an ordinary VPS (InterServer or similar)

A cheap KVM VPS running Proxmox can prove almost everything Xenos does with a host: provisioning, power, snapshots, resize, rebuild, console, boot scripts, floating IPs, anti-spoofing, private networks, and (with two such VPSs) placement and spread groups. It cannot prove speed, real public routing or Hetzner's network rules; those wait for the real server (`docs/host-setup-runbook.md`). Do this first: every problem you find here is one you will not meet on the paid box.

The VMs in this setup sit on a **private NAT network** behind the VPS (they are not reachable from the internet), so you reach them through the VPS.

As with the real runbook, nothing here has been run yet: do each Check, and send me the exact output of anything that fails.

---

## 0. Pick and check the VPS

Order a **KVM** VPS (not OpenVZ/LXC) with at least **4 vCPU, 8 GB RAM and 80 GB disk**, Debian 12, root access and a web/VNC console. Before installing anything, log in and check:

```sh
systemd-detect-virt            # must say kvm (or qemu)
egrep -c '(vmx|svm)' /proc/cpuinfo   # > 0: the VPS exposes hardware virtualization (nested KVM)
ls /dev/kvm                    # must exist if the line above is > 0
```

- **Both present:** nested KVM works. VMs will boot at a normal-ish speed. Best case.
- **Count is 0:** ask InterServer support to enable nested virtualization on the VPS (many KVM hosts can). If they cannot, you can still test with **software emulation**: set `XENOS_PVE_DISABLE_KVM=true` in step 9. Everything works but each VM takes several minutes to boot, so also set `XENOS_PROVISION_TIMEOUT=15m`.

Take a snapshot of the VPS in their panel before step 1 (installing a new kernel on a VPS is the one step that can leave it unbootable; the snapshot and the provider's VNC console are your way back).

## 1. Prepare Debian

```sh
hostnamectl set-hostname pvetest        # one word: this is the Proxmox node name
echo "$(hostname -I | awk '{print $1}') pvetest.example.com pvetest" >> /etc/hosts
apt update && apt full-upgrade -y
```

`hostname --ip-address` must print the VPS's public IP (not 127.0.1.1).

## 2. Install Proxmox VE 8

Same as step 3 of `host-setup-runbook.md`:

```sh
echo "deb [arch=amd64] http://download.proxmox.com/debian/pve bookworm pve-no-subscription" > /etc/apt/sources.list.d/pve.list
wget -qO /etc/apt/trusted.gpg.d/proxmox-release-bookworm.gpg https://enterprise.proxmox.com/debian/proxmox-release-bookworm.gpg
apt update && apt full-upgrade -y
apt install -y proxmox-default-kernel
reboot
```

After the reboot (watch it on the VNC console the first time):

```sh
apt install -y proxmox-ve postfix open-iscsi chrony libguestfs-tools
apt remove -y os-prober
rm -f /etc/apt/sources.list.d/pve-enterprise.list
```

**Check:** `pveversion` prints `pve-manager/8.x` and `https://<vps-ip>:8006` loads.

## 3. Network: a private NAT bridge for the VMs

Do **not** bridge the VPS's public NIC. Add two bridges with no uplink, plus NAT so VMs can reach the internet. Append to `/etc/network/interfaces` (leave the existing public-NIC lines exactly as they are):

```
# VMs live here: private addresses, NAT to the internet through the VPS
auto vmbr0
iface vmbr0 inet static
    address 10.77.0.1/24
    bridge-ports none
    bridge-stp off
    bridge-fd 0
    post-up   echo 1 > /proc/sys/net/ipv4/ip_forward
    post-up   iptables -t nat -A POSTROUTING -s 10.77.0.0/24 -o <public-nic> -j MASQUERADE
    post-down iptables -t nat -D POSTROUTING -s 10.77.0.0/24 -o <public-nic> -j MASQUERADE

# Private networks (customer VLANs): VLAN-aware, no uplink
auto vmbr1
iface vmbr1 inet manual
    bridge-ports none
    bridge-stp off
    bridge-fd 0
    bridge-vlan-aware yes
    bridge-vids 2-4094
```

`<public-nic>` is the name from `ip -br a` (for example `eth0` or `ens3`). Apply with `ifreload -a`.

**Check:** `ip -br a` shows `vmbr0 10.77.0.1/24` and `vmbr1`, and you can still SSH in.

## 4. Storage: `vmdata` as qcow2 on a directory

A VPS has no spare disk for LVM-thin, and a directory storage with qcow2 images supports everything Xenos needs, including snapshots:

```sh
mkdir -p /var/lib/vmdata
pvesm add dir vmdata --path /var/lib/vmdata --content images,rootdir --shared 0
```

**Check:** `pvesm status` lists `vmdata` active. (On the real server this becomes an LVM-thin pool; Xenos does not care which.)

## 5. Firewall (required): lock the host down

The VPS has a public IP, so this matters more here than anywhere: the Proxmox UI and SSH must only be reachable from you. Write the rules **before** enabling, and keep the provider's VNC console open.

`/etc/pve/firewall/cluster.fw`:

```
[OPTIONS]
enable: 1
policy_in: DROP
policy_out: ACCEPT

[IPSET admin]
<your-home-or-office-ip>

[IPSET control]
<where the control plane runs: its public IP, or your own IP if it runs on your laptop>

[RULES]
IN SSH(ACCEPT) -source +admin -log nolog
IN ACCEPT -source +admin -p tcp -dport 8006 -log nolog
IN ACCEPT -source +control -p tcp -dport 8006 -log nolog
IN ACCEPT -p icmp -log nolog
```

`/etc/pve/nodes/pvetest/host.fw`: `[OPTIONS]` then `enable: 1`.

If your home IP changes (mobile data, ISP), you lock yourself out: use the VNC console to fix the set, or put the VPS and your laptop on **Tailscale** (`curl -fsSL https://tailscale.com/install.sh | sh`) and allow only the `100.64.0.0/10` tailnet in `admin` and `control`.

**Check:** from a phone on mobile data, `https://<vps-ip>:8006` does not load; from your address it does. Datacenter → Firewall → Options says Firewall: Yes, and the same on the node.

## 6. API role and token

Exactly step 8 of `host-setup-runbook.md` (role `XenosControl`, user `xenos@pve`, token `control`). Keep the secret.

## 7. Templates

Same as step 9 of `host-setup-runbook.md`, with one difference for directory storage: import the disk as qcow2 with the newer one-line form:

```sh
cd /var/lib/vz/template
wget https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
virt-customize -a debian-12-genericcloud-amd64.qcow2 --install qemu-guest-agent,iproute2,arping,cloud-guest-utils \
   --run-command 'systemctl enable qemu-guest-agent'
qm create 9001 --name debian-12 --memory 1024 --cores 1 --net0 virtio,bridge=vmbr0 --ostype l26 \
   --scsihw virtio-scsi-pci --agent enabled=1 --vga std --serial0 socket
qm set 9001 --scsi0 vmdata:0,import-from=/var/lib/vz/template/debian-12-genericcloud-amd64.qcow2,format=qcow2 \
   --boot order=scsi0 --ide2 vmdata:cloudinit
qm template 9001
```

Do the same for Ubuntu 24.04 as 9000 (`https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img`). You can skip Ubuntu for a first pass: the rebuild test needs two templates, so add it before step 11's rebuild check.

**Check:** `qm config 9001` shows `agent: enabled=1`, `vga: std`, `template: 1`. Manual dry run (step 10 of the real runbook) with `ip=10.77.0.10/24,gw=10.77.0.1`: the clone boots, `ssh root@10.77.0.10` works **from the VPS**, and inside it `ping 1.1.1.1` works (NAT). Destroy it afterwards.

## 8. The control plane for the rehearsal

Easiest: run it on **your own computer** (Linux or macOS, with Go and Postgres), in development mode. That uses the in-memory fake wallet (`XENOS_FAKE_ISPEND_CREDIT_UUSDT` gives every account credit), which is fine: billing was verified against the iswallet sandbox earlier, and this rehearsal is about the host. If your computer's address is not static, use Tailscale (step 5) so the VPS firewall can allow it.

```sh
git clone <your repo> && cd xenos && git checkout claude/magical-archimedes-x4xxoe
make web && go build -o /tmp/xenos-api ./cmd/api && go build -o /tmp/xenosctl ./cmd/xenosctl
createdb xenos_rehearsal      # or any Postgres you have
cat > rehearsal.env <<'EOF'
XENOS_DATABASE_URL=postgres://<user>:<pw>@localhost:5432/xenos_rehearsal?sslmode=disable
XENOS_HTTP_ADDR=127.0.0.1:8080
XENOS_PUBLIC_URL=http://localhost:8080
XENOS_COOKIE_SECURE=false
XENOS_RUN_WORKER=true
XENOS_FAKE_ISPEND_CREDIT_UUSDT=5000000
XENOS_METER_SPREAD_MINUTES=0
XENOS_REGION=test-1
XENOS_PVE_URL=https://<vps-ip-or-tailscale-name>:8006
XENOS_PVE_NODE=pvetest
XENOS_PVE_TOKEN_ID=xenos@pve!control
XENOS_PVE_TOKEN_SECRET=<secret>
XENOS_PVE_INSECURE_TLS=true
XENOS_PVE_STORAGE=vmdata
XENOS_PVE_DISK=scsi0
XENOS_PVE_BRIDGE=vmbr0
XENOS_PVE_PRIVATE_BRIDGE=vmbr1
XENOS_IPV4_PREFIX_LEN=24
XENOS_NAMESERVERS="1.1.1.1 9.9.9.9"
# only if step 0 found no nested KVM:
# XENOS_PVE_DISABLE_KVM=true
# XENOS_PROVISION_TIMEOUT=15m
EOF
set -a; . ./rehearsal.env; set +a
/tmp/xenos-api > /tmp/xenos-api.log 2>&1 &
/tmp/xenosctl ip add 10.77.0.10-10.77.0.60 10.77.0.1
/tmp/xenosctl ip add-floating 10.77.0.100-10.77.0.110
/tmp/xenosctl preflight
```

**Check:** preflight's PROXMOX group is `ok` for connection, storage, **firewall**, private bridge and both templates. **Expected failures on a rehearsal:** the `KVM` line (only if you set `DISABLE_KVM`), the `environment` and IPv6 lines, and anything about production-only settings (SMTP, real iswallet). Everything about Proxmox itself must pass.

The dashboard is at `http://localhost:8080`. Sign up; the verification email is logged, not sent: `grep 'verify' /tmp/xenos-api.log | tail -1` gives the link.

## 9. Reaching the VMs

The VMs are on `10.77.0.0/24` behind the VPS. From your computer: `ssh -J root@<vps-ip> root@10.77.0.10` (ProxyJump through the host). Or just SSH to the VPS and `ssh root@10.77.0.10` from there.

## 10. The test sequence

Walk these in order; each corresponds to an item in `docs/launch-checklist.md` (tick them there once the *real* box also passes, except where noted).

1. **Create a VM** in the dashboard. It must reach `running` and accept SSH from the host. (Timing means nothing on emulation.)
2. **Stop, start, reboot; snapshot twice, change a file, restore the first; resize; rebuild onto the other template; delete.**
3. **Console:** open it in the dashboard; the login prompt must show and accept typing.
4. **Boot script:** create a VM with one (`echo hi > /root/ran`); after it runs, the file exists and the dashboard shows the outcome.
5. **Anti-spoofing** (the key one): VMs A and B. On A: `ip addr add <B's address>/24 dev eth0; arping -U -I eth0 -c3 <B's address>`. On the host: `tcpdump -ni vmbr0 arp` should show it, but A's traffic *as B's address* must be dropped (`ping -I <B's address> 10.77.0.1` from A gets no reply), while B keeps working.
6. **Floating IP:** allocate one, attach to A, `ping` it from the host and `ip -br a` inside A shows it; move to B: it answers from B within seconds and A no longer has it; stop and start B: it is still there; detach, release.
7. **Private network:** create one, put A and B on it (they restart), `ping` across their `10.64.x.x` addresses; create a second account with its own network and VM and confirm it cannot reach the first network's addresses. This also proves cloud-init applies the second NIC's address.
8. **Port 25:** on the control plane `xenosctl firewall nft vmbr0 > xenos_mail.nft`, copy to the VPS `/etc/nftables.d/`, load it, and from a VM check `timeout 5 bash -c '</dev/tcp/smtp.gmail.com/25'` fails while `…/587` connects.
9. **Two hosts (optional):** a second small VPS the same way as steps 1-7 (node name `pvetest2`, subnet `10.78.0.0/24`), then follow **More than one host** in `deploy/proxmox/README.md`. Create two VMs with the same `spread_group`: they land on different hosts.

## Record everything

The whole rehearsal is logged in `docs/rehearsal/` (see its README): every step, every problem and fix, with no secrets. Keep it up to date as you go, and fix this runbook whenever a step turns out to be wrong.

## What this does not prove

Speed (the under-2-minutes target), your provider's real routed subnet and the `/29` versus `/32` prefix question, IPv6 routing, real outbound IP and port-25 behaviour, RAID, and the Storage Box backups. Those are the short session on the real server.

## When something fails

Send me the step number, the command, its full output, and the last 80 lines of `/tmp/xenos-api.log` (it contains the worker's errors). Common causes: `403 Permission check failed (…)` means a privilege is missing from the role; "waiting for the guest agent" means the agent is not in the template or the VM has not booted (emulation: raise `XENOS_PROVISION_TIMEOUT`); a VM with no network means `ip_forward` or the MASQUERADE rule is missing (step 3).
