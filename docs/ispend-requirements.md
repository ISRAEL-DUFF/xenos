# Xenos → iSpend: integration requirements

**For:** the iSpend team. **From:** the Xenos (VPS platform) team.
**Purpose:** tell you exactly what Xenos needs from iSpend so you can send a detailed API document. Everything below comes from the integration we have already built and tested against a stand-in; where we had to guess, we say so (§9) so you can correct us.

Items marked **[blocking]** stop us launching. **[needed]** we need for a complete product. **[nice]** would help but we can work around it.

---

## 1. What Xenos is, and how it uses iSpend

Xenos sells Linux virtual servers to customers in Nigeria. Customers pay in naira; we price and bill in USDT. **Xenos keeps no balances of its own**: iSpend is the ledger and the source of truth for every balance.

```
 Customer ──naira──▶ iSpend virtual account ──▶ iSpend NGN wallet
                                                    │  (1) convert once, at a quoted rate
                                                    ▼
                                           iSpend USDT wallet  ──(2) hourly charge──▶  Xenos merchant USDT wallet
```

1. **Funding.** Each customer gets a personal NGN virtual account (bank transfer) and can pay by card (Paystack inside iSpend). iSpend tells us when money arrives.
2. **Conversion, once.** Naira is converted to USDT "compute credit" at a quoted rate. The rate is applied only here, so a later naira move never changes credit already bought. Conversion is automatic on deposit by default; a customer can switch that off and convert chosen amounts after seeing a quote.
3. **Usage, hourly, in USDT.** Every running VM costs a fixed number of micro-USDT per hour. Once an hour, per VM, we ask iSpend to move that amount from the customer's USDT wallet to the Xenos merchant wallet. Usage never touches FX.
4. **Out of funds.** A charge that fails for insufficient funds starts a 72-hour grace period (VMs suspended, then deleted); a top-up restores them.

Volumes: V1 launches with 5-10 invited users; plan for **1,000 running VMs ≈ 1,000 charges per hour** within the first year (see §5.3).

---

## 2. Ground rules we need to build on

| # | Requirement | Priority |
|---|---|---|
| 2.1 | **Money as integers, never floats.** NGN in kobo (`int64`), USDT in micro-USDT (1 USDT = 1,000,000; `int64`). Please confirm USDT wallets support 6 decimals. Our smallest charge is **6,000 micro-USDT** (0.006 USDT). | blocking |
| 2.2 | **Idempotency on every mutating call** via a key we supply (`Idempotency-Key` header or body field, your choice). Same key + same payload ⇒ the *original* response, byte-for-byte equivalent, with no second ledger effect. Same key + *different* payload ⇒ a clear conflict error (e.g. 409/422), never a silent second operation. | blocking |
| 2.3 | **Idempotency-key retention ≥ 90 days.** After an outage we deliberately replay old charge keys (`vm:42:hour:2026100514`) days later and rely on getting "already done", not a duplicate. If keys expire sooner, tell us the window and we will adapt. | blocking |
| 2.4 | **Timeouts are safe to retry.** If our call times out we do not know whether it succeeded. Retrying with the same key must be correct, and there must be a way to look up an operation by its idempotency key (§4.10). | blocking |
| 2.5 | **Read-your-writes.** After a successful charge/convert/adjust response, a balance read must reflect it. | needed |
| 2.6 | **No overdraft.** A debit that would take a wallet below zero fails atomically with a *distinct, documented* "insufficient funds" error code. We branch on it (it starts suspension), so it must never be confused with a transient failure. | blocking |
| 2.7 | **Atomic multi-leg movements.** Conversion (NGN debit + USDT credit + treasury legs) and charge (customer → merchant) either fully happen or fully don't. | blocking |
| 2.8 | **Concurrency-safe.** Two charges for the same customer arriving together must both be evaluated against the true balance. | needed |
| 2.9 | **Authentication:** a tenant credential for server-to-server calls (API key or OAuth client credentials), rotatable without downtime, scoped to our tenant only. Optional: IP allowlist. | blocking |
| 2.10 | **Environments:** a full **sandbox** and a live environment with separate credentials, and the same API surface in both. | blocking |
| 2.11 | **Errors:** a stable machine-readable `code` per error (not just HTTP status or free text), a human message, and a request id we can quote to your support. Please list every code per endpoint, and which are retryable. | blocking |
| 2.12 | **Versioning and change notice:** a versioned API path/header, and advance notice of breaking changes. | needed |
| 2.13 | **Rate limits** documented, with `429` + `Retry-After`. See §5.3 for what we need. | needed |
| 2.14 | **Timeouts/latency:** expected p95 per call. We use a 10-30 s client timeout. | nice |

