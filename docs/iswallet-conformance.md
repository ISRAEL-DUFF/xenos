# Xenos ↔ iswallet: conformance report and open questions

**For:** the iswallet team. **From:** Xenos engineering.
**Against:** *iswallet → Xenos: Integration Guide v1.0* (5 October 2026).

Thank you for the guide, and for being explicit about what is not built. We have adapted the integration to it. This document records what we did, what we cannot finish without your answers, and the changes you should know we made on the strength of the guide. The client is `internal/billing/iswallet.go`; its behaviour is covered by contract tests written from the guide (`internal/billing/iswallet_test.go`) because we do not yet have a sandbox key.

---

## 1. Questions we need answered (blocking)

These are the things the guide does not say and that we will not guess, because they involve money.

### Q1. How do we read a wallet's balances?
The guide's interface sketch has `Balances(wallet_id) -> {ngn_kobo, usdt_micro} // one call, both currencies`, but §4-§8 give no path or response shape. We need: **method and path, the response JSON, and whether it distinguishes total / available / held.** Our client currently refuses to call anything and returns an explicit error, so the whole product fails safe (shows "wallet unavailable") rather than acting on a guessed balance. We use balances for the dashboard, for the "must cover 24 hours" check at VM creation, and to decide when a customer is out of funds.

### Q2. What is the USDT minor unit?
Your quote example shows `debit_amount: 5000000` (₦50,000) and `credit_amount: 30781` at `fx_rate 0.00061562`, which works out to **30.781 USDT, i.e. three decimals**. Your charge example uses `amount: 6000`, which is our price in *micro*-USDT (0.006 USDT) but would be **6 USDT** at three decimals. These cannot both be right. **How many decimal places does iswallet's USDT use for `amount` / `credit_amount` / balances?** This is a required setting on our side (`XENOS_ISPEND_USDT_DECIMALS`, no default), because a wrong value mis-prices every charge by a power of ten. We also refuse to send an amount that is not a whole number of your minor units rather than rounding it.

### Q3. How does a webhook say what it is?
The `wallet.credit.posted` body in §8.3 has no event-type or event-id field, and no header is named for the type. Where is the **event type** carried (header, body field, envelope)? Is there a stable **event id** distinct from the per-delivery `X-iSpend-Idempotency-Key`? Is the delivery key the *same* across retries of one event? Until we know, we read the type from `X-iSpend-Event`, `event_type` or `type` if present and otherwise infer it (a body with `reversed_provider_reference` is a reversal, anything else a posted credit), logging a warning. We dedupe on the ledger `txn_id`, not the delivery key, so redelivery is safe either way.

### Q4. Which credits fire `wallet.credit.posted`?
Does a **conversion** (the USDT credit) or a **transfer** to a customer wallet (our goodwill adjustments) also fire `wallet.credit.posted`? What are all the **`source_type`** values? We act only on `currency == "NGN"` and `source_type == "pull_inflow"` and ignore everything else, so a USDT credit can never be converted a second time. Please confirm `pull_inflow` is the value for a bank transfer into a virtual account, in sandbox and live.

### Q5. Error envelope and idempotency conflicts
What is the exact JSON of an error (we read `error.code`, `error.message`, `error.original_response`, and `request_id` at the top level or inside `error`, and the `X-Request-Id` header)? **What status and `code` does "same key, different payload" return?** We do not have a code for it today and would like to treat it as a distinct, non-retryable error.

### Q6. Are the Convert credit and the Transfer credit visible in the customer's balance immediately?
We read the balance right after a conversion. The guide says both are synchronous and final; please confirm there is no settlement delay between the response and a balance read.

---

## 2. Changes we made because of the guide

