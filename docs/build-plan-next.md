# Build plan: eight items after rebuild

**Status:** proposed, for review. Nothing here is built. Each item says what changes, how it is tested, when it counts as done, and what could go wrong.
**Order:** CI, plans, statements, metrics, deletion and export, then the three network items (floating IPs, second host, private networks). The first five are small and independent; the last three change how VMs are placed and addressed, so they get the most care.
**Estimate:** about 18 working days in total (one engineer). Each item is its own commits on the working branch, with tests and docs in the same change.

## Rules that apply to every item

- **Same standard as before:** Go tests with a real Postgres (per-test schema), fakes for iswallet and Proxmox, `sqlc` for queries, a migration per schema change (next number is `00012`), the browser journey extended where a page changes, and the README, launch checklist and (where relevant) the PGDock contract updated.
- **Money is integer micro-USDT.** No floats in anything that touches charges or statements.
- **Security checkpoint** after item 4 (tokens, export, deletion) and after item 7 (placement, cross-host console and exec): rerun the read-only security review on those paths and fix findings before continuing.
- **Real-host items stay unchecked.** Anything that can only be proven on a Proxmox host is added to `docs/launch-checklist.md` as a **host** item, not claimed done.

## Order and effort

| # | Item | Days | Depends on |
|---|---|---|---|
| 2 | CI pipeline | 0.5 | nothing |
| 1 | Plan and template management | 1 | nothing |
| 3 | Customer statements | 1.5 | nothing |
| 5 | `/metrics` | 1.5 | nothing |
| 4 | Account deletion and data export | 2 | nothing (reads from 3's queries) |
| 6 | Floating IPs (with anti-spoofing) | 3 | real-host verification later |
| 7 | Second host and placement | 5 | 1 (templates per host) |
| 8 | Private networks (same host first) | 3 | 6 (anti-spoofing), 7 (host-aware) |

---

## #2 CI pipeline (0.5 day)

**Goal:** every push is built and tested; a broken push is visible before it is merged.

**Work**
- `.github/workflows/ci.yml`, triggered on push and pull request:
  - **go job:** `actions/setup-go` using the version in `go.mod`; a `postgres:16` service container; `XENOS_TEST_DATABASE_URL` pointing at a database whose name contains `test`; steps: `gofmt -l` (fail if it prints anything), `go vet ./...`, `go test ./... -count=1` (and `-race` on `internal/vm`, `internal/httpapi`, `internal/metering`, `internal/wallet`).
  - **sqlc job:** install sqlc at the version used here, run `sqlc diff`; fails when generated code is stale.
  - **web job:** Node 22, `npm ci`, `npx tsc --noEmit`, `npm run build`.
  - **e2e job (main branch and manual only):** builds the API and runs `web/e2e/journey.mjs` against the fakes, with the system Chromium; uploads screenshots on failure.
  - **audit job (informational):** `govulncheck ./...` and `npm audit --omit=dev`; reports, does not block.
- The sandbox suite (`-tags sandbox`) is **not** in CI: it needs the real API key and mutates a shared sandbox. It stays a manual command.
- `make ci` runs the same steps locally.

**Tests:** make a throwaway branch with an unformatted file and a stale sqlc output; CI must fail on each, then pass when fixed. I can read the workflow runs through the GitHub tools.

**Done when:** CI is green on the working branch, and both deliberate breakages are caught.

**Risks:** the e2e job is the flaky one (timing). Run it on main only, with one automatic retry, and keep it out of the required checks at first.

**Needs from you:** branch protection (make the go, sqlc and web jobs required) is a repository setting only you can change.

---

## #1 Plan and template management (1 day)

**Goal:** the operator adds and changes plans and templates without writing SQL. Needed before PGDock's sizes can exist.

**Rules (so a price change cannot surprise anyone)**
- A plan's **specs** (vCPU, RAM, disk) are immutable once any VM uses it; create a new slug instead.
- A plan's **hourly price** can change. Metering reads the plan's price each hour, so a change applies to existing VMs from the next charged hour; the command shows how many VMs and what monthly change that is, and needs `--yes`.
- **Disabling** a plan only stops new VMs and resize targets; existing VMs keep running and billing.
- Prices are whole micro-USDT; the monthly cap must be between one hour and 744 hours of the price (the same check preflight applies).

**Work**
- `xenosctl plan list | add <slug> <vcpu> <ram_mb> <disk_gb> <hourly_usdt> [cap_usdt] | price <slug> <hourly_usdt> [--yes] | disable <slug> | enable <slug>`; `xenosctl template list | add <slug> <name> <proxmox_vmid> [--ci-user root] [--skip-host-check] | disable <slug> | enable <slug>`. `template add` checks the VMID exists on the host (through the Proxmox client) unless skipped.
- Admin API `GET/POST/PATCH /v1/admin/plans`, `/v1/admin/templates` with the same validation, and an **Admin → Catalogue** page: tables, add forms, enable switches, a confirm dialog on price changes showing the impact.
- Audit: `admin_audit.admin_id` becomes nullable and gains `source` (for example `xenosctl:root`) so CLI changes are recorded too. Migration `00012`.
- Plans list for customers keeps ordering by price; `GET /v1/plans` already hides disabled plans.

**Tests:** validation (slug, price, cap, spec bounds); immutability of specs once in use; price change shows and applies the right impact; a disabled plan cannot be created or resized into but existing VMs still bill; audit rows for CLI and web; admin-only access.

**Done when:** the PGDock sizes (`d-small` to `d-xlarge`) can be added and priced from the CLI, and the page shows them.

**Risk:** repricing mid-month for VMs with a monthly cap. The cap is per plan, so the new cap applies to the rest of the month's charges; the statement (#3) shows each hour at the price charged, so nothing is hidden.

---

## #3 Customer statements (1.5 days)

**Goal:** a customer, and a program on a token, can see what was charged and why, for any month.

**Data (all exists):** `usage_charges` (VM, hour, amount, status), `conversions`, `adjustments`.

**Work**
- `GET /v1/statements` lists months that have activity. `GET /v1/statements/{yyyy-mm}` returns, for a UTC calendar month: totals; one block per VM (hostname, plan, hours charged, subtotal, any monthly-cap effect, deleted VMs included); wallet movements (conversions with the naira paid and rate, adjustments with notes); unpaid charges. `?format=csv` gives one row per charged hour. `?group_by=label:<key>` groups VM subtotals by a label value, so PGDock can attribute cost per node or role.
- **Allowed for API tokens** (read-only).
- Dashboard: **Wallet → Statements** list and a month page with print styles (the browser's "Save as PDF" is the PDF; there is no server-side PDF in this item).
- Refunded charges are excluded from totals and listed separately.
- CSV cells that start with `=`, `+`, `-` or `@` are prefixed with `'` so a hostname cannot become a spreadsheet formula.
- Large months stream rows rather than building them in memory.

**Tests:** statement total equals the sum of charges in the month; month boundaries are UTC; cap and refund handling; deleted VMs appear; another user's data never appears (two users); label grouping; CSV escaping; a token can read but the statement of another account is never reachable.

**Done when:** a month of test charges produces a statement whose total matches the ledger in `usage_charges` to the micro-USDT, in JSON, CSV and the printable page.

**Open question:** do customers need a naira equivalent per charge? Charges are in USDT; the naira value moved with the rate. The plan shows naira only for conversions (a fact), not for charges.

---

## #5 `/metrics` (1.5 days)

**Goal:** an operator can chart and alert on what is happening, beyond `/readyz` and chat alerts.

**Design**
- API and worker are separate processes, so **each exposes its own metrics** on a loopback-only listener (`XENOS_METRICS_ADDR`, default `127.0.0.1:9090` for the API and `127.0.0.1:9091` for the worker). Nothing is added to the public port, and Caddy does not proxy them.
- Use `prometheus/client_golang`.
- **API:** requests by route *pattern* (never the raw path, to bound cardinality), status class and duration; auth failures; rate-limit rejections by limiter; webhooks by outcome (accepted, bad signature, duplicate); open consoles; token and session auth counts.
- **Worker:** jobs by kind and result with duration, queue depth by kind and status; VMs by state (gauge, from the database); charges by status and the oldest pending charge's age; conversions by status; unpaid total; iswallet client requests by endpoint and status class, rate-limiter wait time, and the up/down flag the meter keeps; meter tick duration and last success time; worker heartbeat age; host pool fraction, committed-RAM fraction and free IPs (per host after #7); webhook failure count.
- DB-derived gauges are computed at scrape time with a 15-second cache, only in the worker (one instance), so they are not duplicated.
- `deploy/prometheus/` gets a scrape config and an alert rules file (worker silent, charges stuck, jobs failing, pool above 85%, iswallet down, 5xx rate), plus a short README. No Grafana in this item.

**Tests:** a scrape contains the expected metric names; labels use route patterns (a request to `/v1/vms/123` is recorded as `/v1/vms/{id}`); a failed job and a stale charge change the right gauges; the listener binds only to the configured address.

**Done when:** a local Prometheus scrapes both processes and the alert rules load (`promtool check rules`).

**Needs from you:** whether you run Prometheus or want a hosted one; the endpoints are the same either way.

---

## #4 Account deletion and data export (2 days)

**Goal:** a customer can take their data and close their account; the platform keeps what the law and the books require.

**Export**
- `POST /v1/account/export` (signed-in session only, password re-entry, 3 per day) streams a zip: `profile.json` (no password hash, no tokens), `ssh_keys.csv`, `vms.csv` (with labels), `charges.csv`, `conversions.csv`, `adjustments.csv`, `statements/`. Streamed, so a large history does not sit in memory.

**Deletion (account closure)**
1. Preconditions, each with a clear message: no live VMs (or the request says `delete_vms: true`, which queues their deletion and waits), nothing unpaid, and the wallet balance is below a dust level (default 0.50 USDT). A larger balance blocks closure with instructions (see the open question).
2. `POST /v1/account/delete {password, email, delete_vms}`: account goes to `closing`, sessions and tokens are revoked, login is refused.
3. A purge job runs after a **30-day** grace (support can reopen the account in that time): the `users` row is anonymised (email becomes `deleted-<id>@invalid`, phone cleared, password hash replaced), SSH keys, verification and reset rows, tokens and sessions are deleted.
4. **Kept:** `usage_charges`, `conversions`, `adjustments`, deposit and webhook records, admin audit, with `user_id` pointing at the anonymised row. A retention note is written in `docs/data-retention.md` and the privacy text points to it.
5. The iswallet wallet cannot be deleted from our side; the anonymised row keeps its customer id so nothing orphaned can be funded again, and the wallet is marked closed in our records.
6. After the purge the same email can sign up again as a new account.

**Tests:** each blocking precondition; export contains only the caller's rows (two users) and no secrets; closing revokes sessions and tokens; purge anonymises and keeps the financial rows; a token cannot export or delete; re-signup with the same email after the purge.

**Done when:** the full flow runs in the browser journey against the fakes, and the data-retention document exists.

**Open questions (need your decision before I build):**
1. What happens to a USDT balance above dust? There is no payout path today. Options: refuse closure and ask support to settle by hand; or add an explicit "forfeit the balance" acknowledgement. I recommend refusing, with a `xenosctl user close --settle` command for you to use after paying the customer back through iswallet.
2. Retention period for financial records (commonly several years for tax; confirm with your accountant).

---

## #6 Floating IPs, with anti-spoofing (3 days)

**First, a security point this item has to settle.** In the routed setup every VM sits on the same `vmbr0` bridge. Unless the host stops it, one VM can answer ARP for another VM's address or send traffic from it. Whether the current host config prevents this is not known until the host exists. The Proxmox firewall can: per-VM `ipfilter` with an IP set of allowed addresses, and `macfilter`. This item makes Xenos manage that, and floating IPs build on it.

**Part A: anti-spoofing (1 day)**
- Proxmox client calls for the VM firewall: enable the VM firewall, set `ipfilter`/`macfilter` on the NIC, and maintain the `ipfilter-net0` IP set (the VM's own IPv4 and IPv6, plus any floating IPs assigned).
- The provisioner applies them in `createGuest` (so provisioning and rebuild both do), and removes nothing else on the host.
- Preflight gains a check that new guests have the filter on.
- **Host item:** from one test VM, try to use another VM's address and confirm traffic is dropped.

**Part B: floating IPs (2 days)**
- Model: `ip_addresses.kind` (`primary` | `floating`) and a `floating_ips` table (address, owner account, optional attached VM, created, label). Floating addresses come from their own pool (`xenosctl ip add --floating`), allocated to an account on request and **billed per hour** like a VM (a small price; unattached floating IPs cost money so they are not hoarded). Billing: a metering line per floating IP, same idempotent hourly key scheme.
- API (tokens allowed): `POST /v1/floating-ips`, `GET /v1/floating-ips`, `POST /v1/floating-ips/{id}/attach {vm_id}`, `POST …/detach`, `DELETE /v1/floating-ips/{id}`. `attach` to a VM that already holds it is a no-op; attaching to another VM **moves** it.
- Worker job `vm.floating`: adds the address to the VM's ipfilter set, then through the guest agent runs `ip addr replace <addr>/32 dev <primary interface>` and announces it (`arping -U`, with a ping to the gateway from the new address as fallback). Detach reverses it. When the old VM cannot be reached (it died; that is the failover case) the move proceeds and only the new VM is configured; the stale address on a dead VM does no harm, and if it returns it is cleaned by the reconcile below.
- **Persistence:** the address is also re-applied after every start, resume and rebuild (a hook in the power and rebuild jobs), and a reconcile pass checks that assigned addresses are present.
- IPv6 floating addresses use the same path (`ip -6`).
- Dashboard: **Networking** page listing floating IPs with attach, detach, release.

**Tests:** allocation and per-account limits; attach, move, detach through the fake guest agent (commands recorded and checked); ownership (another account's IP or VM gives 404); billing lines per hour; reapply after start; moving to a dead VM; ipfilter sets updated on attach and detach.

**Done when:** on the fakes, two VMs can exchange a floating IP through the API with the right guest commands, firewall sets and charges. The real-host check is on the launch checklist.

**Limits to state in the docs:** works between VMs on the **same host's bridge**; moving across hosts needs shared layer 2 (see #8). The guest needs `iproute2` (present in the templates) and `arping` or ping.

---

## #7 Second host and placement (5 days)

**Goal:** more than one Proxmox host behind one Xenos, with sensible placement and anti-affinity, so HA is possible.

**Design decisions**
- Hosts are **standalone Proxmox nodes**, not a cluster (no shared storage or corosync). VMIDs only need to be unique per host; the global sequence stays.
- **Configuration:** a hosts file (`XENOS_HOSTS_FILE`, YAML) holds each host's API URL, node, token and secret, storage, bridge, disk, region, IPv6 prefix and gateway, nameservers and KVM setting. Secrets stay out of the database. With no file, the existing single-host environment variables define one host named `default`, so an upgrade changes nothing. A `hosts` table holds the non-secret state (name, status `active` | `draining` | `disabled`).
- **Schema (migration):** `vms.host` (default `default`), `ip_addresses.host` (each host has its own routed subnet), `host_templates(host, template_id, proxmox_template_id)` replaces the single `templates.proxmox_template_id`, `vms.spread_group TEXT`.

**Work, in four stages (each leaves the system working)**
- **7a: host abstraction (1.5 days).** A `HostPool` returns the Proxmox client for a host name; the provisioner, ops, console, exec, monitor, preflight and worker use `pool.For(vm.host)`. Everything runs on `default` and all tests pass unchanged. This is the risky refactor, so it ships alone.
- **7b: placement (1.5 days).** At create, eligible hosts are `active`, reachable (health cached 30 s), have the template, have a free IP and have RAM for the plan (committed RAM plus the plan within `XENOS_RAM_COMMIT_LIMIT`, default 100%). The host with the **lowest committed-RAM fraction** wins, ties broken by name. `spread_group` (create parameter, also settable by label `spread.group`): VMs with the same group are placed on **different hosts**; if there are not enough hosts the create fails `409` with a clear message, or succeeds on a shared host when the request says `"spread": "prefer"`. Resize checks the VM's own host has the RAM. Snapshots, rebuild and restore stay on the host.
- **7c: operations (1 day).** Monitor alerts per host (pool, RAM, reachability). Admin capacity page lists each host with usage, VM count, free IPs and status. `xenosctl host list | drain <name> | enable <name> | disable <name>`; `drain` stops new placements and lists VMs still there (moving a VM between hosts is not in this item). Preflight checks every host and every template on every host.
- **7d: API and contract (1 day).** VM JSON shows `host` (the region stays one value); `spread_group` accepted by create; the PGDock contract's HA row is updated. Statements and metrics get a host label.

**Tests:** placement picks the least-committed host; skips draining, unreachable, no-template, no-IP and full hosts; anti-affinity places a pair apart and refuses a third when only two hosts exist; `prefer` falls back; existing VMs keep their host across a restart; the worker talks to the right fake for every operation (power, snapshot, console, exec, rebuild); the single-host environment still works with no hosts file.

**Done when:** three fake hosts, a PGDock-style create of two nodes with one `spread_group` lands them on different hosts, and operations on each reach the right host.

**Risks:** touching many files at once (hence 7a alone and unchanged tests as the guard); per-host templates must exist on every host (preflight checks it); IP pools per host mean running out is per host (alerts per host). Real multi-host behaviour needs a second real host.

**Needs from you:** will the second host be at the same provider (so the same routed-subnet model applies) and which region name it should use.

---

## #8 Private networks (3 days, same host first)

**Goal:** VMs of one account can reach each other on a private address space no one else can see, for example PGDock nodes talking to each other without public exposure.

**What this item does and does not do**
- **Does:** per-account private networks implemented as a **VLAN tag on a second bridge** (`vmbr1`) on each host, a second NIC on the VMs, addresses allocated by Xenos from private ranges, and isolation enforced by the VLAN tag plus the anti-spoofing from #6.
- **Does not:** carry a private network **between hosts**. Two standalone hosts need a tunnel (WireGuard or VXLAN) between the hosts' `vmbr1` bridges, configured by the operator on the hosts. Xenos assigns VLAN ids and documents the host recipe; it cannot configure the tunnel through the Proxmox API. Until that exists a network spans one host.

**Work**
- Schema: `private_networks` (account, name, `/24` allocated from `10.64.0.0/10`, VLAN id unique across the installation), `vm_private_ips` (VM, network, address).
- API (tokens allowed): `POST /v1/networks`, `GET /v1/networks`, `DELETE /v1/networks/{id}` (only when empty); create a VM with `"networks": [id]` (at most two per VM); `POST /v1/vms/{id}/networks/{nid}` and `DELETE` to attach and detach (a short operation: stop, change NIC and `ipconfig1`, start; cloud-init applies the new address because the instance id changes with the configuration). Limits: 5 networks per account, 250 VMs per network.
- Provisioner: `net1` on `vmbr1` with the VLAN tag and `ipconfig1` with the private address, no gateway (no routing or NAT); the VM's `ipfilter` set includes it.
- Dashboard: **Networking** page (shared with floating IPs from #6): create network, see members and addresses.
- Host recipe in `deploy/proxmox/README.md`: create `vmbr1` as a VLAN-aware bridge with no uplink; optional WireGuard/VXLAN between hosts, with an MTU note.

**Tests:** address allocation and exhaustion; VLAN id uniqueness; attach and detach through the fake host (NIC config recorded, VM restarted, address set); cross-account isolation (another account's network or VM gives 404, and VLAN ids differ); a network with members cannot be deleted; deleting a VM frees its private address.

**Done when:** on the fakes, two VMs of one account get addresses on one network with the right NIC settings; a third VM of another account cannot be attached to it. The real-host checks (isolation between two accounts' VLANs, reachability inside one) are on the launch checklist.

**Risks:** the cross-host gap above is the real limit for HA; it is stated up front so no one assumes otherwise. VLAN ids are a finite space (4094, minus those used by the operator); the allocator reports exhaustion.

---

## Open questions for your review

1. **Deletion (#4):** forfeit, refuse, or settle-by-hand for balances above dust? Retention period for financial records?
2. **Statements (#3):** is a printable page enough, or do business customers need server-generated PDF invoices (that is a larger item with VAT and numbering, closer to PGDock's invoice work)?
3. **Metrics (#5):** loopback listeners acceptable? Do you already run Prometheus?
4. **Anti-spoofing (#6):** are you willing to enable the Proxmox VM firewall on every guest? It is the only API-controllable way to stop one tenant using another's address.
5. **Floating IP price (#6):** per-hour price for an attached and an unattached address (set from your cost; unattached is not free so it is not hoarded).
6. **Second host (#7):** same provider and subnet model? Hosts file with secrets on the control plane, or secrets in the database encrypted with a master key?
7. **Private networks (#8):** accept that cross-host networks depend on an operator-built tunnel between hosts, or defer this item until a tunnel design is chosen?
8. **Plans (#1):** apply price changes to existing VMs from the next hour (as planned), or only to new VMs?
9. **CI (#2):** GitHub Actions is assumed. Is the repository on a plan that gives enough minutes for the e2e job?

## What I would change in other documents when this lands

- `docs/pgdock-provider-contract.md` §6 (gaps): HA placement, floating IPs and private networks move from "not supported" to "supported on one host" or "supported", with the limits above. One correction can be made now: VRRP between two VMs on **the same host's bridge** may work today in the routed setup (the VMs share layer 2), so the contract should say "untested on a real host" rather than "does not work". Across hosts it does not, until #8's tunnel exists.
