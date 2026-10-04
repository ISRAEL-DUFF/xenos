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
