package vmrt

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// A container rental's seccomp profile: podman's own default, with one change.
// That default allows clone, clone3 and unshare unconditionally, so a renter --
// no capabilities, no privilege -- can still make a new user namespace
// (`unshare -Urn`) and be root inside it, which is how the usual nf_tables and
// other namespace-reachable kernel bugs are reached. Docker's default refuses
// that; this profile does too, the same way: clone and unshare are allowed only
// without CLONE_NEWUSER (EPERM with it), and clone3, whose flags seccomp cannot
// read, answers ENOSYS so the C library falls back to clone. Every other rule
// is the host's own, so a newer podman's allowances carry over.

//go:embed seccomp_default.json
var embeddedSeccompDefault []byte

// seccompDefaultPaths are where podman reads its default profile from, the
// administrator's override first (containers.conf's seccomp_profile default).
var seccompDefaultPaths = []string{"/etc/containers/seccomp.json", "/usr/share/containers/seccomp.json"}

const (
	cloneNewUser = 0x10000000 // CLONE_NEWUSER
	errnoEPERM   = 1
	errnoENOSYS  = 38
)

// RentalSeccompProfile is the profile a rental container runs under: the host's
// default (or, when the host has none on disk, the podman 4.9 default this agent
// carries -- byte for byte Ubuntu 24.04's), less new user namespaces.
func RentalSeccompProfile(h Host) ([]byte, error) {
	base := embeddedSeccompDefault
	for _, p := range seccompDefaultPaths {
		if data, err := h.ReadFile(p); err == nil && len(data) > 0 {
			base = data
			break
		}
	}
	return denyNewUserNamespaces(base)
}

// denyNewUserNamespaces rewrites a seccomp profile so no process in the
// container can create a user namespace. Unknown fields are kept as they are.
func denyNewUserNamespaces(profile []byte) ([]byte, error) {
	var p map[string]interface{}
	if err := json.Unmarshal(profile, &p); err != nil {
		return nil, fmt.Errorf("parse seccomp profile: %w", err)
	}
	rules, _ := p["syscalls"].([]interface{})
	if rules == nil {
		return nil, fmt.Errorf("seccomp profile has no syscall rules")
	}
	gated := map[string]bool{"clone": true, "clone3": true, "unshare": true}
	var kept []interface{}
	for _, r := range rules {
		rule, ok := r.(map[string]interface{})
		if !ok {
			kept = append(kept, r)
			continue
		}
		names := ruleNames(rule)
		var rest []interface{}
		touched := false
		for _, n := range names {
			if gated[n] {
				touched = true
				continue
			}
			rest = append(rest, n)
		}
		if !touched {
			kept = append(kept, rule)
			continue
		}
		// Every rule that let these through goes, whatever its conditions: the
		// three rules below are the whole of what is allowed for them.
		if len(rest) == 0 {
			continue
		}
		rule["names"] = rest
		delete(rule, "name")
		kept = append(kept, rule)
	}
	flags := func(value int) []interface{} {
		return []interface{}{map[string]interface{}{"index": 0, "value": cloneNewUser, "valueTwo": value, "op": "SCMP_CMP_MASKED_EQ"}}
	}
	kept = append(kept,
		map[string]interface{}{"names": []interface{}{"clone", "unshare"}, "action": "SCMP_ACT_ALLOW", "args": flags(0)},
		map[string]interface{}{"names": []interface{}{"clone", "unshare"}, "action": "SCMP_ACT_ERRNO", "errnoRet": errnoEPERM, "args": flags(cloneNewUser)},
		map[string]interface{}{"names": []interface{}{"clone3"}, "action": "SCMP_ACT_ERRNO", "errnoRet": errnoENOSYS},
	)
	p["syscalls"] = kept
	return json.MarshalIndent(p, "", "  ")
}

// ruleNames is a rule's syscalls: "names", or the older single "name".
func ruleNames(rule map[string]interface{}) []string {
	var out []string
	if list, ok := rule["names"].([]interface{}); ok {
		for _, n := range list {
			if s, ok := n.(string); ok {
				out = append(out, s)
			}
		}
	}
	if s, ok := rule["name"].(string); ok && s != "" {
		out = append(out, s)
	}
	return out
}
