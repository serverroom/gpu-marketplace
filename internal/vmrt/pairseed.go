package vmrt

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// What a pair VM finds on its first boot besides what every rental gets:
// /etc/hosts naming both machines, /etc/gpu-pair.json and a profile script
// describing the cable, the intra-pair key, and the link check that reports
// each cable on the serial console. Every value in them has been through
// ValidatePair.

// pairHosts is the pair VM's whole /etc/hosts. Both machines' names resolve
// over the cable and never through DNS or the rental network: this machine's
// name to its own address on the first link, the other machine's name (and
// "peer") to its address on the first link -- so `ping gpu-<8>-b` on machine a
// checks the cable -- and a name per link for both, counted from 1:
// <name>-l1 on the first link, <name>-l2 on the second.
func pairHosts(id string, p *PairOptions) string {
	self := PairHostname(id, p.Node)
	var b strings.Builder
	b.WriteString("127.0.0.1 localhost\n")
	b.WriteString("::1 localhost ip6-localhost ip6-loopback\n")
	b.WriteString("ff02::1 ip6-allnodes\n")
	b.WriteString("ff02::2 ip6-allrouters\n")
	for i, l := range p.Links {
		own, _, _ := net.ParseCIDR(l.CIDR)
		if i == 0 {
			fmt.Fprintf(&b, "%s %s %s-l%d\n", own, self, self, i+1)
			fmt.Fprintf(&b, "%s %s peer %s-l%d\n", l.PeerIP, p.PeerHostname, p.PeerHostname, i+1)
			continue
		}
		fmt.Fprintf(&b, "%s %s-l%d\n", own, self, i+1)
		fmt.Fprintf(&b, "%s %s-l%d\n", l.PeerIP, p.PeerHostname, i+1)
	}
	return b.String()
}

// pairInfo is /etc/gpu-pair.json.
type pairInfo struct {
	Node     string         `json:"node"`
	Hostname string         `json:"hostname"`
	Peer     string         `json:"peer"`
	MTU      int            `json:"mtu"`
	Links    []pairInfoLink `json:"links"`
}

type pairInfoLink struct {
	Name    string `json:"name"`
	MAC     string `json:"mac"`
	Address string `json:"address"`
	PeerIP  string `json:"peer_ip"`
}

func pairInfoJSON(id string, p *PairOptions) string {
	info := pairInfo{Node: p.Node, Hostname: PairHostname(id, p.Node), Peer: p.PeerHostname, MTU: p.MTU}
	for _, l := range p.Links {
		info.Links = append(info.Links, pairInfoLink{Name: l.Name, MAC: l.LocalMAC, Address: l.CIDR, PeerIP: l.PeerIP})
	}
	data, _ := json.MarshalIndent(info, "", "  ")
	return string(data) + "\n"
}

// pairProfile sets non-binding defaults for multi-node work: whatever the
// renter sets first wins.
func pairProfile(p *PairOptions) string {
	return strings.Join([]string{
		"# This machine is one of a linked pair with " + p.PeerHostname + ", cabled over ConnectX-7",
		"# (the links are in /etc/gpu-pair.json). Defaults only: set your own and they win.",
		`export NCCL_SOCKET_IFNAME="${NCCL_SOCKET_IFNAME:-cx7}"`,
		`export NCCL_IB_HCA="${NCCL_IB_HCA:-mlx5}"`,
		"# The links run RoCE v2 over IPv4. If NCCL picks the wrong GID, set NCCL_IB_GID_INDEX",
		"# to the IPv4 v2 entry that show_gids (or ibv_devinfo -v) lists for the port.",
	}, "\n") + "\n"
}

// linkCheckScript pings the other machine over every link with full-size,
// unfragmentable frames for up to ten minutes (the other machine may still be
// booting) and says once per link, on the serial console, whether it got
// through.
func linkCheckScript(p *PairOptions) string {
	lines := []string{
		"#!/bin/bash",
		"# Once per rental: can each cable carry full-size frames to the other machine?",
		"check() {",
		"  local i=$1 ifc=$2 peer=$3 end=$((SECONDS+600))",
		"  while [ $SECONDS -lt $end ]; do",
		fmt.Sprintf(`    if ping -c3 -W1 -M do -s %d -I "$ifc" "$peer" >/dev/null 2>&1; then`, p.MTU-28),
		`      /usr/local/sbin/gpuagent-say "` + markLink + ` ok $i"; return 0`,
		"    fi",
		"    sleep 5",
		"  done",
		`  /usr/local/sbin/gpuagent-say "` + markLink + ` fail $i"`,
		"}",
	}
	for i, l := range p.Links {
		lines = append(lines, fmt.Sprintf("check %d %s %s &", i, l.Name, l.PeerIP))
	}
	lines = append(lines, "wait")
	return strings.Join(lines, "\n") + "\n"
}

