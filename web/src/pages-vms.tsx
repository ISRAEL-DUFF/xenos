import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, type Plan, type VM, type Wallet } from "./api";
import { useAuth } from "./auth";
import { formatDate, formatNaira, formatUSDT, koboForUSDT, memLabel, usdtToKobo } from "./format";
import { usePlans, useSSHKeys, useTemplates, useVM, useVMs, useWallet } from "./hooks";
import { BusyBanner, ResizeCard, SnapshotsCard, useSnapshots } from "./pages-vmops";
import { Banner, Button, Card, CodeLine, ConfirmDialog, Empty, ErrorText, Field, Input, Loading, PageHeader, StateBadge, cx } from "./ui";

const MIN_RUNWAY_HOURS = 24; // keep in step with the server's create check
const hostnameRe = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

const linkClass = "font-medium text-indigo-600 underline-offset-2 hover:underline dark:text-indigo-400";

/** Price shown in both currencies; naira is display only and omitted if iSpend cannot quote. */
function Price({ micro, rate, per }: { micro: number; rate: number | null | undefined; per?: string }) {
  const kobo = usdtToKobo(micro, rate);
  return (
    <span>
      {formatUSDT(micro)}
      {per}
      {kobo !== null && <span className="text-slate-500 dark:text-slate-400"> · ≈ {formatNaira(kobo)}{per}</span>}
    </span>
  );
}

// ---------------------------------------------------------------- list

export function VMList() {
  const vms = useVMs();
  const wallet = useWallet();
  const rate = wallet.data?.rate_kobo_per_usdt;
  return (
    <div>
      <PageHeader
        title="Virtual machines"
        actions={
          <Link to="/vms/new" className="rounded-lg bg-indigo-600 px-3.5 py-2 text-sm font-medium text-white hover:bg-indigo-500">
            Create VM
          </Link>
        }
      />
      {vms.isLoading && <Loading />}
      <ErrorText error={vms.error} />
      {vms.data && vms.data.length === 0 && (
        <Empty title="No VMs yet">
          <Link to="/vms/new" className={linkClass}>
            Create your first VM
          </Link>{" "}
          — it takes about a minute.
        </Empty>
      )}
      <ul className="space-y-3">
        {vms.data?.map((v) => (
          <li key={v.id}>
            <Link to={`/vms/${v.id}`} className="block rounded-xl border border-slate-200 bg-white p-4 shadow-sm transition hover:border-indigo-300 dark:border-slate-700 dark:bg-slate-800/60 dark:hover:border-indigo-500">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex items-center gap-2.5">
                  <span className="font-semibold text-slate-900 dark:text-slate-50">{v.hostname}</span>
                  <StateBadge state={v.state} />
                </div>
                <span className="text-sm capitalize text-slate-500 dark:text-slate-400">
                  {v.plan} · {v.template}
                </span>
              </div>
              <div className="mt-2 flex flex-wrap items-center justify-between gap-x-4 gap-y-1 text-sm">
                <span className="font-mono text-slate-700 dark:text-slate-200">{v.ipv4 ?? "—"}</span>
                <span className="text-slate-600 dark:text-slate-300">
                  <Price micro={v.price_uusdt_hourly} rate={rate} per="/hr" />
                </span>
              </div>
            </Link>
          </li>
        ))}
      </ul>
    </div>
  );
}

// ---------------------------------------------------------------- create

/** Why creating this plan is blocked right now, or null if it is allowed. Mirrors the server's checks. */
export function createBlock(args: {
  plan: Plan | undefined;
  wallet: Wallet | undefined;
  vms: VM[] | undefined;
  vmLimit: number;
  accountStatus: string;
  keyCount: number;
}): { reason: string; action?: { label: string; to: string } } | null {
  const { plan, wallet, vms, vmLimit, accountStatus, keyCount } = args;
  if (accountStatus !== "active") return { reason: "Your account is suspended, so new VMs are disabled. Contact support." };
  if (keyCount === 0) return { reason: "Add an SSH key first so you can log in.", action: { label: "Add an SSH key", to: "/ssh-keys" } };
  if (vms && vms.filter((v) => v.state !== "error").length >= vmLimit) {
    return { reason: `You have reached your limit of ${vmLimit} VM${vmLimit === 1 ? "" : "s"}. Delete one, or contact support to raise it.` };
  }
  if (plan && wallet) {
    const need = (wallet.hourly_uusdt + plan.price_uusdt_hourly) * MIN_RUNWAY_HOURS;
    if (wallet.usdt_uusdt < need) {
      const short = need - wallet.usdt_uusdt;
      const kobo = wallet.rate_kobo_per_usdt ? koboForUSDT(short, wallet.rate_kobo_per_usdt) : null;
      return {
        reason: `Your wallet must cover ${MIN_RUNWAY_HOURS} hours of usage for all your VMs. You need ${formatUSDT(short)} more${kobo !== null ? ` (about ${formatNaira(kobo)})` : ""}.`,
        action: { label: "Top up wallet", to: "/wallet" },
      };
    }
  }
  return null;
}

