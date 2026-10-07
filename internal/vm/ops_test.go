package vm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store/db"
)

// running provisions a VM so the operations have something to work on.
func (e *env) running(ip string) (id int64, vmid int) {
	e.t.Helper()
	id = e.seedVM(ip)
	must(e.t, e.call(e.prov, JobProvision, id, "", 1))
	must(e.t, e.st.Pool.QueryRow(context.Background(), `SELECT proxmox_vmid FROM vms WHERE id=$1`, id).Scan(&vmid))
	return id, vmid
}

func (e *env) claim(id int64, busy string, plan string) {
	e.t.Helper()
	ctx := context.Background()
	var userID int64
	must(e.t, e.st.Pool.QueryRow(ctx, `SELECT user_id FROM vms WHERE id=$1`, id).Scan(&userID))
	var planID pgtype.Int8
	if plan != "" {
		p, err := e.st.Q.GetActivePlanBySlug(ctx, plan)
		must(e.t, err)
		planID = pgtype.Int8{Int64: p.ID, Valid: true}
	}
	n, err := e.st.Q.ClaimVMBusy(ctx, db.ClaimVMBusyParams{ID: id, UserID: userID, Busy: pgtype.Text{String: busy, Valid: true}, ResizePlanID: planID})
	must(e.t, err)
	if n != 1 {
		e.t.Fatalf("claim %s = %d rows", busy, n)
	}
}

func (e *env) busy(id int64) (busy string, plan string) {
	var b, p *string
	must(e.t, e.st.Pool.QueryRow(context.Background(),
		`SELECT v.busy, p.slug FROM vms v JOIN plans p ON p.id = v.plan_id WHERE v.id=$1`, id).Scan(&b, &p))
	if b != nil {
		busy = *b
	}
	return busy, *p
}

func (e *env) callSnap(kind string, id, snap int64, attempt int) error {
	payload, _ := json.Marshal(Payload{VMID: id, SnapshotID: snap})
	return e.prov.Handlers()[kind](context.Background(), &jobs.Job{Kind: kind, Payload: payload, Attempts: attempt})
}

func TestResizeMovesToTheLargerPlan(t *testing.T) {
	e := newEnv(t)
	id, vmid := e.running("203.0.113.30")
	e.claim(id, "resizing", "small")

	must(t, e.call(e.prov, JobResize, id, "", 1))
	g := e.pve.VMs[vmid]
	if g.Cores != 2 || g.MemoryMB != 2048 || g.DiskGB != 40 || !g.Running {
		t.Fatalf("host after resize: %+v", g)
	}
	if busy, plan := e.busy(id); busy != "" || plan != "small" {
		t.Fatalf("busy=%q plan=%q, want released and small", busy, plan)
	}
	if got := e.state(id); got != "running" {
		t.Fatalf("state %s", got)
	}
}

func TestResizeOfAStoppedVMStaysStopped(t *testing.T) {
	e := newEnv(t)
	id, vmid := e.running("203.0.113.31")
	must(t, e.call(e.prov, JobPower, id, "stop", 1))
	e.claim(id, "resizing", "medium")
	must(t, e.call(e.prov, JobResize, id, "", 1))
	if g := e.pve.VMs[vmid]; g.Running || g.DiskGB != 80 || g.MemoryMB != 4096 {
		t.Fatalf("%+v", g)
	}
	if e.state(id) != "stopped" {
		t.Fatal("a stopped VM must stay stopped")
	}
}

// A retry after the disk was already grown must not grow it again (Proxmox refuses a same-size resize).
func TestResizeRetryIsIdempotent(t *testing.T) {
	e := newEnv(t)
	id, vmid := e.running("203.0.113.32")
	e.claim(id, "resizing", "small")
	e.pve.Fail["start"] = errors.New("boom")
	if err := e.call(e.prov, JobResize, id, "", 1); err == nil {
		t.Fatal("expected the start failure")
	}
	if busy, plan := e.busy(id); busy != "resizing" || plan != "nano" {
		t.Fatalf("an attempt that will be retried keeps the claim: busy=%q plan=%q", busy, plan)
	}
	delete(e.pve.Fail, "start")
	e.pve.Calls = nil
	must(t, e.call(e.prov, JobResize, id, "", 2))
	for _, c := range e.pve.Calls {
		if c == "resize:"+itoa(vmid) {
			t.Fatal("the disk was resized again on retry")
		}
	}
	if busy, plan := e.busy(id); busy != "" || plan != "small" {
		t.Fatalf("after the retry: busy=%q plan=%q", busy, plan)
	}
}

