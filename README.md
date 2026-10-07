# Xenos

Step-by-step order for standing the host up: [docs/host-setup-runbook.md](docs/host-setup-runbook.md). To rehearse first on a cheap VPS: [docs/test-host-runbook.md](docs/test-host-runbook.md).

Hourly-billed VPS platform: Go control plane + Proxmox + iSpend wallet billing.
Spec: [VPS V1 Weekend Build Plan.md](<VPS V1 Weekend Build Plan.md>).

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/api` | HTTP server (JSON API, webhooks, embedded dashboard) |
| `cmd/worker` | Background jobs: provisioning, power actions, metering, suspension |
| `internal/config` | `XENOS_*` env config (see `.env.example`) |
| `internal/store` | pgx pool, goose migrations (`migrations/`), sqlc queries (`queries/` → `db/`, run `make sqlc`) |
| `internal/auth` | argon2id passwords, opaque session tokens, rate limiter |
| `internal/mail` | `Mailer` interface (log-only for now) |
| `internal/jobs` | Postgres job queue (`FOR UPDATE SKIP LOCKED`) |
| `internal/proxmox` | `API` interface, REST client (clone, configure, resize, power, status, task wait) and in-memory `Fake` |
| `internal/wallet` | NGN→USDT conversions: deposit handling, manual quotes, retries |
| `internal/metering` | Hourly VM charging, out-of-funds suspension/grace/deletion, low-balance emails |
| `internal/monitor` | Operator checks: failed jobs, disk pool, RAM, IPs, stuck billing, webhook probing, CPU abuse watch |
| `internal/alert` | Telegram/email alerts with per-key cooldown |
| `internal/firewall` | Renders the nftables rule that blocks outbound SMTP |
| `internal/accounts` | Account actions shared by the admin API and `xenosctl` (ban) |
| `internal/worker` | Assembles the job handlers and metering loop |
| `internal/vm` | Worker-side VM lifecycle: provision, power, delete (the only code that changes VM state) |
| `internal/sshkey` | SSH public key validation |
| `internal/testutil` | Per-test Postgres schemas for integration tests |
| `cmd/xenosctl` | Operator CLI: IP pool, admin grants, VM limits |
| `internal/billing` | `ISpend` interface, in-memory `Fake`, metering helpers |
| `web/` | React + Vite + TS + Tailwind dashboard, embedded via `go:embed`; `web/e2e` is the browser journey test |

## Develop

```sh
make db                      # Postgres 16 via docker compose
cp .env.example .env         # then export the vars
make web                     # build dashboard into web/dist
make run                     # API on :8080, migrations run on start
cd web && npm run dev        # dashboard dev server, proxies /v1 to :8080
make test                    # integration tests skip unless XENOS_TEST_DATABASE_URL is set
go run ./cmd/worker          # separate process: provisions VMs (fake Proxmox when XENOS_PVE_URL is empty)
go run ./cmd/xenosctl ip add 203.0.113.10-203.0.113.14 203.0.113.1   # load the IP pool
go run ./cmd/xenosctl ip add-floating 198.51.100.10-198.51.100.20       # load the floating IP pool
```

Integration tests create a throwaway schema per test inside `XENOS_TEST_DATABASE_URL` (the URL must contain `test`) and drop it afterwards; nothing else in that database is touched.

With `XENOS_ISPEND_URL` unset the API uses the in-memory fake iSpend.

## Auth

Opaque random session tokens, stored SHA-256 hashed in Postgres. Two session kinds:

- **cookie** (dashboard): `xenos_session`, httpOnly, SameSite=Lax, Secure unless `XENOS_COOKIE_SECURE=false`. Unsafe methods need an `X-CSRF-Token` header (value from `/v1/auth/me` or the login response).
- **bearer** (CLI/API): send `"token": true` to signup/login, then `Authorization: Bearer <token>`. No CSRF. Each token only works on the transport it was issued for.

Endpoints: `POST /v1/auth/{signup,login,verify,forgot-password,reset-password}`, and when authenticated `GET /v1/auth/me`, `POST /v1/auth/{logout,resend-verification,change-password}`. Password changes and resets revoke all sessions.
Rate limits are in-memory (single instance): signup 3/hour/IP, login 30/15min/IP and 8/15min/account. Behind Caddy set `XENOS_TRUST_PROXY=true`; otherwise `X-Forwarded-For` is ignored.
`requireVerified` and `requireAdmin` middleware exist for the wallet and admin routes to use.

## VMs

`POST /v1/vms {plan, template, hostname?, ssh_key_ids[]}` validates, then in one transaction: locks the user row, checks `vm_limit` and that the wallet covers 24 hours for *all* the user's active VMs plus the new one, inserts the VM, claims a free IPv4 (`FOR UPDATE SKIP LOCKED`) and enqueues `vm.provision`. It answers 202 with the VM in `pending`; poll `GET /v1/vms/{id}`.
Start/stop/reboot/delete validate the current state and enqueue a job. Only the worker (`internal/vm`) changes state: `pending → provisioning → running ⇄ stopped`, `deleting → deleted`, `error`.

Provisioning clones the template, sets cores/memory/cloud-init/IP, resizes the disk, starts the guest and waits for the QEMU agent. Failures retry 3 times; a retry or a restarted worker first removes any half-built guest under the same VMID. After the last attempt the guest is destroyed, the IP released and the VM marked `error` (the IP stays reserved if the guest could not be removed).

Run exactly one worker process (startup requeues every job left `running`). For a test host without KVM set `XENOS_PVE_DISABLE_KVM=true` and a long `XENOS_PROVISION_TIMEOUT`.

## Billing

Plans are priced in USDT (int64 micro-USDT, never floats). Customers fund naira by **bank transfer** to a personal virtual account (iswallet has no card funding); the rate is applied **once**, when naira becomes USDT, so later rate moves never change credit already bought. Naira amounts in the dashboard are display only. The wallet service is **iswallet** (iSpend): one wallet per customer holding NGN and USDT balances, so the wallet id is our customer id. The integration is built from the iswallet guide; see [docs/iswallet-conformance.md](docs/iswallet-conformance.md) for how, and for the questions still open.

- **Deposits:** `POST /v1/webhooks/ispend` verifies `X-iSpend-Signature` (HMAC-SHA256 over `timestamp.rawbody`, 5-minute window). A `wallet.credit.posted` for NGN from a bank inflow is recorded once (deduped on iswallet's transaction id, so redelivery is harmless) and, with auto-convert on and a verified email, queues a conversion of the credited amount. USDT credits and other sources are ignored. `wallet.credit.reversed` is recorded in `deposit_reversals` and alerted; iswallet bears that loss and the customer keeps their credit.
- **Conversion:** quote → execute. Quotes last **60 seconds**. A conversion persists its quote and idempotency key before executing, and every retry replays the same pair (iswallet rejects a repeated key with a different payload); only a quote that expired without executing is replaced, under a new key. A customer's own quote is never silently replaced: expired means "get a new quote". `INSUFFICIENT_LIQUIDITY` (iswallet's own USDT inventory) holds the conversion and retries later; the customer's naira is untouched.
- **Manual conversion:** `POST /v1/wallet/convert {amount_ngn_kobo}` returns a quote with `expires_at` (the dashboard shows a countdown); send `{quote_id}` to confirm it. Needs a verified email. `PATCH /v1/wallet/settings {auto_convert}`.
- **Wallet:** `GET /v1/wallet` shows balances, rate, runway, the virtual account (verified users only; stored locally because iswallet has no call to read it back), the deposit limit, conversions and charges. When rates are paused `quoting_paused` is set; a sweep converts held naira when they resume.
- **Metering** (`internal/metering`, ticks every minute, one instance guarded by a Postgres advisory lock): each hour is charged **in advance**, once per VM, key `vm:{id}:hour:{yyyymmddhh}`, as a transfer from the customer's wallet to the Xenos merchant wallet with the narration `vm:<id> hour:<yyyymmddhh>`. Charges are spread across the first `XENOS_METER_SPREAD_MINUTES` of each hour (iswallet allows 100 calls a minute per key and sends no `Retry-After`). Billing runs from the hour a VM reaches `running` until the hour deletion was requested or the VM was suspended; stopped VMs still bill. If iswallet is down, rows stay `pending` and are collected later with the same key: hours are charged late, never skipped or doubled. Per-VM monthly cap = plan cap, UTC calendar month. A VM that never reaches `running` is never charged.
- **Out of funds:** first failed charge → `unpaid`, 72-hour grace starts, email sent, VMs suspended (stopped, disk kept, billing paused). Wallet covering 24h of the suspended VMs after the debt is paid → VMs return to `stopped`. Grace expiry → VMs deleted.
- **Low balance:** under 24h of runway → one email a day with the naira needed at today's rate.
- **Limits:** new customers are TIER_1: ₦50,000 per transfer and per day (shown on the wallet page). There is no BVN / TIER_2 upgrade flow in V1.

Balances are read with `GET /v1/wallets/{id}/balance`: we use `available`, and refuse any response whose currency `scale` is not what we expect (NGN 2, USDT 6). The merchant wallet is the tenant's operating wallet from `GET /v1/platform/account`. The webhook body is an envelope whose `event_type` we require; we never infer an event's meaning from its shape. If iswallet's execute response and the quote disagree on the USDT credited, we record the quote and alert an operator (iswallet's own guide example is inconsistent here; see the conformance report). The real client matches the documented API and passes contract tests. It has run against the iswallet **sandbox** end to end: wallet creation, the virtual account, balances, deposit simulation, convert and replay, charge/replay/key reuse, `INSUFFICIENT_FUNDS`, adjustments in both directions and quote expiry all behave as designed (conformance report §1d). Two sandbox defects found on the way (USDT credited to a separate wallet; operating wallet unable to transfer USDT) were fixed by iswallet and our workarounds removed (§1e). Seed a fresh sandbox with `simulate/liquidity` (key `xenos-seed-usdt-v1`); the sandbox test does this itself. Re-run the live checks any time with `set -a; . ./.env; set +a; go test -tags sandbox -run Sandbox -v ./internal/billing`. Wallet creation needs the tenant `client_id` (learned from `/v1/platform/account`; the guide omits it). A charge from a customer who has never converted returns `CURRENCY_MISMATCH`, which we treat as "cannot pay". The sandbox deducts a 1.4% fee from deposits; we convert the credited amount.

**Setting up iswallet:** get a sandbox key and set `XENOS_ISPEND_URL`, `XENOS_ISPEND_API_KEY`, a per-environment `XENOS_ISPEND_OWNER_PREFIX` (sandbox is never reset); register the webhook with `xenosctl ispend subscribe https://<your-domain>/v1/webhooks/ispend` and put the printed signing secret (shown once) in `XENOS_ISPEND_WEBHOOK_SECRET`. Do not create a merchant wallet.

