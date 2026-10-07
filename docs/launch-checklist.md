# Launch checklist

This is the plan's acceptance checklist. Items marked **auto** are covered by tests in this repository and run with `go test ./...` (the integration tests need `XENOS_TEST_DATABASE_URL`, see the README). Items marked **host** or **manual** can only be proven on the real systems and must be ticked by a person.

Run `xenosctl preflight` on the server first (add `--send-test you@example.com` to send a real email): it checks configuration, database, iswallet, Proxmox, email and the worker, and exits 1 on any failure. Then run with the iSpend sandbox and test keys first. Switch to live only when every box is ticked.

## Accounts and wallet

- [ ] **auto** New user signs up (AUP required), verifies email, adds an SSH key: `TestAuthFlow`, `TestSignupRequiresAUP`, `TestSSHKeys`
- [ ] **auto** iswallet deposit webhook is deduped on the transaction id, USDT credits are never re-converted, reversals are recorded and alerted once: `TestDepositDedupedByTransactionNotDeliveryKey`, `TestOnlyNairaBankDepositsAreConverted`, `TestDepositReversalIsRecordedAndAlertedOnce`
- [ ] **auto** A conversion whose reply was lost replays the same quote and key; an expired automatic quote is replaced under a new key; a customer's expired quote is refused: `TestLostConvertReplyIsReplayedNotRepeated`, `TestExpiredAutoQuoteIsReplacedUnderANewKey`, `TestManualQuoteExpiresAfterSixtySeconds`, `TestConvertCreditMismatchIsRecordedAsQuotedAndAlerted`
- [ ] **auto** `INSUFFICIENT_LIQUIDITY` holds the conversion without touching the customer's naira, then completes: `TestLiquidityShortageHoldsTheConversionThenCompletes`
- [ ] **auto** `CURRENCY_MISMATCH` from an unconverted customer is treated as out of funds, never as an outage that blocks other customers: `TestCurrencyMismatchForAnEmptyWalletIsOutOfFundsNotAnOutage`, `TestCurrencyMismatchDoesNotBlockOtherCustomers`, `TestCurrencyMismatchDespiteFundsIsAlertedNotSuspended`
- [ ] **auto** A reused idempotency key is alerted and never treated as an outage or as insufficient funds: `TestKeyReusedChargeIsAlertedNotRetriedAsTransient`
- [ ] **auto** The real client matches the documented API (paths, headers, idempotency, error codes, never rounds a charge, paces itself under 100/min): `internal/billing/iswallet_test.go`
- [ ] **auto** A ₦5,000 deposit converts once into USDT at the active rate; replaying the event changes nothing: `TestDepositAutoConvertsOnceEvenIfReplayed`
- [ ] **auto** Deposits made before email verification wait and convert on verification: `TestUnverifiedDepositIsHeldThenConvertedOnVerify`
- [ ] **auto** Changing the FX rate leaves existing USDT balances unchanged: `TestDepositAutoConvertsOnceEvenIfReplayed`
- [ ] **auto** Rejected / forged webhooks are refused and counted: `TestWebhookValidation`, `TestWebhookFailuresAreRecorded`
- [ ] **manual** Run `go test -tags sandbox -run Sandbox -v ./internal/billing` (passes against the sandbox as of 6 Oct 2026; re-run before go-live): it seeds liquidity and exercises create, balances, deposit simulation, quote/convert/replay, charge/replay/key-reuse, and adjustments both ways. Then, against the real iswallet sandbox: `POST /v1/sandbox/simulate/deposit` arrives, verifies and converts once; a redelivered event changes nothing; a 61-second-old quote is refused; a charge replayed after an hour has one ledger effect; `simulate/reversal` raises the alert
- [ ] **manual** The first charge into the operating wallet succeeds although it holds no USDT yet (iswallet answered `CURRENCY_MISMATCH` for the mirror case; see F5)
- [ ] **manual** One real charge of the nano hourly price moves exactly 0.006 USDT (6,000 micro-USDT, 6 decimals), and the sandbox balance response reports USDT `scale` 6
- [ ] **manual** Run a conversion in the sandbox and compare the execute response's `credit_amount` with the quote's (the guide's example disagrees by 1,000×; we alert if they differ)

## VMs

- [ ] **auto** Create refuses when the balance covers under 24 hours or the user is at `vm_limit`: `TestCreateVMLimitAndBalance`
- [ ] **auto** Reboot, stop and start are validated and reflected in state: `TestVMIsolationAndActions`, `TestPowerActions`
- [ ] **auto** Delete removes the VM from Proxmox and returns its IP: `TestDelete`
- [ ] **auto** Killing the worker mid-provision leaves no orphaned VM or leaked IP: `TestCrashMidProvisionRecovers`, `TestProvisionRetriesThenCleansUp`
- [ ] **host** A created VM reaches `running` in under 2 minutes on real hardware and accepts SSH on IPv4 and IPv6
- [ ] **auto** Resize, snapshots and restore: the claim is one-at-a-time, retries are idempotent, a permanently failed job releases the VM, a resize never shrinks and is blocked while snapshots exist: `internal/vm/ops_test.go`, `TestResizeValidation`, `TestSnapshotsAPI`
- [ ] **auto** The browser console works only for the owner, once per session, from our own origin, and closes when the account stops qualifying: `internal/httpapi/console_test.go`
- [ ] **host** Resize a running VM to a larger plan: it comes back with the new CPU and memory, the disk is larger and the filesystem has grown (cloud-init `growpart`; if not, the template needs it), and the next hourly charge uses the new price
- [ ] **auto** Rebuild keeps IP, plan and name, replaces template and keys, erases snapshots, restarts cleanly after a failed attempt, and a permanently failed rebuild errors the VM and stops billing: `TestRebuild*` in `internal/vm/ops_test.go`, `TestRebuildAPI`
- [ ] **host** Rebuild a VM onto another template: the new OS answers SSH at the same IP with the chosen key (the VMID is reused, so the old guest must be fully destroyed first)
- [ ] **auto** API tokens work only for VMs, keys and reading the wallet, stop on revoke, expiry, ban and password reset; create is at-most-once per Idempotency-Key; labels filter; the boot script runs once, is erased, and is never re-run after an interruption: `internal/httpapi/tokens_test.go`, `TestBootScript*`
- [ ] **host** The boot script runs through the real guest agent (`agent/exec`): check the API token role has the guest-agent privilege, the template has `qemu-guest-agent` with exec enabled, and a script's exit code and output come back
- [ ] **auto** Plans and templates: validation, immutable sizes, price changes preview then confirm, disabled plans hidden but existing VMs still bill, audit for CLI and web: `internal/catalogue/catalogue_test.go`, `TestAdminCatalogue`
- [ ] **host** Add a template with `xenosctl template add` against the real host: it is refused for a VMID that does not exist there
- [ ] **auto** Statements: totals equal the charges in the month (UTC boundaries, capped and refunded hours), other accounts never appear, CSV is spreadsheet-safe, label grouping, readable with an API token: `internal/httpapi/statements_test.go`
- [ ] **host** Take two snapshots, change a file, restore the first: the file change is gone and the VM comes back. Check the API token role has the snapshot privileges
- [ ] **host** The browser console shows the login prompt of a running VM and accepts typing (the host's VNC password handshake and `vncwebsocket` authentication with the API token are only proven against the real host)
- [ ] **host** The `root` login works over SSH with the injected key (Ubuntu cloud images may refuse direct root login; confirm and adjust `templates.ci_user`)

## Billing

- [ ] **auto** Metering posts exactly one usage row per VM per hour: `TestThreeHoursThreeCharges`
- [ ] **auto** An iSpend outage produces catch-up charges, not missed hours: `TestOutageProducesCatchUpNotMissedHours`, `TestDeleteDuringOutageStillCharges`
- [ ] **auto** Zero balance suspends VMs; top-up within grace restores them; expiry deletes them: `TestOutOfFundsSuspendsThenRestoresOnTopUp`, `TestGraceExpiryDeletesVMs`
- [ ] **auto** Low-balance warning email, once a day: `TestLowBalanceWarningOncePerDay`
- [ ] **auto** Hourly charges are spread across the hour and carry a `vm:<id> hour:<…>` narration: `TestChargesAreSpreadAcrossTheHour`, `TestChargeNarrationIdentifiesTheVMAndHour`
- [ ] **auto** Only one meter runs at a time: `TestOnlyOneMeterRuns`

## Safety

- [ ] **auto** User A cannot see or act on user B's VMs, keys or quotes: `TestVMIsolationAndActions`, `TestSSHKeys`, `TestAutoConvertOffAndManualConversion`
- [ ] **auto** Non-admin users get 403 (and anonymous callers 401) on every `/admin` route; the test walks the router so a new route cannot be left unguarded: `TestEveryAdminRouteRejectsNonAdmins`
- [ ] **host** Outbound port 25 is blocked from VMs and port 587 still works (see `deploy/proxmox/README.md`)
- [ ] **auto** Signup and login are rate limited: `TestRateLimits`
- [ ] **auto** Sustained 90%+ CPU for 6 hours flags a VM: `TestCPUWatchFlagsSustainedMining`
- [ ] **auto** Admin: ban, suspend, vm_limit, balance adjustment with note, force stop/delete, port 25 exemption, audited: `TestAdminSuspendBanAndLimit`, `TestAdminBalanceAdjustment`, `TestAdminVMActionsAndPort25`
- [ ] **manual** `xenosctl user ban`, `flagged`, `port25 allow` work on the production database

## Backups and monitoring

- [ ] **host** Database backup restored once with `deploy/backup/pg-restore-test.sh`, from the Storage Box copy (the script itself is exercised in development)
- [ ] **host** One VM backup restored with `qmrestore` and booted (see `deploy/proxmox/README.md`)
- [ ] **auto** Alerts fire for a failed job, a full pool and a flagged VM: `TestFailedJobsAlertedOnce`, `TestDiskPoolAndRAMThresholds`, `TestCPUWatchFlagsSustainedMining`
- [ ] **manual** The real Telegram/email channel receives a test alert (see `deploy/README.md` §8)
- [ ] **manual** The webhook is registered (`xenosctl ispend subscribe`) for the live environment, its signing secret is in the server's env, and one real deposit is observed end to end
- [ ] **manual** The uptime monitor alerts when `/readyz` goes red (stop the worker for 4 minutes)

## Dashboard

- [ ] **auto** The full customer journey (signup, verify, key, create, provision, stop/start, delete, wallet, account) and the admin area work in a real browser on desktop, and every page fits a phone screen without sideways scrolling: `cd web && npm run e2e`
- [ ] **manual** One pass through the same journey on a real phone, against the real services (the browser test uses the fakes)

## Go live

1. Everything above ticked, on test keys.
2. Switch to live keys; do one real ₦2,000 top-up yourself.
3. Invite 5-10 people. Watch the logs and alerts closely for 48 hours.
