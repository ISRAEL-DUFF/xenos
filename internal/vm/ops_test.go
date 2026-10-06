package vm

import (
	"context"
	"encoding/json"
	"errors"
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
	for k := range e.pve.VMs {
		vmid = k
	}
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
