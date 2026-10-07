package metrics

import "fmt"

func fmtSscanf(s string, v *float64) (int, error) { return fmt.Sscanf(s, "%g", v) }
