package tunnel

import (
	"fmt"
	"net"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/config"
)

// LatencyResult holds the result of a latency test to a hub.
type LatencyResult struct {
	Hub     config.Hub
	AvgMs   float64
	Success bool
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
			continue
		}
		conn.Close()
		total += float64(elapsed.Microseconds()) / 1000.0
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
