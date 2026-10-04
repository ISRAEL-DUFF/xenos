# VPS V1 Weekend Build Plan

Oct 3, 2026 · @EaziDeFi

## V1 scope

By Sunday night, a paying user can sign up in a web dashboard, fund a naira wallet with Paystack, create a Linux VM with their SSH key, and get billed hourly from that wallet. Everything runs on one rented server with Proxmox and a single Go service backed by Postgres.

**In scope**

- One dedicated host running Proxmox VE, with public IPv4 for VMs plus an IPv6 /64
- Two OS templates (Ubuntu 24.04, Debian 12) built from cloud images with cloud-init
- Three fixed plans, for example Nano (1 vCPU, 1 GB, 20 GB), Small (2 vCPU, 2 GB, 40 GB), Medium (2 vCPU, 4 GB, 80 GB)
- Go API: signup/login, SSH keys, create/list/reboot/delete VM, wallet balance, top-up
- Full web dashboard from day one: customer pages for VMs, SSH keys, wallet and account, plus an admin view (Phase 5)
- Billing through the iSpend wallet: NGN funding by virtual account or card, converted once into USDT compute credit at a daily rate; hourly metering in USDT
- Auto-suspend at zero balance, delete after a grace period
- Minimal guardrails: email verification, port 25 blocked, per-user VM cap, nightly backups

**Out of scope for V1**

- Browser console (noVNC) inside the dashboard
- Custom ISOs, resizing, snapshots by users, private networking, floating IPs, load balancers
- Multiple hosts or regions, live migration, high availability
- Managed Postgres (the natural V2 product on the same control plane)
- Invoicing, VAT handling, teams or organisations

**Assumptions**

Plans are priced in USDT, held as compute credit in each customer's iSpend wallet; customers pay and see prices in naira, converted at a daily rate (Phase 3). Every VM carries a region field (for example eu-de-1) from day one so a Lagos region can be added later without schema changes. Times assume a Friday-evening start; shift as needed. Hetzner is used as the example host provider because it offers cheap dedicated servers that support Proxmox and extra IPs; start in Germany or Finland, since routes from Nigeria to Europe are shorter than to the US.

## Architecture and stack

Two machines: a small cloud VM runs the control plane, and a rented dedicated server runs Proxmox and every customer VM.

&#91;embedded content: V1 architecture · control plane, Proxmox host, payments, backups\]

The Go service is the only thing that talks to Proxmox. Users reach their VMs directly over SSH, and Paystack reaches the API only through signed webhooks.

| Layer | Choice | Why |
| --- | --- | --- |
| Host | Hetzner auction dedicated server, 2 x NVMe | cheap, allows Proxmox and extra IPs |
| Hypervisor | Proxmox VE 8 (KVM) | free, solid REST API, built-in backups |
| VM images | Ubuntu 24.04 and Debian 12 cloud images + cloud-init | seconds to provision, SSH keys injected |
| Control plane | Go, chi, pgx, sqlc; React dashboard embedded with go:embed | your existing stack |
| Database | Postgres 16, also used as the job queue | one less moving part |
| Payments | iSpend wallet | balances, NGN virtual accounts, USDT credit; Paystack inside iSpend for cards |
| Edge | Caddy | automatic HTTPS |
| Backups | vzdump + pg\_dump to a Hetzner Storage Box | off-host, cheap |

## Phase 0 — Friday night: host and Proxmox (about 3 hours)

Goal: a reachable Proxmox host you can log into over HTTPS, locked down to your IP.

1. **Rent the server.** Pick a Hetzner Server Auction box with at least 6 cores, 64 GB RAM and 2 x NVMe (around €35–50/month). Two disks matter: they become a mirror. Order 1 extra IPv4 subnet (a /29 gives 6 usable addresses) or start with single additional IPs.
2. **Install Debian 12 from the rescue system** using `installimage`, with software RAID1 across both NVMe disks. Leave most space unpartitioned or as one large partition for LVM-thin.
3. **Install Proxmox VE 8 on top of Debian** by adding the Proxmox repository and installing `proxmox-ve`. Remove the enterprise repo and add the no-subscription repo. Reboot into the Proxmox kernel.
4. **Create storage.** Make an LVM-thin pool named `vmdata` for VM disks. Thin provisioning lets you sell more disk than you have physically allocated, so watch usage.
5. **Lock it down.**
   - Create a non-root admin user and an API token for the control plane (role limited to VM operations on `vmdata` and the bridge).
   - Restrict ports 8006 (web UI) and 22 to your own IP with the Proxmox firewall or Hetzner's firewall.
   - Enable fail2ban for SSH and turn off password SSH login.
