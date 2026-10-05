# Xenos ↔ iswallet: conformance report and open questions

**For:** the iswallet team. **From:** Xenos engineering.
**Against:** *iswallet → Xenos: Integration Guide v1.0* (5 October 2026).

Thank you for the guide, and for being explicit about what is not built. We have adapted the integration to it. This document records what we did, what we cannot finish without your answers, and the changes you should know we made on the strength of the guide. The client is `internal/billing/iswallet.go`; its behaviour is covered by contract tests written from the guide (`internal/billing/iswallet_test.go`) because we do not yet have a sandbox key.

---

## 1. Status of our questions (v1.1 of the guide and iswallet's reply)

All six blocking questions were answered. What we did with each:

| Q | Answer | What we did |
|---|---|---|
| Q1 balances (verified live) | `GET /v1/wallets/{id}/balance`; read `balances[]`, use `available`; an absent currency is zero | Implemented. We read `available` and refuse any response whose `scale` is not what we expect (NGN 2, USDT 6). |
| Q2 USDT scale | 6 decimals (micro-USDT); the guide's quote example was wrong | Setting kept as a guard with default 6. Their own correction of the quote example confirms our reading; the 6,000 micro-USDT charge example was right all along. |
| Q3 webhook type | Envelope: `event_type` and stable `id` in the body, payload under `data` | Parser rewritten to the envelope. **The type-inference code is deleted**: a signed body with no `event_type` is rejected (400), never guessed at. |
| Q4 which credits | Posted fires for `pull_inflow`, `wallet_transfer` (our adjustments) and legacy `va_deposit`; convert emits `convert.completed` instead | We convert only `NGN` + `pull_inflow`; everything else is acknowledged and ignored (tested for USDT, `wallet_transfer`, `va_deposit`). |
| Q5 errors | `error.{code,message,request_id}`; `X-Request-ID` header; key reuse is `422 IDEMPOTENCY_KEY_REUSED`, non-retryable | Request id read from the header and from `error`. New `ErrIdempotencyKeyReused`: alerted to operators, never treated as an outage or as insufficient funds. |
| Q6 consistency | A read after a convert/transfer response reflects it | We read balances straight after conversions. |
| Merchant wallet | Do not create one: `GET /v1/platform/account` → `operating_wallet_id` | Charges and adjustments use it (resolved once and cached; `XENOS_ISPEND_MERCHANT_WALLET` is now an optional override). The admin revenue page shows its USDT balance, since admin credits are paid from it. |
| Adjustments | Transfers in both directions are acceptable | Unchanged. We will move the staff note into structured metadata once transfers have it. We have noted the point about customers seeing a debit they did not initiate; it is a question for our terms of service. |

## 1a. Still open

1. **§7.2 example still shows `credit_amount: 30781`** in the *execute response*, although v1.1 corrected the *quote* to 30,781,000. If the response really reports micro-USDT, that example is a typo; if it is accurate it contradicts the quote by 1,000×. We treat the **quote** as the binding amount, record it, and **raise an operator alert if the execute response disagrees** (tested). Please confirm which is right.
2. **`GET /v1/wallets/{id}/virtual-accounts`** exists but the guide shows no response shape. We do not call it; we keep our own copy of the account issued at signup and re-issue idempotently if it is missing. Tell us the shape and we will use it to reconcile.
3. Awaiting from you: the **sandbox key**, the **error catalogue**, **lookup by idempotency key**, **`Retry-After`**, **USDT limits**, and later the per-client rate limit and batch charge.

---

## 1b. Findings from the first live sandbox run (5 October 2026)

We ran `go test -tags sandbox ./internal/billing` against `https://synledger.name.ng/iwallet` with our client key. What worked: wallet creation and its idempotent replay, issuing the virtual account, `IDEMPOTENCY_KEY_REUSED` on a changed payload, `GET …/balance` (scales asserted), `GET /v1/platform/account`, and `POST /v1/sandbox/simulate/deposit`. Where the sandbox differs from the guide, or blocks us, with request ids for your logs:

