import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Plan, type VM } from "./api";
import { formatDate, formatUSDT, memLabel } from "./format";
import { usePlans } from "./hooks";
import { Banner, Button, Card, ConfirmDialog, ErrorText, Input } from "./ui";

const busyText: Record<string, string> = {
  resizing: "Resizing. The VM restarts when it finishes; this page updates by itself.",
  snapshotting: "Working on a snapshot. This page updates by itself.",
  restoring: "Restoring a snapshot. The VM restarts when it finishes; this page updates by itself.",
};

/** What the worker is doing to the VM right now, if anything. */
export function BusyBanner({ vm }: { vm: VM }) {
  if (!vm.busy) return null;
  return <Banner tone="info">{busyText[vm.busy] ?? "Working on this VM."}</Banner>;
}

const larger = (p: Plan, cur: Plan) =>
  p.vcpu >= cur.vcpu && p.ram_mb >= cur.ram_mb && p.disk_gb >= cur.disk_gb && p.price_uusdt_hourly > cur.price_uusdt_hourly;

export function ResizeCard({ vm, snapshotCount }: { vm: VM; snapshotCount: number }) {
  const qc = useQueryClient();
  const plans = usePlans();
  const [target, setTarget] = useState<Plan | null>(null);
  const resize = useMutation({
    mutationFn: (plan: string) => api(`/vms/${vm.id}/resize`, { json: { plan } }),
    onSuccess: () => {
      setTarget(null);
      qc.invalidateQueries({ queryKey: ["vm", String(vm.id)] });
    },
  });
  const current = plans.data?.find((p) => p.slug === vm.plan);
  if (!current) return null;
  const options = (plans.data ?? []).filter((p) => larger(p, current));
  const settled = (vm.state === "running" || vm.state === "stopped") && !vm.busy;

  return (
    <Card className="space-y-3">
      <h2 className="font-medium text-slate-900 dark:text-slate-50">Resize</h2>
      <p className="text-sm text-slate-500 dark:text-slate-400">
        Move to a bigger plan. You can only go up: the disk cannot shrink. {vm.state === "running" ? "The VM restarts, which takes about a minute. " : ""}
        The new price applies from the next hour that is charged.
      </p>
      {options.length === 0 ? (
        <p className="text-sm text-slate-500 dark:text-slate-400">This is the largest plan.</p>
      ) : snapshotCount > 0 ? (
        <p className="text-sm text-slate-500 dark:text-slate-400">Delete this VM&apos;s snapshots first: a disk with snapshots cannot be grown safely.</p>
      ) : (
        <div className="flex flex-wrap gap-2">
          {options.map((p) => (
            <Button key={p.slug} variant="secondary" disabled={!settled} onClick={() => setTarget(p)}>
              {p.slug}: {p.vcpu} vCPU, {memLabel(p.ram_mb)}, {p.disk_gb} GB · {formatUSDT(p.price_uusdt_hourly)}/h
            </Button>
          ))}
        </div>
      )}
      {target && (
        <ConfirmDialog
          title={`Resize to ${target.slug}?`}
          confirmLabel="Resize"
          busy={resize.isPending}
          error={resize.error}
          onConfirm={() => resize.mutate(target.slug)}
          onCancel={() => setTarget(null)}
        >
          <p>
            {target.vcpu} vCPU, {memLabel(target.ram_mb)} memory and a {target.disk_gb} GB disk, at {formatUSDT(target.price_uusdt_hourly)} USDT per hour.
            {vm.state === "running" && " The VM is stopped and started again."} This cannot be undone: a bigger disk cannot be made smaller.
          </p>
        </ConfirmDialog>
      )}
    </Card>
  );
}

interface Snapshot {
  id: number;
  name: string;
  status: "creating" | "ready" | "deleting" | "error";
  created_at: string;
}
interface SnapshotList {
  snapshots: Snapshot[];
  limit: number;
}

