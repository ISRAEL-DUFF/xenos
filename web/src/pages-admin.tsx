import { useEffect, useState, type FormEvent } from "react";
import { Link, NavLink, Outlet, useLocation, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type AdminJob, type AdminUser, type AdminVM, type Capacity, type Revenue, type VM } from "./api";
import { bytesToGiB, formatDate, formatNaira, formatUSDT, parseUSDT, usdtToKobo } from "./format";
import { Badge, Banner, Button, Card, ConfirmDialog, Empty, ErrorText, Field, Input, Loading, PageHeader, ProgressBar, Stat, StateBadge, cx, inputClass, statusTone } from "./ui";

const linkClass = "font-medium text-indigo-600 underline-offset-2 hover:underline dark:text-indigo-400";
const tableWrap = "overflow-x-auto rounded-xl border border-slate-200 dark:border-slate-700";
const th = "px-3 py-2";
const thead = "bg-slate-100 text-left text-xs uppercase text-slate-500 dark:bg-slate-800 dark:text-slate-400";
const tbody = "divide-y divide-slate-200 dark:divide-slate-700";

const tabs = [
  { to: "/admin", label: "Users", end: true },
  { to: "/admin/vms", label: "VMs" },
  { to: "/admin/capacity", label: "Capacity" },
  { to: "/admin/jobs", label: "Jobs" },
  { to: "/admin/revenue", label: "Revenue" },
];

export function AdminLayout() {
  const { pathname } = useLocation();
  return (
    <div>
      <PageHeader title="Admin" />
      <nav className="-mx-1 mb-5 flex gap-1 overflow-x-auto px-1" aria-label="Admin">
        {tabs.map((t) => (
          <NavLink
            key={t.to}
            to={t.to}
            end={t.end}
            className={({ isActive }) =>
              // The Users tab also covers /admin/users/:id.
              cx("whitespace-nowrap rounded-lg px-3 py-2 text-sm font-medium", isActive || (t.end && pathname.startsWith("/admin/users")) ? "bg-indigo-600 text-white" : "text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-800")
            }
          >
            {t.label}
          </NavLink>
        ))}
      </nav>
      <Outlet />
    </div>
  );
}

// ---------------------------------------------------------------- users

