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
  if (res.status === 204 || res.status === 202) return undefined as T;
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, body.error ?? res.statusText);
  return body as T;
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

export const formatUSDT = (micro: number) => (micro / 1_000_000).toFixed(4);
