package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
)

// hostFlag removes "--host <name>" from args. Without it a single-host install uses "default"; with a hosts file the
// operator must say which host the addresses belong to.
func hostFlag(cfg config.Config, args []string) ([]string, string, error) {
	rest, host := takeValue(args, "--host")
	if host != "" {
		return rest, host, nil
	}
	if cfg.HostsFile != "" {
		return nil, "", fmt.Errorf("a hosts file is configured: say which host with --host <name>")
	}
	return rest, "default", nil
}

// hostCmd handles "host list|drain|enable|disable".
func hostCmd(ctx context.Context, cfg config.Config, st *store.Store, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "host" {
		return false, nil
	}
	usage := fmt.Errorf("usage: xenosctl host list | drain <name> | enable <name> | disable <name>")
	if len(args) < 2 {
		return true, usage
	}
	switch args[1] {
	case "list":
		return true, hostList(ctx, st)
	case "drain", "enable", "disable":
		if len(args) != 3 {
			return true, usage
		}
		status := map[string]string{"drain": "draining", "enable": "active", "disable": "disabled"}[args[1]]
		n, err := st.Q.SetHostStatus(ctx, db.SetHostStatusParams{Name: args[2], Status: status})
		if err != nil {
			return true, err
		}
		if n == 0 {
			return true, fmt.Errorf("no host called %s (start the API or worker once with it configured, then try again)", args[2])
		}
		fmt.Printf("host %s is now %s\n", args[2], status)
		if status != "active" {
			fmt.Println("no new VMs are placed there; VMs already on it keep running (moving a VM between hosts is not supported)")
		}
		return true, hostList(ctx, st)
	}
	return true, usage
}

func hostList(ctx context.Context, st *store.Store) error {
	rows, err := st.Pool.Query(ctx, `
		SELECT h.name, h.status,
		       (SELECT count(*) FROM vms v WHERE v.host = h.name AND v.state NOT IN ('deleted'))::int,
		       (SELECT count(*) FROM ip_addresses i WHERE i.host = h.name AND i.vm_id IS NULL)::int,
		       (SELECT COALESCE(sum(p.ram_mb), 0) FROM vms v JOIN plans p ON p.id = v.plan_id WHERE v.host = h.name AND v.state IN ('pending','provisioning','running','stopped'))::bigint
		FROM hosts h ORDER BY h.name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tSTATUS\tVMS\tFREE IPS\tCOMMITTED RAM (MB)")
	for rows.Next() {
		var name, status string
		var vms, free int
		var ram int64
		if err := rows.Scan(&name, &status, &vms, &free, &ram); err != nil {
			return err
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", name, status, vms, free, ram)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tw.Flush()
}
