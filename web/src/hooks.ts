import { useQuery } from "@tanstack/react-query";
import { api, type Plan, type SSHKey, type Template, type VM, type Wallet } from "./api";

/** VM pages poll so provisioning -> running appears without a refresh. */
export const VM_POLL_MS = 5000;

export const useVMs = () => useQuery({ queryKey: ["vms"], queryFn: () => api<VM[]>("/vms"), refetchInterval: VM_POLL_MS });
export const useVM = (id: string | undefined) =>
  useQuery({ queryKey: ["vm", id], queryFn: () => api<VM>(`/vms/${id}`), refetchInterval: VM_POLL_MS, enabled: !!id });
export const useWallet = () => useQuery({ queryKey: ["wallet"], queryFn: () => api<Wallet>("/wallet"), refetchInterval: 30_000 });
export const usePlans = () => useQuery({ queryKey: ["plans"], queryFn: () => api<Plan[]>("/plans"), staleTime: 5 * 60_000 });
export const useTemplates = () => useQuery({ queryKey: ["templates"], queryFn: () => api<Template[]>("/templates"), staleTime: 5 * 60_000 });
export const useSSHKeys = () => useQuery({ queryKey: ["ssh-keys"], queryFn: () => api<SSHKey[]>("/ssh-keys") });