export function useSnapshots(vmId: number) {
  return useQuery({
    queryKey: ["snapshots", String(vmId)],
    queryFn: () => api<SnapshotList>(`/vms/${vmId}/snapshots`),
    refetchInterval: (q) => (q.state.data?.snapshots.some((s) => s.status !== "ready") ? 3000 : 15000),
  });
}

export function SnapshotsCard({ vm, list }: { vm: VM; list: SnapshotList | undefined }) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [restoring, setRestoring] = useState<Snapshot | null>(null);
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["snapshots", String(vm.id)] });
    qc.invalidateQueries({ queryKey: ["vm", String(vm.id)] });
  };
  const create = useMutation({
    mutationFn: () => api(`/vms/${vm.id}/snapshots`, { json: { name } }),
    onSuccess: () => {
      setName("");
      refresh();
    },
  });
  const remove = useMutation({ mutationFn: (s: Snapshot) => api(`/vms/${vm.id}/snapshots/${s.id}`, { method: "DELETE" }), onSuccess: refresh });
  const restore = useMutation({
    mutationFn: (s: Snapshot) => api(`/vms/${vm.id}/snapshots/${s.id}/restore`, { json: { confirm: true } }),
    onSuccess: () => {
      setRestoring(null);
      refresh();
    },
  });

  const snaps = list?.snapshots ?? [];
  const limit = list?.limit ?? 2;
  const settled = (vm.state === "running" || vm.state === "stopped") && !vm.busy;
  const full = snaps.length >= limit;
  const onCreate = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };

  return (
    <Card className="space-y-3">
      <h2 className="font-medium text-slate-900 dark:text-slate-50">Snapshots</h2>
      <p className="text-sm text-slate-500 dark:text-slate-400">
        A snapshot saves the disk as it is now, so you can go back to it. You can keep {limit} per VM. Snapshots are not backups: they live on the same server as the VM.
      </p>
      {snaps.length > 0 && (
        <ul className="divide-y divide-slate-200 text-sm dark:divide-slate-700">
          {snaps.map((s) => (
            <li key={s.id} className="flex flex-wrap items-center gap-2 py-2">
              <span className="font-medium text-slate-900 dark:text-slate-50">{s.name}</span>
              <span className="text-slate-500 dark:text-slate-400">
                {s.status === "ready" ? formatDate(s.created_at) : s.status === "creating" ? "creating…" : "deleting…"}
              </span>
              <span className="ml-auto flex gap-2">
                <Button variant="secondary" disabled={!settled || s.status !== "ready"} onClick={() => setRestoring(s)}>
                  Restore
                </Button>
                <Button variant="ghost" disabled={!settled || s.status !== "ready" || remove.isPending} onClick={() => remove.mutate(s)}>
                  Delete
                </Button>
              </span>
            </li>
          ))}
        </ul>
      )}
      <form onSubmit={onCreate} className="flex flex-wrap gap-2">
        <Input
          className="max-w-xs"
          placeholder="Name, e.g. before upgrade"
          value={name}
          maxLength={60}
          onChange={(e) => setName(e.target.value)}
          aria-label="Snapshot name"
        />
        <Button type="submit" disabled={!settled || full || name.trim() === "" || create.isPending}>
          Take snapshot
        </Button>
      </form>
      {full && <p className="text-sm text-slate-500 dark:text-slate-400">Delete a snapshot to take another.</p>}
      <ErrorText error={create.error ?? remove.error} />
      {restoring && (
        <ConfirmDialog
          title={`Restore "${restoring.name}"?`}
          confirmLabel="Restore"
          typeToConfirm={vm.hostname}
          busy={restore.isPending}
          error={restore.error}
          onConfirm={() => restore.mutate(restoring)}
          onCancel={() => setRestoring(null)}
        >
          <p>
            The disk goes back to how it was on {formatDate(restoring.created_at)}. Everything written since then is lost.
            {vm.state === "running" && " The VM is stopped and started again."}
          </p>
        </ConfirmDialog>
      )}
    </Card>
  );
}