---

## 3. Customer and wallet setup

### 3.1 Create customer **[blocking]**
Called once at signup. Today we send `email` and `phone` and nothing else.

- **Input:** idempotency key (`signup:<email>`), email, phone. *What else do you require?* (name, BVN/NIN, date of birth, address…). **This directly changes our signup form**, so please be explicit about mandatory vs optional fields and what is needed for each KYC tier.
- **Output:** `customer_id`; the **NGN wallet id**; the **USDT wallet id**; the **virtual account** (bank name, account number, account name).
- Can virtual-account creation be **asynchronous**? If so, what does the customer record look like until it's ready, and which webhook tells us it is?
- Can we **link an existing iSpend user** instead of creating a new one (the plan allows "create or link")? How is the user authorised to link?
- What happens on a duplicate email/phone?

### 3.2 Get customer **[blocking]**
Return the same customer object, including virtual-account details, by `customer_id`. We show these on the wallet page.

### 3.3 Balances **[blocking]**
For a `customer_id`: NGN balance and USDT balance as integers. Please distinguish **total / available / held-or-pending** if those differ (e.g. an unsettled deposit). We use *available*. We call this often (dashboard, every VM creation, hourly metering) and cache for 60 s; tell us if that is too chatty.

---

## 4. Money movement

### 4.1 FX rate (display) **[needed]**
The current NGN-per-USDT **buy** rate as customers would receive it (i.e. including your spread), in kobo per 1 USDT. Display only; no money moves on it. We show naira equivalents of prices and balances from it. Tell us how often it changes and if it is cacheable.

### 4.2 Quote **[blocking]**
- **Input:** `customer_id`, NGN amount in kobo.
- **Output:** `quote_id`, NGN amount, USDT amount (micro), the rate, **expiry time**.
- Rounding: who rounds, and in whose favour? Minimum and maximum amounts?
- A quote must be **binding** until it expires and **single-use**.
- *Quoting unavailable* (provider down, rates paused) must be a distinct error: we then pause conversion and tell customers their naira is safe.
- Do quotes depend on the customer (tier/spread), or only on the tenant's configured spread? We understand the spread is configured per tenant in iSpend's FX settings (we plan to start near 5%): please confirm how we set and change it.

### 4.3 Convert (execute a quote) **[blocking]**
- **Input:** idempotency key (`conversion:<our id>`), `customer_id`, `quote_id`.
- **Effect:** debit the customer's NGN wallet, credit their USDT wallet, with the counter-legs in your treasury wallets, atomically.
- **Output:** `movement_id` plus the final amounts and rate.
- **Errors we must distinguish:** insufficient NGN balance; quote expired; quote already used (with a *different* key); quote not found / belongs to another customer.
- **Replay rule:** repeating the same key returns the original result, including after the quote has expired.
- We also run *automatic* conversions (deposit arrives → we request a fresh quote → convert). Is there a supported "convert this NGN amount at the current rate in one call" that skips the quote step? We would use it for automatic conversions if it is atomic and idempotent.

### 4.4 Charge: customer USDT wallet → Xenos merchant wallet **[blocking]**
The call that makes us money, made for every running VM, every hour.

- **Input:** idempotency key (`vm:<vm id>:hour:<yyyymmddhh>`), `customer_id`, amount (micro-USDT). Please also let us attach **free-form metadata/memo** (VM id, hour, plan) that appears on the ledger entry and in reports: it is how we will reconcile.
- **Destination:** our single USDT merchant wallet, identified by tenant configuration or an id we send. Tell us which.
- **Output:** `movement_id`.
- **Errors:** insufficient funds (§2.6); customer frozen/closed; wallet not found.
- **Replay rule:** per §2.2/2.3. This key is permanent for that VM-hour; a charge is never legitimately issued twice.
- Should a **batch charge** endpoint exist (many `{key, customer, amount}` in one call, per-item results, each independently idempotent)? That is our preferred shape at scale (§5.3).

### 4.5 Reverse / refund a movement **[nice]**
Undo a prior movement by its id, idempotently, fully or partially. We have the hook but do not use it in V1 (a VM that fails to provision is never charged).

