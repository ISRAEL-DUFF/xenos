# Data retention and account closure

What happens to a customer's data, and when. This is operational policy for you to confirm with counsel before launch.

## Closing an account

1. The customer asks to close (Account page, or `xenosctl user close <email>`).
2. A suspended or banned account cannot be closed by the customer (unsuspend it first). Closing is **refused** while any of these hold (all are reported together):
   - VMs still exist (unless the customer ticks "delete my VMs", which queues their deletion);
   - charges are unpaid;
   - the wallet holds more than dust (0.50 USDT or ₦100). We cannot pay balances out automatically: settle it with the customer by hand, then run `xenosctl user close <email> --settle`.
3. On success the account becomes `closing`: sessions and API tokens are revoked, login is refused, auto-convert is off, and a confirmation email is sent. Deposits that arrive afterwards are not converted; operators are alerted to refund them.
4. For **30 days** support can undo it: `xenosctl user reopen <email>`.
5. After the grace, the monitor (in the worker) purges the account once none of its VMs remain on the host.

## What the purge erases

- the email address (replaced by `closed-<id>@invalid`, which frees the original email for a new signup), phone, password hash and bank virtual-account name (the iswallet customer id stays, so ledger rows still tie to it);
- SSH keys, API tokens, verification and reset rows;
- VM hostnames, labels, keys and boot script text and output (VM rows stay, scrubbed);
- the account's address in the audit log targets.

## What is kept, and for how long

Ledger records are kept **6 years** (`XENOS_FINANCIAL_RETENTION_YEARS`, default 6): usage charges, conversions, adjustments, deposits and the audit log entries that explain them, tied to the anonymised user id. `xenosctl retention list` shows closed accounts older than the retention period. Nothing deletes them automatically: the first eligible account is years away, so a deletion tool is deliberately not built yet. Review the list and remove rows by hand until then.

## Data export

`POST /v1/account/export` returns a zip of the caller's own data: profile, SSH keys, VMs, charges, conversions, adjustments and API token names/prefixes. It never contains the password hash, session tokens, token secrets or boot script text. It needs the password and is limited to 3 a day.
