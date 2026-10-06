import { useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./api";
import { useAuth } from "./auth";
import { formatDate } from "./format";
import { Badge, Banner, Button, Card, CodeLine, ErrorText, Field, Input, PageHeader, statusTone } from "./ui";

export function AccountPage() {
  const { user, refresh, logout } = useAuth();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [changed, setChanged] = useState(false);

  const resend = useMutation({ mutationFn: () => api("/auth/resend-verification", { method: "POST" }) });
  const change = useMutation({
    mutationFn: () => api("/auth/change-password", { json: { current, new: next } }),
    onSuccess: async () => {
      setCurrent("");
      setNext("");
      setChanged(true);
      await refresh(); // the server issued a fresh session (and CSRF token)
    },
  });
  if (!user) return null;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setChanged(false);
    change.mutate();
  };

  return (
    <div className="space-y-6">
      <PageHeader title="Account" />

      <Card className="space-y-4">
        <dl className="grid gap-4 text-sm sm:grid-cols-2">
          <div>
            <dt className="text-slate-500 dark:text-slate-400">Email</dt>
            <dd className="flex flex-wrap items-center gap-2 font-medium text-slate-900 dark:text-slate-50">
              {user.email}
              <Badge tone={user.email_verified ? "green" : "amber"}>{user.email_verified ? "verified" : "not verified"}</Badge>
            </dd>
          </div>
          <div>
            <dt className="text-slate-500 dark:text-slate-400">Phone</dt>
            <dd className="font-medium text-slate-900 dark:text-slate-50">{user.phone}</dd>
          </div>
          <div>
            <dt className="text-slate-500 dark:text-slate-400">Status</dt>
            <dd><Badge tone={statusTone(user.status)}>{user.status}</Badge></dd>
          </div>
          <div>
            <dt className="text-slate-500 dark:text-slate-400">VM limit</dt>
            <dd className="font-medium text-slate-900 dark:text-slate-50">{user.vm_limit}</dd>
          </div>
        </dl>
        {!user.email_verified && (
          <div className="space-y-2">
            <Button variant="secondary" onClick={() => resend.mutate()} disabled={resend.isPending || resend.isSuccess}>
              {resend.isSuccess ? "Link sent" : "Resend verification email"}
            </Button>
            <ErrorText error={resend.error} />
          </div>
        )}
      </Card>

      <Card>
        <form onSubmit={submit} className="space-y-3">
          <h2 className="font-medium text-slate-900 dark:text-slate-50">Change password</h2>
          <Field label="Current password">
            <Input type="password" autoComplete="current-password" required value={current} onChange={(e) => setCurrent(e.target.value)} />
          </Field>
          <Field label="New password" hint="At least 10 characters. Other devices are signed out.">
            <Input type="password" autoComplete="new-password" minLength={10} required value={next} onChange={(e) => setNext(e.target.value)} />
          </Field>
          <ErrorText error={change.error} />
          {changed && <Banner tone="ok">Password changed. Other sessions were signed out.</Banner>}
          <Button disabled={change.isPending}>{change.isPending ? "Saving…" : "Change password"}</Button>
        </form>
      </Card>

      <APITokens verified={user.email_verified} />

      <Card className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm text-slate-600 dark:text-slate-300">
          By using Xenos you agree to the{" "}
          <Link to="/aup" className="font-medium text-indigo-600 underline-offset-2 hover:underline dark:text-indigo-400">
            acceptable use policy
          </Link>
          .
        </p>
        <Button variant="secondary" onClick={() => logout()}>Log out</Button>
      </Card>
    </div>
  );
}

interface APIToken {
  id: number;
  name: string;
  prefix: string;
  created_at: string;
  last_used_at: string | null;
  expires_at: string | null;
}

/** Tokens for programs. A token can manage VMs and SSH keys and read the wallet; it cannot change the account or move money. */
function APITokens({ verified }: { verified: boolean }) {
  const qc = useQueryClient();
  const tokens = useQuery({ queryKey: ["api-tokens"], queryFn: () => api<APIToken[]>("/tokens") });
  const [name, setName] = useState("");
  const [days, setDays] = useState("90");
  const [fresh, setFresh] = useState<string | null>(null);
  const create = useMutation({
    mutationFn: () => api<{ token: string }>("/tokens", { json: { name, expires_in_days: days === "" ? 0 : Number(days) } }),
    onSuccess: (r) => {
      setFresh(r.token);
      setName("");
      qc.invalidateQueries({ queryKey: ["api-tokens"] });
    },
  });
  const revoke = useMutation({
    mutationFn: (id: number) => api(`/tokens/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["api-tokens"] }),
  });
  const onCreate = (e: FormEvent) => {
    e.preventDefault();
    setFresh(null);
    create.mutate();
  };

  return (
    <Card className="space-y-3">
      <h2 className="font-medium text-slate-900 dark:text-slate-50">API tokens</h2>
      <p className="text-sm text-slate-500 dark:text-slate-400">
        For programs that create and manage VMs for you. A token can manage your VMs and SSH keys and read your wallet. It cannot change your account, move money or open consoles.
      </p>
      {fresh && (
        <Banner tone="warn">
          <div className="space-y-2">
            <p>Copy this token now. It is shown once and cannot be recovered.</p>
            <CodeLine text={fresh} />
          </div>
        </Banner>
      )}
      {(tokens.data ?? []).length > 0 && (
        <ul className="divide-y divide-slate-200 text-sm dark:divide-slate-700">
          {(tokens.data ?? []).map((t) => (
            <li key={t.id} className="flex flex-wrap items-center gap-2 py-2">
              <span className="font-medium text-slate-900 dark:text-slate-50">{t.name}</span>
              <code className="text-slate-500 dark:text-slate-400">{t.prefix}…</code>
              <span className="text-slate-500 dark:text-slate-400">
                {t.last_used_at ? `used ${formatDate(t.last_used_at)}` : "never used"}
                {t.expires_at ? ` · expires ${formatDate(t.expires_at)}` : " · does not expire"}
              </span>
              <Button variant="ghost" className="ml-auto" disabled={revoke.isPending} onClick={() => revoke.mutate(t.id)}>
                Revoke
              </Button>
            </li>
          ))}
        </ul>
      )}
      {verified ? (
        <form onSubmit={onCreate} className="flex flex-wrap items-end gap-2">
          <Field label="Name">
            <Input value={name} maxLength={60} required placeholder="e.g. pgdock" onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Expires in (days)" hint="Empty: never">
            <Input type="number" min={1} max={365} value={days} onChange={(e) => setDays(e.target.value)} />
          </Field>
          <Button disabled={create.isPending || name.trim() === ""}>Create token</Button>
        </form>
      ) : (
        <p className="text-sm text-slate-500 dark:text-slate-400">Verify your email to create tokens.</p>
      )}
      <ErrorText error={create.error ?? revoke.error ?? tokens.error} />
    </Card>
  );
}