| # | What we saw | Request ids | What we need |
|---|---|---|---|
| F1 **blocking** | **FX is unavailable.** `GET /v1/rates?from=NGN&to=USDT` returns **`500 INTERNAL "fx quote NGN/USDT: not found"`** (the guide documents `503 FX_UNAVAILABLE` for a paused rate), and `POST /v1/convert/quotes` returns `503 FX_UNAVAILABLE "fx rate unavailable, try again"`. Retried over several minutes. | `req_c5be55f4b8e9af159c7a781d`, `req_f14e2934c0c29f14c7a94f61`, `req_bf02c952d4f2101b24106223` (rates); `req_9a092566eb717f99035c87e3`, `req_2cedb0fa8ac40f9ac9611016` (quote) | Seed or enable an NGN/USDT rate in sandbox. Until then we cannot test convert, charge or adjustments, all of which need a USDT balance. |
| F2 | **`POST /v1/wallets` requires `client_id`** in the body, equal to the API key's (`403 SCOPE_INSUFFICIENT: client_id must match your API key's client_id`). The guide's example omits it. | `req_639d6e209afd0ab76e3b756b` | Add it to the guide. We now send the value from `GET /v1/platform/account` (`"xenos"`). |
| F3 | **`WALLET_ALREADY_EXISTS` carries an empty wallet** in `error.original_response` (every field blank), not the existing wallet as the guide says. We therefore cannot recover the wallet id from that 409. | `req_d9ddb5e39713c1fbba642d5a`, `req_4517175468fde2865e378faf` | Populate `original_response`, or give us a lookup by `owner_ref` (e.g. `GET /v1/wallets?owner_ref=`). Our stable per-user signup key avoids this path in normal operation, but it is our only recovery if a signup is interrupted after iswallet created the wallet under a *different* key. |
| F4 | **A deposit fee is deducted.** `simulate/deposit` of 5,000,000 kobo (₦50,000) left **4,930,000 spendable**: a fee of 70,000 kobo, **1.40%**. | wallet `98aef08b-b727-4445-b931-d9dafe10b959` and others | Is this the mock provider's fee only, or what live bank transfers will cost? Customers will see it before our own margin and your 1.5% spread. We reconcile on the credited amount, as you advise. |
| F5 | **Charging a wallet with no USDT returns `422 CURRENCY_MISMATCH` ("wallets must share currency USDT"), not `INSUFFICIENT_FUNDS`.** A customer who has never converted has no USDT balance. | `req_c452dc8136271ecd8cc0098c`, `req_1e66984e57072281bba02806` | Confirm this is intended. We now treat it as "cannot pay" when the customer's USDT is below the charge and alert otherwise. **Related:** does a transfer *into* an operating wallet that holds no USDT balance yet also return `CURRENCY_MISMATCH`? The first hourly charge would hit it. We can only test that once F1 is fixed. |
| F6 | The simulator's reply and `/v1/sandbox/simulate/reversal` are undocumented beyond their paths. | | Request and response shapes for `simulate/reversal`. We have not called it. |

Not yet exercised (blocked by F1, or needing a public webhook URL): quote, convert and replay, `QUOTE_EXPIRED`, `QUOTE_ALREADY_USED`, charge and replay, adjustments in both directions, the real webhook delivery and signature, reversals, the first charge into the operating wallet.

---

## 2. Changes we made because of the guide

| Guide | What we did |
|---|---|
| §0.1 one wallet, no customer | `wallet_id` is our customer id. `CreateCustomer` does `POST /v1/wallets` then `POST …/virtual-account`. The interface no longer has separate NGN/USDT wallet ids. Balances come from `GET …/balance`. |
| §0.2 spread is 1.5%, platform-wide | We no longer assume a configurable spread. Our margin lives in the per-hour VM price; naira prices are shown from `GET /v1/rates` `effective_rate` (converted to kobo per USDT with exact arithmetic). |
| §0.3 no card | Removed card top-up end to end (API, interface, dashboard). The wallet page offers bank transfer only and states the account name, bank and limits. |
| §3 idempotency, forever | We rely on it. Keys are `signup:<email>`, `conversion:<id>[:<attempt>]`, `vm:<id>:hour:<yyyymmddhh>`, `adjustment:<id>`, `va:<wallet>`, `xenos:sub:primary`. Convert sends the key in the header **and** as `client_idempotency_key`. |
| §4 namespace `owner_ref` | `owner_ref = <prefix>:user:<id>`, prefix configurable (`XENOS_ISPEND_OWNER_PREFIX`, default `xenos`). Use a different prefix in sandbox: sandbox is never reset, so a rebuilt dev database would otherwise reuse refs. |
| §4 `409 WALLET_ALREADY_EXISTS` | We would read the nested wallet as success, but the sandbox returns it empty (F3), so this surfaces as an error. |
| §4.1 virtual account | Issued at signup; the account details are **stored in our database** (re-issuing is idempotent, so we do that lazily if signup could not). Shown to the customer only after email verification. |
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

## 3. Decision confirmed by iswallet

**Admin adjustments use transfers, both directions** (iswallet confirmed this is acceptable). Your guide says a tenant-initiated *debit* is not an API. We needed goodwill credits and corrections, so a credit is a transfer **merchant wallet → customer** and a debit is a transfer **customer → merchant wallet**, both with a mandatory staff note in the narration, both audited on our side. These are the same primitive we already use for hourly charges between two wallets of our own tenant. If a staff-initiated debit is not something you want a tenant to do through `/v1/transfers`, tell us and we will remove debits (credits would remain). We did not use `POST /v1/platform/credits`, because the guide gives no request shape.

## 4. What we will need to go live

- A **sandbox client key** (base URL `https://synledger.name.ng/iwallet`).
- The **error catalogue** you plan to write (every code per endpoint, and which are retryable).
- **USDT limits**, before launch (we show customers none).
- A **per-client rate-limit override** and a **batch charge** before we pass about 100 running VMs (one charge per VM per hour; today we stay under 100 calls a minute by spreading them, which holds to roughly 2,000 VM-hours of headroom per hour but leaves little for balance reads as we grow).
- **Lookup by idempotency key** and **`Retry-After` on 429**, when ready.

## 5. How we will test against your sandbox

Once we have a key we will run, in order: create a customer and issue its account; `POST /v1/sandbox/simulate/deposit` and confirm the webhook arrives, verifies, and converts exactly once even when redelivered; force `QUOTE_EXPIRED` by waiting 61 s; force `INSUFFICIENT_FUNDS` on a charge; `POST /v1/sandbox/simulate/reversal` and confirm the alert; replay a charge key after an hour and confirm one ledger effect. Failure injection we will simulate on our side, as you suggest.