func TestResizeFinalFailureReleasesTheVMAndRestartsIt(t *testing.T) {
	e := newEnv(t)
	id, vmid := e.running("203.0.113.33")
	e.claim(id, "resizing", "small")
	e.pve.Fail["setresources"] = errors.New("boom")
	if err := e.call(e.prov, JobResize, id, "", jobs.MaxAttempts); err == nil {
		t.Fatal("expected failure")
	}
	if busy, plan := e.busy(id); busy != "" || plan != "nano" {
		t.Fatalf("a permanently failed resize must release the VM and keep the old plan: busy=%q plan=%q", busy, plan)
	}
	if !e.pve.VMs[vmid].Running {
		t.Fatal("the customer's VM must not be left powered off")
	}
}

func TestSnapshotCreateRestoreDelete(t *testing.T) {
	e := newEnv(t)
	id, vmid := e.running("203.0.113.34")
	ctx := context.Background()

	e.claim(id, "snapshotting", "")
	sid, err := e.st.Q.CreateSnapshot(ctx, db.CreateSnapshotParams{VmID: id, Name: "before upgrade"})
	must(t, err)
	must(t, e.st.Q.SetSnapshotPVEName(ctx, db.SetSnapshotPVENameParams{ID: sid, PveName: SnapshotName(sid)}))
	must(t, e.callSnap(JobSnapshot, id, sid, 1))
	if names := e.pve.VMs[vmid].Snapshots; len(names) != 1 || names[0] != SnapshotName(sid) {
		t.Fatalf("host snapshots %v", names)
	}
	snaps, _ := e.st.Q.ListVMSnapshots(ctx, id)
	if len(snaps) != 1 || snaps[0].Status != "ready" {
		t.Fatalf("rows %+v", snaps)
	}
	if busy, _ := e.busy(id); busy != "" {
		t.Fatal("snapshot must release the VM")
	}
	// A repeated job (lost reply) does not create it twice.
	e.claim(id, "snapshotting", "")
	must(t, e.callSnap(JobSnapshot, id, sid, 1))
	if len(e.pve.VMs[vmid].Snapshots) != 1 {
		t.Fatal("snapshot created twice")
	}

	e.claim(id, "restoring", "")
	must(t, e.callSnap(JobRestore, id, sid, 1))
	if !e.pve.VMs[vmid].Running || e.state(id) != "running" {
		t.Fatal("a running VM must be running again after restore")
	}
	if busy, _ := e.busy(id); busy != "" {
		t.Fatal("restore must release the VM")
	}

	e.claim(id, "snapshotting", "")
	must(t, e.callSnap(JobSnapshotDelete, id, sid, 1))
	if len(e.pve.VMs[vmid].Snapshots) != 0 {
		t.Fatal("snapshot not removed from the host")
	}
	if rows, _ := e.st.Q.ListVMSnapshots(ctx, id); len(rows) != 0 {
		t.Fatalf("row not removed: %+v", rows)
	}
}

func TestSnapshotFinalFailureIsMarkedAndReleased(t *testing.T) {
	e := newEnv(t)
	id, _ := e.running("203.0.113.35")
	ctx := context.Background()
	e.claim(id, "snapshotting", "")
	sid, _ := e.st.Q.CreateSnapshot(ctx, db.CreateSnapshotParams{VmID: id, Name: "x"})
	must(t, e.st.Q.SetSnapshotPVEName(ctx, db.SetSnapshotPVENameParams{ID: sid, PveName: SnapshotName(sid)}))
	e.pve.Fail["snapshot"] = errors.New("no space")
	if err := e.callSnap(JobSnapshot, id, sid, jobs.MaxAttempts); err == nil {
		t.Fatal("expected failure")
	}
	if busy, _ := e.busy(id); busy != "" {
		t.Fatal("must release after the last attempt")
	}
	if rows, _ := e.st.Q.ListVMSnapshots(ctx, id); len(rows) != 0 { // errored rows are hidden from the customer
		t.Fatalf("%+v", rows)
	}
	var status string
	must(t, e.st.Pool.QueryRow(ctx, `SELECT status FROM snapshots WHERE id=$1`, sid).Scan(&status))
	if status != "error" {
		t.Fatalf("status %s", status)
	}
}

