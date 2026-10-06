# Xenos as an infrastructure provider for PGDock

**Audience:** whoever builds the `xenos` provider inside PGDock (V3 §5.1, the `Provider` interface).
**Status:** the API below exists today and is covered by tests against the in-memory Proxmox; nothing here has run on a real Proxmox host yet. Gaps against PGDock V3 are listed in §6 so they are decided on, not discovered.

PGDock is a normal Xenos customer: it has an account, a wallet funded in naira that becomes USDT credit, and it is billed hourly per VM. There is no special back door. What it gets, beyond the dashboard, is **API tokens** and a few fields meant for programs.

## 1. Authentication

1. A person signs up, verifies their email and opens **Account → API tokens** (or `POST /v1/tokens` with a session).
2. The token looks like `xt_…`, is shown **once**, and is sent as `Authorization: Bearer xt_…`. Only its hash is stored.
3. Tokens last up to 365 days (or never), at most 10 per account, and are revoked in the dashboard or `DELETE /v1/tokens/{id}`. A password reset revokes all of them. A banned account's tokens stop working.
4. A token can: manage the account's **VMs** (create, list, get, delete, power, resize, snapshots, rebuild, labels), manage **SSH keys**, and **read** the wallet, plans and templates. It cannot: change the password, move money (`/wallet/convert`, wallet settings), mint tokens, open browser consoles, or reach `/admin` (all answer `403`).
5. Rate limit: 600 requests per minute per token (`429` beyond that).

Errors are `{"error": "message"}` with the usual status codes (`400` bad input, `401` no or bad credentials, `402` not enough balance, `403`, `404`, `409` conflict or busy, `422`, `429`, `502/503` host or wallet unavailable).

## 2. Mapping the `Provider` interface

| PGDock call | Xenos | Notes |
|---|---|---|
| `CreateServer` | `POST /v1/vms` | §3 |
| `DeleteServer` | `DELETE /v1/vms/{id}` | `202`; the VM is gone (`404`) when deletion finishes. Billing stops at the next hour boundary after the request. |
| `ListServers(filter)` | `GET /v1/vms?label=k=v&label=k2=v2` | Labels are ANDed. Without a filter you get every VM of the account. |
| `PriceCatalog` | `GET /v1/plans` | Prices are **micro-USDT per hour** (`price_uusdt_hourly`, 1 USDT = 1,000,000). Convert to naira with `GET /v1/wallet` → `rate_kobo_per_usdt` for display only. |
| `CreateVolume` / `AttachVolume` | not supported | §6. Disk comes with the plan. |
| `AssignFloatingIP` | not supported | §6. |
| Placement group | not supported | §6. |

### Sizes
PGDock's sizes need matching **plans** (an operator adds them; plans are rows in the `plans` table). Suggested slugs: `d-small` (2 vCPU, 4 GB, 40 GB), `d-medium` (4, 8, 80), `d-large` (8, 16, 160), `d-xlarge` (16, 32, 320). The `plan` field of a create names the plan by slug. A VM can later move to a larger plan only (`POST /v1/vms/{id}/resize`, §5), never a smaller one.

### Images
`template` is a template slug from `GET /v1/templates` (today `ubuntu-24.04` and `debian-12`). Arbitrary images are not supported. The default login user is in the VM's `ssh_user` (`root` unless the template says otherwise).

## 3. CreateServer

```http
POST /v1/vms
Authorization: Bearer xt_…
Idempotency-Key: pgdock-node-7f3a-create-1

{
  "plan": "d-small",
  "template": "ubuntu-24.04",
  "hostname": "pgd-eu-n7",
  "ssh_key_ids": [3],
  "labels": {"pgdock.node": "7f3a", "pgdock.role": "shared", "pgdock.region": "eu-central"},
  "boot_script": "#!/bin/bash\nset -e\ncurl -fsSL https://… | sh -s -- --token $TOKEN\n"
}
```

