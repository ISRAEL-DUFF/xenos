import { lazy, Suspense } from "react";
import { Route, Routes } from "react-router-dom";
import { Layout, RequireAdmin, RequireAuth } from "./Layout";
import { AdminCapacity, AdminJobs, AdminLayout, AdminRevenue, AdminUserDetail, AdminUsers, AdminVMs } from "./pages-admin";
import { AccountPage } from "./pages-account";
import { AdminCatalogue } from "./pages-catalogue";
import { ForgotPassword, Login, ResetPassword, Signup, VerifyEmail } from "./pages-auth";
import { AUP, Home, NotFound } from "./pages-misc";
import { SSHKeys } from "./pages-sshkeys";
import { VMCreate, VMDetail, VMList } from "./pages-vms";
import { NetworkingPage } from "./pages-networking";
import { StatementList, StatementPage } from "./pages-statements";
import { WalletPage } from "./pages-wallet";

const ConsolePage = lazy(() => import("./pages-console")); // pulls in noVNC only when opened

export default function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route path="/" element={<Home />} />
        <Route path="/login" element={<Login />} />
        <Route path="/signup" element={<Signup />} />
        <Route path="/verify-email" element={<VerifyEmail />} />
        <Route path="/forgot-password" element={<ForgotPassword />} />
        <Route path="/reset-password" element={<ResetPassword />} />
        <Route path="/aup" element={<AUP />} />

        <Route element={<RequireAuth />}>
          <Route path="/vms" element={<VMList />} />
          <Route path="/vms/new" element={<VMCreate />} />
          <Route path="/vms/:id" element={<VMDetail />} />
          <Route path="/vms/:id/console" element={<Suspense fallback={null}><ConsolePage /></Suspense>} />
          <Route path="/ssh-keys" element={<SSHKeys />} />
          <Route path="/networking" element={<NetworkingPage />} />
          <Route path="/wallet" element={<WalletPage />} />
          <Route path="/statements" element={<StatementList />} />
          <Route path="/statements/:month" element={<StatementPage />} />
          <Route path="/account" element={<AccountPage />} />

          <Route element={<RequireAdmin />}>
            <Route path="/admin" element={<AdminLayout />}>
              <Route index element={<AdminUsers />} />
              <Route path="users/:id" element={<AdminUserDetail />} />
              <Route path="vms" element={<AdminVMs />} />
              <Route path="capacity" element={<AdminCapacity />} />
              <Route path="jobs" element={<AdminJobs />} />
              <Route path="revenue" element={<AdminRevenue />} />
              <Route path="catalogue" element={<AdminCatalogue />} />
            </Route>
          </Route>
        </Route>

        <Route path="*" element={<NotFound />} />
      </Route>
    </Routes>
  );
}
