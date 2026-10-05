import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, type Quote } from "./api";
import { useAuth } from "./auth";
import { formatDate, formatNaira, formatRunway, formatUSDT, parseNaira, usdtToKobo } from "./format";
import { usePlans, useWallet } from "./hooks";
import { Badge, Banner, Button, Card, CopyButton, ErrorText, Field, Input, Loading, PageHeader, Stat, statusTone } from "./ui";

const linkClass = "font-medium text-indigo-600 underline-offset-2 hover:underline dark:text-indigo-400";

export function WalletPage() {
  const qc = useQueryClient();
  const { refresh } = useAuth();
  const w = useWallet();
  const plans = usePlans();

  if (w.isLoading) return <Loading />;
  if (w.error || !w.data) return <ErrorText error={w.error ?? new Error("Wallet unavailable")} />;
  const wallet = w.data;
  const rate = wallet.rate_kobo_per_usdt;
  const smallest = plans.data?.[0];
  const creditInNaira = usdtToKobo(wallet.usdt_uusdt, rate);
  const reload = () => qc.invalidateQueries({ queryKey: ["wallet"] });

  return (
    <div className="space-y-6">
      <PageHeader title="Wallet" subtitle="Top up in naira. We convert it once into USDT compute credit at the rate shown at the time, and VMs are billed in USDT." />

      {wallet.quoting_paused && (
        <Banner tone="warn">Conversion is paused right now. Anything you deposit stays safe in your naira balance and can be converted as soon as it resumes.</Banner>
      )}
      {wallet.unpaid_uusdt > 0 && <Banner tone="danger">You have {formatUSDT(wallet.unpaid_uusdt)} of unpaid usage. Top up to settle it.</Banner>}

      <div className="grid gap-3 sm:grid-cols-3">
        <Stat label="Compute credit" value={formatUSDT(wallet.usdt_uusdt)} sub={creditInNaira !== null ? `≈ ${formatNaira(creditInNaira)} today` : undefined} />
        <Stat label="Naira balance" value={formatNaira(wallet.ngn_kobo)} sub="Not yet converted" />
        <Stat label="Runway" value={formatRunway(wallet.runway_hours)} sub={wallet.hourly_uusdt > 0 ? `at ${formatUSDT(wallet.hourly_uusdt)}/hr` : "No VMs running"} />
      </div>

      <FundingCard wallet={wallet} />

      <Card className="space-y-3">
        <div className="flex items-start justify-between gap-3">
          <div>
            <h2 className="font-medium text-slate-900 dark:text-slate-50">Auto-convert deposits</h2>
            <p className="text-sm text-slate-500 dark:text-slate-400">
              On: every naira deposit is converted to compute credit straight away. Off: it waits in your naira balance until you convert it below.
            </p>
          </div>
          <AutoConvertToggle value={wallet.auto_convert} onChanged={async () => { await refresh(); reload(); }} />
        </div>
      </Card>

      <ConvertCard ngnKobo={wallet.ngn_kobo} verified={wallet.email_verified} paused={wallet.quoting_paused} smallestPlan={smallest} onDone={reload} />

      <section className="space-y-3">
        <h2 className="font-medium text-slate-900 dark:text-slate-50">Conversions</h2>
        {wallet.conversions.length === 0 ? (
          <p className="text-sm text-slate-500 dark:text-slate-400">No conversions yet.</p>
        ) : (
          <div className="overflow-x-auto rounded-xl border border-slate-200 dark:border-slate-700">
            <table className="w-full min-w-[32rem] text-left text-sm">
              <thead className="bg-slate-100 text-xs uppercase text-slate-500 dark:bg-slate-800 dark:text-slate-400">
                <tr><th className="px-3 py-2">Date</th><th className="px-3 py-2">Naira</th><th className="px-3 py-2">Credit</th><th className="px-3 py-2">Rate</th><th className="px-3 py-2">Status</th></tr>
              </thead>
              <tbody className="divide-y divide-slate-200 dark:divide-slate-700">
                {wallet.conversions.map((c) => (
                  <tr key={c.id}>
                    <td className="px-3 py-2">{formatDate(c.created_at)}</td>
                    <td className="px-3 py-2 tabular-nums">{formatNaira(c.amount_ngn_kobo)}</td>
                    <td className="px-3 py-2 tabular-nums">{c.amount_uusdt !== null ? formatUSDT(c.amount_uusdt) : "—"}</td>
                    <td className="px-3 py-2 tabular-nums">{c.rate ? `₦${c.rate}/USDT` : "—"}</td>
                    <td className="px-3 py-2"><Badge tone={statusTone(c.status)}>{c.status}</Badge></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="space-y-3">
        <h2 className="font-medium text-slate-900 dark:text-slate-50">Recent charges</h2>
        {wallet.charges.length === 0 ? (
          <p className="text-sm text-slate-500 dark:text-slate-400">Nothing has been charged yet.</p>
        ) : (
          <div className="overflow-x-auto rounded-xl border border-slate-200 dark:border-slate-700">
            <table className="w-full min-w-[28rem] text-left text-sm">
              <thead className="bg-slate-100 text-xs uppercase text-slate-500 dark:bg-slate-800 dark:text-slate-400">
                <tr><th className="px-3 py-2">Hour</th><th className="px-3 py-2">VM</th><th className="px-3 py-2">Amount</th><th className="px-3 py-2">Status</th></tr>
              </thead>
              <tbody className="divide-y divide-slate-200 dark:divide-slate-700">
                {wallet.charges.map((c) => (
                  <tr key={c.id}>
                    <td className="px-3 py-2">{formatDate(c.hour)}</td>
                    <td className="px-3 py-2"><Link to={`/vms/${c.vm_id}`} className={linkClass}>{c.hostname}</Link></td>
                    <td className="px-3 py-2 tabular-nums">{formatUSDT(c.amount_uusdt)}</td>
                    <td className="px-3 py-2"><Badge tone={statusTone(c.status)}>{c.status}</Badge></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}

function FundingCard({ wallet }: { wallet: NonNullable<ReturnType<typeof useWallet>["data"]> }) {
  return (
    <Card className="space-y-3">
      <h2 className="font-medium text-slate-900 dark:text-slate-50">Bank transfer</h2>
      {wallet.virtual_account ? (
        <>
          <p className="text-sm text-slate-500 dark:text-slate-400">Send naira to your personal account below from any bank app. It arrives within minutes and is converted automatically if auto-convert is on. This is the only way to fund your wallet.</p>
          <dl className="grid gap-3 text-sm sm:grid-cols-2">
            <div>
              <dt className="text-slate-500 dark:text-slate-400">Bank</dt>
              <dd className="font-medium text-slate-900 dark:text-slate-50">{wallet.virtual_account.bank}</dd>
            </div>
            {wallet.virtual_account.account_name && (
              <div>
                <dt className="text-slate-500 dark:text-slate-400">Account name</dt>
                <dd className="font-medium text-slate-900 dark:text-slate-50">{wallet.virtual_account.account_name}</dd>
              </div>
            )}
            <div>
              <dt className="text-slate-500 dark:text-slate-400">Account number</dt>
              <dd className="flex items-center gap-2 font-mono text-base font-medium text-slate-900 dark:text-slate-50">
                {wallet.virtual_account.account_number}
                <CopyButton text={wallet.virtual_account.account_number} />
              </dd>
            </div>
          </dl>
          {wallet.deposit_limit_kobo > 0 && (
            <p className="text-xs text-slate-500 dark:text-slate-400">
              Limits: a basic account can receive up to {formatNaira(wallet.deposit_limit_kobo)} per transfer and per day. Larger deposits are rejected by the bank, so split them or contact support.
            </p>
          )}
        </>
      ) : (
        <p className="text-sm text-slate-600 dark:text-slate-300">
          {wallet.email_verified ? "Your account number is being set up. Check back shortly." : "Verify your email to see your account number."}
        </p>
      )}
    </Card>
  );
}

function AutoConvertToggle({ value, onChanged }: { value: boolean; onChanged: () => void }) {
  const set = useMutation({
    mutationFn: (next: boolean) => api("/wallet/settings", { method: "PATCH", json: { auto_convert: next } }),
    onSuccess: onChanged,
  });
  return (
    <div className="shrink-0 text-right">
      <button
        type="button"
        role="switch"
        aria-checked={value}
        aria-label="Auto-convert deposits"
        disabled={set.isPending}
        onClick={() => set.mutate(!value)}
        className={`relative inline-flex h-6 w-11 items-center rounded-full transition disabled:opacity-50 ${value ? "bg-indigo-600" : "bg-slate-300 dark:bg-slate-600"}`}
      >
        <span className={`inline-block h-5 w-5 rounded-full bg-white shadow transition ${value ? "translate-x-5" : "translate-x-0.5"}`} />
      </button>
      <ErrorText error={set.error} />
    </div>
  );
}

/** Seconds left until an ISO timestamp, ticking twice a second; 0 once it has passed. */
function useSecondsLeft(iso: string | undefined): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!iso) return;
    setNow(Date.now());
    const t = window.setInterval(() => setNow(Date.now()), 500);
    return () => window.clearInterval(t);
  }, [iso]);
  return iso ? Math.max(0, Math.ceil((new Date(iso).getTime() - now) / 1000)) : 0;
}

function ConvertCard({
  ngnKobo,
  verified,
  paused,
  smallestPlan,
  onDone,
}: {
  ngnKobo: number;
  verified: boolean;
  paused: boolean;
  smallestPlan: { slug: string; price_uusdt_hourly: number } | undefined;
  onDone: () => void;
}) {
  const [amount, setAmount] = useState("");
  const [quote, setQuote] = useState<Quote | null>(null);
  const [message, setMessage] = useState("");
  const kobo = parseNaira(amount);
  const secondsLeft = useSecondsLeft(quote?.expires_at);
  // The rate is only valid for 60 seconds; once it lapses the customer must ask again.
  useEffect(() => {
    if (quote && secondsLeft === 0) {
      setQuote(null);
      setMessage("That quote expired (rates are held for 60 seconds). Get a new one to continue.");
    }
  }, [quote, secondsLeft]);

  const getQuote = useMutation({
    mutationFn: () => api<Quote>("/wallet/convert", { json: { amount_ngn_kobo: kobo } }),
    onSuccess: (q) => {
      setQuote(q);
      setMessage("");
    },
  });
  const confirm = useMutation({
    mutationFn: () => api<{ status: string }>("/wallet/convert", { json: { quote_id: quote!.quote_id } }),
    onSuccess: (r) => {
      setMessage(r.status === "complete" ? "Converted. Your credit is updated." : "Conversion is being processed and will complete shortly.");
      setQuote(null);
      setAmount("");
      onDone();
    },
    onError: () => setQuote(null), // an expired quote must be requested again
  });

  if (ngnKobo <= 0 && !message) return null;

  const days = quote && smallestPlan && smallestPlan.price_uusdt_hourly > 0 ? Math.floor(quote.amount_uusdt / smallestPlan.price_uusdt_hourly / 24) : null;

  return (
    <Card className="space-y-3">
      <h2 className="font-medium text-slate-900 dark:text-slate-50">Convert naira to compute credit</h2>
      {message && <Banner tone={message.startsWith("That quote expired") ? "warn" : "ok"}>{message}</Banner>}
      {!quote ? (
        <form
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault();
            if (kobo !== null) getQuote.mutate();
          }}
        >
          <Field label="Amount (₦)" hint={`You have ${formatNaira(ngnKobo)} available.`}>
            <Input inputMode="decimal" placeholder="20,000" value={amount} onChange={(e) => setAmount(e.target.value)} disabled={!verified || paused} />
          </Field>
          <ErrorText error={getQuote.error ?? confirm.error} />
          <Button disabled={!verified || paused || kobo === null || kobo < 10_000 || getQuote.isPending}>{getQuote.isPending ? "Getting quote…" : "Get quote"}</Button>
        </form>
      ) : (
        <div className="space-y-3">
          <div className="rounded-lg bg-indigo-50 p-4 dark:bg-indigo-950/40">
            <p className="text-lg font-semibold text-slate-900 dark:text-slate-50">
              {formatNaira(quote.amount_ngn_kobo)} → {formatUSDT(quote.amount_uusdt)}
            </p>
            <p className="text-sm text-slate-600 dark:text-slate-300">
              Rate ₦{quote.rate} per USDT
              {days !== null && smallestPlan && ` · about ${days} days of ${smallestPlan.slug}`}
              {quote.added_runway_hours ? ` · adds ${formatRunway(quote.added_runway_hours)} of runway` : ""}
            </p>
            <p className="mt-1 text-xs text-slate-500 dark:text-slate-400" role="timer">
              This rate is held for {secondsLeft}s. Later rate changes will not affect credit you already hold.
            </p>
          </div>
          <ErrorText error={confirm.error} />
          <div className="flex gap-2">
            <Button onClick={() => confirm.mutate()} disabled={confirm.isPending || secondsLeft === 0}>{confirm.isPending ? "Converting…" : "Confirm conversion"}</Button>
            <Button variant="secondary" onClick={() => setQuote(null)} disabled={confirm.isPending}>Cancel</Button>
          </div>
        </div>
      )}
    </Card>
  );
}
