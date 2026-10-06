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
