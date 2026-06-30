package tunnel

import (
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/config"
)

// LatencyResult holds the result of a latency test to a hub.
type LatencyResult struct {
	Hub     config.Hub
	AvgMs   float64
	Success bool
}

// SelectBestHub pings all hubs and returns the one with lowest latency.
func SelectBestHub(hubs []config.Hub) (*config.Hub, error) {
	results := make([]LatencyResult, 0, len(hubs))

	for _, hub := range hubs {
		avg, ok := measureLatency(hub.Host, hub.Port, 3)
		results = append(results, LatencyResult{
			Hub:     hub,
			AvgMs:   avg,
			Success: ok,
		})
	}

	// Filter successful results
	var successful []LatencyResult
	for _, r := range results {
		if r.Success {
			successful = append(successful, r)
		}
	}

	if len(successful) == 0 {
		return nil, fmt.Errorf("no hubs reachable")
	}

	// Sort by latency
	sort.Slice(successful, func(i, j int) bool {
		return successful[i].AvgMs < successful[j].AvgMs
	})

	best := successful[0]
	fmt.Printf("Selected hub %s (%s) with %.1fms latency\n", best.Hub.Name, best.Hub.Host, best.AvgMs)
	return &best.Hub, nil
}

// measureLatency does TCP connect probes to measure RTT.
// We use TCP instead of ICMP because ICMP may be blocked.
func measureLatency(host string, port int, attempts int) (float64, bool) {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	var total float64
	var ok int

	for i := 0; i < attempts; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		elapsed := time.Since(start)

		if err != nil {
			// Try ICMP-like approach with UDP
			start = time.Now()
			conn2, err2 := net.DialTimeout("udp", addr, 5*time.Second)
			elapsed = time.Since(start)
			if err2 == nil {
				conn2.Close()
				total += float64(elapsed.Milliseconds())
				ok++
			}
			continue
		}
		conn.Close()
		total += float64(elapsed.Milliseconds())
		ok++
	}

	if ok == 0 {
		return 0, false
	}
	return total / float64(ok), true
}

// TestAllHubs tests latency to all hubs and prints results.
func TestAllHubs(hubs []config.Hub) []LatencyResult {
	results := make([]LatencyResult, 0, len(hubs))
	for _, hub := range hubs {
		avg, ok := measureLatency(hub.Host, hub.Port, 3)
		results = append(results, LatencyResult{
			Hub:     hub,
			AvgMs:   avg,
			Success: ok,
		})
		if ok {
			fmt.Printf("  %s (%s): %.1fms\n", hub.Name, hub.Host, avg)
		} else {
			fmt.Printf("  %s (%s): unreachable\n", hub.Name, hub.Host)
		}
	}
	return results
}