## Guardrails and operations

- **Signup:** the acceptable-use policy must be accepted (recorded as `aup_accepted_at`), a phone number is collected, signup is limited to 3 per hour per IP. Email verification gates the virtual account details, manual conversion, and the conversion of deposits: naira that arrives before verification waits in the NGN wallet and converts when the email is verified.
- **Abuse:** `vm_limit` 2 per new user. The monitor flags a VM that holds 90%+ CPU for 6 hours (reset only when it drops under 50%, so a throttling miner is not missed); review with `xenosctl flagged`, act with `xenosctl user ban <email>` or by deleting the VM. Outbound port 25 is blocked on the host by an nftables rule generated from the database (`xenosctl firewall nft`, exemptions via `xenosctl port25 allow <vm-id>`).
- **Alerts** (Telegram and/or email, one message per problem per cooldown): failed jobs, disk pool ≥ 80%, host RAM committed ≥ 90%, free IPs low, charges or conversions stuck (iSpend trouble), ≥ 5 rejected webhooks in 10 minutes, VMs flagged for CPU, and the Proxmox API not answering.
- **Health:** `/healthz` is liveness; `/readyz` also needs Postgres and a worker heartbeat from the last 3 minutes. Point the uptime monitor at `/readyz`.
- **Operations:** [deploy/README.md](deploy/README.md) (control-plane VM: systemd, Caddy, first setup, nightly `pg_dump` with off-host copy and restore test), [deploy/proxmox/README.md](deploy/proxmox/README.md) (SMTP block, nightly `vzdump`, API lockdown), [docs/launch-checklist.md](docs/launch-checklist.md) (the acceptance checklist mapped to tests and to the manual host steps).