6. **Smoke test.** Log into `https://<host>:8006`, confirm both disks are in the RAID (`cat /proc/mdstat`), and confirm the thin pool appears under Storage.

**Done when:** the Proxmox UI loads from your IP only, and an API token can list nodes with `curl`.

## Phase 1 — Saturday morning: networking and templates (about 4 hours)

Goal: you can clone a template by hand, it boots with a public IP, and you SSH in with your key.

1. **Bridge networking.** Create bridge `vmbr0` for VM traffic. On Hetzner, use the routed setup: the host routes the extra subnet to `vmbr0`, and VMs use the host's bridge address as their gateway. Hetzner blocks unknown MAC addresses on the main link, which is why routed beats bridged here.
2. **IPv6.** Assign addresses from your /64 to VMs the same way. IPv6 is free and gives every VM a public address even when IPv4 runs out.
3. **Block outbound SMTP.** Add a host-level firewall rule dropping outbound TCP 25 from `vmbr0`. Unblock per VM only after manual review.
4. **Build templates.**
   - Download the Ubuntu 24.04 and Debian 12 cloud images (`.img` / `.qcow2`).
   - `qm create` a VM, import the image as its disk on `vmdata`, attach a cloud-init drive, set serial console and `virtio` NIC.
   - Enable the QEMU guest agent in the VM config and install it in the image (`virt-customize` can bake it in).
   - `qm template <id>`. Use fixed IDs, for example 9000 for Ubuntu and 9001 for Debian.
5. **IP pool.** Write your usable IPv4 addresses into a list. In Phase 2 these become rows in an `ip_addresses` table that the API allocates from.
6. **Manual dry run.** Full-clone template 9000 to VM 100, set cloud-init user, SSH key, IP and gateway with `qm set`, resize the disk to 20 GB, start it, and SSH in. Then delete it. Time each step: this is exactly what your API will automate.

**Done when:** a cloned VM boots in under 60 seconds, is reachable over SSH on IPv4 and IPv6, and cannot send mail on port 25.

## Phase 2 — Saturday afternoon and evening: Go control plane (about 7 hours)

Goal: one API call creates a working VM, and the database always knows which IP, plan and Proxmox VMID belongs to whom.

**Project layout**

- `cmd/api` — HTTP server (chi or net/http)
- `cmd/worker` — background jobs (provisioning, metering, suspension); can run in the same binary at first
- `internal/proxmox` — thin client over the Proxmox REST API using the API token
- `internal/store` — Postgres access (pgx + sqlc), migrations with goose or migrate
- `internal/billing` — wallet ledger and metering

**Core tables**

| Table | Key columns | Notes |
| --- | --- | --- |
| users | id, email, password\_hash, ispend\_customer\_id, email\_verified\_at, phone, status, vm\_limit | status: active, suspended, banned |
| ssh\_keys | id, user\_id, name, public\_key, fingerprint | validate with golang.org/x/crypto/ssh |
| plans | id, slug, vcpu, ram\_mb, disk\_gb, price\_uusdt\_hourly, price\_uusdt\_monthly\_cap | micro-USDT; monthly cap = hourly x 730 |
| templates | id, slug, proxmox\_template\_id | 9000 Ubuntu, 9001 Debian |
| vms | id, user\_id, region, plan\_id, template\_id, proxmox\_vmid, hostname, ipv4\_id, ipv6, state, created\_at, deleted\_at | state machine below |
| ip\_addresses | id, address, gateway, vm\_id (nullable) | allocate with SELECT ... FOR UPDATE SKIP LOCKED |
| jobs | id, kind, payload jsonb, status, attempts, run\_after, last\_error | Postgres-backed queue |
| usage\_charges | id, user\_id, vm\_id, hour, amount\_uusdt, ispend\_movement\_id, status | reporting only; balances live in iSpend |

**VM state machine:** `pending` → `provisioning` → `running` ⇄ `stopped` → `suspended` → `deleting` → `deleted`, with `error` reachable from provisioning. Only the worker moves states; the API only requests changes.

