import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./api";

interface SSHKey {
  id: number;
  name: string;
  public_key: string;
  fingerprint: string;
  created_at: string;
}

export function SSHKeys() {
  const qc = useQueryClient();
  const keys = useQuery({ queryKey: ["ssh-keys"], queryFn: () => api<SSHKey[]>("/ssh-keys") });
  const [name, setName] = useState("");
  const [publicKey, setPublicKey] = useState("");

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
    onSuccess: () => qc.invalidateQueries({ queryKey: ["ssh-keys"] }),
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    add.mutate();
  };

  return (
    <div className="space-y-6">
      <h1 className="text-xl font-semibold">SSH keys</h1>

      <form onSubmit={submit} className="space-y-3 rounded-lg border p-4">
        <input className="w-full rounded border px-3 py-2" placeholder="Name (e.g. work laptop)" maxLength={64} required value={name} onChange={(e) => setName(e.target.value)} />
        <textarea
          className="h-24 w-full rounded border px-3 py-2 font-mono text-xs"
          placeholder="Paste your public key (ssh-ed25519 AAAA… or ssh-rsa AAAA…)"
          required
          value={publicKey}
          onChange={(e) => setPublicKey(e.target.value)}
        />
        {add.error && <p className="text-sm text-red-600">{(add.error as Error).message}</p>}
        <button className="rounded bg-black px-4 py-2 text-white disabled:opacity-50" disabled={add.isPending}>
          Add key
        </button>
      </form>

      {keys.isLoading && <p>Loading…</p>}
      {keys.error && <p className="text-red-600">{(keys.error as Error).message}</p>}
      {keys.data && keys.data.length === 0 && <p className="text-gray-600">No keys yet. Add one before creating a VM.</p>}
      <ul className="space-y-2">
        {keys.data?.map((k) => (
          <li key={k.id} className="flex items-center justify-between gap-3 rounded border p-3">
            <div className="min-w-0">
              <p className="font-medium">{k.name}</p>
              <p className="truncate font-mono text-xs text-gray-600">{k.fingerprint}</p>
            </div>
            <button
              className="shrink-0 text-sm text-red-600 underline disabled:opacity-50"
              disabled={remove.isPending}
              onClick={() => confirm(`Delete key "${k.name}"? Existing VMs keep it until rebuilt.`) && remove.mutate(k.id)}
            >
              Delete
            </button>
          </li>
        ))}
      </ul>
      {remove.error && <p className="text-sm text-red-600">{(remove.error as Error).message}</p>}
    </div>
  );
}
