package billing

import (
	"fmt"
	"time"
)

// ChargeKey is the idempotency key for one VM-hour. Retrying a missed hour
// reuses the key, so hours are charged late but never twice.
func ChargeKey(vmID int64, hour time.Time) string {
	return fmt.Sprintf("vm:%d:hour:%s", vmID, hour.UTC().Format("2006010215"))
}

// FloatingChargeKey is the idempotency key for one floating-IP-hour.
func FloatingChargeKey(id int64, hour time.Time) string {
	return fmt.Sprintf("fip:%d:hour:%s", id, hour.UTC().Format("2006010215"))
}

// MonthlyCapReached reports whether charges so far this month hit the plan cap.
func MonthlyCapReached(chargedThisMonth, capUUSDT int64) bool {
	return chargedThisMonth >= capUUSDT
}

// RunwayHours is how many hours of usage a balance covers.
func RunwayHours(balanceUUSDT, hourlyUUSDT int64) int64 {
	if hourlyUUSDT <= 0 {
		return 0
	}
	return balanceUUSDT / hourlyUUSDT
}
