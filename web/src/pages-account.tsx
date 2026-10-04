import { useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { useMutation } from "@tanstack/react-query";
import { api } from "./api";
import { useAuth } from "./auth";
import { Badge, Banner, Button, Card, ErrorText, Field, Input, PageHeader, statusTone } from "./ui";

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
