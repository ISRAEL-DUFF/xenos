import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, type SSHKey } from "./api";
import { formatDate } from "./format";
import { useSSHKeys } from "./hooks";
import { Button, Card, ConfirmDialog, Empty, ErrorText, Field, Input, Loading, PageHeader, inputClass } from "./ui";

export function SSHKeys() {
  const qc = useQueryClient();
  const keys = useSSHKeys();
  const [name, setName] = useState("");
  const [publicKey, setPublicKey] = useState("");
  const [toDelete, setToDelete] = useState<SSHKey | null>(null);

  const add = useMutation({
    mutationFn: () => api<SSHKey>("/ssh-keys", { json: { name, public_key: publicKey } }),
    onSuccess: () => {
      setName("");
      setPublicKey("");
      qc.invalidateQueries({ queryKey: ["ssh-keys"] });
    },
  });
  const remove = useMutation({
    mutationFn: (id: number) => api(`/ssh-keys/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      setToDelete(null);
      qc.invalidateQueries({ queryKey: ["ssh-keys"] });
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    add.mutate();
  };

  return (
    <div className="space-y-6">
      <PageHeader title="SSH keys" subtitle="Keys are installed on a VM when you create it. Password login is disabled." />

      <Card>
        <form onSubmit={submit} className="space-y-3">
          <Field label="Name">
            <Input placeholder="e.g. work laptop" maxLength={64} required value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Public key" hint="Paste the contents of your .pub file (ssh-ed25519 …). RSA keys need at least 2048 bits.">
            <textarea
              className={`${inputClass} h-24 font-mono text-xs`}
              placeholder="ssh-ed25519 AAAA…"
              required
              value={publicKey}
              onChange={(e) => setPublicKey(e.target.value)}
            />
          </Field>
          <ErrorText error={add.error} />
          <Button disabled={add.isPending}>{add.isPending ? "Adding…" : "Add key"}</Button>
        </form>
      </Card>

      {keys.isLoading && <Loading />}
      <ErrorText error={keys.error} />
      {keys.data && keys.data.length === 0 && <Empty title="No keys yet">Add one above before creating a VM.</Empty>}
      <ul className="space-y-2">
        {keys.data?.map((k) => (
          <li key={k.id}>
            <Card className="flex items-center justify-between gap-3 !p-3 sm:!p-4">
              <div className="min-w-0">
                <p className="font-medium text-slate-900 dark:text-slate-50">{k.name}</p>
                <p className="truncate font-mono text-xs text-slate-500 dark:text-slate-400">{k.fingerprint}</p>
                <p className="text-xs text-slate-400">Added {formatDate(k.created_at)}</p>
              </div>
              <Button variant="secondary" className="shrink-0 !text-red-600 dark:!text-red-400" onClick={() => setToDelete(k)}>
                Delete
              </Button>
            </Card>
          </li>
        ))}
      </ul>

      {toDelete && (
        <ConfirmDialog
          title={`Delete "${toDelete.name}"?`}
          confirmLabel="Delete key"
          busy={remove.isPending}
          error={remove.error}
          onConfirm={() => remove.mutate(toDelete.id)}
          onCancel={() => setToDelete(null)}
        >
          <p>VMs that already have this key keep it until they are rebuilt. Deleting it here only stops it being offered for new VMs.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}