func TestClaimIsOneAtATime(t *testing.T) {
	e := newEnv(t)
	id, _ := e.running("203.0.113.36")
	e.claim(id, "resizing", "small")
	ctx := context.Background()
	var userID int64
	must(t, e.st.Pool.QueryRow(ctx, `SELECT user_id FROM vms WHERE id=$1`, id).Scan(&userID))
	n, err := e.st.Q.ClaimVMBusy(ctx, db.ClaimVMBusyParams{ID: id, UserID: userID, Busy: pgtype.Text{String: "snapshotting", Valid: true}})
	must(t, err)
	if n != 0 {
		t.Fatal("a second claim on a busy VM must fail")
	}
	// Another user cannot claim it either.
	must(t, e.st.Q.ReleaseVMBusy(ctx, id))
	n, _ = e.st.Q.ClaimVMBusy(ctx, db.ClaimVMBusyParams{ID: id, UserID: userID + 999, Busy: pgtype.Text{String: "snapshotting", Valid: true}})
	if n != 0 {
		t.Fatal("only the owner can claim")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (e *env) claimRebuild(id int64, tpl, keys string) {
	e.t.Helper()
	ctx := context.Background()
	var userID int64
	must(e.t, e.st.Pool.QueryRow(ctx, `SELECT user_id FROM vms WHERE id=$1`, id).Scan(&userID))
	t, err := e.st.Q.GetActiveTemplateBySlug(ctx, tpl)
	must(e.t, err)
	n, err := e.st.Q.ClaimVMRebuild(ctx, db.ClaimVMRebuildParams{ID: id, UserID: userID,
		RebuildTemplateID: pgtype.Int8{Int64: t.ID, Valid: true}, RebuildKeys: pgtype.Text{String: keys, Valid: true}})
	must(e.t, err)
	if n != 1 {
		e.t.Fatalf("claim rebuild = %d rows", n)
	}
}

func TestRebuildReinstallsKeepingIPAndPlan(t *testing.T) {
	e := newEnv(t)
	id, vmid := e.running("203.0.113.40")
	ctx := context.Background()

	// A snapshot exists and the VM is stopped: the rebuild erases the snapshot and brings the VM up.
	sid, _ := e.st.Q.CreateSnapshot(ctx, db.CreateSnapshotParams{VmID: id, Name: "old"})
	must(t, e.st.Q.SetSnapshotPVEName(ctx, db.SetSnapshotPVENameParams{ID: sid, PveName: SnapshotName(sid)}))
	must(t, e.st.Q.SetSnapshotStatus(ctx, db.SetSnapshotStatusParams{ID: sid, Status: "ready"}))
	e.pve.VMs[vmid].Snapshots = []string{SnapshotName(sid)}
	must(t, e.call(e.prov, JobPower, id, "stop", 1))

	e.claimRebuild(id, "debian-12", "ssh-ed25519 NEWKEY")
	must(t, e.call(e.prov, JobRebuild, id, "", 1))

	g := e.pve.VMs[vmid]
	if g == nil || !g.Running || g.Template != 9001 || g.Config.IPConfig0 != "ip=203.0.113.40/32,gw=203.0.113.1" ||
		g.Config.SSHKeys != "ssh-ed25519 NEWKEY" || g.DiskGB != 20 || len(g.Snapshots) != 0 {
		t.Fatalf("guest after rebuild: %+v", g)
	}
	var tpl, keys, state string
	var busy *string
	must(t, e.st.Pool.QueryRow(ctx, `SELECT t.slug, v.authorized_keys, v.state, v.busy FROM vms v JOIN templates t ON t.id=v.template_id WHERE v.id=$1`, id).Scan(&tpl, &keys, &state, &busy))
	if tpl != "debian-12" || keys != "ssh-ed25519 NEWKEY" || state != "running" || busy != nil {
		t.Fatalf("row: %s %s %s busy=%v", tpl, keys, state, busy)
	}
	if rows, _ := e.st.Q.ListVMSnapshots(ctx, id); len(rows) != 0 {
		t.Fatalf("snapshot rows survived the rebuild: %+v", rows)
	}
	if h := e.ipHolder("203.0.113.40"); h == nil || *h != id {
		t.Fatal("the VM must keep its IP")
	}
}

func TestRebuildRetryStartsFromACleanSlate(t *testing.T) {
	e := newEnv(t)
	id, vmid := e.running("203.0.113.41")
	e.claimRebuild(id, "ubuntu-24.04", "ssh-ed25519 K")
	e.pve.Fail["start"] = errors.New("boom")
	if err := e.call(e.prov, JobRebuild, id, "", 1); err == nil {
		t.Fatal("expected the start failure")
	}
	if busy, _ := e.busy(id); busy != "rebuilding" {
		t.Fatalf("a retried attempt keeps the claim: %q", busy)
	}
	delete(e.pve.Fail, "start")
	must(t, e.call(e.prov, JobRebuild, id, "", 2)) // the half-built guest is replaced, not "already exists"
	if !e.pve.VMs[vmid].Running {
		t.Fatal("not running after the retry")
	}
	if busy, _ := e.busy(id); busy != "" {
		t.Fatal("not released")
	}
}

func TestRebuildFinalFailureMarksTheVMErroredAndStopsBilling(t *testing.T) {
	e := newEnv(t)
	id, vmid := e.running("203.0.113.42")
	e.claimRebuild(id, "ubuntu-24.04", "ssh-ed25519 K")
	e.pve.Fail["clone"] = errors.New("pool full")
	if err := e.call(e.prov, JobRebuild, id, "", jobs.MaxAttempts); err == nil {
		t.Fatal("expected failure")
	}
	if got := e.state(id); got != "error" {
		t.Fatalf("state %s, want error", got)
	}
	var until *string
	must(t, e.st.Pool.QueryRow(context.Background(), `SELECT billing_until::text FROM vms WHERE id=$1`, id).Scan(&until))
	if until == nil {
		t.Fatal("billing must stop for a VM the rebuild destroyed")
	}
	if _, ok := e.pve.VMs[vmid]; ok {
		t.Fatal("no half-built guest may be left on the host")
	}
	if busy, _ := e.busy(id); busy != "" {
		t.Fatal("released")
	}
}

func (e *env) setScript(id int64, script string) {
	e.t.Helper()
	_, err := e.st.Pool.Exec(context.Background(), `UPDATE vms SET boot_script=$2, boot_script_status='pending' WHERE id=$1`, id, script)
	must(e.t, err)
}

func (e *env) script(id int64) (status string, exit *int, out string, text *string) {
	e.t.Helper()
	var o *string
	must(e.t, e.st.Pool.QueryRow(context.Background(), `SELECT boot_script_status, boot_script_exit, boot_script_output, boot_script FROM vms WHERE id=$1`, id).Scan(&status, &exit, &o, &text))
	if o != nil {
		out = *o
	}
	return
}

func TestProvisionQueuesTheBootScriptWithTheStateChange(t *testing.T) {
	e := newEnv(t)
	id := e.seedVM("203.0.113.50")
	e.setScript(id, "echo registering with token abc123")
	must(t, e.call(e.prov, JobProvision, id, "", 1))
	var n int
	must(t, e.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE kind='vm.bootscript'`).Scan(&n))
	if n != 1 {
		t.Fatalf("boot script jobs queued with the VM running: %d", n)
	}
	// A VM without a script queues nothing.
	id2 := e.seedVM("203.0.113.51")
	must(t, e.call(e.prov, JobProvision, id2, "", 1))
	must(t, e.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE kind='vm.bootscript'`).Scan(&n))
	if n != 1 {
		t.Fatalf("a VM with no script must not queue one: %d jobs", n)
	}
}

func TestBootScriptRunsOnceAndIsErased(t *testing.T) {
	e := newEnv(t)
	id, vmid := e.running("203.0.113.52")
	e.setScript(id, "echo registering with token abc123")
	e.pve.ExecFn = func(int, []string, string) (int, string) { return 0, "registered\n" }

	must(t, e.call(e.prov, JobBootScript, id, "", 1))
	if len(e.pve.Execs) != 1 {
		t.Fatalf("execs: %+v", e.pve.Execs)
	}
	c := e.pve.Execs[0]
	if c.VMID != vmid || strings.Join(c.Command, " ") != "/bin/bash -s" || c.Stdin != "echo registering with token abc123" {
		t.Fatalf("the script must reach bash on stdin: %+v", c)
	}
	status, exit, out, text := e.script(id)
	if status != "ok" || exit == nil || *exit != 0 || out != "registered\n" {
		t.Fatalf("outcome: %s %v %q", status, exit, out)
	}
	if text != nil {
		t.Fatal("the script text (it may hold a token) must be erased after it ran")
	}
	// A repeated job does not run it again.
	must(t, e.call(e.prov, JobBootScript, id, "", 1))
	if len(e.pve.Execs) != 1 {
		t.Fatal("the script ran twice")
	}
}

func TestBootScriptFailureIsRecorded(t *testing.T) {
	e := newEnv(t)
	id, _ := e.running("203.0.113.53")
	e.setScript(id, "exit 3")
	e.pve.ExecFn = func(int, []string, string) (int, string) { return 3, strings.Repeat("x", 6000) + "END" }
	must(t, e.call(e.prov, JobBootScript, id, "", 1))
	status, exit, out, _ := e.script(id)
	if status != "failed" || exit == nil || *exit != 3 {
		t.Fatalf("outcome: %s %v", status, exit)
	}
	if len(out) > bootOutputTail+8 || !strings.HasSuffix(out, "END") || !strings.HasPrefix(out, "…") {
		t.Fatalf("output must keep only its tail: %d bytes", len(out))
	}
}

func TestBootScriptStartFailureRetriesThenFails(t *testing.T) {
	e := newEnv(t)
	id, _ := e.running("203.0.113.54")
	e.setScript(id, "echo hi")
	e.pve.Fail["exec"] = errors.New("guest agent not ready")
	if err := e.call(e.prov, JobBootScript, id, "", 1); err == nil {
		t.Fatal("a start failure is retried by the queue")
	}
	if status, _, _, _ := e.script(id); status != "pending" {
		t.Fatalf("a script that never started goes back to pending, got %s", status)
	}
	delete(e.pve.Fail, "exec")
	must(t, e.call(e.prov, JobBootScript, id, "", 2))
	if status, _, _, _ := e.script(id); status != "ok" {
		t.Fatalf("after the retry: %s", status)
	}

	id2, _ := e.running2("203.0.113.55")
	e.setScript(id2, "echo hi")
	e.pve.Fail["exec"] = errors.New("guest agent not ready")
	if err := e.call(e.prov, JobBootScript, id2, "", jobs.MaxAttempts); err == nil {
		t.Fatal("expected failure")
	}
	if status, exit, out, text := e.script(id2); status != "failed" || exit == nil || *exit != -1 || !strings.Contains(out, "guest agent") || text != nil {
		t.Fatalf("final failure: %s %v %q %v", status, exit, out, text)
	}
}

// A script that began and was then interrupted may have done half its work: it is never run again.
func TestInterruptedBootScriptIsNotRerun(t *testing.T) {
	e := newEnv(t)
	id, _ := e.running("203.0.113.56")
	e.setScript(id, "echo hi")
	_, err := e.st.Pool.Exec(context.Background(), `UPDATE vms SET boot_script_status='running' WHERE id=$1`, id)
	must(t, err)
	must(t, e.call(e.prov, JobBootScript, id, "", 1))
	if len(e.pve.Execs) != 0 {
		t.Fatal("an interrupted script must not be run a second time")
	}
	if status, _, out, text := e.script(id); status != "failed" || !strings.Contains(out, "interrupted") || text != nil {
		t.Fatalf("%s %q %v", status, out, text)
	}
}

func TestRebuildRunsTheNewBootScript(t *testing.T) {
	e := newEnv(t)
	id, _ := e.running("203.0.113.57")
	ctx := context.Background()
	var userID int64
	must(t, e.st.Pool.QueryRow(ctx, `SELECT user_id FROM vms WHERE id=$1`, id).Scan(&userID))
	tpl, _ := e.st.Q.GetActiveTemplateBySlug(ctx, "ubuntu-24.04")
	n, err := e.st.Q.ClaimVMRebuild(ctx, db.ClaimVMRebuildParams{ID: id, UserID: userID,
		RebuildTemplateID: pgtype.Int8{Int64: tpl.ID, Valid: true}, RebuildKeys: pgtype.Text{String: "ssh-ed25519 K", Valid: true},
		RebuildBootScript: pgtype.Text{String: "echo after rebuild", Valid: true}})
	must(t, err)
	if n != 1 {
		t.Fatal("claim")
	}
	must(t, e.call(e.prov, JobRebuild, id, "", 1))
	if status, _, _, _ := e.script(id); status != "pending" {
		t.Fatalf("a rebuild with a script leaves it pending: %s", status)
	}
	var jobsQueued int
	must(t, e.st.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind='vm.bootscript'`).Scan(&jobsQueued))
	if jobsQueued != 1 {
		t.Fatalf("boot script jobs: %d", jobsQueued)
	}
	must(t, e.call(e.prov, JobBootScript, id, "", 1))
	if status, _, _, _ := e.script(id); status != "ok" || len(e.pve.Execs) != 1 || e.pve.Execs[0].Stdin != "echo after rebuild" {
		t.Fatalf("after the job: %s %+v", status, e.pve.Execs)
	}
}

// running2 is running for a second VM in the same test (the fake host then holds two guests).
func (e *env) running2(ip string) (int64, int) {
	id := e.seedVM(ip)
	must(e.t, e.call(e.prov, JobProvision, id, "", 1))
	var vmid int
	must(e.t, e.st.Pool.QueryRow(context.Background(), `SELECT proxmox_vmid FROM vms WHERE id=$1`, id).Scan(&vmid))
	return id, vmid
}
