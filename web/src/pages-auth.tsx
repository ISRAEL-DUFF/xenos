import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";
import { api } from "./api";
import { useAuth } from "./auth";
import { Button, Card, ErrorText, Field, Input } from "./ui";

function AuthCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="mx-auto mt-4 max-w-sm sm:mt-10">
      <Card className="space-y-4">
        <h1 className="text-xl font-semibold text-slate-900 dark:text-slate-50">{title}</h1>
        {children}
      </Card>
    </div>
  );
}

function useSubmit(fn: () => Promise<void>) {
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      await fn();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };
  return { error, busy, onSubmit };
}

const linkClass = "font-medium text-indigo-600 underline-offset-2 hover:underline dark:text-indigo-400";

export function Login() {
  const { user, login } = useAuth();
  const nav = useNavigate();
  const location = useLocation();
  const from = (location.state as { from?: string } | null)?.from ?? "/";
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const f = useSubmit(async () => {
    await login(email, password);
    nav(from, { replace: true });
  });
  if (user) return <Navigate to={from} replace />;
  return (
    <AuthCard title="Log in">
      <form onSubmit={f.onSubmit} className="space-y-3">
        <Field label="Email">
          <Input type="email" autoComplete="email" required value={email} onChange={(e) => setEmail(e.target.value)} />
        </Field>
        <Field label="Password">
          <Input type="password" autoComplete="current-password" required value={password} onChange={(e) => setPassword(e.target.value)} />
        </Field>
        <ErrorText error={f.error} />
        <Button className="w-full" disabled={f.busy}>
          {f.busy ? "Logging in…" : "Log in"}
        </Button>
      </form>
      <p className="text-sm text-slate-600 dark:text-slate-300">
        <Link to="/forgot-password" className={linkClass}>
          Forgot password?
        </Link>{" "}
        ·{" "}
        <Link to="/signup" className={linkClass}>
          Create account
        </Link>
      </p>
    </AuthCard>
  );
}

export function Signup() {
  const { user, signup } = useAuth();
  const nav = useNavigate();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [phone, setPhone] = useState("");
  const [agree, setAgree] = useState(false);
  const f = useSubmit(async () => {
    await signup(email, password, phone);
    nav("/");
  });
  if (user) return <Navigate to="/" replace />;
  return (
    <AuthCard title="Create your account">
      <form onSubmit={f.onSubmit} className="space-y-3">
        <Field label="Email">
          <Input type="email" autoComplete="email" required value={email} onChange={(e) => setEmail(e.target.value)} />
        </Field>
        <Field label="Phone" hint="International format, for account security and support.">
          <Input type="tel" placeholder="+234…" autoComplete="tel" required value={phone} onChange={(e) => setPhone(e.target.value)} />
        </Field>
        <Field label="Password" hint="At least 10 characters.">
          <Input type="password" autoComplete="new-password" minLength={10} required value={password} onChange={(e) => setPassword(e.target.value)} />
        </Field>
        <label className="flex gap-2 text-sm text-slate-600 dark:text-slate-300">
          <input type="checkbox" className="mt-0.5 h-4 w-4 shrink-0" checked={agree} onChange={(e) => setAgree(e.target.checked)} required />
          <span>
            I accept the{" "}
            <Link to="/aup" target="_blank" className={linkClass}>
              acceptable use policy
            </Link>
            : no spam, mining, scanning or attacks. Violations mean deletion without refund.
          </span>
        </label>
        <ErrorText error={f.error} />
        <Button className="w-full" disabled={f.busy || !agree}>
          {f.busy ? "Creating account…" : "Sign up"}
        </Button>
      </form>
      <p className="text-sm text-slate-600 dark:text-slate-300">
        Already registered?{" "}
        <Link to="/login" className={linkClass}>
          Log in
        </Link>
      </p>
    </AuthCard>
  );
}

// Emailed links carry the token in the URL fragment, which browsers never send to a server, so it cannot
// end up in proxy or access logs. Read it once and clear it from the address bar.
function emailedToken(): string {
  const token = new URLSearchParams(window.location.hash.replace(/^#/, "")).get("token") ?? "";
  if (token) window.history.replaceState(null, "", window.location.pathname);
  return token;
}

export function VerifyEmail() {
  const [token] = useState(emailedToken);
  const { user, refresh } = useAuth();
  const [state, setState] = useState<"working" | "ok" | "error">("working");
  const [msg, setMsg] = useState("");
  useEffect(() => {
    if (!token) {
      setState("error");
      setMsg("This link is missing its token.");
      return;
    }
    api("/auth/verify", { json: { token } })
      .then(async () => {
        setState("ok");
        if (user) await refresh();
      })
      .catch((e: Error) => {
        setState("error");
        setMsg(e.message);
      });
  }, []);
  return (
    <AuthCard title="Verify email">
      {state === "working" && <p className="text-sm">Verifying…</p>}
      {state === "ok" && (
        <p className="text-sm">
          Your email is verified.{" "}
          <Link to="/" className={linkClass}>
            Continue
          </Link>
        </p>
      )}
      {state === "error" && (
        <>
          <p role="alert" className="text-sm text-red-600 dark:text-red-400">{msg}</p>
          <p className="text-sm">
            <Link to="/account" className={linkClass}>
              Request a new link
            </Link>{" "}
            from your account page.
          </p>
        </>
      )}
    </AuthCard>
  );
}

export function ForgotPassword() {
  const [email, setEmail] = useState("");
  const [sent, setSent] = useState(false);
  const f = useSubmit(async () => {
    await api("/auth/forgot-password", { json: { email } });
    setSent(true);
  });
  return (
    <AuthCard title="Reset password">
      {sent ? (
        <p className="text-sm">If an account exists for that email, a reset link is on its way.</p>
      ) : (
        <form onSubmit={f.onSubmit} className="space-y-3">
          <Field label="Email">
            <Input type="email" required value={email} onChange={(e) => setEmail(e.target.value)} />
          </Field>
          <ErrorText error={f.error} />
          <Button className="w-full" disabled={f.busy}>
            Send reset link
          </Button>
        </form>
      )}
    </AuthCard>
  );
}

export function ResetPassword() {
  const [token] = useState(emailedToken);
  const nav = useNavigate();
  const [password, setPassword] = useState("");
  const f = useSubmit(async () => {
    await api("/auth/reset-password", { json: { token, password } });
    nav("/login");
  });
  return (
    <AuthCard title="Choose a new password">
      <form onSubmit={f.onSubmit} className="space-y-3">
        <Field label="New password" hint="At least 10 characters.">
          <Input type="password" autoComplete="new-password" minLength={10} required value={password} onChange={(e) => setPassword(e.target.value)} />
        </Field>
        <ErrorText error={f.error} />
        <Button className="w-full" disabled={f.busy}>
          Update password
        </Button>
      </form>
    </AuthCard>
  );
}
