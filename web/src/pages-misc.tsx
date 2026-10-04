import { Link } from "react-router-dom";
import { useAuth } from "./auth";
import { AccountBanners } from "./Layout";
import { formatNaira, formatRunway, formatUSDT, memLabel, usdtToKobo } from "./format";
import { usePlans, useVMs, useWallet } from "./hooks";
import { Banner, Card, Empty, Loading, PageHeader, Stat, StateBadge } from "./ui";

const linkClass = "font-medium text-indigo-600 underline-offset-2 hover:underline dark:text-indigo-400";
const primaryLink = "inline-flex rounded-lg bg-indigo-600 px-3.5 py-2 text-sm font-medium text-white hover:bg-indigo-500";

/** "/" shows the overview when signed in and the landing page when not. */
export function Home() {
  const { user, loading } = useAuth();
  if (loading) return <Loading />;
  return user ? <Overview /> : <Landing />;
}

function Landing() {
  const plans = usePlans();
  return (
    <div className="space-y-10">
      <section className="space-y-4 pt-4 text-center sm:pt-10">
        <h1 className="text-3xl font-bold tracking-tight text-slate-900 sm:text-5xl dark:text-slate-50">Linux servers, paid for in naira.</h1>
        <p className="mx-auto max-w-xl text-slate-600 dark:text-slate-300">Fund your wallet by bank transfer or card, launch a VM in about a minute, and pay by the hour. Stop paying the moment you delete it.</p>
        <div className="flex justify-center gap-3">
          <Link to="/signup" className={primaryLink}>Create an account</Link>
          <Link to="/login" className="inline-flex rounded-lg border border-slate-300 px-3.5 py-2 text-sm font-medium text-slate-700 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-200 dark:hover:bg-slate-800">Log in</Link>
        </div>
      </section>
      <section className="space-y-3">
        <h2 className="text-center font-medium text-slate-900 dark:text-slate-50">Plans</h2>
        <div className="grid gap-3 sm:grid-cols-3">
          {plans.data?.map((p) => (
            <Card key={p.id}>
              <h3 className="font-semibold capitalize text-slate-900 dark:text-slate-50">{p.slug}</h3>
              <p className="mt-1 text-sm text-slate-600 dark:text-slate-300">{p.vcpu} vCPU · {memLabel(p.ram_mb)} RAM · {p.disk_gb} GB disk</p>
              <p className="mt-3 text-sm font-medium text-slate-900 dark:text-slate-50">{formatUSDT(p.price_uusdt_hourly)}/hr</p>
              <p className="text-xs text-slate-500 dark:text-slate-400">Naira price shown at today's rate once you sign in.</p>
            </Card>
          ))}
        </div>
      </section>
    </div>
  );
}

function Overview() {
  const { user } = useAuth();
  const wallet = useWallet();
  const vms = useVMs();
  const w = wallet.data;
  const rate = w?.rate_kobo_per_usdt;
  const inNaira = w ? usdtToKobo(w.usdt_uusdt, rate) : null;
  const active = vms.data?.filter((v) => v.state !== "error") ?? [];

  return (
    <div>
      <PageHeader title="Overview" subtitle={user?.email} />
      <AccountBanners />

      {w?.grace_ends_at && (
        <Banner tone="danger">
          Your wallet ran out, so your VMs were suspended. Top up before {new Date(w.grace_ends_at).toLocaleString()} or they will be deleted.{" "}
          <Link to="/wallet" className="font-semibold underline">Top up now</Link>
        </Banner>
      )}
      {w && !w.grace_ends_at && w.runway_hours !== null && w.runway_hours < 24 && (
        <Banner tone="warn">
          Your balance covers about {formatRunway(w.runway_hours)} of usage. Top up soon to avoid your VMs being suspended.{" "}
          <Link to="/wallet" className="font-semibold underline">Top up</Link>
        </Banner>
      )}
      {w?.quoting_paused && <Banner tone="info">Naira conversion is paused for a moment. Deposits are safe and will convert when it resumes.</Banner>}

      <div className="grid gap-3 sm:grid-cols-3">
        <Stat
          label="Wallet"
          value={w ? formatUSDT(w.usdt_uusdt) : "…"}
          sub={inNaira !== null ? `≈ ${formatNaira(inNaira)}` : w && w.ngn_kobo > 0 ? `${formatNaira(w.ngn_kobo)} not yet converted` : undefined}
        />
        <Stat label="Runway" value={w ? formatRunway(w.runway_hours) : "…"} sub={w && w.hourly_uusdt > 0 ? `at ${formatUSDT(w.hourly_uusdt)}/hr` : "No VMs running"} />
        <Stat label="VMs" value={vms.data ? `${active.length} / ${user?.vm_limit ?? 0}` : "…"} sub="in use / allowed" />
      </div>

      <section className="mt-8 space-y-3">
        <div className="flex items-center justify-between">
          <h2 className="font-medium text-slate-900 dark:text-slate-50">Your VMs</h2>
          <Link to="/vms/new" className={linkClass}>Create VM</Link>
        </div>
        {vms.isLoading && <Loading />}
        {vms.data && vms.data.length === 0 && (
          <Empty title="You have no VMs yet">
            <Link to="/vms/new" className={primaryLink}>Create your first VM</Link>
          </Empty>
        )}
        <ul className="space-y-2">
          {vms.data?.slice(0, 5).map((v) => (
            <li key={v.id}>
              <Link to={`/vms/${v.id}`} className="flex items-center justify-between gap-3 rounded-xl border border-slate-200 bg-white px-4 py-3 hover:border-indigo-300 dark:border-slate-700 dark:bg-slate-800/60 dark:hover:border-indigo-500">
                <span className="font-medium text-slate-900 dark:text-slate-50">{v.hostname}</span>
                <span className="flex items-center gap-3">
                  <span className="hidden font-mono text-sm text-slate-500 sm:inline dark:text-slate-400">{v.ipv4}</span>
                  <StateBadge state={v.state} />
                </span>
              </Link>
            </li>
          ))}
        </ul>
      </section>
    </div>
  );
}

export function AUP() {
  return (
    <article className="mx-auto max-w-2xl space-y-4 text-sm leading-relaxed text-slate-700 dark:text-slate-300">
      <h1 className="text-2xl font-semibold text-slate-900 dark:text-slate-50">Acceptable use policy</h1>
      <p>Xenos servers are for legitimate workloads. By creating an account you agree not to use them for any of the following:</p>
      <ul className="list-disc space-y-1 pl-6">
        <li><strong>Spam or unsolicited bulk messaging.</strong> Outbound email on port 25 is blocked by default.</li>
        <li><strong>Cryptocurrency mining</strong> or other sustained use of all CPU for the benefit of a third party.</li>
        <li><strong>Scanning, probing or attacking</strong> systems you do not own or have permission to test, including denial-of-service traffic.</li>
        <li><strong>Hosting malware, phishing or illegal content</strong>, or anything that infringes other people's rights.</li>
        <li>Reselling access in a way that hides who is responsible for the traffic.</li>
      </ul>
      <p>We monitor resource use and may suspend a VM or account that appears to break these rules, with or without notice when there is an ongoing harm. <strong>Violations mean deletion of the VM without refund.</strong></p>
      <p>You are responsible for everything that runs on your VMs, including keeping them patched and your SSH keys private.</p>
      <p>Questions or abuse reports: contact support.</p>
    </article>
  );
}

export function NotFound() {
  return (
    <Empty title="Page not found">
      <Link to="/" className={linkClass}>Go to the overview</Link>
    </Empty>
  );
}