## Dashboard and admin

Everything a customer does is in the browser, on desktop and phone: sign up, verify email, reset password, overview (wallet with naira equivalent, hours of runway, low-balance and grace banners), VM list, create flow (plan cards in USDT and naira, OS, SSH keys, hostname; the button is disabled with the reason when the wallet, VM limit or keys block it), VM detail (copyable `ssh` command, start/stop/reboot, delete with typed-name confirmation, cost this month), SSH keys, wallet (bank-transfer details and limits, auto-convert toggle, quote-and-confirm conversion with a 60-second countdown, history) and account. VM pages poll every 5 seconds, so provisioning → running appears without a refresh.

The admin area (`/admin`, admins only) has users (search, status, balances; suspend, ban, raise the VM limit, manual balance adjustment with a required note, paid from / collected into the merchant wallet), all VMs (force stop, delete, port-25 exemption, CPU-flagged VMs), host capacity (vCPU/RAM committed vs physical, thin-pool use, free IPs), failed jobs with retry, and revenue and the iSpend rate (read-only). Every admin action is written to `admin_audit`. Admin accounts are created and protected from the command line: `xenosctl admin grant <email>`.

Auth for the dashboard is the httpOnly session cookie with a CSRF header; bearer tokens remain for scripts. The server sets a strict Content-Security-Policy, so the UI uses no inline scripts or styles.

