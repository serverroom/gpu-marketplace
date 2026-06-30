package control

import "log"

// StubProvisioner logs actions without touching hardware. It is the placeholder
// the agent uses until the real Kata/VFIO provisioner (Plan 4) replaces it.
type StubProvisioner struct {
	status string
}

func NewStubProvisioner() *StubProvisioner {
	return &StubProvisioner{status: "free"}
}

func (p *StubProvisioner) Provision(rentalID, renterPubkey string) error {
	log.Printf("[stub] provision rental=%s (no microVM yet)", rentalID)
	p.status = "rented"
	return nil
}

func (p *StubProvisioner) Teardown(rentalID string) error {
	log.Printf("[stub] teardown rental=%s (no wipe yet)", rentalID)
	p.status = "free"
	return nil
}

func (p *StubProvisioner) Status() string { return p.status }
