import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { api } from "./api";
import { useAuth } from "./auth";

const input = "w-full rounded border px-3 py-2";
const button = "w-full rounded bg-black px-3 py-2 text-white disabled:opacity-50";

function Card({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="mx-auto mt-10 max-w-sm space-y-4">
      <h1 className="text-xl font-semibold">{title}</h1>
      {children}
    </div>
  );
}

function useSubmit(fn: () => Promise<void>) {
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      await fn();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return { error, busy, onSubmit };
}

const Err = ({ msg }: { msg: string }) => (msg ? <p className="text-sm text-red-600">{msg}</p> : null);

export function Login() {
  const { user, login } = useAuth();
  const nav = useNavigate();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const f = useSubmit(async () => {
    await login(email, password);
    nav("/vms");
  });
  if (user) return <Navigate to="/vms" replace />;
  return (
    <Card title="Log in">
      <form onSubmit={f.onSubmit} className="space-y-3">
        <input className={input} type="email" placeholder="Email" autoComplete="email" required value={email} onChange={(e) => setEmail(e.target.value)} />
        <input className={input} type="password" placeholder="Password" autoComplete="current-password" required value={password} onChange={(e) => setPassword(e.target.value)} />
        <Err msg={f.error} />
        <button className={button} disabled={f.busy}>Log in</button>
      </form>
      <p className="text-sm">
        <Link to="/forgot-password" className="underline">Forgot password?</Link> · <Link to="/signup" className="underline">Create account</Link>
      </p>
    </Card>
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
    nav("/vms");
  });
  if (user) return <Navigate to="/vms" replace />;
  return (
    <Card title="Create account">
      <form onSubmit={f.onSubmit} className="space-y-3">
        <input className={input} type="email" placeholder="Email" autoComplete="email" required value={email} onChange={(e) => setEmail(e.target.value)} />
        <input className={input} type="tel" placeholder="Phone (+234…)" autoComplete="tel" required value={phone} onChange={(e) => setPhone(e.target.value)} />
        <input className={input} type="password" placeholder="Password (min 10 characters)" autoComplete="new-password" minLength={10} required value={password} onChange={(e) => setPassword(e.target.value)} />
        <label className="flex gap-2 text-sm">
          <input type="checkbox" checked={agree} onChange={(e) => setAgree(e.target.checked)} required />
          <span>I accept the acceptable-use policy: no spam, mining, scanning or attacks. Violations mean deletion without refund.</span>
        </label>
        <Err msg={f.error} />
        <button className={button} disabled={f.busy || !agree}>Sign up</button>
      </form>
      <p className="text-sm">
        Already registered? <Link to="/login" className="underline">Log in</Link>
      </p>
    </Card>
  );
}

export function VerifyEmail() {
  const [params] = useSearchParams();
  const { user, refresh } = useAuth();
  const [state, setState] = useState<"working" | "ok" | "error">("working");
  const [msg, setMsg] = useState("");
  useEffect(() => {
    const token = params.get("token");
    if (!token) {
      setState("error");
      setMsg("Missing token.");
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
    <Card title="Verify email">
      {state === "working" && <p>Verifying…</p>}
      {state === "ok" && <p>Your email is verified. <Link to="/vms" className="underline">Continue</Link></p>}
      {state === "error" && <Err msg={msg} />}
    </Card>
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
    <Card title="Reset password">
      {sent ? (
        <p>If an account exists for that email, a reset link is on its way.</p>
      ) : (
        <form onSubmit={f.onSubmit} className="space-y-3">
          <input className={input} type="email" placeholder="Email" required value={email} onChange={(e) => setEmail(e.target.value)} />
          <Err msg={f.error} />
          <button className={button} disabled={f.busy}>Send reset link</button>
        </form>
      )}
    </Card>
  );
}

export function ResetPassword() {
  const [params] = useSearchParams();
  const nav = useNavigate();
  const [password, setPassword] = useState("");
  const f = useSubmit(async () => {
    await api("/auth/reset-password", { json: { token: params.get("token") ?? "", password } });
    nav("/login");
  });
  return (
    <Card title="Choose a new password">
      <form onSubmit={f.onSubmit} className="space-y-3">
        <input className={input} type="password" placeholder="New password (min 10 characters)" autoComplete="new-password" minLength={10} required value={password} onChange={(e) => setPassword(e.target.value)} />
        <Err msg={f.error} />
        <button className={button} disabled={f.busy}>Update password</button>
      </form>
    </Card>
  );
}
