// Package accounts holds account-level actions shared by the admin API and the operator CLI.
package accounts

import (
	"context"

	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/vm"
)

// Ban marks the account banned, signs it out everywhere and queues suspension
// of its running and stopped VMs (their disks are kept; deleting is a separate
// decision). It returns how many VMs were queued for suspension.
func Ban(ctx context.Context, st *store.Store, q *jobs.Queue, userID int64) (int, error) {
	if _, err := st.Q.SetUserStatusByID(ctx, db.SetUserStatusByIDParams{ID: userID, Status: "banned"}); err != nil {
		return 0, err
	}
	if err := st.Q.DeleteUserSessions(ctx, userID); err != nil {
		return 0, err
	}
	ids, err := st.Q.ListUserLiveVMIDs(ctx, userID)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := q.Enqueue(ctx, vm.JobSuspend, vm.Payload{VMID: id}); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// Suspend marks the account suspended and queues suspension of its running and stopped VMs. Unlike Ban the
// customer can still sign in (to see why, fund the wallet, or delete VMs), but cannot start anything.
func Suspend(ctx context.Context, st *store.Store, q *jobs.Queue, userID int64) (int, error) {
	if _, err := st.Q.SetUserStatusByID(ctx, db.SetUserStatusByIDParams{ID: userID, Status: "suspended"}); err != nil {
		return 0, err
	}
	ids, err := st.Q.ListUserLiveVMIDs(ctx, userID)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := q.Enqueue(ctx, vm.JobSuspend, vm.Payload{VMID: id}); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// Reactivate returns the account to active and queues the resume of VMs that were suspended on the account's
// behalf. VMs suspended for an unpaid balance (the user is in grace) are left to the billing path, which
// restores them only once the wallet covers them.
func Reactivate(ctx context.Context, st *store.Store, q *jobs.Queue, userID int64) (int, error) {
	u, err := st.Q.GetUserByID(ctx, userID)
	if err != nil {
		return 0, err
	}
	if _, err := st.Q.SetUserStatusByID(ctx, db.SetUserStatusByIDParams{ID: userID, Status: "active"}); err != nil {
		return 0, err
	}
	if u.GraceStartedAt.Valid || u.Status == "active" {
		return 0, nil
	}
	vms, err := st.Q.ListUserSuspendedVMs(ctx, userID)
	if err != nil {
		return 0, err
	}
	for _, v := range vms {
		if err := q.Enqueue(ctx, vm.JobResume, vm.Payload{VMID: v.ID}); err != nil {
			return 0, err
		}
	}
	return len(vms), nil
}