* **`ssh_key_ids`** are keys already uploaded with `POST /v1/ssh-keys` (the same token can do it). They are injected for `ssh_user`. At least one is required.
* **`Idempotency-Key`** (up to 128 characters) makes creation **at most once per account**: a repeat with the same key and the same plan, template and hostname returns the VM the first call made (`200`, not `202`); the same key with a different request is `422`. Use a stable key per intended server (for example a capacity proposal id) so a retried create after a timeout never makes a second, billed VM. Keys do not expire.
* **`labels`**: up to 16; keys `[a-z0-9._/-]` (1–63 chars, alphanumeric at both ends); values `[A-Za-z0-9._/:@+-]` (0–63). Replace the whole set later with `PATCH /v1/vms/{id}` `{"labels": {…}}`.
* **`boot_script`**: see §4.
* `202` with the VM in state `pending`. Otherwise `402` (balance does not cover 24 hours of usage for all the account's VMs), `409` (VM limit reached, or no free IP right now), `400`.

### Waiting for the server
Poll `GET /v1/vms/{id}` (every 5 s is fine; it normally takes under two minutes).

| `state` | Meaning |
|---|---|
| `pending`, `provisioning` | Being built. |
| `running` | Up and the guest agent answered. `ipv4` (and `ipv6` if configured) are set. |
| `error` | Provisioning failed after retries. **Not billed.** Delete it and create another. |
| `stopped`, `suspended` | Powered off by the customer, or by Xenos for an empty wallet. |
| `deleting` | Being removed. A deleted VM answers `404`. |

`busy` (`resizing`, `snapshotting`, `restoring`, `rebuilding`) is set while a long operation runs; power actions and further operations return `409` until it clears.

### Response (abridged)

```json
{
  "id": 41, "hostname": "pgd-eu-n7", "region": "eu-de-1", "plan": "d-small", "template": "ubuntu-24.04",
  "state": "running", "ipv4": "203.0.113.17", "ipv6": "2001:db8::129", "ssh_user": "root",
  "price_uusdt_hourly": 48000, "created_at": "…",
  "labels": {"pgdock.node": "7f3a"},
  "boot_script": {"status": "ok", "exit_code": 0, "output": "…"}
}
```

## 4. The boot script (Xenos' version of cloud-init user data)

Hetzner takes cloud-init user data. Xenos does **not**: the host's API cannot place cloud-init snippets, so PGDock's bootstrap cannot be a `#cloud-config` document. What Xenos offers instead:

* A script of up to 16 KiB, run **once, as root**, through the QEMU guest agent as soon as the VM is `running`: it is fed to `/bin/bash -s` on stdin. It is for work that finishes (install the agent, write config, register with the control plane); background anything long-lived (`nohup … &`, or enable a systemd unit).
* It has a 10-minute limit. The VM is already usable and billed while it runs.
* **At most once.** If it never started (guest agent not ready) Xenos retries; if it started and was interrupted (worker restart, timeout) it is recorded as failed and **not run again**, because it may not be repeatable. Make it idempotent anyway.
* The result is in the VM's `boot_script`: `status` is `none` (no script), `pending`, `running`, `ok` or `failed`; `exit_code` (`-1` when Xenos could not start or finish it); `output`, the **last 4 KiB** of stdout and stderr.
* **The script text is erased once it has run**, so a one-time registration token inside it is not kept. Do not print secrets: the output is stored and visible to the account.
* A `rebuild` takes its own `boot_script` and runs it after the rebuild.

If PGDock prefers, it can ignore this and SSH in with the key it supplied; the boot script exists so no inbound SSH is needed for the first step.

## 5. Other calls PGDock may want

| Need | Call |
|---|---|
| Power | `POST /v1/vms/{id}/start`, `/stop`, `/reboot` (`409` when the state does not allow it or the VM is busy) |
| Bigger plan | `POST /v1/vms/{id}/resize` `{"plan":"d-medium"}`: the VM restarts, the disk grows, the new price applies from the next charged hour. Refused with snapshots, or on a smaller plan. |
| Snapshot / restore | `POST /v1/vms/{id}/snapshots` `{"name":…}` (2 per VM), `POST …/snapshots/{sid}/restore` `{"confirm":true}`, `DELETE …/snapshots/{sid}` |
| Reinstall | `POST /v1/vms/{id}/rebuild` `{"template":…,"ssh_key_ids":[…],"boot_script":…,"confirm":true}`: erases the disk, keeps IP and plan |
| Cost and budget | `GET /v1/wallet`: `usdt_uusdt` (credit), `hourly_uusdt` (current burn), `runway_hours`, `unpaid_uusdt`, `grace_ends_at` |

Check `runway_hours` before creating servers: a create needs the balance to cover 24 hours of usage for **all** the account's VMs, and an empty wallet suspends the VMs after the charges go unpaid (72-hour grace, then deletion).

## 6. Gaps against PGDock V3, and what to do about them

| PGDock V3 expects | Xenos today | Plan |
|---|---|---|
| **HA on different physical hosts** (§2.2, placement group) | One Proxmox host. Two VMs share a host, so a host failure takes both. | A second host and a placement rule are on the Xenos backlog. **Until then do not sell the HA SLA on Xenos-backed nodes.** |
| **Floating IP** for the pooler pair (§2.1) | Each VM has one fixed routed address. VRRP between two VMs on the **same host's bridge** may work (they share layer 2) but is untested and nothing stops other tenants spoofing the address; across hosts it cannot work. | Reassignable, billed floating IPs with anti-spoofing are planned (docs/build-plan-next.md #6). Workaround for now: DNS-based failover for the pooler, with the health checker updating a low-TTL record. |
| **Volumes** (`CreateVolume`/`AttachVolume`) | None. Disk is part of the plan and can grow by resizing the plan. | Use larger plans for storage-heavy nodes. Separate volumes are not planned for V1. |
| **Private network** between nodes (§5.1 cloud-init joins one) | VMs have public addresses only, on the same bridge. | Use WireGuard between PGDock nodes, or restrict by firewall to the known addresses. A private network is a Xenos backlog item. |
| **cloud-init user data** | Boot script via the guest agent (§4). | As §4. |
| **Several regions** | One region per Xenos installation (`region` in the VM). | A Lagos installation would be a second Xenos deployment, and a second `xenos` provider instance in PGDock with its own token. |
| **Server types by name, `PriceCatalog` in a fiat currency** | Plans priced in micro-USDT per hour. | Convert with the wallet's rate for display; PGDock's own naira prices come from its price book (§3.9). |

## 7. Money between the two products

* Xenos bills the PGDock account hourly in USDT through iswallet. PGDock's own customers pay PGDock in naira (V3 §3). The two are separate ledgers: PGDock's cost attribution (V3 §5.4) should read `price_uusdt_hourly` and the wallet rate.
* Fund the Xenos account like any customer: bank transfer to its virtual account, converted once to USDT credit. Deposits arrive **net of the provider's fee** (Flutterwave's 1.4%, capped at ₦2,000 per deposit), so top up slightly more than the amount you want credited.
* iswallet's API is not the one drafted in PGDock V3 §3.4.6 (`/merchant/*` customers, mandates, checkout sessions). Xenos uses wallets, permanent virtual accounts, conversions with 60-second quotes, transfers with idempotency keys, and an HMAC-signed webhook envelope. Settle with the iswallet team what PGDock's merchant integration will really use before building it from §3.4.6.

## 8. Operator checklist (before PGDock goes live on Xenos)

- [ ] Plans added for each PGDock size, priced.
- [ ] PGDock's Xenos account created, email verified, `xenosctl user limit <email> <n>` raised above the default of 2.
- [ ] SSH key uploaded, API token created and stored in PGDock's secrets.
- [ ] Wallet funded and runway checked.
- [ ] A test create / wait / boot script / label list / delete cycle run against the real host with `Idempotency-Key` repeated once.