// writePairFiles adds a pair VM's files to write_files.
func writePairFiles(b *strings.Builder, id string, p *PairOptions) {
	writeText(b, "/etc/hosts", "0644", pairHosts(id, p))
	writeText(b, "/etc/gpu-pair.json", "0644", pairInfoJSON(id, p))
	writeText(b, "/etc/profile.d/gpu-pair.sh", "0644", pairProfile(p))
	writeText(b, "/usr/local/sbin/gpuagent-linkcheck", "0755", linkCheckScript(p))
	if k := p.IntraKey; k != nil {
		writeB64(b, "/root/.ssh/id_ed25519", "0600", []byte(k.PrivateOpenSSH), true)
		writeB64(b, "/root/.ssh/id_ed25519.pub", "0644", []byte(k.Public+"\n"), true)
	}
}

// PairTestPlan is what a pair's self-test VM measures besides the single
// self-test: the MTU over every link, then RDMA write bandwidth (ib_write_bw
// over RDMA CM), one link at a time -- machine a serves, machine b connects.
type PairTestPlan struct {
	Seconds  int // per link
	BasePort int // link i uses BasePort+i
}

// DefaultPairTestPlan is 5 seconds per link from port 18515.
var DefaultPairTestPlan = PairTestPlan{Seconds: 5, BasePort: 18515}

// pairTestScript reports, one marker line at a time: PAIR PING ok|fail <i>,
// RDMA <i> <Gb/s> or RDMAFAIL <i> <why>, NCCL skipped, PAIR END.
func pairTestScript(p *PairOptions, plan PairTestPlan) string {
	say := "/usr/local/sbin/gpuagent-say"
	m := markSelfTest
	size := p.MTU - 28
	lines := []string{
		"#!/bin/bash",
		say + " '" + m + " PAIR BEGIN'",
		"waitping() {",
		"  local end=$((SECONDS+600))",
		"  while [ $SECONDS -lt $end ]; do",
		fmt.Sprintf(`    ping -c3 -W1 -M do -s %d -I "$1" "$2" >/dev/null 2>&1 && return 0`, size),
		"    sleep 3",
		"  done",
		"  return 1",
		"}",
		// The RDMA device behind a netdev, from sysfs (rdma-core has no ibdev2netdev).
		`ibdev() { for d in /sys/class/infiniband/*; do [ -e "$d/device/net/$1" ] && { basename "$d"; return 0; }; done; return 1; }`,
		// The BW average column of ib_write_bw's result line.
		`bw() { awk '$1 ~ /^[0-9]+$/ && NF >= 4 && $4 ~ /^[0-9.]+$/ {v=$4} END {if (v != "") print v}'; }`,
	}
	for i, l := range p.Links {
		port := plan.BasePort + i
		secs := plan.Seconds
		lines = append(lines,
			fmt.Sprintf(`if waitping %s %s; then`, l.Name, l.PeerIP),
			fmt.Sprintf(`  %s "%s PAIR PING ok %d"`, say, m, i),
			fmt.Sprintf(`  if ! ibdev %s >/dev/null; then %s "%s RDMAFAIL %d no RDMA device behind %s (mlx5_ib)";`, l.Name, say, m, i, l.Name),
		)
		if p.Node == "a" {
			lines = append(lines,
				fmt.Sprintf(`  else out=$(timeout 300 ib_write_bw -R -F --report_gbits -D %d -p %d 2>&1)`, secs, port))
		} else {
			lines = append(lines,
				"  else out=''; end=$((SECONDS+300))",
				"    while [ $SECONDS -lt $end ]; do",
				fmt.Sprintf(`      out=$(timeout 60 ib_write_bw -R -F --report_gbits -D %d -p %d %s 2>&1) && break`, secs, port, l.PeerIP),
				"      sleep 3",
				"    done")
		}
		lines = append(lines,
			`    v=$(printf '%s\n' "$out" | bw)`,
			fmt.Sprintf(`    if [ -n "$v" ]; then %s "%s RDMA %d $v"; else %s "%s RDMAFAIL %d $(printf '%%s' "$out" | tail -c 200 | tr '\n' ' ')"; fi`, say, m, i, say, m, i),
			"  fi",
			"else",
			fmt.Sprintf(`  %s "%s PAIR PING fail %d"`, say, m, i),
			"fi",
		)
	}
	lines = append(lines, say+" '"+m+" NCCL skipped'", say+" '"+m+" PAIR END'")
	return strings.Join(lines, "\n") + "\n"
}