**Provisioning job (what the API enqueues)**

1. Reserve an IPv4 row and the next free VMID inside one transaction; insert the VM as `pending`.
2. Clone the template (`POST /nodes/{node}/qemu/{template}/clone`, full clone) and poll the returned task until done.
3. Set cores, memory, cloud-init user, SSH keys, IP config and nameservers on the new VMID.
4. Resize the disk to the plan size.
5. Start the VM, wait for the guest agent to respond, mark `running`.
6. On any failure: retry up to 3 times with backoff, then destroy the half-built VM, release the IP, mark `error`, and refund any charge.

**API endpoints**

| Method and path | Purpose |
| --- | --- |
| POST /v1/auth/signup, /login, /verify | account, session token, email verification |
| GET, POST, DELETE /v1/ssh-keys | manage keys |
| GET /v1/plans, /v1/templates | catalogue |
| POST /v1/vms | create (checks balance covers 24 hours and vm\_limit) |
| GET /v1/vms, /v1/vms/{id} | list, detail with IPs and state |
| POST /v1/vms/{id}/reboot, /stop, /start | power actions (enqueue jobs) |
| DELETE /v1/vms/{id} | destroy and release IP |
| GET /v1/wallet | NGN and USDT balances from iSpend, recent charges |
| POST /v1/wallet/convert | quote and convert NGN to USDT credit (Phase 3) |
| POST /v1/webhooks/ispend | deposit and movement events from iSpend (Phase 3) |

Authenticate with opaque session tokens stored hashed in Postgres; skip JWT for V1. Every VM query filters by `user_id` so one user can never touch another's VM.

**Done when:** `curl -X POST /v1/vms` with a plan and template returns a VM that reaches `running`, and `DELETE` removes it from Proxmox and frees its IP.

## Phase 3 — Sunday morning: billing through the iSpend wallet (about 4 hours)

Goal: customers fund an iSpend NGN wallet, naira is converted once into USDT compute credit at a quoted rate, and every running hour moves USDT from the customer's wallet to the VPS merchant wallet. The VPS keeps no balances of its own.

**Who owns what**

| Concern | Owner |
| --- | --- |
| Balances, ledger entries, idempotency | iSpend (double-entry, append-only) |
| NGN funding: a virtual account per customer, card as fallback | iSpend providers (Paystack as the card provider) |
| FX rates, spread, quotes and the NGN → USDT conversion | iSpend, executed on the VPS's request |
| Plans, metering, suspension | VPS control plane |

**Currency model**

