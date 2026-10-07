# Rehearsal log (chronological)

Conventions: see [README.md](README.md). Times are UTC. The VPS address is written `<vps-ip>`; passwords and keys are never recorded.

---

### Step 0a: Reach the VPS from the Claude environment   FAIL (open)
2026-10-07 ~12:00 UTC

**Goal:** open an SSH session to the rehearsal VPS from the cloud sandbox where Claude works, so the runbook can be carried out and logged without relaying commands through a person.

**What was done**
1. The user supplied `SSH_HOST`, `SSH_USER`, `SSH_PASSWORD`. First the variables were looked for in the sandbox environment: **none of the three existed** (`env | cut -d= -f1 | grep ^SSH_` printed nothing) and `~/.ssh` was empty. The user then pasted the values into the chat as well (this is why the password should be rotated, see notes).
2. `ssh`/`sshpass` were not installed. `apt-get install openssh-client sshpass` failed first with a 404 on a stale package index; `apt-get update` then `apt-get install` succeeded.
3. TCP probes with `bash -c '</dev/tcp/<vps-ip>/<port>'`: port 22 blocked, 8006 blocked, 2222 and 2200 blocked; 80 and 443 "open". A control probe to an unrelated address (`1.1.1.1:22`) was also blocked, and `1.1.1.1:443` "open", so the pattern is the sandbox's, not the VPS's.
4. `curl http://<vps-ip>/` returned `HTTP/1.1 503`, `content-length: 121`, `content-type: text/plain`. With `--noproxy '*'` the body was "upstream connect error or disconnect/reset before headers ... remote connection failure". That text is the sandbox egress proxy's, not a web server on the VPS.
5. `curl $HTTPS_PROXY/__agentproxy/status` showed the sandbox sends all outbound traffic through a proxy that only relays HTTP(S); `noProxy` lists internal ranges and a few package registries only.
6. HTTP `CONNECT` through the proxy to `<vps-ip>:22`, `<vps-ip>:443` and, as a control, `github.com:22` all returned `200 Connection Established`, but no SSH banner ever arrived (10 s timeout) for any of them. So the proxy accepts the tunnel request and then does not forward port 22.

**Result:** no SSH session could be opened. Nothing was changed on the VPS.

**Root cause (established, not guessed):** the sandbox has no raw outbound TCP at all. The "open" results for 80/443 were the local proxy answering, not the VPS (`1.1.1.1:443` showed the same and the 503 body is the proxy's error text). Outbound port 22 is not forwarded even through `CONNECT`, to any host (GitHub included), so this is the environment's network policy and not a problem with the VPS or its firewall.

**What is needed to proceed (either one):**
- Allow outbound SSH to the VPS in the environment's network settings (the cloud environment menu in the session title bar, Edit, Network access: a broader access level, or the VPS address under Allowed domains), then start a new session so it takes effect; **or**
- Make the VPS's sshd also listen on port 443 (provider console: add `Port 443` to `/etc/ssh/sshd_config`, `systemctl restart ssh`), because the proxy does tunnel HTTPS-port traffic and an `ssh -o ProxyCommand=` through `CONNECT <vps-ip>:443` can then be tried.

**Notes for the team**
- The environment variables the user "saved" were not visible to this session. Environment variable changes apply to *new* sessions.
- The password was pasted into the chat. Treat it as exposed: change it (`passwd`) or reinstall the VPS when the rehearsal ends.
- `ssh` and `sshpass` are not preinstalled in the sandbox; installing them needs `apt-get update` first.

---

### Step 0b: Retry after the user confirmed SSH works from their own computer   FAIL (open)
2026-10-07 ~12:30 UTC

**What was done:** checked the sandbox again. The `SSH_HOST`, `SSH_USER`, `SSH_PASSWORD` variables are now present (R-2 resolved: the environment was reloaded). `ssh` and `sshpass` are installed. Then, with the password passed through `sshpass -e` (never on a command line or in a file): `ssh -o ConnectTimeout=10 root@<vps-ip> 'echo connected'`.

**Result:** `ssh: connect to host <vps-ip> port 22: Connection timed out`. A raw TCP probe to port 22 is blocked and an HTTP `CONNECT` tunnel through the sandbox proxy to `<vps-ip>:22` and `:443` receives no SSH banner within 10 s.

**Root cause:** same as 0a. That the user can log in from their own computer proves the VPS and its credentials are fine; it says nothing about the sandbox, whose outbound network policy still does not forward port 22. Nothing about the VPS is wrong.

**Still needed:** the environment's Network access setting must allow the VPS (or SSH), or sshd must also listen on 443 and that must be reachable through the proxy.

---

### Step 0c: Retry with Network access set to "Full"   FAIL (open)
2026-10-07 ~12:50 UTC

The user showed the environment settings: Network access is already **Full** (and the three `SSH_*` variables are set). Retested from the sandbox: raw TCP to `<vps-ip>:22` still blocked, `ssh` still `Connection timed out`, proxy status unchanged (`enabled: true`, `selective: false`).

**Conclusion:** the access levels decide which *domains* the sandbox's proxy will relay, and the proxy relays web (HTTP/HTTPS) traffic only. Even at "Full", a raw SSH connection on port 22 is not a thing the proxy forwards. This is a property of the sandbox, not of the setting or the VPS.

**Next to try:** have the VPS's sshd also listen on port 443, then `ssh -o ProxyCommand` through an HTTP `CONNECT` to `<vps-ip>:443` (the proxy does open tunnels to port 443). The result of that attempt is the next entry.