export function VMCreate() {
  const nav = useNavigate();
  const qc = useQueryClient();
  const { user } = useAuth();
  const plans = usePlans();
  const templates = useTemplates();
  const keys = useSSHKeys();
  const wallet = useWallet();
  const vms = useVMs();

  const [planSlug, setPlanSlug] = useState("");
  const [templateSlug, setTemplateSlug] = useState("");
  const [keyIds, setKeyIds] = useState<number[]>([]);
  const [hostname, setHostname] = useState("");

  useEffect(() => {
    if (!planSlug && plans.data?.[0]) setPlanSlug(plans.data[0].slug);
  }, [plans.data, planSlug]);
  useEffect(() => {
    if (!templateSlug && templates.data?.[0]) setTemplateSlug(templates.data[0].slug);
  }, [templates.data, templateSlug]);
  useEffect(() => {
    if (keys.data && keyIds.length === 0 && keys.data.length > 0) setKeyIds([keys.data[0].id]);
  }, [keys.data]);

  const plan = plans.data?.find((p) => p.slug === planSlug);
  const rate = wallet.data?.rate_kobo_per_usdt;
  const host = hostname.trim().toLowerCase();
  const hostnameBad = host !== "" && !hostnameRe.test(host);

  const block = useMemo(
    () =>
      createBlock({
        plan,
        wallet: wallet.data,
        vms: vms.data,
        vmLimit: user?.vm_limit ?? 0,
        accountStatus: user?.status ?? "active",
        keyCount: keys.data?.length ?? 0,
      }),
    [plan, wallet.data, vms.data, user, keys.data],
  );

  const create = useMutation({
    mutationFn: () => api<VM>("/vms", { json: { plan: planSlug, template: templateSlug, hostname: host, ssh_key_ids: keyIds } }),
    onSuccess: (vm) => {
      qc.invalidateQueries({ queryKey: ["vms"] });
      qc.invalidateQueries({ queryKey: ["wallet"] });
      nav(`/vms/${vm.id}`);
    },
  });

  const loading = plans.isLoading || templates.isLoading || keys.isLoading || wallet.isLoading || vms.isLoading;
  const reason = block?.reason ?? (keyIds.length === 0 ? "Choose at least one SSH key." : hostnameBad ? "Fix the hostname below." : null);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!reason) create.mutate();
  };

  if (loading) return <Loading />;

  return (
    <form onSubmit={submit} className="space-y-6">
      <PageHeader title="Create a VM" subtitle="Billed hourly from your wallet. Stopped VMs still bill, because they keep their disk and IP." />

      <section aria-labelledby="plan-h" className="space-y-3">
        <h2 id="plan-h" className="font-medium text-slate-900 dark:text-slate-50">Plan</h2>
        <div className="grid gap-3 sm:grid-cols-3">
          {plans.data?.map((p) => {
            const selected = p.slug === planSlug;
            return (
              <label
                key={p.id}
                className={cx(
                  "cursor-pointer rounded-xl border-2 p-4 transition",
                  selected ? "border-indigo-500 bg-indigo-50/60 dark:bg-indigo-950/40" : "border-slate-200 bg-white hover:border-slate-300 dark:border-slate-700 dark:bg-slate-800/60",
                )}
              >
                <input type="radio" name="plan" className="sr-only" checked={selected} onChange={() => setPlanSlug(p.slug)} />
                <span className="block text-base font-semibold capitalize text-slate-900 dark:text-slate-50">{p.slug}</span>
                <span className="mt-1 block text-sm text-slate-600 dark:text-slate-300">
                  {p.vcpu} vCPU · {memLabel(p.ram_mb)} RAM · {p.disk_gb} GB disk
                </span>
                <span className="mt-3 block text-sm font-medium text-slate-900 dark:text-slate-50">
                  <Price micro={p.price_uusdt_hourly} rate={rate} per="/hr" />
                </span>
                <span className="block text-xs text-slate-500 dark:text-slate-400">
                  at most <Price micro={p.price_uusdt_monthly_cap} rate={rate} per="/month" />
                </span>
              </label>
            );
          })}
        </div>
      </section>

      <section aria-labelledby="os-h" className="space-y-3">
        <h2 id="os-h" className="font-medium text-slate-900 dark:text-slate-50">Operating system</h2>
        <div className="flex flex-wrap gap-3">
          {templates.data?.map((t) => (
            <label
              key={t.id}
              className={cx(
                "cursor-pointer rounded-lg border-2 px-4 py-2.5 text-sm font-medium",
                t.slug === templateSlug ? "border-indigo-500 bg-indigo-50/60 dark:bg-indigo-950/40" : "border-slate-200 bg-white dark:border-slate-700 dark:bg-slate-800/60",
              )}
            >
              <input type="radio" name="template" className="sr-only" checked={t.slug === templateSlug} onChange={() => setTemplateSlug(t.slug)} />
              {t.name}
            </label>
          ))}
        </div>
      </section>

      <section aria-labelledby="keys-h" className="space-y-3">
        <h2 id="keys-h" className="font-medium text-slate-900 dark:text-slate-50">SSH keys</h2>
        {keys.data?.length === 0 ? (
          <p className="text-sm text-slate-600 dark:text-slate-300">
            You have no SSH keys yet.{" "}
            <Link to="/ssh-keys" className={linkClass}>
              Add one
            </Link>
            .
          </p>
        ) : (
          <ul className="space-y-2">
            {keys.data?.map((k) => (
              <li key={k.id}>
                <label className="flex cursor-pointer items-center gap-3 rounded-lg border border-slate-200 bg-white px-3 py-2.5 dark:border-slate-700 dark:bg-slate-800/60">
                  <input
                    type="checkbox"
                    className="h-4 w-4"
                    checked={keyIds.includes(k.id)}
                    onChange={(e) => setKeyIds((ids) => (e.target.checked ? [...ids, k.id] : ids.filter((i) => i !== k.id)))}
                  />
                  <span className="min-w-0">
                    <span className="block text-sm font-medium text-slate-900 dark:text-slate-50">{k.name}</span>
                    <span className="block truncate font-mono text-xs text-slate-500 dark:text-slate-400">{k.fingerprint}</span>
                  </span>
                </label>
              </li>
            ))}
          </ul>
        )}
      </section>

      <Field label="Hostname (optional)" hint="Lowercase letters, digits and hyphens. We generate one if you leave it empty.">
        <Input value={hostname} onChange={(e) => setHostname(e.target.value)} placeholder="e.g. web-1" maxLength={63} aria-invalid={hostnameBad} />
      </Field>
      {hostnameBad && <p role="alert" className="text-sm text-red-600 dark:text-red-400">Hostnames use lowercase letters, digits and hyphens, and cannot start or end with a hyphen.</p>}

      {reason && (
        <Banner tone="warn">
          {reason}{" "}
          {block?.action && (
            <Link to={block.action.to} className="font-semibold underline">
              {block.action.label}
            </Link>
          )}
        </Banner>
      )}
      <ErrorText error={create.error} />
      <Button type="submit" disabled={!!reason || create.isPending} className="w-full sm:w-auto">
        {create.isPending ? "Creating…" : "Create VM"}
      </Button>
    </form>
  );
}

