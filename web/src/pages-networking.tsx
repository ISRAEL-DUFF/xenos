import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type FloatingIP } from "./api";
import { formatUSDT } from "./format";
import { useVMs } from "./hooks";
import { Badge, Banner, Button, Card, ConfirmDialog, Empty, ErrorText, Field, Input, Loading, PageHeader, inputClass } from "./ui";

interface FloatingList {
  floating_ips: FloatingIP[];
  limit: number;
  price_uusdt_hourly: number;
}

/** Networking: floating IPs an account holds and points at its VMs. */
export function NetworkingPage() {
  const qc = useQueryClient();
  const list = useQuery({ queryKey: ["floating-ips"], queryFn: () => api<FloatingList>("/floating-ips"), refetchInterval: 5000 });
  const vms = useVMs();
  const [label, setLabel] = useState("");
  const [toRelease, setToRelease] = useState<FloatingIP | null>(null);
  const refresh = () => qc.invalidateQueries({ queryKey: ["floating-ips"] });

  const allocate = useMutation({
    mutationFn: () => api<FloatingIP>("/floating-ips", { json: { label } }),
    onSuccess: () => {
      setLabel("");
      refresh();
    },
  });
  const attach = useMutation({
    mutationFn: ({ id, vm }: { id: number; vm: number }) => api(`/floating-ips/${id}/attach`, { json: { vm_id: vm } }),
    onSuccess: refresh,
  });
  const detach = useMutation({ mutationFn: (id: number) => api(`/floating-ips/${id}/detach`, { method: "POST" }), onSuccess: refresh });
  const release = useMutation({
    mutationFn: (id: number) => api(`/floating-ips/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      setToRelease(null);
      refresh();
    },
  });

  if (list.isLoading) return <Loading />;
  const data = list.data;
  const ips = data?.floating_ips ?? [];
  const usable = (vms.data ?? []).filter((v) => v.state === "running" || v.state === "stopped");
  const nameOf = (id: number | null) => (vms.data ?? []).find((v) => v.id === id)?.hostname ?? (id ? `VM ${id}` : "");

  return (
    <div className="space-y-5">
      <PageHeader title="Networking" subtitle="Floating IPs stay yours when a VM goes away: point one at another VM to fail over. Each costs the same whether or not it is attached." />
      <ErrorText error={list.error ?? attach.error ?? detach.error} />
      {ips.length === 0 ? (
        <Empty title="No floating IPs">Allocate one to get an address you can move between your VMs.</Empty>
      ) : (
        <Card>
          <ul className="divide-y divide-slate-200 dark:divide-slate-700">
            {ips.map((f) => (
              <li key={f.id} className="flex flex-wrap items-center gap-3 py-3">
                <div className="min-w-0 flex-1">
                  <div className="font-mono text-sm font-medium text-slate-900 dark:text-slate-50">{f.address}</div>
                  <div className="text-xs text-slate-500 dark:text-slate-400">
                    {f.label || "no label"} · {formatUSDT(f.price_uusdt_hourly)} per hour
                  </div>
                </div>
                {f.vm_id ? (
                  <Badge tone={f.applied ? "green" : "amber"}>{f.applied ? `on ${nameOf(f.vm_id)}` : `moving to ${nameOf(f.vm_id)}…`}</Badge>
                ) : (
                  <Badge tone="slate">not attached</Badge>
                )}
                <select
                  aria-label={`Attach ${f.address} to`}
                  className={inputClass + " w-auto"}
                  value=""
                  onChange={(e) => e.target.value && attach.mutate({ id: f.id, vm: Number(e.target.value) })}
                >
                  <option value="">{f.vm_id ? "Move to…" : "Attach to…"}</option>
                  {usable
                    .filter((v) => v.id !== f.vm_id && v.region === f.region)
                    .map((v) => (
                      <option key={v.id} value={v.id}>
                        {v.hostname}
                      </option>
                    ))}
                </select>
                {f.vm_id && (
                  <Button variant="secondary" onClick={() => detach.mutate(f.id)} disabled={detach.isPending}>
                    Detach
                  </Button>
                )}
                <Button variant="danger" onClick={() => setToRelease(f)}>
                  Release
                </Button>
              </li>
            ))}
          </ul>
        </Card>
      )}

      <Card>
        <form
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault();
            allocate.mutate();
          }}
        >
          <h2 className="font-medium text-slate-900 dark:text-slate-50">Allocate a floating IP</h2>
          <Field label="Label (optional)" hint={`You can hold up to ${data?.limit ?? 0}. Billed ${formatUSDT(data?.price_uusdt_hourly ?? 0)} per hour from now until you release it.`}>
            <Input value={label} maxLength={64} onChange={(e) => setLabel(e.target.value)} />
          </Field>
          <ErrorText error={allocate.error} />
          <Button disabled={allocate.isPending || ips.length >= (data?.limit ?? 0)}>{allocate.isPending ? "Allocating…" : "Allocate"}</Button>
        </form>
      </Card>

      <Banner tone="info">
        The address is added inside your VM through the guest agent and survives restarts. It works between VMs in the same region and needs <code>iproute2</code> in the guest.
      </Banner>

      {toRelease && (
        <ConfirmDialog title="Release this floating IP?" confirmLabel="Release" busy={release.isPending} error={release.error} onCancel={() => setToRelease(null)} onConfirm={() => release.mutate(toRelease.id)}>
          <p>
            {toRelease.address} goes back to the pool and billing for it stops. Anything pointing at it will stop working, and you may not get the same address again.
          </p>
        </ConfirmDialog>
      )}
    </div>
  );
}
