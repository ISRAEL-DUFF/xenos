import { Route, Routes } from "react-router-dom";
import { Layout, RequireAdmin, RequireAuth } from "./Layout";
import { AdminCapacity, AdminJobs, AdminLayout, AdminRevenue, AdminUserDetail, AdminUsers, AdminVMs } from "./pages-admin";
import { AccountPage } from "./pages-account";
import { ForgotPassword, Login, ResetPassword, Signup, VerifyEmail } from "./pages-auth";
import { AUP, Home, NotFound } from "./pages-misc";
import { SSHKeys } from "./pages-sshkeys";
import { VMCreate, VMDetail, VMList } from "./pages-vms";
import { WalletPage } from "./pages-wallet";

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
          <Route path="/ssh-keys" element={<SSHKeys />} />
          <Route path="/wallet" element={<WalletPage />} />
          <Route path="/account" element={<AccountPage />} />

          <Route element={<RequireAdmin />}>
            <Route path="/admin" element={<AdminLayout />}>
              <Route index element={<AdminUsers />} />
              <Route path="users/:id" element={<AdminUserDetail />} />
              <Route path="vms" element={<AdminVMs />} />
              <Route path="capacity" element={<AdminCapacity />} />
              <Route path="jobs" element={<AdminJobs />} />
              <Route path="revenue" element={<AdminRevenue />} />
            </Route>
          </Route>
        </Route>

        <Route path="*" element={<NotFound />} />
      </Route>
    </Routes>
  );
}
