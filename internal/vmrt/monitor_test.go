package vmrt

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeQMP is a QMP server on a unix socket: it greets, and for each command it
// is sent it says what answers[command] holds, line by line. It returns the
// socket's path and a way to read the commands it was sent, in order.
func fakeQMP(t *testing.T, greeting string, answers map[string][]string) (string, func() string) {
	t.Helper()
	// A short path of its own: a unix socket's is limited to about a hundred
	// characters, which a test's temporary directory can pass.
	dir, err := os.MkdirTemp("", "qmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "qmp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var got []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if _, err := conn.Write([]byte(greeting + "\n")); err != nil {
					return
				}
				in := bufio.NewScanner(conn)
				for in.Scan() {
					var req struct {
						Execute string `json:"execute"`
					}
					if json.Unmarshal(in.Bytes(), &req) != nil {
						continue
					}
					mu.Lock()
					got = append(got, req.Execute)
					mu.Unlock()
					for _, line := range answers[req.Execute] {
						if _, err := conn.Write([]byte(line + "\n")); err != nil {
							return
						}
					}
				}
			}()
		}
	}()
	return sock, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(got, " ")
	}
}

const (
	qmpGreeting = `{"QMP": {"version": {"qemu": {"micro": 1, "minor": 2, "major": 10}, "package": ""}, "capabilities": ["oob"]}}`
	qmpReset    = `{"timestamp": {"seconds": 1791489198, "microseconds": 143380}, "event": "RESET", "data": {"guest": false, "reason": "host-qmp-system-reset"}}`
	qmpOther    = `{"timestamp": {"seconds": 1791489198, "microseconds": 143000}, "event": "NIC_RX_FILTER_CHANGED", "data": {}}`
	qmpStatus   = `{"return": {"status": "prelaunch", "running": false}}`
)

// The monitor's client against a server that talks as QEMU does. The event a
// reset causes comes before the command's own answer on a current QEMU, and
// after it on an older one: a wait for it holds either way, and the status is
// asked only after both.
func TestQMPWaitsForAnEventOnEitherSideOfItsCommandsAnswer(t *testing.T) {
	for name, reset := range map[string][]string{
		"the event first, as QEMU 10 says it": {qmpOther, qmpReset, `{"return": {}}`},
		"the answer first":                    {`{"return": {}}`, qmpOther, qmpReset},
	} {
		sock, got := fakeQMP(t, qmpGreeting, map[string][]string{
			"qmp_capabilities": {`{"return": {}}`},
			"query-cpus-fast":  {`{"return": [{"thread-id": 9001, "cpu-index": 0, "qom-path": "/machine/unattached/device[0]", "target": "x86_64"}]}`},
			"system_reset":     reset,
			"query-status":     {qmpStatus},
		})
		out, err := OSHost{}.QMP(sock, "query-cpus-fast", "system_reset", "event:RESET", "query-status")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(out) != 4 || vmStatus(out[3]) != "prelaunch" {
			t.Fatalf("%s: answers = %q", name, out)
		}
		if threads, err := vcpuThreads(out[0]); err != nil || threads[0] != 9001 {
			t.Errorf("%s: threads = %v, %v", name, threads, err)
		}
		if sent := got(); sent != "qmp_capabilities query-cpus-fast system_reset query-status" {
			t.Errorf("%s: sent %q", name, sent)
		}
	}
}

func TestQMPSaysWhatTheMonitorRefused(t *testing.T) {
	sock, _ := fakeQMP(t, qmpGreeting, map[string][]string{
		"qmp_capabilities": {`{"return": {}}`},
		"cont":             {`{"error": {"class": "GenericError", "desc": "Resetting the Virtual Machine is required"}}`},
	})
	out, err := OSHost{}.QMP(sock, "cont", "query-status")
	if err == nil || !strings.Contains(err.Error(), "cont: Resetting the Virtual Machine is required") || len(out) != 0 {
		t.Errorf("answers %q, err %v", out, err)
	}

	// Something that is not a monitor at all.
	sock, _ = fakeQMP(t, `{"hello": "world"}`, nil)
	if _, err := (OSHost{}).QMP(sock, "query-status"); err == nil || !strings.Contains(err.Error(), "did not greet as a QMP monitor") {
		t.Errorf("err = %v", err)
	}
	if _, err := (OSHost{}).QMP(filepath.Join(t.TempDir(), "none.sock"), "query-status"); err == nil {
		t.Errorf("a socket that is not there answered")
	}
}