**Browser test:** `cd web && npm run e2e` drives a real Chromium through the whole customer journey, the admin area and the phone layout (and fails on console errors such as CSP violations). It needs the stack running with the fakes; see [web/e2e/README.md](web/e2e/README.md).

## Status

All five phases are code-complete and tested against fakes. What is **not** verified: anything on a real Proxmox host (provisioning, cloud-init networking, the SMTP block, `vzdump`), the real iswallet service (the client passes its contract tests and the live sandbox end-to-end run; the real webhook is untested because we have no public webhook URL), and the real Telegram/email alert channel.
**Customer operations (backlog items, built):** *resize* moves a VM to a strictly larger plan (stop, set cores and memory, grow the disk once, start; the new price applies from the next charged hour; needs the usual 24-hour runway; refused while snapshots exist); *snapshots* are disk snapshots, two per VM, restore needs the VM name typed; the *browser console* is noVNC in the dashboard, bridged by the API to the host's VNC websocket (one-time session, same-origin check, closes after 15 idle minutes or when the account stops qualifying). Each long operation claims the VM through `vms.busy` so only one runs at a time and power actions wait. These run against the in-memory fake and contract tests; see the launch checklist for what must be proven on the real host.

*Rebuild* reinstalls a VM from a template (same or another) into the same VMID, IP, plan and hostname, optionally with new SSH keys (the way back in after losing a key); it erases the disk and snapshots, needs the VM name typed, and a permanently failed rebuild marks the VM errored and stops its billing, since the old guest is already gone.

**Metrics:** Prometheus metrics on loopback-only listeners (API `127.0.0.1:9090`, worker `127.0.0.1:9091`; never the public port), with scrape config and alert rules in `deploy/prometheus/`. Route labels are patterns, not paths, so cardinality stays bounded; a test checks every metric an alert rule names exists.

