import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, type Statement } from "./api";
import { formatDate, formatNaira, formatUSDT } from "./format";
import { Button, Card, Empty, ErrorText, Loading, PageHeader } from "./ui";

const linkClass = "font-medium text-indigo-600 underline-offset-2 hover:underline dark:text-indigo-400";
const tableWrap = "overflow-x-auto rounded-xl border border-slate-200 dark:border-slate-700 print:border-slate-400";
const th = "px-3 py-2";
const thead = "bg-slate-100 text-left text-xs uppercase text-slate-500 dark:bg-slate-800 dark:text-slate-400 print:bg-white";

const monthName = (m: string) => new Date(`${m}-01T00:00:00Z`).toLocaleDateString("en-GB", { month: "long", year: "numeric", timeZone: "UTC" });

/** Wallet → Statements: one page per UTC month, with a printable layout and a CSV of every charged hour. */
export function StatementList() {
  const q = useQuery({ queryKey: ["statements"], queryFn: () => api<{ months: string[] }>("/statements") });
  if (q.isLoading) return <Loading />;
  return (
    <div className="space-y-5">
      <PageHeader title="Statements" subtitle="What was charged, month by month (UTC months)." actions={<Link to="/wallet" className={linkClass}>Back to wallet</Link>} />
      <ErrorText error={q.error} />
      {(q.data?.months ?? []).length === 0 ? (
        <Empty title="No activity yet">Statements appear once you have had a charge, a top-up or an adjustment.</Empty>
      ) : (
        <Card>
          <ul className="divide-y divide-slate-200 text-sm dark:divide-slate-700">
            {q.data!.months.map((m) => (
              <li key={m} className="py-2">
                <Link to={`/statements/${m}`} className={linkClass}>
                  {monthName(m)}
                </Link>
              </li>
            ))}
          </ul>
        </Card>
      )}
    </div>
  );
}

export function StatementPage() {
  const { month = "" } = useParams();
  const q = useQuery({ queryKey: ["statement", month], queryFn: () => api<Statement>(`/statements/${month}`) });
  if (q.isLoading) return <Loading />;
  if (q.error || !q.data) return <ErrorText error={q.error ?? new Error("Statement not found")} />;
  const s = q.data;

  return (
    <div className="space-y-5">
      <PageHeader
        title={`Statement for ${monthName(s.month)}`}
        subtitle="All amounts are in USDT. Months run from the 1st 00:00 to the end of the last day, UTC."
        actions={
          <div className="flex gap-2 print:hidden">
            <Link to="/statements" className={linkClass + " self-center"}>
              All statements
            </Link>
            <a href={`/v1/statements/${s.month}?format=csv`} className="inline-flex items-center rounded-lg border border-slate-300 px-3 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 dark:border-slate-600 dark:text-slate-200 dark:hover:bg-slate-800">
              Download CSV
            </a>
            <Button variant="secondary" onClick={() => window.print()}>
              Print or save as PDF
            </Button>
          </div>
        }
      />

      <Card>
        <dl className="grid gap-4 text-sm sm:grid-cols-4">
          <div>
            <dt className="text-slate-500 dark:text-slate-400">Charged</dt>
            <dd className="text-lg font-semibold tabular-nums text-slate-900 dark:text-slate-50">{formatUSDT(s.totals.charged_uusdt)}</dd>
          </div>
          <div>
            <dt className="text-slate-500 dark:text-slate-400">Hours charged</dt>
            <dd className="text-lg font-semibold tabular-nums text-slate-900 dark:text-slate-50">
              {s.totals.charged_hours}
              {s.totals.capped_hours > 0 && <span className="ml-1 text-xs font-normal text-slate-500">+ {s.totals.capped_hours} free (monthly cap)</span>}
            </dd>
          </div>
          <div>
            <dt className="text-slate-500 dark:text-slate-400">Refunded</dt>
            <dd className="text-lg font-semibold tabular-nums text-slate-900 dark:text-slate-50">{formatUSDT(s.totals.refunded_uusdt)}</dd>
          </div>
          <div>
            <dt className="text-slate-500 dark:text-slate-400">Unpaid now (all months)</dt>
            <dd className="text-lg font-semibold tabular-nums text-slate-900 dark:text-slate-50">{formatUSDT(s.totals.unpaid_uusdt)}</dd>
          </div>
        </dl>
      </Card>

      <section className="space-y-2">
        <h2 className="font-medium text-slate-900 dark:text-slate-50">VMs</h2>
        {s.vms.length === 0 ? (
          <p className="text-sm text-slate-500 dark:text-slate-400">No VM was charged this month.</p>
        ) : (
          <div className={tableWrap}>
            <table className="w-full text-sm">
              <thead className={thead}>
                <tr>
                  <th className={th}>VM</th>
                  <th className={th}>Plan</th>
                  <th className={th + " text-right"}>Hours</th>
                  <th className={th + " text-right"}>Charged</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-200 dark:divide-slate-700">
                {s.vms.map((v) => (
                  <tr key={v.vm_id}>
                    <td className={th + " font-medium"}>{v.hostname}</td>
                    <td className={th}>{v.plan}</td>
                    <td className={th + " text-right tabular-nums"}>
                      {v.charged_hours}
                      {v.capped_hours > 0 ? ` (+${v.capped_hours} capped)` : ""}
                    </td>
                    <td className={th + " text-right tabular-nums"}>{formatUSDT(v.charged_uusdt)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {s.conversions.length > 0 && (
        <section className="space-y-2">
          <h2 className="font-medium text-slate-900 dark:text-slate-50">Top-ups converted to USDT</h2>
          <div className={tableWrap}>
            <table className="w-full text-sm">
              <thead className={thead}>
                <tr>
                  <th className={th}>Date</th>
                  <th className={th + " text-right"}>Naira</th>
                  <th className={th + " text-right"}>USDT</th>
                  <th className={th + " text-right"}>Rate</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-200 dark:divide-slate-700">
                {s.conversions.map((c) => (
                  <tr key={c.id}>
                    <td className={th}>{formatDate(c.at)}</td>
                    <td className={th + " text-right tabular-nums"}>{formatNaira(c.amount_ngn_kobo)}</td>
                    <td className={th + " text-right tabular-nums"}>{formatUSDT(c.amount_uusdt)}</td>
                    <td className={th + " text-right tabular-nums"}>{c.rate_kobo_per_usdt ? `₦${(Number(c.rate_kobo_per_usdt) / 100).toLocaleString("en-NG")}/USDT` : "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      {s.adjustments.length > 0 && (
        <section className="space-y-2">
          <h2 className="font-medium text-slate-900 dark:text-slate-50">Adjustments</h2>
          <div className={tableWrap}>
            <table className="w-full text-sm">
              <thead className={thead}>
                <tr>
                  <th className={th}>Date</th>
                  <th className={th}>Note</th>
                  <th className={th + " text-right"}>Amount</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-200 dark:divide-slate-700">
                {s.adjustments.map((a) => (
                  <tr key={a.id}>
                    <td className={th}>{formatDate(a.at)}</td>
                    <td className={th}>{a.note}</td>
                    <td className={th + " text-right tabular-nums"}>
                      {a.amount_uusdt >= 0 ? "+" : "−"}
                      {formatUSDT(Math.abs(a.amount_uusdt))}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </div>
  );
}
