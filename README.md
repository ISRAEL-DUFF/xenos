# Xenos

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

Balances are read with `GET /v1/wallets/{id}/balance`: we use `available`, and refuse any response whose currency `scale` is not what we expect (NGN 2, USDT 6). The merchant wallet is the tenant's operating wallet from `GET /v1/platform/account`. The webhook body is an envelope whose `event_type` we require; we never infer an event's meaning from its shape. If iswallet's execute response and the quote disagree on the USDT credited, we record the quote and alert an operator (iswallet's own guide example is inconsistent here; see the conformance report). The real client matches the documented API and passes contract tests. It has run against the iswallet **sandbox**: wallet creation, the virtual account, balances, the platform account and deposit simulation work, but sandbox **FX is currently unavailable**, which blocks testing convert, charge and adjustments (see section 1b of the conformance report). Re-run the live checks any time with `set -a; . ./.env; set +a; go test -tags sandbox -run Sandbox -v ./internal/billing`. Wallet creation needs the tenant `client_id` (learned from `/v1/platform/account`; the guide omits it). A charge from a customer who has never converted returns `CURRENCY_MISMATCH`, which we treat as "cannot pay". The sandbox deducts a 1.4% fee from deposits; we convert the credited amount.

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

All five phases are code-complete and tested against fakes. What is **not** verified: anything on a real Proxmox host (provisioning, cloud-init networking, the SMTP block, `vzdump`), the real iswallet service (the client passes its contract tests and the live sandbox checks that do not need FX; conversion, charges and the real webhook are untested because sandbox FX is down and we have no public webhook URL), and the real Telegram/email alert channel.
Not built: BVN / TIER_2 upgrade, card funding (iswallet has none), email delivery (emails are logged), and the browser console and other backlog items from the plan.
Plan prices in the seed migration are placeholders.
