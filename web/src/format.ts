// Money is int64 on the server: NGN in kobo, USDT in micro-USDT. These helpers only format for display;
// BigInt is used where two large integers are multiplied so nothing silently loses precision.

const naira = new Intl.NumberFormat("en-NG", { style: "currency", currency: "NGN", minimumFractionDigits: 2 });

export const formatNaira = (kobo: number) => naira.format(kobo / 100);

/** 1_500_000 -> "1.50 USDT"; keeps up to 4 decimals only when they matter (hourly prices are fractions of a cent). */
export function formatUSDT(micro: number, opts: { unit?: boolean } = {}): string {
  const v = micro / 1_000_000;
  const dp = micro % 10_000 === 0 ? 2 : 4;
  const s = v.toLocaleString("en-US", { minimumFractionDigits: dp, maximumFractionDigits: dp });
  return opts.unit === false ? s : `${s} USDT`;
}

/** USDT (micro) to kobo at a rate of kobo per USDT. Display only. */
export function usdtToKobo(micro: number, rateKoboPerUSDT: number | null | undefined): number | null {
  if (!rateKoboPerUSDT) return null;
  return Number((BigInt(Math.round(micro)) * BigInt(Math.round(rateKoboPerUSDT))) / 1_000_000n);
}

/** Kobo needed to buy `micro` USDT, rounded up. */
export function koboForUSDT(micro: number, rateKoboPerUSDT: number): number {
  return Number((BigInt(Math.round(micro)) * BigInt(Math.round(rateKoboPerUSDT)) + 999_999n) / 1_000_000n);
}

/** Parse a decimal USDT string ("12.5") into micro-USDT without floating point error. */
export function parseUSDT(input: string): number | null {
  const m = /^\s*(-?)(\d+)(?:\.(\d{0,6}))?\s*$/.exec(input);
  if (!m) return null;
  const micro = BigInt(m[2]) * 1_000_000n + BigInt((m[3] ?? "").padEnd(6, "0") || "0");
  return Number(m[1] ? -micro : micro);
}

/** Parse a naira string ("1,500.50") into kobo. */
export function parseNaira(input: string): number | null {
  const m = /^\s*(\d[\d,]*)(?:\.(\d{0,2}))?\s*$/.exec(input.replace(/₦/g, ""));
  if (!m) return null;
  return Number(BigInt(m[1].replace(/,/g, "")) * 100n + BigInt((m[2] ?? "").padEnd(2, "0") || "0"));
}

export function formatRunway(hours: number | null): string {
  if (hours === null) return "—";
  if (hours < 1) return "under 1 hour";
  if (hours < 48) return `${hours} hour${hours === 1 ? "" : "s"}`;
  const days = Math.floor(hours / 24);
  if (days < 60) return `${days} days`;
  return `${Math.floor(days / 30)} months`;
}

export function formatDate(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

export const memLabel = (mb: number) => (mb >= 1024 ? `${mb / 1024} GB` : `${mb} MB`);

export const bytesToGiB = (b: number) => `${(b / 2 ** 30).toFixed(1)} GiB`;
