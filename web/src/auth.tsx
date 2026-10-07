import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { api, ApiError, setCSRF, type SessionResponse, type User } from "./api";

interface AuthState {
  user: User | null;
  loading: boolean;
  signup: (email: string, password: string, phone: string) => Promise<void>;
  login: (email: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
}

const Ctx = createContext<AuthState>(null!);
export const useAuth = () => useContext(Ctx);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null);
  const [loading, setLoading] = useState(true);
  const qc = useQueryClient();

  const apply = (s: SessionResponse | null) => {
    setCSRF(s?.csrf_token ?? "");
    setUser(s?.user ?? null);
  };

  const refresh = async () => {
    try {
      apply(await api<SessionResponse>("/auth/me"));
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 401)) throw e;
      apply(null);
    }
  };

  useEffect(() => {
    refresh().finally(() => setLoading(false));
  }, []);

  const value: AuthState = {
    user,
    loading,
    refresh,
    signup: async (email, password, phone) => apply(await api<SessionResponse>("/auth/signup", { json: { email, password, phone, accept_aup: true } })),
    login: async (email, password) => apply(await api<SessionResponse>("/auth/login", { json: { email, password } })),
    logout: async () => {
      await api("/auth/logout", { method: "POST" });
      apply(null);
      qc.clear();
    },
  };
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}
