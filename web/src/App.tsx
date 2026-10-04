import { Link, Route, Routes } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, formatUSDT, type Plan } from "./api";

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

export default function App() {
  return (
    <div className="mx-auto max-w-4xl p-4">
      <nav className="mb-6 flex flex-wrap gap-4 border-b pb-3 text-sm">
        <Link to="/" className="font-bold">Xenos</Link>
        <Link to="/vms">VMs</Link>
        <Link to="/ssh-keys">SSH keys</Link>
        <Link to="/wallet">Wallet</Link>
        <Link to="/account">Account</Link>
      </nav>
      <Routes>
        <Route path="/" element={<Plans />} />
        <Route path="/vms" element={<Soon name="VMs" />} />
        <Route path="/ssh-keys" element={<Soon name="SSH keys" />} />
        <Route path="/wallet" element={<Soon name="Wallet" />} />
        <Route path="/account" element={<Soon name="Account" />} />
        <Route path="*" element={<Soon name="Not found" />} />
      </Routes>
    </div>
  );
}
