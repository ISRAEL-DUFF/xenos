import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type AdminPlan, type AdminTemplate, type PriceImpact } from "./api";
import { formatUSDT, memLabel, parseUSDT } from "./format";
import { Badge, Banner, Button, Card, ConfirmDialog, ErrorText, Field, Input, Loading } from "./ui";

const tableWrap = "overflow-x-auto rounded-xl border border-slate-200 dark:border-slate-700";
const th = "px-3 py-2";
const thead = "bg-slate-100 text-left text-xs uppercase text-slate-500 dark:bg-slate-800 dark:text-slate-400";
const tbody = "divide-y divide-slate-200 dark:divide-slate-700";

/** Admin → Catalogue: the plans and templates customers can choose. Changes are audited. */
export function AdminCatalogue() {
  return (
    <div className="space-y-6">
      <Plans />
      <Templates />
    </div>
  );
}

function Plans() {
  const qc = useQueryClient();
  const plans = useQuery({ queryKey: ["admin", "plans"], queryFn: () => api<AdminPlan[]>("/admin/plans") });
  const refresh = () => qc.invalidateQueries({ queryKey: ["admin", "plans"] });
  const [form, setForm] = useState({ slug: "", vcpu: "2", ram: "4096", disk: "40", price: "" });
  const [pricing, setPricing] = useState<AdminPlan | null>(null);

  const add = useMutation({
    mutationFn: () =>
      api("/admin/plans", {
        json: {
          slug: form.slug,
          vcpu: Number(form.vcpu),
          ram_mb: Number(form.ram),
          disk_gb: Number(form.disk),
          price_uusdt_hourly: parseUSDT(form.price) ?? 0,
        },
      }),
    onSuccess: () => {
      setForm({ ...form, slug: "", price: "" });
      refresh();
    },
  });
  const toggle = useMutation({
    mutationFn: (p: AdminPlan) => api(`/admin/plans/${p.slug}/active`, { json: { active: !p.active } }),
    onSuccess: refresh,
  });
  const onAdd = (e: FormEvent) => {
    e.preventDefault();
    add.mutate();
  };
  const set = (k: keyof typeof form) => (e: { target: { value: string } }) => setForm({ ...form, [k]: e.target.value });

  return (
    <Card className="space-y-4">
      <h2 className="font-medium text-slate-900 dark:text-slate-50">Plans</h2>
      {plans.isLoading ? (
        <Loading />
      ) : (
        <div className={tableWrap}>
          <table className="w-full text-sm">
            <thead className={thead}>
              <tr>
                <th className={th}>Plan</th>
                <th className={th}>Size</th>
                <th className={th}>Per hour</th>
                <th className={th}>Monthly cap</th>
                <th className={th}>VMs</th>
                <th className={th}>State</th>
                <th className={th} />
              </tr>
            </thead>
            <tbody className={tbody}>
              {(plans.data ?? []).map((p) => (
                <tr key={p.id}>
                  <td className={th + " font-medium"}>{p.slug}</td>
                  <td className={th}>
                    {p.vcpu} vCPU, {memLabel(p.ram_mb)}, {p.disk_gb} GB
                  </td>
                  <td className={th + " tabular-nums"}>{formatUSDT(p.price_uusdt_hourly)}</td>
                  <td className={th + " tabular-nums"}>{formatUSDT(p.price_uusdt_monthly_cap)}</td>
                  <td className={th}>{p.vm_count}</td>
                  <td className={th}>
                    <Badge tone={p.active ? "green" : "slate"}>{p.active ? "active" : "disabled"}</Badge>
                  </td>
                  <td className={th + " whitespace-nowrap text-right"}>
                    <Button variant="ghost" onClick={() => setPricing(p)}>
                      Change price
                    </Button>
                    <Button variant="ghost" disabled={toggle.isPending} onClick={() => toggle.mutate(p)}>
                      {p.active ? "Disable" : "Enable"}
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <p className="text-sm text-slate-500 dark:text-slate-400">
        A plan&apos;s size cannot be edited: add a new plan instead. Disabling a plan only stops new VMs and resizes into it; VMs already on it keep running and billing.
      </p>
      <form onSubmit={onAdd} className="grid gap-3 sm:grid-cols-6">
        <Field label="Slug">
          <Input value={form.slug} onChange={set("slug")} placeholder="d-small" required />
        </Field>
        <Field label="vCPU">
          <Input type="number" min={1} value={form.vcpu} onChange={set("vcpu")} required />
        </Field>
        <Field label="Memory (MB)">
          <Input type="number" min={256} value={form.ram} onChange={set("ram")} required />
        </Field>
        <Field label="Disk (GB)">
          <Input type="number" min={1} value={form.disk} onChange={set("disk")} required />
        </Field>
        <Field label="USDT per hour">
          <Input value={form.price} onChange={set("price")} placeholder="0.048" required />
        </Field>
        <div className="flex items-end">
          <Button disabled={add.isPending}>Add plan</Button>
        </div>
      </form>
      <ErrorText error={add.error ?? toggle.error ?? plans.error} />
      {pricing && <PriceDialog plan={pricing} onClose={() => setPricing(null)} onDone={refresh} />}
    </Card>
  );
}

/** Two steps: preview what the change touches, then apply it. */
function PriceDialog({ plan, onClose, onDone }: { plan: AdminPlan; onClose: () => void; onDone: () => void }) {
  const [price, setPrice] = useState((plan.price_uusdt_hourly / 1_000_000).toString());
  const [impact, setImpact] = useState<PriceImpact | null>(null);
  const call = (confirm: boolean) =>
    api<PriceImpact>(`/admin/plans/${plan.slug}/price`, { json: { price_uusdt_hourly: parseUSDT(price) ?? 0, confirm } });
  const preview = useMutation({ mutationFn: () => call(false), onSuccess: setImpact });
  const apply = useMutation({
    mutationFn: () => call(true),
    onSuccess: () => {
      onDone();
      onClose();
    },
  });

  return (
    <ConfirmDialog
      title={`Change the price of ${plan.slug}`}
      confirmLabel={impact ? "Apply price" : "Preview"}
      busy={preview.isPending || apply.isPending}
      error={preview.error ?? apply.error}
      onConfirm={() => (impact ? apply.mutate() : preview.mutate())}
      onCancel={onClose}
    >
      <Field label="New price (USDT per hour)">
        <Input
          value={price}
          onChange={(e) => {
            setPrice(e.target.value);
            setImpact(null);
          }}
        />
      </Field>
      {impact && (
        <Banner tone={impact.billing_vms > 0 ? "warn" : "info"}>
          {formatUSDT(impact.old_hourly_uusdt)} → {formatUSDT(impact.new_hourly_uusdt)} per hour. {impact.billing_vms} VM(s) are billing on this plan and pay the new price from
          their next charged hour; if all ran a full month that is {impact.monthly_delta_uusdt >= 0 ? "+" : "−"}
          {formatUSDT(Math.abs(impact.monthly_delta_uusdt))} in total. Tell affected customers first.
        </Banner>
      )}
    </ConfirmDialog>
  );
}

function Templates() {
  const qc = useQueryClient();
  const tpls = useQuery({ queryKey: ["admin", "templates"], queryFn: () => api<AdminTemplate[]>("/admin/templates") });
  const refresh = () => qc.invalidateQueries({ queryKey: ["admin", "templates"] });
  const [form, setForm] = useState({ slug: "", name: "", vmid: "", user: "root", skip: false });
  const add = useMutation({
    mutationFn: () =>
      api("/admin/templates", {
        json: { slug: form.slug, name: form.name, proxmox_template_id: Number(form.vmid), ci_user: form.user, skip_host_check: form.skip },
      }),
    onSuccess: () => {
      setForm({ ...form, slug: "", name: "", vmid: "" });
      refresh();
    },
  });
  const toggle = useMutation({
    mutationFn: (t: AdminTemplate) => api(`/admin/templates/${t.slug}/active`, { json: { active: !t.active } }),
    onSuccess: refresh,
  });
  const onAdd = (e: FormEvent) => {
    e.preventDefault();
    add.mutate();
  };
  const set = (k: "slug" | "name" | "vmid" | "user") => (e: { target: { value: string } }) => setForm({ ...form, [k]: e.target.value });

  return (
    <Card className="space-y-4">
      <h2 className="font-medium text-slate-900 dark:text-slate-50">Templates</h2>
      {tpls.isLoading ? (
        <Loading />
      ) : (
        <div className={tableWrap}>
          <table className="w-full text-sm">
            <thead className={thead}>
              <tr>
                <th className={th}>Template</th>
                <th className={th}>Name</th>
                <th className={th}>Proxmox VMID</th>
                <th className={th}>Login user</th>
                <th className={th}>VMs</th>
                <th className={th}>State</th>
                <th className={th} />
              </tr>
            </thead>
            <tbody className={tbody}>
              {(tpls.data ?? []).map((t) => (
                <tr key={t.id}>
                  <td className={th + " font-medium"}>{t.slug}</td>
                  <td className={th}>{t.name}</td>
                  <td className={th + " tabular-nums"}>{t.proxmox_template_id}</td>
                  <td className={th}>{t.ci_user}</td>
                  <td className={th}>{t.vm_count}</td>
                  <td className={th}>
                    <Badge tone={t.active ? "green" : "slate"}>{t.active ? "active" : "disabled"}</Badge>
                  </td>
                  <td className={th + " text-right"}>
                    <Button variant="ghost" disabled={toggle.isPending} onClick={() => toggle.mutate(t)}>
                      {t.active ? "Disable" : "Enable"}
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <form onSubmit={onAdd} className="grid gap-3 sm:grid-cols-6">
        <Field label="Slug">
          <Input value={form.slug} onChange={set("slug")} placeholder="alma-9" required />
        </Field>
        <Field label="Name">
          <Input value={form.name} onChange={set("name")} placeholder="AlmaLinux 9" required />
        </Field>
        <Field label="Proxmox VMID">
          <Input type="number" min={100} value={form.vmid} onChange={set("vmid")} required />
        </Field>
        <Field label="Login user">
          <Input value={form.user} onChange={set("user")} required />
        </Field>
        <label className="flex items-end gap-2 pb-2 text-sm text-slate-600 dark:text-slate-300">
          <input type="checkbox" checked={form.skip} onChange={(e) => setForm({ ...form, skip: e.target.checked })} />
          Skip host check
        </label>
        <div className="flex items-end">
          <Button disabled={add.isPending}>Add template</Button>
        </div>
      </form>
      <ErrorText error={add.error ?? toggle.error ?? tpls.error} />
    </Card>
  );
}
