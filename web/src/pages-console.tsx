import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import RFB from "@novnc/novnc";
import { api } from "./api";
import { useVM } from "./hooks";
import { Banner, Button, Card, ErrorText, Loading, PageHeader } from "./ui";

type Phase = "idle" | "connecting" | "connected" | "closed" | "failed";

/**
 * Browser console for a VM. A POST (CSRF-protected) returns a one-time session id and the VNC password; the
 * VNC client then opens a same-origin websocket that our server bridges to the host. noVNC is loaded only
 * on this page.
 */
export default function ConsolePage() {
  const { id } = useParams();
  const vmq = useVM(id);
  const screen = useRef<HTMLDivElement>(null);
  const rfb = useRef<RFB | null>(null);
  const [phase, setPhase] = useState<Phase>("idle");
  const [error, setError] = useState<unknown>(null);

  const disconnect = useCallback(() => {
    rfb.current?.disconnect();
    rfb.current = null;
  }, []);

  const connect = useCallback(async () => {
    if (!screen.current) return;
    disconnect();
    setError(null);
    setPhase("connecting");
    try {
      const { session, password } = await api<{ session: string; password: string }>(`/vms/${id}/console`, { method: "POST", json: {} });
      const proto = window.location.protocol === "https:" ? "wss" : "ws";
      const url = `${proto}://${window.location.host}/v1/vms/${id}/console/ws?session=${session}`;
      const client = new RFB(screen.current, url, { credentials: { password } });
      client.scaleViewport = true;
      client.resizeSession = false;
      let opened = false;
      client.addEventListener("connect", () => {
        opened = true;
        setPhase("connected");
        client.focus();
      });
      client.addEventListener("disconnect", () => {
        if (rfb.current === client) rfb.current = null;
        setPhase(opened ? "closed" : "failed");
      });
      client.addEventListener("securityfailure", () => setError(new Error("The VM console refused the connection.")));
      rfb.current = client;
    } catch (e) {
      setError(e);
      setPhase("failed");
    }
  }, [id, disconnect]);

  // Connect once the VM is known to be running; always close on leaving the page.
  const running = vmq.data?.state === "running" && !vmq.data?.busy;
  useEffect(() => {
    if (running && phase === "idle") void connect();
  }, [running, phase, connect]);
  useEffect(() => disconnect, [disconnect]);

  if (vmq.isLoading) return <Loading />;
  if (vmq.error || !vmq.data) return <ErrorText error={vmq.error ?? new Error("VM not found")} />;
  const v = vmq.data;

  return (
    <div className="space-y-4">
      <PageHeader
        title={`Console: ${v.hostname}`}
        subtitle="A screen and keyboard connected to your VM. Use SSH for everyday work; the console is for when SSH is not available."
        actions={
          <Link to={`/vms/${v.id}`} className="text-sm font-medium text-indigo-600 underline-offset-2 hover:underline dark:text-indigo-400">
            Back to VM
          </Link>
        }
      />
      {!running && <Banner tone="warn">The console is available while the VM is running and not being resized or restored.</Banner>}
      <Card className="space-y-3">
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="secondary" disabled={phase !== "connected"} onClick={() => rfb.current?.sendCtrlAltDel()}>
            Send Ctrl+Alt+Del
          </Button>
          {phase === "connected" || phase === "connecting" ? (
            <Button variant="secondary" onClick={disconnect}>
              Disconnect
            </Button>
          ) : (
            <Button disabled={!running} onClick={() => void connect()}>
              {phase === "idle" ? "Connect" : "Reconnect"}
            </Button>
          )}
          <span className="text-sm text-slate-500 dark:text-slate-400" role="status">
            {phase === "connecting" && "Connecting…"}
            {phase === "connected" && "Connected. Click the screen to type."}
            {phase === "closed" && "Disconnected."}
            {phase === "failed" && "Could not connect."}
          </span>
        </div>
        <ErrorText error={error} />
        <div ref={screen} className="h-[70vh] min-h-72 w-full overflow-hidden rounded-lg bg-black" aria-label="VM console screen" />
        <p className="text-xs text-slate-500 dark:text-slate-400">The console closes after 15 minutes without activity.</p>
      </Card>
    </div>
  );
}
