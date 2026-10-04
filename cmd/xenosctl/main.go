// Command xenosctl is the operator CLI for tasks that have no admin UI yet.
//
//	xenosctl ip add <first>-<last> <gateway> [region]   load addresses into the IP pool
//	xenosctl ip list                                   show the pool and what uses each address
//	xenosctl admin grant <email>                       make a user an admin
//	xenosctl user limit <email> <n>                    set a user's VM limit
package main

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: xenosctl ip add|list | admin grant <email> | user limit <email> <n>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	switch {
	case len(args) >= 4 && args[0] == "ip" && args[1] == "add":
		region := cfg.Region
		if len(args) > 4 {
			region = args[4]
		}
		return ipAdd(ctx, st, args[2], args[3], region)
	case len(args) == 2 && args[0] == "ip" && args[1] == "list":
		return ipList(ctx, st)
	case len(args) == 3 && args[0] == "admin" && args[1] == "grant":
		return setAdmin(ctx, st, args[2])
	case len(args) == 4 && args[0] == "user" && args[1] == "limit":
		n, err := strconv.Atoi(args[3])
		if err != nil || n < 0 {
			return fmt.Errorf("limit must be a non-negative integer")
		}
		return setLimit(ctx, st, args[2], n)
	}
	return fmt.Errorf("unknown command %q", strings.Join(args, " "))
}

func ipAdd(ctx context.Context, st *store.Store, rng, gateway, region string) error {
	first, last, ok := strings.Cut(rng, "-")
	if !ok {
		last = first
	}
	a, err := netip.ParseAddr(first)
	if err != nil {
		return fmt.Errorf("first address: %w", err)
	}
	b, err := netip.ParseAddr(last)
	if err != nil {
		return fmt.Errorf("last address: %w", err)
	}
	gw, err := netip.ParseAddr(gateway)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	if !a.Is4() || !b.Is4() || !gw.Is4() || b.Less(a) {
		return fmt.Errorf("need an IPv4 range first<=last and an IPv4 gateway")
	}
	n := 0
	for ip := a; ; ip = ip.Next() {
		if ip == gw {
			return fmt.Errorf("gateway %s is inside the range", gw)
		}
		tag, err := st.Pool.Exec(ctx,
			`INSERT INTO ip_addresses (address, gateway, region) VALUES ($1::inet, $2::inet, $3) ON CONFLICT (address) DO NOTHING`,
			ip.String(), gw.String(), region)
		if err != nil {
			return err
		}
		n += int(tag.RowsAffected())
		if ip == b {
			break
		}
	}
	fmt.Printf("added %d address(es) to %s\n", n, region)
	return nil
}

func ipList(ctx context.Context, st *store.Store) error {
	rows, err := st.Pool.Query(ctx,
		`SELECT host(address), host(gateway), region, COALESCE(vm_id::text, '-') FROM ip_addresses ORDER BY address`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a, g, r, v string
		if err := rows.Scan(&a, &g, &r, &v); err != nil {
			return err
		}
		fmt.Printf("%-16s gw %-16s %-10s vm %s\n", a, g, r, v)
	}
	return rows.Err()
}

func setAdmin(ctx context.Context, st *store.Store, email string) error {
	tag, err := st.Pool.Exec(ctx, `UPDATE users SET is_admin = TRUE WHERE email = $1`, strings.ToLower(email))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no user with email %s", email)
	}
	fmt.Println("ok")
	return nil
}

func setLimit(ctx context.Context, st *store.Store, email string, n int) error {
	tag, err := st.Pool.Exec(ctx, `UPDATE users SET vm_limit = $2 WHERE email = $1`, strings.ToLower(email), n)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no user with email %s", email)
	}
	fmt.Println("ok")
	return nil
}
