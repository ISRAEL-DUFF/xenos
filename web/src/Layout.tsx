import { useEffect, useState } from "react";
import { Link, NavLink, Navigate, Outlet, useLocation } from "react-router-dom";
import { useAuth } from "./auth";
import { Banner, Button, cx, Loading } from "./ui";

const customerLinks = [
  { to: "/", label: "Overview", end: true },
  { to: "/vms", label: "VMs" },
  { to: "/ssh-keys", label: "SSH keys" },
  { to: "/networking", label: "Networking" },
  { to: "/wallet", label: "Wallet" },
  { to: "/account", label: "Account" },
];

const linkClass = ({ isActive }: { isActive: boolean }) =>
  cx(
    "rounded-lg px-3 py-2 text-sm font-medium",
    isActive
      ? "bg-indigo-50 text-indigo-700 dark:bg-indigo-950/60 dark:text-indigo-300"
      : "text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-800",
  );

export function Layout() {
  const { user, logout } = useAuth();
  const [open, setOpen] = useState(false);
  const location = useLocation();
  useEffect(() => setOpen(false), [location.pathname]);

  const links = user?.is_admin ? [...customerLinks, { to: "/admin", label: "Admin" }] : customerLinks;

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-40 print:hidden border-b border-slate-200 bg-white/90 backdrop-blur dark:border-slate-700 dark:bg-slate-900/90">
        <div className="mx-auto flex max-w-5xl items-center gap-2 px-4 py-2.5">
          <Link to="/" className="mr-2 text-lg font-bold tracking-tight text-indigo-600 dark:text-indigo-400">
            Xenos
          </Link>
          {user && (
            <nav className="hidden items-center gap-1 md:flex" aria-label="Main">
              {links.map((l) => (
                <NavLink key={l.to} to={l.to} end={l.end} className={linkClass}>
                  {l.label}
                </NavLink>
              ))}
            </nav>
          )}
          <div className="ml-auto flex items-center gap-2">
            {user ? (
              <>
                <span className="hidden max-w-[14rem] truncate text-sm text-slate-500 lg:inline dark:text-slate-400">{user.email}</span>
                {/* hidden/block on a wrapper: the button's own inline-flex would override `hidden` */}
                <span className="hidden md:block">
                  <Button variant="secondary" onClick={() => logout()}>
                    Log out
                  </Button>
                </span>
                <span className="md:hidden">
                  <Button variant="secondary" aria-expanded={open} aria-controls="mobile-menu" aria-label="Menu" onClick={() => setOpen((o) => !o)}>
                    {open ? "Close" : "Menu"}
                  </Button>
                </span>
              </>
            ) : (
              <>
                <Link to="/login" className="rounded-lg px-3 py-2 text-sm font-medium text-slate-700 hover:bg-slate-100 dark:text-slate-200 dark:hover:bg-slate-800">
                  Log in
                </Link>
                <Link to="/signup" className="rounded-lg bg-indigo-600 px-3.5 py-2 text-sm font-medium text-white hover:bg-indigo-500">
                  Sign up
                </Link>
              </>
            )}
          </div>
        </div>
        {user && open && (
          <nav id="mobile-menu" className="border-t border-slate-200 px-4 py-2 md:hidden dark:border-slate-700" aria-label="Main">
            <ul className="space-y-1">
              {links.map((l) => (
                <li key={l.to}>
                  <NavLink to={l.to} end={l.end} className={(s) => cx(linkClass(s), "block")}>
                    {l.label}
                  </NavLink>
                </li>
              ))}
              <li>
                <button className="block w-full rounded-lg px-3 py-2 text-left text-sm font-medium text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-800" onClick={() => logout()}>
                  Log out ({user.email})
                </button>
              </li>
            </ul>
          </nav>
        )}
      </header>

      <main className="mx-auto max-w-5xl px-4 py-6 sm:py-8">
        <Outlet />
      </main>

      <footer className="mx-auto max-w-5xl px-4 pb-8 text-xs print:hidden text-slate-500 dark:text-slate-400">
        <Link to="/aup" className="underline">
          Acceptable use policy
        </Link>
      </footer>
    </div>
  );
}

export function RequireAuth() {
  const { user, loading } = useAuth();
  const location = useLocation();
  if (loading) return <Loading />;
  if (!user) return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  return (
    <>
      <AccountBanners />
      <Outlet />
    </>
  );
}

/** Account-level notices shown on every signed-in page (including the overview, which sits outside RequireAuth). */
export function AccountBanners() {
  const { user } = useAuth();
  if (!user) return null;
  return (
    <>
      {!user.email_verified && (
        <Banner tone="warn">
          Verify your email to fund your wallet. Check your inbox for the link, or request a new one from your{" "}
          <Link to="/account" className="font-semibold underline">
            account page
          </Link>
          .
        </Banner>
      )}
      {user.status === "suspended" && <Banner tone="danger">Your account is suspended. You can still see your wallet, but you cannot create new VMs. Contact support.</Banner>}
    </>
  );
}

export function RequireAdmin() {
  const { user } = useAuth();
  if (!user?.is_admin) return <Navigate to="/" replace />;
  return <Outlet />;
}
