# Issues found during the rehearsal

| ID | Step | Symptom | Root cause | Fix | Verified by | Where fixed | Real-server impact |
|---|---|---|---|---|---|---|---|
| R-1 | 0a | `ssh` to the VPS times out; ports probe as "open" on 80/443 but nothing real answers | the sandbox has no raw outbound TCP; its HTTP(S) proxy does not forward port 22 (not even to github.com) | open: allow SSH in the environment network settings, or run sshd on 443 on the VPS | not yet | environment settings (not code) | none: this only concerns how Claude reaches a test host |
| R-2 | 0a | the `SSH_*` variables saved in the environment were empty in the running session | environment variables are read at session start | start a new session after saving them | not yet | none | none |
| R-3 | 0a | `apt-get install openssh-client sshpass` failed with a 404 | stale package index in the sandbox | `apt-get update` first | `ssh` and `sshpass` installed | none | none |
