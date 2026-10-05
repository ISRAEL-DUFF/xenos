# Launch checklist

This is the plan's acceptance checklist. Items marked **auto** are covered by tests in this repository and run with `go test ./...` (the integration tests need `XENOS_TEST_DATABASE_URL`, see the README). Items marked **host** or **manual** can only be proven on the real systems and must be ticked by a person.

Run with the iSpend sandbox and test keys first. Switch to live only when every box is ticked.

## Accounts and wallet

- [ ] **auto** New user signs up (AUP required), verifies email, adds an SSH key: `TestAuthFlow`, `TestSignupRequiresAUP`, `TestSSHKeys`
- [ ] **auto** iswallet deposit webhook is deduped on the transaction id, USDT credits are never re-converted, reversals are recorded and alerted once: `TestDepositDedupedByTransactionNotDeliveryKey`, `TestOnlyNairaBankDepositsAreConverted`, `TestDepositReversalIsRecordedAndAlertedOnce`
- [ ] **auto** A conversion whose reply was lost replays the same quote and key; an expired automatic quote is replaced under a new key; a customer's expired quote is refused: `TestLostConvertReplyIsReplayedNotRepeated`, `TestExpiredAutoQuoteIsReplacedUnderANewKey`, `TestManualQuoteExpiresAfterSixtySeconds`
- [ ] **auto** `INSUFFICIENT_LIQUIDITY` holds the conversion without touching the customer's naira, then completes: `TestLiquidityShortageHoldsTheConversionThenCompletes`
- [ ] **auto** The real client matches the documented API (paths, headers, idempotency, error codes, never rounds a charge, paces itself under 100/min): `internal/billing/iswallet_test.go`
- [ ] **auto** A ₦5,000 deposit converts once into USDT at the active rate; replaying the event changes nothing: `TestDepositAutoConvertsOnceEvenIfReplayed`
- [ ] **auto** Deposits made before email verification wait and convert on verification: `TestUnverifiedDepositIsHeldThenConvertedOnVerify`
- [ ] **auto** Changing the FX rate leaves existing USDT balances unchanged: `TestDepositAutoConvertsOnceEvenIfReplayed`
- [ ] **auto** Rejected / forged webhooks are refused and counted: `TestWebhookValidation`, `TestWebhookFailuresAreRecorded`
- [ ] **manual** Against the real iswallet sandbox (needs a key and answers to Q1/Q2 in `docs/iswallet-conformance.md`): `POST /v1/sandbox/simulate/deposit` arrives, verifies and converts once; a redelivered event changes nothing; a 61-second-old quote is refused; a charge replayed after an hour has one ledger effect; `simulate/reversal` raises the alert
- [ ] **manual** The USDT decimal scale is confirmed and `XENOS_ISPEND_USDT_DECIMALS` is set; one real charge of the nano hourly price moves exactly 0.006 USDT

## VMs

- [ ] **auto** Create refuses when the balance covers under 24 hours or the user is at `vm_limit`: `TestCreateVMLimitAndBalance`
- [ ] **auto** Reboot, stop and start are validated and reflected in state: `TestVMIsolationAndActions`, `TestPowerActions`
- [ ] **auto** Delete removes the VM from Proxmox and returns its IP: `TestDelete`
- [ ] **auto** Killing the worker mid-provision leaves no orphaned VM or leaked IP: `TestCrashMidProvisionRecovers`, `TestProvisionRetriesThenCleansUp`
- [ ] **host** A created VM reaches `running` in under 2 minutes on real hardware and accepts SSH on IPv4 and IPv6
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
