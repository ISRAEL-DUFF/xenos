# Issues found during the rehearsal

| ID | Step | Symptom | Root cause | Fix | Verified by | Where fixed | Real-server impact |
|---|---|---|---|---|---|---|---|
| R-1 | 0a, 0b | `ssh` to the VPS times out (still after the user confirmed it works from their own computer); ports probe as "open" on 80/443 but nothing real answers | the sandbox has no raw outbound TCP; its HTTP(S) proxy does not forward port 22 (not even to github.com) | not fixable from inside the sandbox: it carries only web traffic on every port (probed 22, 80, 443, 2222, 4443, 8080, 8443). Decision: continue another way (user-run commands, or a session on the user's own computer) | n/a | environment settings (not code) | none: this only concerns how Claude reaches a test host |
| R-2 | 0a | the `SSH_*` variables saved in the environment were empty in the running session | environment variables are read at session start | reload the environment (done) | variables present in step 0b | none | none |
| R-3 | 0a | `apt-get install openssh-client sshpass` failed with a 404 | stale package index in the sandbox | `apt-get update` first | `ssh` and `sshpass` installed | none | none |
