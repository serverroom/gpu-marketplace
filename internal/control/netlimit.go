package control

import "fmt"

// NetLimit is the host's bandwidth limit the marketplace sends with a rental
// (POST /provision net_limit, agents after v0.3.18): the speed to the rental
// and from it in Mbit/s, a monthly data allowance in GB (10^9 bytes, both
// directions together), and the speed each way once that is used. 0 is none.
type NetLimit struct {
	DownMbps  int `json:"down_mbps,omitempty"`
	UpMbps    int `json:"up_mbps,omitempty"`
	MonthlyGB int `json:"monthly_gb,omitempty"`
	AfterMbps int `json:"after_mbps,omitempty"`
}

// The bounds the control API takes; vmrt.NetLimit.Validate holds the same.
const (
	maxLimitMbps = 100000
	maxMonthlyGB = 10000000
	maxAfterMbps = 10000
)

// Validate says what is wrong with a limit, or nil.
func (l NetLimit) Validate() error {
	switch {
	case l.DownMbps < 0 || l.DownMbps > maxLimitMbps, l.UpMbps < 0 || l.UpMbps > maxLimitMbps:
		return fmt.Errorf("net_limit: a speed limit is from 1 to %d Mbit/s, or 0 for none", maxLimitMbps)
	case l.MonthlyGB < 0 || l.MonthlyGB > maxMonthlyGB:
		return fmt.Errorf("net_limit: a monthly allowance is from 1 to %d GB, or 0 for none", maxMonthlyGB)
	case l.AfterMbps < 0 || l.AfterMbps > maxAfterMbps:
		return fmt.Errorf("net_limit: the speed after the allowance is from 1 to %d Mbit/s", maxAfterMbps)
	}
	return nil
}

// Empty reports whether the limit limits nothing.
func (l NetLimit) Empty() bool { return l.DownMbps == 0 && l.UpMbps == 0 && l.MonthlyGB == 0 }

// NetLimits is whether this machine can hold a rental to a host's bandwidth
// limit (Capability.NetLimits, agents after v0.3.18). Usable once a test boot
// of this agent version on this kernel held its test rental to one and read it
// back; Pending until a test boot has tried; Why is what stopped it.
type NetLimits struct {
	Usable  bool   `json:"usable"`
	Pending bool   `json:"pending,omitempty"`
	Why     string `json:"why,omitempty"`
}

// LimitedProvisioner is a WaitingProvisioner that also holds the rental to
// the host's bandwidth limit.
type LimitedProvisioner interface {
	ProvisionLimited(rentalID, renterPubkey string, startBy int64, limit NetLimit) (*PendingRental, error)
}
