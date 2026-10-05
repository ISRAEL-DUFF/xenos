// Fetch wrapper for the /v1 API. Dashboard auth uses an httpOnly session cookie;
// unsafe requests must echo the CSRF token the server returned at login/me.
let csrfToken = "";
export const setCSRF = (t: string) => {
  csrfToken = t;
};

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

export async function api<T>(path: string, init: RequestInit & { json?: unknown } = {}): Promise<T> {
  const { json, ...rest } = init;
  const headers = new Headers(rest.headers);
  if (json !== undefined) headers.set("Content-Type", "application/json");
  const method = (rest.method ?? (json !== undefined ? "POST" : "GET")).toUpperCase();
  if (method !== "GET" && csrfToken) headers.set("X-CSRF-Token", csrfToken);
  const res = await fetch(`/v1${path}`, {
    credentials: "same-origin",
    ...rest,
    method,
    headers,
    body: json !== undefined ? JSON.stringify(json) : rest.body,
  });
  if (res.status === 204) return undefined as T;
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, body.error ?? res.statusText);
  return body as T;
}

// ---- types (mirror the Go JSON) ----

export interface Plan {
  id: number;
  slug: string;
  vcpu: number;
  ram_mb: number;
  disk_gb: number;
  price_uusdt_hourly: number;
  price_uusdt_monthly_cap: number;
}

export interface Template {
  id: number;
  slug: string;
  name: string;
}

export interface User {
  id: number;
  email: string;
  phone: string;
  email_verified: boolean;
  is_admin: boolean;
  status: string;
  vm_limit: number;
  auto_convert: boolean;
}

export interface SessionResponse {
  user: User;
  csrf_token: string;
}

export type VMState = "pending" | "provisioning" | "running" | "stopped" | "suspended" | "deleting" | "deleted" | "error";

export interface VM {
  id: number;
  hostname: string;
  region: string;
  plan: string;
  template: string;
  state: VMState;
  ipv4?: string;
  ipv6?: string;
  ssh_user: string;
  ssh_command?: string;
  price_uusdt_hourly: number;
  created_at: string;
  month_cost_uusdt?: number;
}

export interface SSHKey {
  id: number;
  name: string;
  public_key: string;
  fingerprint: string;
  created_at: string;
}

export interface Wallet {
  ngn_kobo: number;
  usdt_uusdt: number;
  rate_kobo_per_usdt: number | null;
  usdt_in_ngn_kobo: number | null;
  hourly_uusdt: number;
  runway_hours: number | null;
  unpaid_uusdt: number;
  auto_convert: boolean;
  email_verified: boolean;
  quoting_paused: boolean;
  grace_ends_at: string | null;
  virtual_account: { bank: string; account_number: string; account_name: string } | null;
  deposit_limit_kobo: number;
  conversions: Conversion[];
  charges: Charge[];
}

export interface Conversion {
  id: number;
  amount_ngn_kobo: number;
  amount_uusdt: number | null;
  rate: string | null;
  status: "pending" | "complete" | "failed";
  created_at: string;
}

export interface Charge {
  id: number;
  vm_id: number;
  hostname: string;
  hour: string;
  amount_uusdt: number;
  status: "pending" | "paid" | "unpaid" | "refunded";
}

export interface Quote {
  quote_id: string;
  amount_ngn_kobo: number;
  amount_uusdt: number;
  rate: string;
  expires_at: string; // quotes live 60 seconds
  added_runway_hours?: number;
}

// ---- admin ----

export interface AdminUser {
  id: number;
  email: string;
  status: string;
  is_admin: boolean;
  vm_limit: number;
  vm_count: number;
  email_verified: boolean;
  created_at: string;
  usdt_uusdt: number | null;
  ngn_kobo: number | null;
  grace_ends_at?: string;
}

export interface AdminVM {
  id: number;
  hostname: string;
  state: VMState;
  region: string;
  plan: string;
  price_uusdt_hourly: number;
  ipv4: string;
  ipv6: string;
  user_id: number;
  owner: string;
  port25_unblocked: boolean;
  created_at: string;
  flagged: boolean;
  flag_reason: string;
}

export interface Capacity {
  vcpu: { committed: number; physical: number | null };
  ram_mb: { committed: number; physical: number | null };
  pool: { name: string; used_bytes: number; total_bytes: number; fraction: number } | null;
  ips: { free: number; total: number };
  host_reachable: boolean;
}

export interface AdminJob {
  id: number;
  kind: string;
  payload: unknown;
  status: string;
  attempts: number;
  last_error: string;
  created_at: string;
}

export interface Revenue {
  days: { date: string; converted_ngn_kobo: number; converted_uusdt: number; usage_uusdt: number }[];
  fx: { managed_in: string; quoting_paused: boolean; rate_kobo_per_usdt?: number };
  totals: { converted_ngn_kobo: number; converted_uusdt: number; usage_uusdt: number };
  merchant_uusdt: number | null; // USDT in the Xenos operating wallet; admin credits are paid from it
}
