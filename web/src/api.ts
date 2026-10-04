// Thin fetch wrapper. Dashboard auth will use httpOnly session cookies + CSRF header.
export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/v1${path}`, {
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error ?? res.statusText);
  }
  return res.json();
}

export interface Plan {
  id: number;
  slug: string;
  vcpu: number;
  ram_mb: number;
  disk_gb: number;
  price_uusdt_hourly: number;
  price_uusdt_monthly_cap: number;
}

export const formatUSDT = (micro: number) => (micro / 1_000_000).toFixed(4);