// ---------------------------------------------------------------- detail

const stateNotes: Partial<Record<VM["state"], { tone: "info" | "warn" | "danger"; text: string }>> = {
  pending: { tone: "info", text: "Queued. Your VM will start provisioning in a moment." },
  provisioning: { tone: "info", text: "Provisioning. This usually takes under two minutes; this page updates by itself." },
  suspended: { tone: "warn", text: "This VM was suspended because your wallet ran out. Top up to bring it back, otherwise it will be deleted after the grace period." },
  deleting: { tone: "warn", text: "This VM is being deleted." },
  error: { tone: "danger", text: "Provisioning failed. You were not charged. Delete this VM and try again; contact support if it keeps happening." },
};

export function VMDetail() {
  const { id } = useParams();
  const nav = useNavigate();
  const qc = useQueryClient();
  const vmq = useVM(id);
  const snaps = useSnapshots(Number(id));
  const wallet = useWallet();
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [sent, setSent] = useState("");

  const act = useMutation({
    mutationFn: (action: "start" | "stop" | "reboot") => api(`/vms/${id}/${action}`, { method: "POST" }),
    onSuccess: (_d, action) => {
      setSent(action);
      qc.invalidateQueries({ queryKey: ["vm", id] });
    },
  });
  const del = useMutation({
    mutationFn: () => api(`/vms/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["vms"] });
      nav("/vms");
    },
  });

  // A power request is applied by the worker; clear the notice once the state moves on.
  const state = vmq.data?.state;
  useEffect(() => setSent(""), [state]);

  if (vmq.isLoading) return <Loading />;
  if (vmq.error || !vmq.data) {
    return (
      <div>
        <ErrorText error={vmq.error ?? new Error("VM not found")} />
        <Link to="/vms" className={linkClass}>
          Back to your VMs
        </Link>
      </div>
    );
  }
  const v = vmq.data;
  const rate = wallet.data?.rate_kobo_per_usdt;
  const note = stateNotes[v.state];
  const busy = act.isPending || sent !== "" || !!v.busy;

  return (
    <div className="space-y-5">
      <PageHeader
        title={v.hostname}
        subtitle={`${v.plan} · ${v.template} · ${v.region}`}
        actions={<StateBadge state={v.state} />}
      />
      {note && (
        <Banner tone={note.tone}>
          {note.text}{" "}
          {v.state === "suspended" && (
            <Link to="/wallet" className="font-semibold underline">
              Go to wallet
            </Link>
          )}
        </Banner>
      )}

      <BusyBanner vm={v} />

      <Card className="space-y-3">
        <h2 className="font-medium text-slate-900 dark:text-slate-50">Connect</h2>
        {v.ssh_command && v.state !== "pending" && v.state !== "provisioning" ? (
          <CodeLine text={v.ssh_command} />
        ) : (
          <p className="text-sm text-slate-500 dark:text-slate-400">The connection command appears once the VM is running.</p>
        )}
        <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
          <div>
            <dt className="text-slate-500 dark:text-slate-400">IPv4</dt>
            <dd className="font-mono text-slate-900 dark:text-slate-50">{v.ipv4 ?? "—"}</dd>
          </div>
          <div>
            <dt className="text-slate-500 dark:text-slate-400">IPv6</dt>
            <dd className="break-all font-mono text-slate-900 dark:text-slate-50">{v.ipv6 ?? "—"}</dd>
          </div>
          <div>
            <dt className="text-slate-500 dark:text-slate-400">Cost per hour</dt>
            <dd className="text-slate-900 dark:text-slate-50"><Price micro={v.price_uusdt_hourly} rate={rate} /></dd>
          </div>
          <div>
            <dt className="text-slate-500 dark:text-slate-400">Cost this month</dt>
            <dd className="text-slate-900 dark:text-slate-50">
              {v.month_cost_uusdt !== undefined ? <Price micro={v.month_cost_uusdt} rate={rate} /> : "—"}
            </dd>
          </div>
          <div>
            <dt className="text-slate-500 dark:text-slate-400">Created</dt>
            <dd className="text-slate-900 dark:text-slate-50">{formatDate(v.created_at)}</dd>
          </div>
        </dl>
      </Card>

      <Card className="space-y-3">
        <h2 className="font-medium text-slate-900 dark:text-slate-50">Actions</h2>
        <div className="flex flex-wrap gap-2">
          <Button variant="secondary" disabled={v.state !== "stopped" || busy} onClick={() => act.mutate("start")}>
            Start
          </Button>
          <Button variant="secondary" disabled={v.state !== "running" || busy} onClick={() => act.mutate("stop")}>
            Stop
          </Button>
          <Button variant="secondary" disabled={v.state !== "running" || busy} onClick={() => act.mutate("reboot")}>
            Reboot
          </Button>
          <Link
            to={`/vms/${v.id}/console`}
            className={cx(
              "inline-flex items-center rounded-lg border border-slate-300 px-3 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 dark:border-slate-600 dark:text-slate-200 dark:hover:bg-slate-800",
              (v.state !== "running" || busy) && "pointer-events-none opacity-50",
            )}
            aria-disabled={v.state !== "running" || busy}
          >
            Console
          </Link>
          <Button variant="danger" className="sm:ml-auto" disabled={v.state === "deleting"} onClick={() => setConfirmDelete(true)}>
            Delete
          </Button>
        </div>
        {sent && <p className="text-sm text-slate-500 dark:text-slate-400" role="status">Request sent. The state updates in a few seconds.</p>}
        <ErrorText error={act.error} />
      </Card>

      <SnapshotsCard vm={v} list={snaps.data} />
      <ResizeCard vm={v} snapshotCount={snaps.data?.snapshots.length ?? 0} />

      {confirmDelete && (
        <ConfirmDialog
          title={`Delete ${v.hostname}?`}
          confirmLabel="Delete VM"
          typeToConfirm={v.hostname}
          busy={del.isPending}
          error={del.error}
          onConfirm={() => del.mutate()}
          onCancel={() => setConfirmDelete(false)}
        >
          <p>This destroys the VM and its disk permanently and releases its IP address. Billing stops after the current hour.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}