### 4.6 Admin adjustment **[needed]**
Staff credit or debit a customer's USDT wallet with a mandatory reason (goodwill credit, correcting a mistake).

- **Input:** idempotency key (`adjustment:<id>`), `customer_id`, signed amount, note.
- Which wallet is the counterparty (our merchant wallet?), and may it be a debit from the customer? Insufficient funds on a debit should be the §2.6 error.
- Do we need a separate permission/credential for this?

### 4.7 Card top-up **[needed]**
- **Input:** idempotency/attempt key, `customer_id`, amount (kobo), and our **success/cancel redirect URLs** (or tell us how those are configured).
- **Output:** a hosted **checkout URL** to send the customer to, plus an id for the payment attempt and its expiry.
- The resulting deposit must arrive through the **same webhook** as a bank transfer (§6), so we have one deposit path.
- Fees: who bears Paystack fees, and is the amount credited to the NGN wallet gross or net? We need to know the *credited* amount in the webhook.
- Min/max amounts; 3-D Secure; saved cards (we don't need them in V1).

### 4.8 Bank-transfer deposits **[blocking]**
Customers pay into their virtual account; iSpend's suspense protocol confirms the inbound movement. We only need the **deposit webhook** (§6) after the NGN wallet is credited. Please describe: expected delay, how under/over-payments, wrong-name or mismatched-amount transfers are handled, any per-deposit/daily limits, and whether the virtual account can be restricted to the customer's own bank account.

### 4.9 Merchant wallet **[needed]**
- A way to read the merchant wallet balance and ledger.
- The treasury process: USDT accrues in our merchant wallet and we convert some to EUR monthly to pay a hosting provider. Is that done through iSpend (API or dashboard) or outside? If through iSpend, we need the corresponding endpoints and rates.

### 4.10 Look up an operation **[blocking]**
- Get a movement **by id** and **by idempotency key**, with its status, amounts, parties, metadata and timestamps. This resolves the "did my timed-out call succeed?" question without guessing.
- List movements for a customer or for the merchant wallet, filterable by time range and metadata, **paginated**: for reconciliation. We store one usage row per VM-hour and want to match each to a ledger entry, daily.
- A **daily settlement/ledger export** (CSV or API) would be welcome.

---

## 5. Behaviour and operations

### 5.1 Sync vs async
State, per endpoint, whether it completes synchronously. If anything can be `pending` (e.g. conversion waiting on a liquidity provider), say so and which webhook/status call resolves it. Our code treats Convert and Charge as synchronous and final.

### 5.2 Reversals of money we have already spent
**[blocking] business question.** Can a deposit be reversed *after* it was confirmed and we converted it and charged against it (card chargeback, bank-transfer recall)? If yes: which webhook announces it, what is the window, and who bears the loss? Do you hold card deposits for a period before the NGN is spendable? We need to design around the answer.

### 5.3 Rate limits and bursts
Our meter charges *in advance* at the top of every hour, so calls arrive in a burst: with 1,000 VMs, ~1,000 charges within a minute, plus retries. Please state limits per credential, whether bursts are tolerated, and whether a batch endpoint (§4.4) is available. Balance reads are lower-volume but frequent.

### 5.4 Availability and support
Target uptime, maintenance windows and how they are announced (status page?), an incident/escalation contact, and your support SLA. Charges are designed to be delayed rather than lost during an outage, so we can tolerate downtime, but we need to know its shape.

---

## 6. Webhooks (iSpend → Xenos)

We need at least one: **deposit confirmed**. Please document each of the following.

| Topic | What we need to know |
|---|---|
| **Events** | `deposit.confirmed` **[blocking]** — fires once funds are final and the NGN wallet is credited, for *both* bank transfer and card. Payload we need: stable `event_id`, `customer_id`, **credited amount in kobo**, currency, source (bank/card), your deposit/movement id, timestamp. Further events we would use: deposit failed/reversed (§5.2), virtual account ready, customer frozen/unfrozen, and any async state change of a conversion/charge. |
| **Delivery** | At-least-once? Retry schedule (intervals, total duration, max attempts), what counts as success (any 2xx?), timeout we must answer within. Ordering guarantees (we assume none). |
| **Identity** | `event_id` stable across retries so we can dedupe (we store it and ignore repeats). |
| **Authentication** | How we verify it is you: HMAC signature scheme, header name, what is signed, replay window, **secret rotation** (two active secrets during a switch), and the source IPs if you publish them. |
| **Registration** | How we register our endpoint URL per environment; can we have separate URLs for sandbox and live. |
| **Testing** | Sandbox tooling to **trigger test events on demand** (deposit to a given virtual account, card success/failure) and to **resend** an event. |
| **Monitoring** | A delivery log we can inspect (status codes, attempts) and an alert when a webhook endpoint keeps failing. |

---

## 7. Sandbox requirements **[blocking]**

We run an automated test-suite and a launch checklist that need, in sandbox:

1. Create customers and receive a working virtual account.
2. **Simulate an inbound bank transfer** to that account (any amount) and have the deposit webhook fire.
3. Simulate a card payment succeeding and failing.
4. Real quote/convert/charge/adjust behaviour, including insufficient-funds and expired-quote errors reproduced on demand.
5. **Simulate FX movement** (change the rate) to verify that existing USDT balances are unaffected.
6. **Simulate outages or latency** (or at least a way to force 5xx/timeouts) so we can prove charges are retried late, never skipped or doubled.
7. Reset/clean up test data.

We will write contract tests against the sandbox and keep running them against every API change.

---

## 8. Compliance and product questions

1. **KYC/tiers:** what is needed at each tier to hold a virtual account, to deposit, to convert; what limits apply per tier (single deposit, daily, monthly, balance)? We must not onboard people iSpend would reject.
2. **Legal position of the USDT balance** held for our customers: is iSpend the regulated party, and what do we need to say in our terms? Can the customer withdraw it, and if so how does that interact with us (we would rather it be non-withdrawable compute credit; is that supported)?
3. **Refunds of unused credit:** if a customer leaves with a USDT balance, what is the supported path (back to naira at which rate)?
4. **Fees:** every fee we or customers incur (virtual account, deposit, card, conversion spread, transfer, adjustment, treasury conversion), and how each is reported.
5. **Data:** which customer data we may store, where yours is hosted, retention.
6. **Support path** for end customers with a missing deposit: do they contact you, us, or both, and what reference should we show them?

---

## 9. What we have assumed (please confirm or correct)

Our code speaks to iSpend through one Go interface. These are *our guesses*; the real API may differ, and only the adapter changes.

```
CreateCustomer(key, email, phone)            -> {customer_id, virtual account, ngn wallet id, usdt wallet id}
Customer(customer_id)                        -> same object
Balances(customer_id)                        -> {ngn_kobo, usdt_micro}
Rate()                                       -> ngn kobo per 1 usdt (buy, incl. spread)
Quote(customer_id, amount_kobo)              -> {quote_id, amount_kobo, amount_usdt_micro, rate}
Convert(key, customer_id, quote_id)          -> {movement_id}
Charge(key, customer_id, usdt_micro)         -> {movement_id}      # customer -> merchant wallet
Adjust(key, customer_id, signed_micro, note) -> {movement_id}
CardTopUp(key, customer_id, amount_kobo)     -> {checkout_url}
Reverse(key, movement_id)                    -> {movement_id}      # not used in V1
```

Idempotency keys we use today: `signup:<email>`, `conversion:<id>`, `vm:<id>:hour:<yyyymmddhh>`, `adjustment:<id>`, `card:<random>`.

Assumed webhook (placeholder; please replace with the real format):

```
POST <our url>/v1/webhooks/ispend
X-Ispend-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<raw body>")>
{"id":"evt_…","type":"deposit.confirmed","customer_id":"…","amount_kobo":500000}
```
We reject signatures older than 5 minutes, dedupe on `id`, and answer 2xx for anything we have safely recorded (including unknown event types and unknown customers, so you stop retrying).

---

## 10. What would unblock us first

If the full document will take time, these answers alone let us write the real client for the main path:

1. Auth scheme and base URLs for sandbox and live (§2.9, 2.10).
2. Create customer: required fields and response (§3.1), and whether virtual-account creation is synchronous.
3. Balances, Quote, Convert, Charge: request/response shapes, idempotency mechanism and retention, and the exact error codes for *insufficient funds* and *quote expired* (§2.2, 2.3, 2.6, 4.2-4.4).
4. The deposit webhook: payload and signature (§6).
5. The answer on reversals of confirmed deposits (§5.2).

Everything else (adjustment, card top-up, reconciliation endpoints, batch charge) can follow.

---

*Contact: the Xenos team. We will reply to the API document with a short conformance report listing any place our assumptions in §9 differed and how we adapted.*
