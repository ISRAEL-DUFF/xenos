import { Link, Navigate, Outlet, Route, Routes } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, formatUSDT, type Plan } from "./api";
import { useAuth } from "./auth";
import { SSHKeys } from "./pages-sshkeys";
import { ForgotPassword, Login, ResetPassword, Signup, VerifyEmail } from "./pages-auth";

function Plans() {
  const { data, error, isLoading } = useQuery({ queryKey: ["plans"], queryFn: () => api<Plan[]>("/plans") });
  if (isLoading) return <p>Loading…</p>;
  if (error) return <p className="text-red-600">{(error as Error).message}</p>;
  return (
    <div className="grid gap-4 sm:grid-cols-3">
      {data!.map((p) => (
        <div key={p.id} className="rounded-lg border p-4">
          <h3 className="font-semibold capitalize">{p.slug}</h3>
          <p className="text-sm text-gray-600">
            {p.vcpu} vCPU · {p.ram_mb / 1024} GB · {p.disk_gb} GB
          </p>
          <p className="mt-2">{formatUSDT(p.price_uusdt_hourly)} USDT/hr</p>
        </div>
      ))}
    </div>
  );
}

const Soon = ({ name }: { name: string }) => <p className="text-gray-600">{name} — coming soon.</p>;

function Layout() {
  const { user, logout } = useAuth();
  return (
    <div className="mx-auto max-w-4xl p-4">
      <nav className="mb-6 flex flex-wrap items-center gap-4 border-b pb-3 text-sm">
        <Link to="/" className="font-bold">Xenos</Link>
        {user && (
          <>
            <Link to="/vms">VMs</Link>
            <Link to="/ssh-keys">SSH keys</Link>
            <Link to="/wallet">Wallet</Link>
            <Link to="/account">Account</Link>
            <button className="ml-auto underline" onClick={() => logout()}>Log out ({user.email})</button>
          </>
        )}
        {!user && (
          <span className="ml-auto flex gap-4">
            <Link to="/login">Log in</Link>
            <Link to="/signup">Sign up</Link>
          </span>
        )}
      </nav>
      <Outlet />
    </div>
  );
}

function RequireAuth() {
  const { user, loading } = useAuth();
  if (loading) return <p>Loading…</p>;
  if (!user) return <Navigate to="/login" replace />;
  return (
    <>
      {!user.email_verified && (
        <p className="mb-4 rounded bg-amber-100 p-3 text-sm">
          Verify your email to fund your wallet. Check your inbox for the link.
        </p>
      )}
      <Outlet />
    </>
  );
}

export default function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route path="/" element={<Plans />} />
        <Route path="/login" element={<Login />} />
        <Route path="/signup" element={<Signup />} />
        <Route path="/verify-email" element={<VerifyEmail />} />
        <Route path="/forgot-password" element={<ForgotPassword />} />
        <Route path="/reset-password" element={<ResetPassword />} />
        <Route element={<RequireAuth />}>
          <Route path="/vms" element={<Soon name="VMs" />} />
          <Route path="/ssh-keys" element={<SSHKeys />} />
          <Route path="/wallet" element={<Soon name="Wallet" />} />
          <Route path="/account" element={<Soon name="Account" />} />
        </Route>
        <Route path="*" element={<Soon name="Not found" />} />
      </Route>
    </Routes>
  );
}
