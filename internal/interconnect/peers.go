package interconnect

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// Peers other agents announced on this machine's ConnectX-7 ports are kept on
// disk for a day, so an agent restart does not forget a cable it heard an hour
// ago, and a cable unplugged yesterday drops out of the report on its own.
const (
	PeerMaxAge    = 24 * 60 * 60
	peersKeptMax  = 16
	peersFileName = "peers.json"
)

// PeersPath is where the peers heard are kept.
func PeersPath(dataDir string) string { return filepath.Join(dataDir, peersFileName) }

// LoadPeers returns every peer on disk (nil when none, or unreadable).
func LoadPeers(h vmrt.Host, dataDir string) []control.Peer {
	data, err := h.ReadFile(PeersPath(dataDir))
	if err != nil {
		return nil
	}
	var peers []control.Peer
	if json.Unmarshal(data, &peers) != nil {
		return nil
	}
	return peers
}

func peerKey(p control.Peer) string { return p.ListingID + "|" + p.LocalMAC + "|" + p.PeerMAC }

// MergePeers adds what was just heard to what was known, keeps the newest
// sighting of each (listing, local port, peer port), and drops what is older
// than a day.
func MergePeers(known, heard []control.Peer, now int64) []control.Peer {
	by := map[string]control.Peer{}
	for _, p := range append(append([]control.Peer(nil), known...), heard...) {
		if now-p.SeenAt > PeerMaxAge || p.SeenAt > now+60 {
			continue
		}
		if old, ok := by[peerKey(p)]; !ok || p.SeenAt > old.SeenAt {
			by[peerKey(p)] = p
		}
	}
	out := make([]control.Peer, 0, len(by))
	for _, p := range by {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SeenAt != out[j].SeenAt {
			return out[i].SeenAt > out[j].SeenAt
		}
		return peerKey(out[i]) < peerKey(out[j])
	})
	if len(out) > peersKeptMax {
		out = out[:peersKeptMax]
	}
	return out
}

// SavePeers merges heard into the peers on disk.
func SavePeers(h vmrt.Host, dataDir string, heard []control.Peer, now int64) ([]control.Peer, error) {
	merged := MergePeers(LoadPeers(h, dataDir), heard, now)
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return merged, err
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return merged, err
	}
	return merged, h.WriteFile(PeersPath(dataDir), data, 0600)
}

// RecentPeers are the peers to report: heard within a day on a port this
// machine still has, newest first, at most MaxInterconnectPeers.
func RecentPeers(peers []control.Peer, now int64, portMACs []string) []control.Peer {
	present := map[string]bool{}
	for _, m := range portMACs {
		present[strings.ToLower(m)] = true
	}
	var out []control.Peer
	for _, p := range MergePeers(nil, peers, now) {
		if present[p.LocalMAC] {
			out = append(out, p)
		}
	}
	if len(out) > control.MaxInterconnectPeers {
		out = out[:control.MaxInterconnectPeers]
	}
	return out
}
