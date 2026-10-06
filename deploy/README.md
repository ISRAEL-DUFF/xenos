# Deploying the control plane

The control plane (API, worker, Postgres, Caddy) runs on a small cloud VM, **not** on the Proxmox host. If the host dies you can still see what happened and talk to customers.
Sizing: 2 vCPU, 4 GB RAM. Debian 12 assumed. Replace `api.example.com` throughout.

## 1. Server basics

```sh
apt install -y postgresql-16 caddy rsync   # postgresql-16 from the PGDG repo if Debian 12 only offers 15; 15 works too
useradd --system --home /opt/xenos --shell /usr/sbin/nologin xenos
mkdir -p /opt/xenos/bin /opt/xenos/deploy /etc/xenos /var/backups/xenos /var/log/caddy
chown xenos:xenos /var/backups/xenos
```

Firewall: allow 22 (from your IP only), 80 and 443. Port 5432 and 8080 must **not** be reachable from outside.

## 2. Database

```sh
sudo -u postgres createuser --pwprompt xenos
sudo -u postgres createdb -O xenos xenos
```

## 3. Build and install

On your machine: `make release`, then copy `bin/linux-amd64/{xenos,xenos-worker,xenosctl}` to `/opt/xenos/bin/` and the `deploy/` directory to `/opt/xenos/deploy/`. The dashboard is inside the `xenos` binary.

## 4. Configure

Copy `.env.example` to `/etc/xenos/xenos.env` (`chmod 600`, owner `xenos`) and set at least:

```
XENOS_DATABASE_URL=postgres://xenos:...@localhost:5432/xenos?sslmode=disable
XENOS_HTTP_ADDR=127.0.0.1:8080
XENOS_PUBLIC_URL=https://api.example.com
XENOS_COOKIE_SECURE=true
XENOS_TRUST_PROXY=true     # Caddy is on loopback; list other proxy addresses in XENOS_TRUSTED_PROXIES
XENOS_ENV=production       # refuses to start without real iSpend, SMTP and secure cookies
XENOS_SMTP_HOST=smtp.<provider>
XENOS_SMTP_PORT=587
XENOS_SMTP_USER=...
XENOS_SMTP_PASS=...
XENOS_MAIL_FROM="Xenos <no-reply@example.com>"
XENOS_REGION=eu-de-1
XENOS_PVE_URL=https://<proxmox-host>:8006
XENOS_PVE_NODE=<node name>
XENOS_PVE_TOKEN_ID=xenos@pve!control
XENOS_PVE_TOKEN_SECRET=...
XENOS_PVE_STORAGE=vmdata
XENOS_ISPEND_URL=https://...    # iswallet base URL (sandbox: https://synledger.name.ng/iwallet); empty = in-memory fake
XENOS_ISPEND_API_KEY=...
XENOS_ISPEND_USDT_DECIMALS=6      # iswallet USDT is micro-USDT; a guard against a mismatched balance response
# XENOS_ISPEND_MERCHANT_WALLET is optional: by default charges go to the operating wallet from GET /v1/platform/account
XENOS_ISPEND_OWNER_PREFIX=xenos-prod   # distinct per environment
XENOS_ISPEND_WEBHOOK_SECRET=...   # printed once by `xenosctl ispend subscribe`
XENOS_TELEGRAM_BOT_TOKEN=...    # and/or XENOS_ALERT_EMAIL
XENOS_TELEGRAM_CHAT_ID=...
```

Leave `XENOS_RUN_WORKER` unset (false): the worker is its own service. Run **exactly one** worker.
`XENOS_PVE_INSECURE_TLS=true` is acceptable only until Proxmox has a real certificate, and only because the API is firewalled to this server.

## 5. Services

```sh
cp /opt/xenos/deploy/systemd/*.service /opt/xenos/deploy/systemd/*.timer /etc/systemd/system/
cp /opt/xenos/deploy/Caddyfile /etc/caddy/Caddyfile        # edit the hostname
systemctl daemon-reload
systemctl enable --now xenos-api xenos-worker
systemctl reload caddy
```

The API applies database migrations when it starts. Check `curl https://api.example.com/healthz` (process is up) and `/readyz` (database reachable and the worker has ticked in the last 3 minutes).

## 6. First setup

```sh
export $(grep -v '^#' /etc/xenos/xenos.env | xargs)
/opt/xenos/bin/xenosctl ip add 203.0.113.10-203.0.113.14 203.0.113.1   # your routed IPs and the gateway VMs use
# sign up in the dashboard, then:
/opt/xenos/bin/xenosctl admin grant you@example.com
```

Register the iswallet webhook once the API is reachable at its public URL, and store the secret it prints (it is shown only once):

```sh
/opt/xenos/bin/xenosctl ispend subscribe https://api.example.com/v1/webhooks/ispend
# then add XENOS_ISPEND_WEBHOOK_SECRET=... to /etc/xenos/xenos.env and restart the API
```

Review the placeholder plan prices in migration `00001_init.sql` **before** the first real customer: price changes after launch need an `UPDATE plans`.

## 7. Backups

```sh
cp /opt/xenos/deploy/backup/backup.env.example /etc/xenos/backup.env   # chmod 600, fill in
systemctl enable --now xenos-pg-backup.timer xenos-pg-restore-test.timer
sudo -u xenos systemctl start xenos-pg-backup.service                   # run one now
journalctl -u xenos-pg-backup -n 20
sudo -u xenos /opt/xenos/deploy/backup/pg-restore-test.sh              # the pre-launch restore test
```

Set up the Storage Box SSH key (port 23) and create the `xenos-db/` directory on it first. VM backups are configured on the Proxmox host: see [proxmox/README.md](proxmox/README.md).

## 8. Monitoring

- **Uptime:** point Uptime Kuma (or a free external service) at `https://api.example.com/readyz` and at the Proxmox host's port 8006 from your allowed IP. `/readyz` going red covers a dead database *and* a dead worker.
- **Alerts** come from the worker over Telegram/email: failed jobs, disk pool ≥ 80%, host RAM committed ≥ 90%, free IPs low, stuck charges or conversions, rejected webhooks, VMs flagged for sustained 90%+ CPU, and the Proxmox API not answering. Test the channel once: stop Proxmox's API access for a minute and confirm a "Proxmox API is not answering" message arrives.
- **Logs** are structured JSON with a request id: `journalctl -u xenos-api -o cat | jq .`.
- A missed backup is caught only if `HEALTHCHECK_URL` is set; use it.

## Operating notes

- Deploy: replace binaries, `systemctl restart xenos-api xenos-worker`. Stopping the worker lets the current provisioning step finish (up to 60s); an interrupted job is requeued, not lost.
- Abuse: `xenosctl flagged`, `xenosctl user ban <email>`, `xenosctl port25 allow <vm-id>` (then reload the firewall, see the Proxmox README).
- The worker requeues every job left `running` at startup. That is only correct with a single worker; never start a second one.