- Plans are priced in USDT and stored as int64 micro-USDT (1 USDT = 1,000,000, matching USDT's own 6 decimals). Never floats.
- Each customer has two iSpend wallets: NGN for funding and USDT for compute credit. The VPS has one USDT merchant wallet.
- The exchange rate is applied once, when naira moves into USDT. Usage never touches FX, so a naira devaluation cannot erode credit already sold, and a customer's runway does not shrink when the rate moves.
- The dashboard shows balances and plan prices in naira at the current rate, for display only.

**Account linking**

- On VPS signup, create the customer in iSpend (or link an existing iSpend user) and store `ispend_customer_id`. iSpend provisions the NGN virtual account and the USDT wallet.
- The VPS calls iSpend's service API with a tenant credential. Every call carries an idempotency key.

**FX (handled by iSpend)**

- iSpend's FX module owns rates, spread and quotes. The VPS requests a quote for a naira amount, shows it, and executes the conversion against that quote.
- Configure the VPS tenant's spread in iSpend's FX settings. Start around 5%: it covers naira movement and the USDT → EUR conversion when paying the host.
- If iSpend cannot quote, conversions pause and the dashboard says so; deposits stay safe in the NGN wallet until quoting resumes.

| Table | Key columns | Notes |
| --- | --- | --- |
| conversions | id, user\_id, ispend\_quote\_id, amount\_ngn\_kobo, amount\_uusdt, rate, ispend\_movement\_id, status | local record for the dashboard and reporting; iSpend stays the source of truth |

**Funding and conversion flow**

1. The customer pays naira into their iSpend virtual account by bank transfer, or by card through Paystack. iSpend's suspense protocol confirms the inbound movement and sends the VPS a deposit event.
2. By default every deposit auto-converts at the current rate; customers can switch this off and convert a chosen amount from the dashboard. Either way the VPS creates a `conversions` row and, for manual conversions, requests an iSpend quote and shows it, such as “₦20,000 → 12.10 USDT, about 2 months of Nano” (example rate).
3. The VPS asks iSpend to execute the conversion: debit the customer's NGN wallet, credit their USDT wallet, with the counter-legs in the treasury wallets. It executes against the iSpend quote; the idempotency key is the conversion id.
4. On success, store `ispend_movement_id` and mark the conversion complete. A replayed event or a retried call finds the same key and changes nothing.

**Hourly metering job**

1. Runs at minute 0 every hour (a ticker in the worker, guarded by a Postgres advisory lock so only one instance runs).
2. For every VM in `running` or `stopped` during that hour, ask iSpend to transfer the plan's hourly price from the customer's USDT wallet to the VPS merchant wallet, idempotency key `vm:{id}:hour:{yyyymmddhh}`. Record it in `usage_charges` for reporting. Stopped VMs still hold disk and IP, so they still bill.
3. Stop charging once a VM's charges this calendar month reach the plan's monthly cap.
4. Insufficient funds: record the charge as unpaid and start the suspension path below. iSpend unreachable: retry later with the same key, so hours are charged late, never skipped.

**Low-balance handling**

- Balances are read from iSpend (cached for 60 seconds).
- Below 24 hours of usage: send a warning email showing the naira needed at today's rate.
- At or below zero: enqueue `suspend` for all the customer's VMs (stop them, keep disks). Start a 72-hour grace timer.
- Funded during grace: VMs return to `stopped`; the customer starts them.
- Grace expires: enqueue `delete` and release IPs.

**Treasury:** USDT accumulates in the merchant wallet. Convert enough to EUR each month to pay the host, and keep at least one month of host cost in reserve.

Use iSpend's sandbox and Paystack test keys all weekend; switch to live only after the full checklist passes.

**Done when:** a test deposit to a virtual account auto-converts once at the active rate even if the iSpend event is replayed, changing the FX rate afterwards leaves that USDT balance unchanged, a VM left running for 3 hours produces exactly 3 transfers to the merchant wallet, and an iSpend outage during metering produces catch-up charges rather than missed hours.

## Phase 4 — Sunday afternoon: guardrails, backups, launch (about 5 hours)

Goal: the system is safe enough to hand to 5–10 people you know, and you will find out about problems before they do.

**Abuse guardrails**

- Email verification required before the first top-up; phone number collected at signup.
- `vm_limit` of 2 per new user; raise manually.
- Port 25 blocked (Phase 1); unblock only on request after review.
- Signup rate limit per IP (for example 3 per hour) and login rate limit per account.
- Simple CPU watch: if a VM sits above 90% CPU for 6+ hours, flag it for review. That pattern usually means crypto mining.
- Short acceptable-use policy on the signup page: no spam, mining, scanning or attacks; violations mean deletion without refund.

**Backups**

- Nightly `vzdump` of all VMs in snapshot mode to a Hetzner Storage Box (cheap, off-host), keeping 3 days. Tell users V1 backups are best-effort.
- Nightly `pg_dump` of the control-plane database to the same Storage Box, keeping 14 days. Losing this database is worse than losing a VM, because it is the record of who owns what and who paid.
- Do one restore test of each before launch.

**Deploy the control plane**

- Run the Go binary and Postgres on a small separate cloud VM (2 vCPU, 4 GB), not on the Proxmox host. If the host dies, you can still see what happened and talk to users.
- Put Caddy in front for automatic HTTPS on your API domain.
- Run as systemd services with restart-on-failure.
- The control plane talks to Proxmox over the API token only; allowlist its IP on port 8006.

**Monitoring**

- Uptime check (Uptime Kuma or a free external service) on the API and on the Proxmox host.
- Alerts to Telegram or email for: failed jobs, disk pool above 80%, host RAM committed above 90%, webhook signature failures, iSpend quote or conversion failures.
- Structured logs (slog) with request IDs.

**Launch**

1. Run the acceptance checklist below end to end with Paystack test keys.
2. Switch to live keys and do one real ₦2,000 top-up yourself.
3. Invite 5–10 users directly. Watch logs closely for the first 48 hours.

**Done when:** every item in the checklist below is ticked.

## Phase 5 — Monday (or a third day): full dashboard (about 9 hours)

Goal: a customer can do everything from the browser without curl, and you can run the business from an admin view. A complete dashboard roughly adds a day, so either extend the weekend or hold the launch step in Phase 4 until this phase is done.

**Stack and serving**

- React + Vite + TypeScript + Tailwind in a `web/` folder of the same monorepo, built and embedded into the Go binary with `go:embed`. One binary, one deploy.
- TanStack Query for API calls, with 5-second polling on VM pages so state changes (provisioning → running) appear without refresh.
- Switch dashboard auth to secure, httpOnly session cookies with CSRF protection. Keep bearer tokens for the CLI and API.

**Customer pages**

| Page | What it shows and does |
| --- | --- |
| Signup, login, verify email, reset password | standard flows; rate-limited |
| Overview | wallet balance (USDT credit with naira equivalent), hours of runway left at current usage, VM count, low-balance banner |
| VMs list | name, plan, IPv4, state badge, hourly cost; Create button |
| Create VM | plan cards with naira prices at today's rate, template picker, SSH key picker, hostname; disabled with a reason if balance or vm\_limit blocks it |
| VM detail | IPv4/IPv6 with copyable `ssh root@ip` command, state, start/stop/reboot, delete with typed-name confirmation, cost this month |
| SSH keys | add (paste), list fingerprints, delete |
| Wallet | virtual account details for bank transfer, card top-up, auto-convert toggle, manual conversion with a USDT and runway preview, charge history |
| Account | email, phone, password change, acceptable-use policy link |

**Admin view** (role flag on users, separate `/admin` routes)

- Users: search, status, balance; suspend, ban, raise vm\_limit, manual balance adjustment (written as an `adjustment` ledger row with a note)
- VMs: all VMs with owner and state; force stop, delete; unblock port 25 per VM
- Host capacity: vCPU and RAM committed vs physical, thin-pool usage, free IPv4 count
- Jobs: failed jobs with error text and a retry button
- Revenue: conversions (naira and USDT) and usage per day. FX: current iSpend rate and spread, read-only (managed in iSpend)

**Build order:** auth pages → VMs list and detail → create flow → wallet → SSH keys and account → admin. Each page only calls endpoints that already exist from Phase 2 and 3; add `GET /v1/admin/*` endpoints as you reach the admin pages.

**Done when:** a fresh user completes signup, top-up, VM creation, SSH login and deletion entirely in the browser, on a phone screen as well as desktop.

## Acceptance checklist and backlog

V1 is ready for invited users when all of these pass.

- [ ] New user signs up, verifies email, adds an SSH key
- [ ] Test deposit of ₦5,000 to a virtual account converts once into USDT at the active rate; replaying the iSpend event changes nothing
- [ ] Create VM refuses when balance covers less than 24 hours or the user is at vm\_limit
- [ ] Created VM reaches running in under 2 minutes and accepts SSH on IPv4 and IPv6
- [ ] Reboot, stop and start work and are reflected in state
- [ ] Delete removes the VM from Proxmox and returns its IP to the pool
- [ ] Killing the worker mid-provision leaves no orphaned VM or leaked IP after restart
- [ ] Metering posts exactly one usage row per VM per hour; changing the FX rate leaves existing balances unchanged; an iSpend outage produces catch-up charges, not missed hours
- [ ] Zero balance suspends VMs; top-up within grace restores them; expiry deletes them
- [ ] Outbound port 25 is blocked from VMs
- [ ] User A cannot see or act on user B's VMs (test with two accounts)
- [ ] The full customer journey works in the dashboard on desktop and phone
- [ ] Non-admin users get 403 on every /admin route
- [ ] VM backup and database backup each restored successfully once
- [ ] Alerts fire for a failed job and for an uptime outage

**Backlog after the weekend, in rough order**

1. Browser console in the dashboard via the Proxmox noVNC proxy, scoped per user
2. User snapshots and rebuild from template
3. Resize to a larger plan
4. Second host and a placement rule (least committed RAM wins)
5. Managed Postgres as a product on the same control plane
6. Lagos colocation for local latency and data residency

**Open questions**

- Final plan prices in USDT, set after checking your real host cost and target overcommit ratio; the spread to configure for the VPS tenant in iSpend
- Which email provider sends verification and warning emails