| Guide | What we did |
|---|---|
| §0.1 one wallet, no customer | `wallet_id` is our customer id. `CreateCustomer` does `POST /v1/wallets` then `POST …/virtual-account`. The interface no longer has separate NGN/USDT wallet ids. |
| §0.2 spread is 1.5%, platform-wide | We no longer assume a configurable spread. Our margin lives in the per-hour VM price; naira prices are shown from `GET /v1/rates` `effective_rate` (converted to kobo per USDT with exact arithmetic). |
| §0.3 no card | Removed card top-up end to end (API, interface, dashboard). The wallet page offers bank transfer only and states the account name, bank and limits. |
| §3 idempotency, forever | We rely on it. Keys are `signup:<email>`, `conversion:<id>[:<attempt>]`, `vm:<id>:hour:<yyyymmddhh>`, `adjustment:<id>`, `va:<wallet>`, `xenos:sub:primary`. Convert sends the key in the header **and** as `client_idempotency_key`. |
| §4 namespace `owner_ref` | `owner_ref = <prefix>:user:<id>`, prefix configurable (`XENOS_ISPEND_OWNER_PREFIX`, default `xenos`). Use a different prefix in sandbox: sandbox is never reset, so a rebuilt dev database would otherwise reuse refs. |
| §4 `409 WALLET_ALREADY_EXISTS` | Read as success; the nested wallet is used. |
| §4.1 virtual account | Issued at signup; the account details are **stored in our database**, because there is no call to read them back (re-issuing is idempotent, so we do that lazily if signup could not). Shown to the customer only after email verification. |
| §6.1 quotes last 60 s | The quote's `expires_at` is saved and shown as a live countdown; confirming an expired quote is refused locally without calling you. |
| §6.2 replay after expiry | Every conversion **persists its quote and key before executing**, and a retry replays the *same* quote under the *same* key (a new quote under an old key would be a different payload and rejected). Only when the replay returns `QUOTE_EXPIRED`, which means it never executed, does an automatic conversion take a fresh quote under a new key (`conversion:<id>:2`). A customer's own quote is never silently replaced. |
| §6.2 `INSUFFICIENT_LIQUIDITY` | Treated like a paused rate: the conversion stays pending and is retried by a sweep, the customer's naira is untouched, and operators are alerted (at most hourly). |
| §6.2 `QUOTE_ALREADY_USED` | Fails the conversion and alerts operators to check the ledger. |
| §7 transfer as the charge | `POST /v1/transfers` customer → merchant wallet. `narration` carries `vm:<id> hour:<yyyymmddhh>`, the only metadata we can attach. |
| §8 webhook scheme | `X-iSpend-Signature: sha256=<hex>` over `"<timestamp>.<raw body>"`, timestamp tolerance 300 s, constant-time compare, raw body read before parsing. `wallet.credit.posted` starts an automatic conversion of the *credited* amount; `wallet.credit.reversed` is recorded and alerted (see below). |
| §8.4 reversals | iswallet bears the loss and the customer keeps their credit, so we do nothing to the customer's balance; we record the reversal (`deposit_reversals`) and alert an operator once per event. |
| §9 100 requests/min | Our client paces itself (default 80/min, burst 10) and charges are spread across the first 40 minutes of each hour by a stable per-VM offset (`XENOS_METER_SPREAD_MINUTES`) instead of firing at :00. A `429` is treated as a transient failure and retried by the next metering pass. |
| §10 tiers | New accounts are TIER_1. The wallet page tells customers the ₦50,000 per-transfer and per-day limit (`XENOS_DEPOSIT_LIMIT_KOBO`). We have **no BVN / TIER_2 upgrade flow in V1**; customers who need to fund more must split transfers. |
| §11 not built | We built nothing that depends on card, batch charge, structured transfer metadata, tenant reversal, lookup by idempotency key, or sandbox failure injection. |

## 3. Two decisions we would like you to confirm are acceptable

1. **Admin adjustments use transfers, both directions.** Your guide says a tenant-initiated *debit* is not an API. We needed goodwill credits and corrections, so a credit is a transfer **merchant wallet → customer** and a debit is a transfer **customer → merchant wallet**, both with a mandatory staff note in the narration, both audited on our side. These are the same primitive we already use for hourly charges between two wallets of our own tenant. If a staff-initiated debit is not something you want a tenant to do through `/v1/transfers`, tell us and we will remove debits (credits would remain). We did not use `POST /v1/platform/credits`, because the guide gives no request shape.
2. **Merchant wallet.** We create one wallet for Xenos ourselves with `POST /v1/wallets` and set its id in `XENOS_ISPEND_MERCHANT_WALLET`. Which `owner_type` and `currency` do you want for it (the guide says "`client` or a normal wallet")? A credit adjustment is paid from this wallet, so it must hold USDT: it receives it from the hourly charges.

## 4. What we will need to go live

- A **sandbox client key** and the base URL (`https://synledger.name.ng/iwallet`).
- Answers to Q1-Q6.
- The **error catalogue** you plan to write (every code per endpoint, and which are retryable).
- **USDT limits**, before launch (we show customers none).
- A **per-client rate-limit override** and a **batch charge** before we pass about 100 running VMs (one charge per VM per hour; today we stay under 100 calls a minute by spreading them, which holds to roughly 2,000 VM-hours of headroom per hour but leaves little for balance reads as we grow).
- **Lookup by idempotency key** and **`Retry-After` on 429**, when ready.

## 5. How we will test against your sandbox

Once we have a key we will run, in order: create a customer and issue its account; `POST /v1/sandbox/simulate/deposit` and confirm the webhook arrives, verifies, and converts exactly once even when redelivered; force `QUOTE_EXPIRED` by waiting 61 s; force `INSUFFICIENT_FUNDS` on a charge; `POST /v1/sandbox/simulate/reversal` and confirm the alert; replay a charge key after an hour and confirm one ledger effect. Failure injection we will simulate on our side, as you suggest.