**Statements:** `GET /v1/statements` and `/v1/statements/{yyyy-mm}` (UTC months; JSON, or `?format=csv` with one row per charged hour; `?group_by=label:<key>` totals VMs by a label, for cost per node). Every total is a sum of the rows in `usage_charges`, conversions and adjustments. The dashboard has Wallet → Statements with a print layout (the browser's Save as PDF is the PDF; there are no numbered invoices). API tokens may read statements.

**Private networks:** `POST /v1/networks {"name"}` allocates a `/24` (from 10.64.0.0/10) and a VLAN id, `GET /v1/networks` lists them with members, `DELETE /v1/networks/{id}` works only when empty. VMs join at creation (`"networks": [id, ...]`, at most two) or later with `POST /v1/vms/{id}/networks/{nid}` and leave with `DELETE` of the same path (the VM is claimed `busy: "networking"`, stopped, its second NIC changed and started again). Layer 2 only, no routing or NAT, isolated by VLAN plus the per-NIC guest firewall. Limits: `XENOS_PRIVATE_NETWORK_LIMIT` (5), 250 VMs per network, VLAN ids from `XENOS_PRIVATE_VLAN_MIN`/`MAX`. A network is pinned to its first VM's host unless `XENOS_PRIVATE_NETWORK_TUNNEL=true` says the operator connected the hosts (recipe in `deploy/proxmox/README.md`). Closing an account requires deleting its networks. A deleted network's /24 and VLAN id are not reused for 24 hours, creating one needs a verified email, a restore from a snapshot rebuilds the private NICs from the database before the VM starts, and a join that fails and cannot be cleaned up leaves the membership `stuck` (VLAN reserved) until a detach succeeds.

**Hosts and placement:** one Xenos can drive several standalone Proxmox hosts (`XENOS_HOSTS_FILE`, see `deploy/proxmox/README.md`). A new VM goes to an active, reachable host in the region that holds its template, has a free IP and enough RAM, choosing the lowest committed-RAM fraction; `POST /v1/vms` accepts `spread_group` (or the label `spread.group`) to keep a group of VMs on different hosts (`409` when impossible, or `"spread": "prefer"` to share). VM JSON shows `host`. `xenosctl host list|drain|enable|disable`. The admin Capacity page and the monitor report each host separately.

**Floating IPs:** `POST /v1/floating-ips` (`label`, optional `vm_id`), `GET /v1/floating-ips`, `GET /v1/floating-ips/{id}`, `POST …/attach {vm_id}` (moves it when attached elsewhere), `POST …/detach`, `DELETE …/{id}`. Addresses come from the pool loaded with `xenosctl ip add-floating`; an account may hold `XENOS_FLOATING_IP_LIMIT` (default 3), each billed `XENOS_FLOATING_IP_PRICE_UUSDT_HOURLY` (default 2000 = 0.002 USDT) per hour attached or not, as `usage_charges` rows that appear on statements. A worker job puts the address in the target VM's Proxmox firewall set and on its interface through the guest agent, and removes it from the previous VM (a dead one does not block the move). Every guest is created with MAC and IP filtering, so only the VM an address points at can use it. Closing an account requires releasing them. Same host and region only; see `docs/pgdock-provider-contract.md`.

**Account closure and data export:** `POST /v1/account/export` (password; a zip of your own data, never secrets; 3 a day) and `POST /v1/account/close` (password, typed email, optional `delete_vms`; 409 with `blockers` while VMs, unpaid charges or wallet credit remain). Both are session-only (API tokens cannot call them). Closing signs the account out everywhere, revokes API tokens and starts a 30-day grace; support can undo it with `xenosctl user reopen <email>`. After the grace the worker erases personal data (see `docs/data-retention.md`). A wallet with credit above 0.50 USDT / ₦100 is refused: settle it by hand, then `xenosctl user close <email> --settle`.

**Catalogue:** plans and templates are managed with `xenosctl plan|template ...` or Admin → Catalogue, not SQL. A plan's size cannot be edited (add a new slug); its hourly price can, and applies to existing VMs from the next charged hour, after a preview of how many VMs it touches and an explicit confirmation; disabling a plan or template only stops new VMs. Changes are audited (CLI changes carry `xenosctl:<user>` as the source).

**Programmatic use:** API tokens (`xt_…`, managed in Account) let a program manage VMs and read the wallet but not change the account or move money; VMs take labels, a once-only create `Idempotency-Key`, and a boot script run once as root through the guest agent. See [docs/pgdock-provider-contract.md](docs/pgdock-provider-contract.md), which maps this to PGDock's provider interface and lists the gaps (HA placement, floating IPs, volumes, private networks).

Not built: BVN / TIER_2 upgrade, card funding (iswallet has none), a second host and the rest of the plan's backlog.
Plan prices in the seed migration are placeholders.