export function AdminUsers() {
  const [q, setQ] = useState("");
  const [debounced, setDebounced] = useState("");
  useEffect(() => {
    const t = setTimeout(() => setDebounced(q.trim()), 250);
    return () => clearTimeout(t);
  }, [q]);
  const users = useQuery({ queryKey: ["admin", "users", debounced], queryFn: () => api<AdminUser[]>(`/admin/users?q=${encodeURIComponent(debounced)}`) });

  return (
    <div className="space-y-4">
      <Input type="search" placeholder="Search by email" aria-label="Search users" value={q} onChange={(e) => setQ(e.target.value)} className="max-w-sm" />
      {users.isLoading && <Loading />}
      <ErrorText error={users.error} />
      {users.data && users.data.length === 0 && <Empty title="No users match" />}
      {users.data && users.data.length > 0 && (
        <div className={tableWrap}>
          <table className="w-full min-w-[40rem] text-sm">
            <thead className={thead}>
              <tr><th className={th}>Email</th><th className={th}>Status</th><th className={th}>VMs</th><th className={th}>Credit</th><th className={th}>Naira</th><th className={th}>Joined</th></tr>
            </thead>
            <tbody className={tbody}>
              {users.data.map((u) => (
                <tr key={u.id}>
                  <td className="px-3 py-2">
                    <Link to={`/admin/users/${u.id}`} className={linkClass}>{u.email}</Link>
                    {u.is_admin && <span className="ml-2"><Badge tone="blue">admin</Badge></span>}
                    {!u.email_verified && <span className="ml-2"><Badge tone="amber">unverified</Badge></span>}
                  </td>
                  <td className="px-3 py-2"><Badge tone={statusTone(u.status)}>{u.status}</Badge></td>
                  <td className="px-3 py-2 tabular-nums">{u.vm_count} / {u.vm_limit}</td>
                  <td className="px-3 py-2 tabular-nums">{u.usdt_uusdt !== null ? formatUSDT(u.usdt_uusdt) : "—"}</td>
                  <td className="px-3 py-2 tabular-nums">{u.ngn_kobo !== null ? formatNaira(u.ngn_kobo) : "—"}</td>
                  <td className="px-3 py-2 text-slate-500 dark:text-slate-400">{formatDate(u.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

interface UserDetail {
  user: AdminUser;
  vms: VM[];
  adjustments: { id: number; amount_uusdt: number; note: string; status: string; created_at: string; admin: string }[];
}

export function AdminUserDetail() {
  const { id } = useParams();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["admin", "user", id], queryFn: () => api<UserDetail>(`/admin/users/${id}`) });
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["admin", "user", id] });
    qc.invalidateQueries({ queryKey: ["admin", "users"] });
  };

  const [limit, setLimit] = useState("");
  const [confirmBan, setConfirmBan] = useState(false);
  const [amount, setAmount] = useState("");
  const [note, setNote] = useState("");
  // One id per attempt: a retry of the same submission (timeout, double click) is the same adjustment server-side.
  const [requestId, setRequestId] = useState(() => crypto.randomUUID());

  const patch = useMutation({ mutationFn: (body: object) => api(`/admin/users/${id}`, { method: "PATCH", json: body }), onSuccess: () => { setConfirmBan(false); refresh(); } });
  const adjust = useMutation({
    mutationFn: (micro: number) => api(`/admin/users/${id}/adjustments`, { json: { amount_uusdt: micro, note, request_id: requestId } }),
    onSuccess: () => { setAmount(""); setNote(""); setRequestId(crypto.randomUUID()); refresh(); },
  });

  if (q.isLoading) return <Loading />;
  if (q.error || !q.data) return <ErrorText error={q.error} />;
  const { user: u, vms, adjustments } = q.data;
  const micro = parseUSDT(amount);

  const onAdjust = (e: FormEvent) => {
    e.preventDefault();
    if (micro !== null && micro !== 0) adjust.mutate(micro);
  };

  return (
    <div className="space-y-5">
      <Link to="/admin" className={linkClass}>← All users</Link>
      <PageHeader title={u.email} subtitle={`Joined ${formatDate(u.created_at)}`} actions={<Badge tone={statusTone(u.status)}>{u.status}</Badge>} />
      {u.grace_ends_at && <Banner tone="warn">In the out-of-funds grace period until {formatDate(u.grace_ends_at)}.</Banner>}

      <div className="grid gap-3 sm:grid-cols-3">
        <Stat label="Compute credit" value={u.usdt_uusdt !== null ? formatUSDT(u.usdt_uusdt) : "—"} />
        <Stat label="Naira balance" value={u.ngn_kobo !== null ? formatNaira(u.ngn_kobo) : "—"} />
        <Stat label="VMs" value={`${u.vm_count} / ${u.vm_limit}`} sub={u.email_verified ? "email verified" : "email NOT verified"} />
      </div>

      <Card className="space-y-4">
        <h2 className="font-medium text-slate-900 dark:text-slate-50">Account controls</h2>
        <div className="flex flex-wrap items-end gap-2">
          <Field label="VM limit">
            <Input type="number" min={0} max={100} className="w-28" placeholder={String(u.vm_limit)} value={limit} onChange={(e) => setLimit(e.target.value)} />
          </Field>
          <Button variant="secondary" disabled={limit === "" || patch.isPending} onClick={() => { patch.mutate({ vm_limit: Number(limit) }); setLimit(""); }}>
            Save limit
          </Button>
        </div>
        {!u.is_admin && (
          <div className="flex flex-wrap gap-2">
            {u.status === "active" && <Button variant="secondary" disabled={patch.isPending} onClick={() => patch.mutate({ status: "suspended" })}>Suspend (blocks new VMs)</Button>}
            {u.status !== "active" && <Button variant="secondary" disabled={patch.isPending} onClick={() => patch.mutate({ status: "active" })}>Reactivate</Button>}
            {u.status !== "banned" && <Button variant="danger" onClick={() => setConfirmBan(true)}>Ban…</Button>}
          </div>
        )}
        {u.is_admin && <p className="text-sm text-slate-500 dark:text-slate-400">Admin accounts are managed from the command line.</p>}
        <ErrorText error={patch.error} />
      </Card>

      <Card>
        <form onSubmit={onAdjust} className="space-y-3">
          <h2 className="font-medium text-slate-900 dark:text-slate-50">Adjust balance</h2>
          <p className="text-sm text-slate-500 dark:text-slate-400">Credits or debits compute credit through iSpend. Use a negative number to debit. The note is kept with the entry.</p>
          <div className="grid gap-3 sm:grid-cols-[10rem_1fr]">
            <Field label="Amount (USDT)" hint="e.g. 5 or -2.5">
              <Input inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="5.00" />
            </Field>
            <Field label="Note" hint="Why? e.g. refund for outage on 3 Oct.">
              <Input value={note} onChange={(e) => setNote(e.target.value)} maxLength={200} />
            </Field>
          </div>
          <ErrorText error={adjust.error} />
          <Button disabled={micro === null || micro === 0 || note.trim().length < 3 || adjust.isPending}>
            {adjust.isPending ? "Applying…" : micro ? `${micro > 0 ? "Credit" : "Debit"} ${formatUSDT(Math.abs(micro))}` : "Apply"}
          </Button>
        </form>
      </Card>

      <section className="space-y-2">
        <h2 className="font-medium text-slate-900 dark:text-slate-50">VMs</h2>
        {vms.length === 0 ? <p className="text-sm text-slate-500 dark:text-slate-400">None.</p> : (
          <div className={tableWrap}>
            <table className="w-full min-w-[28rem] text-sm">
              <thead className={thead}><tr><th className={th}>Name</th><th className={th}>Plan</th><th className={th}>IPv4</th><th className={th}>State</th></tr></thead>
              <tbody className={tbody}>
                {vms.map((v) => (
                  <tr key={v.id}>
                    <td className="px-3 py-2">{v.hostname}</td>
                    <td className="px-3 py-2 capitalize">{v.plan}</td>
                    <td className="px-3 py-2 font-mono">{v.ipv4 ?? "—"}</td>
                    <td className="px-3 py-2"><StateBadge state={v.state} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="space-y-2">
        <h2 className="font-medium text-slate-900 dark:text-slate-50">Balance adjustments</h2>
        {adjustments.length === 0 ? <p className="text-sm text-slate-500 dark:text-slate-400">None.</p> : (
          <div className={tableWrap}>
            <table className="w-full min-w-[32rem] text-sm">
              <thead className={thead}><tr><th className={th}>When</th><th className={th}>Amount</th><th className={th}>Note</th><th className={th}>By</th><th className={th}>Status</th></tr></thead>
              <tbody className={tbody}>
                {adjustments.map((a) => (
                  <tr key={a.id}>
                    <td className="px-3 py-2">{formatDate(a.created_at)}</td>
                    <td className="px-3 py-2 tabular-nums">{a.amount_uusdt > 0 ? "+" : "−"}{formatUSDT(Math.abs(a.amount_uusdt))}</td>
                    <td className="px-3 py-2">{a.note}</td>
                    <td className="px-3 py-2">{a.admin}</td>
                    <td className="px-3 py-2"><Badge tone={statusTone(a.status)}>{a.status}</Badge></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {confirmBan && (
        <ConfirmDialog title={`Ban ${u.email}?`} confirmLabel="Ban account" typeToConfirm={u.email} busy={patch.isPending} error={patch.error} onConfirm={() => patch.mutate({ status: "banned" })} onCancel={() => setConfirmBan(false)}>
          <p>They are signed out everywhere and cannot log in. Their running VMs are suspended (stopped, disk kept). Deleting the VMs is a separate decision.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- vms

export function AdminVMs() {
  const qc = useQueryClient();
  const [state, setState] = useState("");
  const [search, setSearch] = useState("");
  const [flagged, setFlagged] = useState(false);
  const [toDelete, setToDelete] = useState<AdminVM | null>(null);
  const [note, setNote] = useState("");
  const params = new URLSearchParams({ state, q: search.trim(), flagged: String(flagged) });
  const vms = useQuery({ queryKey: ["admin", "vms", state, search.trim(), flagged], queryFn: () => api<AdminVM[]>(`/admin/vms?${params}`), refetchInterval: 10_000 });
  const refresh = () => qc.invalidateQueries({ queryKey: ["admin", "vms"] });

  const stop = useMutation({ mutationFn: (id: number) => api(`/admin/vms/${id}/stop`, { method: "POST" }), onSuccess: refresh });
  const del = useMutation({ mutationFn: (id: number) => api(`/admin/vms/${id}`, { method: "DELETE" }), onSuccess: () => { setToDelete(null); refresh(); } });
  const port25 = useMutation({
    mutationFn: ({ id, allow }: { id: number; allow: boolean }) => api<{ next_step: string }>(`/admin/vms/${id}/port25`, { json: { allow } }),
    onSuccess: (r) => { setNote(r.next_step); refresh(); },
  });

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <Input type="search" placeholder="Search owner or hostname" aria-label="Search VMs" value={search} onChange={(e) => setSearch(e.target.value)} className="max-w-xs" />
        <select aria-label="State" className={cx(inputClass, "w-auto")} value={state} onChange={(e) => setState(e.target.value)}>
          <option value="">All states</option>
          {["pending", "provisioning", "running", "stopped", "suspended", "deleting", "error"].map((s) => <option key={s}>{s}</option>)}
        </select>
        <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={flagged} onChange={(e) => setFlagged(e.target.checked)} /> Flagged only</label>
      </div>
      {note && <Banner tone="info">Port 25 setting saved. To apply it: {note}</Banner>}
      <ErrorText error={stop.error ?? del.error ?? port25.error} />
      {vms.isLoading && <Loading />}
      {vms.data && vms.data.length === 0 && <Empty title="No VMs match" />}
      {vms.data && vms.data.length > 0 && (
        <div className={tableWrap}>
          <table className="w-full min-w-[56rem] text-sm">
            <thead className={thead}><tr><th className={th}>VM</th><th className={th}>Owner</th><th className={th}>IPv4</th><th className={th}>State</th><th className={th}>Port 25</th><th className={th}>Actions</th></tr></thead>
            <tbody className={tbody}>
              {vms.data.map((v) => (
                <tr key={v.id} className={v.flagged ? "bg-amber-50/60 dark:bg-amber-950/20" : ""}>
                  <td className="px-3 py-2">
                    <span className="font-medium">{v.hostname}</span> <span className="text-slate-500 capitalize dark:text-slate-400">{v.plan}</span>
                    {v.flagged && <div className="text-xs text-amber-700 dark:text-amber-300">⚑ {v.flag_reason}</div>}
                  </td>
                  <td className="px-3 py-2"><Link to={`/admin/users/${v.user_id}`} className={linkClass}>{v.owner}</Link></td>
                  <td className="px-3 py-2 font-mono">{v.ipv4 || "—"}</td>
                  <td className="px-3 py-2"><StateBadge state={v.state} /></td>
                  <td className="px-3 py-2">{v.port25_unblocked ? <Badge tone="amber">unblocked</Badge> : <span className="text-slate-500 dark:text-slate-400">blocked</span>}</td>
                  <td className="px-3 py-2">
                    <div className="flex flex-wrap gap-1.5">
                      <Button variant="secondary" className="px-2 py-1 text-xs" disabled={v.state !== "running" || stop.isPending} onClick={() => stop.mutate(v.id)}>Force stop</Button>
                      <Button variant="secondary" className="px-2 py-1 text-xs" disabled={port25.isPending} onClick={() => port25.mutate({ id: v.id, allow: !v.port25_unblocked })}>
                        {v.port25_unblocked ? "Block 25" : "Unblock 25"}
                      </Button>
                      <Button variant="danger" className="px-2 py-1 text-xs" disabled={v.state === "deleting"} onClick={() => setToDelete(v)}>Delete</Button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {toDelete && (
        <ConfirmDialog title={`Delete ${toDelete.hostname}?`} confirmLabel="Delete VM" typeToConfirm={toDelete.hostname} busy={del.isPending} error={del.error} onConfirm={() => del.mutate(toDelete.id)} onCancel={() => setToDelete(null)}>
          <p>Owned by {toDelete.owner}. The VM and its disk are destroyed and the IP is released. This cannot be undone.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- capacity

function Gauge({ label, used, total, unit, detail }: { label: string; used: number; total: number | null; unit: string; detail?: string }) {
  const frac = total ? used / total : 0;
  return (
    <Card className="space-y-2">
      <div className="flex items-baseline justify-between">
        <h3 className="font-medium text-slate-900 dark:text-slate-50">{label}</h3>
        <span className="text-sm tabular-nums text-slate-600 dark:text-slate-300">{total ? `${(frac * 100).toFixed(0)}%` : "—"}</span>
      </div>
      <ProgressBar fraction={frac} />
      <p className="text-sm text-slate-500 dark:text-slate-400">{used.toLocaleString()} {unit} of {total !== null ? total.toLocaleString() : "?"} {unit}{detail ? ` · ${detail}` : ""}</p>
    </Card>
  );
}

export function AdminCapacity() {
  const cap = useQuery({ queryKey: ["admin", "capacity"], queryFn: () => api<Capacity>("/admin/capacity"), refetchInterval: 30_000 });
  if (cap.isLoading) return <Loading />;
  if (cap.error || !cap.data) return <ErrorText error={cap.error} />;
  const c = cap.data;
  return (
    <div className="space-y-4">
      {!c.host_reachable && <Banner tone="danger">The Proxmox host could not be reached, so physical capacity is unknown. Committed figures come from the database.</Banner>}
      <div className="grid gap-3 sm:grid-cols-2">
        <Gauge label="vCPU committed" used={c.vcpu.committed} total={c.vcpu.physical} unit="vCPU" detail={c.vcpu.physical ? `${(c.vcpu.committed / c.vcpu.physical).toFixed(1)}× overcommit` : undefined} />
        <Gauge label="RAM committed" used={c.ram_mb.committed} total={c.ram_mb.physical} unit="MB" />
        {c.pool && <Gauge label={`Disk pool “${c.pool.name}”`} used={Math.round(c.pool.used_bytes / 2 ** 30)} total={Math.round(c.pool.total_bytes / 2 ** 30)} unit="GiB" detail={bytesToGiB(c.pool.total_bytes - c.pool.used_bytes) + " free"} />}
        <Gauge label="IPv4 addresses in use" used={c.ips.total - c.ips.free} total={c.ips.total} unit="IPs" detail={`${c.ips.free} free`} />
      </div>
      <p className="text-xs text-slate-500 dark:text-slate-400">Thin provisioning means the pool can fill up even when every VM is within its plan. Alerts fire at 80% pool use and 90% committed RAM.</p>
    </div>
  );
}

// ---------------------------------------------------------------- jobs

export function AdminJobs() {
  const qc = useQueryClient();
  const jobs = useQuery({ queryKey: ["admin", "jobs"], queryFn: () => api<AdminJob[]>("/admin/jobs?status=failed"), refetchInterval: 15_000 });
  const retry = useMutation({ mutationFn: (id: number) => api(`/admin/jobs/${id}/retry`, { method: "POST" }), onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "jobs"] }) });
  return (
    <div className="space-y-4">
      <p className="text-sm text-slate-500 dark:text-slate-400">Jobs that failed after all retries. Retrying re-queues the job from scratch.</p>
      {jobs.isLoading && <Loading />}
      <ErrorText error={jobs.error ?? retry.error} />
      {jobs.data && jobs.data.length === 0 && <Empty title="No failed jobs" />}
      {jobs.data && jobs.data.length > 0 && (
        <div className={tableWrap}>
          <table className="w-full min-w-[40rem] text-sm">
            <thead className={thead}><tr><th className={th}>#</th><th className={th}>Kind</th><th className={th}>Payload</th><th className={th}>Error</th><th className={th} /></tr></thead>
            <tbody className={tbody}>
              {jobs.data.map((j) => (
                <tr key={j.id} className="align-top">
                  <td className="px-3 py-2 tabular-nums">{j.id}</td>
                  <td className="px-3 py-2 font-mono text-xs">{j.kind}</td>
                  <td className="px-3 py-2 font-mono text-xs">{JSON.stringify(j.payload)}</td>
                  <td className="max-w-md break-words px-3 py-2 text-red-700 dark:text-red-300">{j.last_error}<div className="text-xs text-slate-500">{j.attempts} attempts · {formatDate(j.created_at)}</div></td>
                  <td className="px-3 py-2"><Button variant="secondary" className="px-2 py-1 text-xs" disabled={retry.isPending} onClick={() => retry.mutate(j.id)}>Retry</Button></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- revenue

export function AdminRevenue() {
  const rev = useQuery({ queryKey: ["admin", "revenue"], queryFn: () => api<Revenue>("/admin/revenue?days=30") });
  if (rev.isLoading) return <Loading />;
  if (rev.error || !rev.data) return <ErrorText error={rev.error} />;
  const { days, fx, totals } = rev.data;
  const max = Math.max(1, ...days.map((d) => d.usage_uusdt));
  const rate = fx.rate_kobo_per_usdt ?? null;
  return (
    <div className="space-y-5">
      <div className="grid gap-3 sm:grid-cols-5">
        <Stat label="Operating wallet" value={rev.data.merchant_uusdt !== null ? formatUSDT(rev.data.merchant_uusdt) : "—"} sub="pays admin credits" />
        <Stat label="Naira converted (30d)" value={formatNaira(totals.converted_ngn_kobo)} />
        <Stat label="Credit sold (30d)" value={formatUSDT(totals.converted_uusdt)} />
        <Stat label="Usage billed (30d)" value={formatUSDT(totals.usage_uusdt)} sub={usdtToKobo(totals.usage_uusdt, rate) !== null ? `≈ ${formatNaira(usdtToKobo(totals.usage_uusdt, rate)!)}` : undefined} />
        <Stat label="FX rate" value={rate ? `₦${(rate / 100).toLocaleString()}` : "—"} sub={fx.quoting_paused ? "quoting paused" : `per USDT, read-only; managed in ${fx.managed_in}`} />
      </div>
      <div className={tableWrap}>
        <table className="w-full min-w-[36rem] text-sm">
          <thead className={thead}><tr><th className={th}>Day</th><th className={th}>Naira converted</th><th className={th}>Credit sold</th><th className={th}>Usage billed</th><th className={th} /></tr></thead>
          <tbody className={tbody}>
            {days.map((d) => (
              <tr key={d.date}>
                <td className="px-3 py-2">{d.date}</td>
                <td className="px-3 py-2 tabular-nums">{formatNaira(d.converted_ngn_kobo)}</td>
                <td className="px-3 py-2 tabular-nums">{formatUSDT(d.converted_uusdt)}</td>
                <td className="px-3 py-2 tabular-nums">{formatUSDT(d.usage_uusdt)}</td>
                <td className="w-32 px-3 py-2">
                  <svg viewBox="0 0 100 6" preserveAspectRatio="none" className="h-2 w-full" aria-hidden="true">
                    <rect width={(d.usage_uusdt / max) * 100} height="6" className="fill-indigo-500" />
                  </svg>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
