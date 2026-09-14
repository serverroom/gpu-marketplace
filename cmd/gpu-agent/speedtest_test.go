package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/speedtest"
)

var jobTarget = speedtest.Target{
	Server:      "speedtest-ny.example.net",
	DownloadURL: "https://speedtest-ny.example.net/backend/garbage",
	UploadURL:   "https://speedtest-ny.example.net/backend/empty",
	LatencyHost: "console-nyc.example.net",
	LatencyPort: 2222,
}

var jobAnswer = &register.CapabilityResponse{Speedtest: &jobTarget, MeasureURL: "https://example.net/api/marketplace/measure"}

var jobResult = speedtest.Result{Server: "speedtest-ny.example.net", DownMbps: 940.2, UpMbps: 512.7, LatencyMs: 18.4, UploadMethod: speedtest.UploadStreamed}

// fakeJob is a job whose machine is in status, whose test answers with run,
// and which records what it posted and logged.
type fakeJob struct {
	speedtestJob
	runs, posts int
	postedTo    string
	log         []string
}

func newFakeJob(status func() string, run func(context.Context) (speedtest.Result, error)) *fakeJob {
	f := &fakeJob{}
	logf := func(format string, args ...interface{}) { f.log = append(f.log, fmt.Sprintf(format, args...)) }
	f.speedtestJob = speedtestJob{
		status: status,
		run: func(ctx context.Context, _ speedtest.Target) (speedtest.Result, error) {
			f.runs++
			return run(ctx)
		},
		post: func(url string, _ speedtest.Result) error {
			f.posts++
			f.postedTo = url
			return nil
		},
		say:        logf,
		warn:       logf,
		retryAfter: time.Millisecond,
		poll:       5 * time.Millisecond,
	}
	return f
}

func always(status string) func() string { return func() string { return status } }

func succeed(context.Context) (speedtest.Result, error) { return jobResult, nil }

func TestSpeedtestDoesNotRunWhileRented(t *testing.T) {
	for _, status := range []string{provisioner.StatusRented, provisioner.StatusProvisioning, provisioner.StatusWiping, provisioner.StatusDirty} {
		f := newFakeJob(always(status), succeed)
		f.initial(context.Background(), jobAnswer)
		if f.runs != 0 || f.posts != 0 {
			t.Errorf("machine %s: %d runs, %d posts; want none", status, f.runs, f.posts)
		}
		if len(f.log) != 1 || !strings.Contains(f.log[0], "next agent start") {
			t.Errorf("machine %s: log %q, want one line deferring to the next start", status, f.log)
		}
	}
}

func TestSpeedtestRunsAndPostsWhenFree(t *testing.T) {
	f := newFakeJob(always(provisioner.StatusFree), succeed)
	f.initial(context.Background(), jobAnswer)
	if f.runs != 1 || f.posts != 1 {
		t.Fatalf("%d runs, %d posts; want one of each", f.runs, f.posts)
	}
	if f.postedTo != jobAnswer.MeasureURL {
		t.Errorf("posted to %q, want %q", f.postedTo, jobAnswer.MeasureURL)
	}
	want := "Speed test to speedtest-ny.example.net: 940.2 Mbps down, 512.7 Mbps up, 18.4 ms latency"
	if len(f.log) != 1 || f.log[0] != want {
		t.Errorf("log %q, want %q", f.log, want)
	}
}

func TestSpeedtestNotAskedForDoesNothing(t *testing.T) {
	f := newFakeJob(always(provisioner.StatusFree), succeed)
	f.initial(context.Background(), &register.CapabilityResponse{MeasureURL: "https://example.net/measure"})
	f.initial(context.Background(), nil)
	if f.runs != 0 || f.posts != 0 || len(f.log) != 0 {
		t.Errorf("%d runs, %d posts, log %q; want nothing", f.runs, f.posts, f.log)
	}
}

func TestSpeedtestStopsWhenARentalStarts(t *testing.T) {
	var status atomic.Value
	status.Store(provisioner.StatusFree)
	f := newFakeJob(func() string { return status.Load().(string) }, func(ctx context.Context) (speedtest.Result, error) {
		status.Store(provisioner.StatusProvisioning) // a renter arrives mid-test
		<-ctx.Done()
		return speedtest.Result{}, ctx.Err()
	})

	done := make(chan struct{})
	go func() {
		f.initial(context.Background(), jobAnswer)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the speed test kept running after a rental started")
	}
	if f.runs != 1 || f.posts != 0 {
		t.Errorf("%d runs, %d posts; want the one run stopped and nothing posted or retried", f.runs, f.posts)
	}
}

func TestSpeedtestRetriesOnceAfterAFailure(t *testing.T) {
	f := newFakeJob(always(provisioner.StatusFree), func(context.Context) (speedtest.Result, error) {
		return speedtest.Result{}, errors.New("download: connection reset")
	})
	f.initial(context.Background(), jobAnswer)
	if f.runs != 2 || f.posts != 0 {
		t.Errorf("%d runs, %d posts; want two runs and no post", f.runs, f.posts)
	}

	// A rejected post is a failure too.
	f = newFakeJob(always(provisioner.StatusFree), succeed)
	f.post = func(string, speedtest.Result) error {
		f.posts++
		return &register.EndpointError{Op: "speed test report", Code: 219, Body: "bad values"}
	}
	f.initial(context.Background(), jobAnswer)
	if f.runs != 2 || f.posts != 2 {
		t.Errorf("rejected post: %d runs, %d posts; want two of each", f.runs, f.posts)
	}
}

func TestSpeedtestRetryWaitsForAFreeMachine(t *testing.T) {
	var status atomic.Value
	status.Store(provisioner.StatusFree)
	f := newFakeJob(func() string { return status.Load().(string) }, func(context.Context) (speedtest.Result, error) {
		status.Store(provisioner.StatusRented) // rented while the retry waits
		return speedtest.Result{}, errors.New("upload: timeout")
	})
	f.initial(context.Background(), jobAnswer)
	if f.runs != 1 || f.posts != 0 {
		t.Errorf("%d runs, %d posts; the retry must not run on a rented machine", f.runs, f.posts)
	}
}

func TestSpeedtestRetryStopsWithTheAgent(t *testing.T) {
	f := newFakeJob(always(provisioner.StatusFree), func(context.Context) (speedtest.Result, error) {
		return speedtest.Result{}, errors.New("latency: no route")
	})
	f.retryAfter = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.initial(ctx, jobAnswer)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the retry wait outlived the agent")
	}
}

func TestChooseSpeedtestTarget(t *testing.T) {
	other := jobTarget
	other.Server = "speedtest-ams.example.net"
	saved := register.CapabilityResponse{Speedtest: &other, MeasureURL: "https://example.net/saved/measure"}

	target, url, err := chooseSpeedtestTarget(jobAnswer, saved)
	if err != nil || target != jobTarget || url != jobAnswer.MeasureURL {
		t.Errorf("fresh answer: %+v %q %v; want the marketplace's current target", target, url, err)
	}

	target, url, err = chooseSpeedtestTarget(&register.CapabilityResponse{}, saved)
	if err != nil || target != other || url != saved.MeasureURL {
		t.Errorf("already measured: %+v %q %v; want the saved target", target, url, err)
	}

	target, _, err = chooseSpeedtestTarget(nil, saved)
	if err != nil || target != other {
		t.Errorf("marketplace unreachable: %+v %v; want the saved target", target, err)
	}

	if _, _, err := chooseSpeedtestTarget(&register.CapabilityResponse{}, register.CapabilityResponse{}); err == nil {
		t.Error("no target anywhere: want an error")
	}
}
