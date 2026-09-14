package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/provisioner"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/speedtest"
)

// speedtestJob is a listing's network measurement: run by the daemon when the
// marketplace asks for one, and by `gpu-agent speedtest`. It never measures
// while the machine is rented -- a speed test saturates the link the renter
// is paying for -- and it stops the moment a rental starts.
type speedtestJob struct {
	status     func() string // provisioner.StatusFree, or what the machine is busy with
	run        func(ctx context.Context, t speedtest.Target) (speedtest.Result, error)
	post       func(measureURL string, r speedtest.Result) error
	say, warn  func(format string, args ...interface{})
	retryAfter time.Duration // before the daemon's one retry
	poll       time.Duration // how often a running test looks for a rental
}

// busyError: the machine was not free, so nothing was measured.
type busyError struct{ status string }

func (e *busyError) Error() string { return "this machine is " + e.status }

var errRentalStarted = errors.New("a rental started, so the speed test was stopped")

func (a *gpuAgent) speedtestJob() speedtestJob {
	return speedtestJob{
		status:     a.prov.Status,
		run:        speedtest.Runner{}.Run,
		post:       register.PostMeasurement,
		say:        a.say,
		warn:       a.warn,
		retryAfter: 10 * time.Minute,
		poll:       time.Second,
	}
}

// measure runs one test, only if the machine is free, and cancels it as soon
// as the machine is not.
func (j speedtestJob) measure(ctx context.Context, t speedtest.Target) (speedtest.Result, error) {
	if st := j.status(); st != provisioner.StatusFree {
		return speedtest.Result{}, &busyError{status: st}
	}
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var rented atomic.Bool
	go func() {
		tick := time.NewTicker(j.poll)
		defer tick.Stop()
		for {
			select {
			case <-mctx.Done():
				return
			case <-tick.C:
				if j.status() != provisioner.StatusFree {
					rented.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	res, err := j.run(mctx, t)
	if rented.Load() {
		return speedtest.Result{}, errRentalStarted
	}
	return res, err
}

// initial is the daemon's measurement for a listing that still needs one,
// after a capability report whose answer asked for it. It runs in the
// background, retries once after a failure, and leaves a busy machine to the
// next agent start: the marketplace keeps asking until it has a result.
func (j speedtestJob) initial(ctx context.Context, resp *register.CapabilityResponse) {
	if resp == nil || resp.Speedtest == nil {
		return
	}
	target := *resp.Speedtest
	if err := target.Validate(); err != nil {
		j.warn("Speed test not run: %v", err)
		return
	}
	for attempt := 1; ; attempt++ {
		res, err := j.measure(ctx, target)
		if err == nil {
			if err = j.post(resp.MeasureURL, res); err == nil {
				j.say("%s", res.Summary())
				return
			}
			err = fmt.Errorf("the result was not posted to the listing: %w", err)
		}
		var busy *busyError
		switch {
		case ctx.Err() != nil:
			return // the agent is stopping
		case errors.As(err, &busy) || errors.Is(err, errRentalStarted):
			j.say("Speed test not run: %v; it runs on the next agent start", err)
			return
		case attempt >= 2:
			j.warn("Speed test failed again: %v; it runs on the next agent start", err)
			return
		}
		j.warn("Speed test failed: %v; retrying once in %s", err, j.retryAfter)
		select {
		case <-ctx.Done():
			return
		case <-time.After(j.retryAfter):
		}
	}
}

// chooseSpeedtestTarget prefers what the marketplace just answered, then what
// it answered last: a listing that already has its measurement is no longer
// offered a target, but can be measured again against the same server.
func chooseSpeedtestTarget(fresh *register.CapabilityResponse, saved register.CapabilityResponse) (speedtest.Target, string, error) {
	target, measureURL := saved.Speedtest, saved.MeasureURL
	if fresh != nil {
		if fresh.Speedtest != nil {
			target = fresh.Speedtest
		}
		if fresh.MeasureURL != "" {
			measureURL = fresh.MeasureURL
		}
	}
	if target == nil {
		return speedtest.Target{}, "", errors.New("the marketplace has not named a speed test server for this machine, and none is saved from an earlier agent start")
	}
	if err := target.Validate(); err != nil {
		return speedtest.Target{}, "", err
	}
	return *target, measureURL, nil
}

// runSpeedtest measures now, prints the result and posts it to the listing.
func runSpeedtest(args []string) {
	fs := flag.NewFlagSet("speedtest", flag.ExitOnError)
	fs.Parse(args)

	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		exitf("speedtest needs root to read the agent's token: run 'sudo gpu-agent speedtest'")
	}
	st := register.LoadState()
	if !st.Registered || st.Unreadable {
		exitf("speedtest posts to this machine's listing, so it needs a registration: %s", idleReason(st))
	}

	p := detectProvisioner()
	rt := p.Runtime()
	rentalPresent := func() bool { return rt != nil && rt.Present() }
	if rentalPresent() {
		exitf("a rental (or the leftover of one) is on this machine; the speed test does not run while the machine is rented")
	}

	fresh, err := register.ReportCapability(p.Capability())
	if err != nil {
		fmt.Printf("Could not ask the marketplace for its speed test server (%v); using the last one it named.\n", err)
	}
	target, measureURL, err := chooseSpeedtestTarget(fresh, register.SavedSpeedtest())
	if err != nil {
		exitf("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	job := speedtestJob{
		status: func() string {
			if rentalPresent() {
				return provisioner.StatusRented
			}
			return provisioner.StatusFree
		},
		run:  speedtest.Runner{}.Run,
		poll: time.Second,
	}
	fmt.Printf("Measuring the network to %s (up to 40 seconds) ...\n", target.Server)
	res, err := job.measure(ctx, target)
	if err != nil {
		exitf("speed test failed: %v", err)
	}
	fmt.Println()
	fmt.Printf("Server:    %s\n", res.Server)
	fmt.Printf("Latency:   %.1f ms\n", res.LatencyMs)
	fmt.Printf("Download:  %.1f Mbps\n", res.DownMbps)
	fmt.Printf("Upload:    %.1f Mbps (%s)\n", res.UpMbps, res.UploadNote())
	fmt.Println()
	if err := register.PostMeasurement(measureURL, res); err != nil {
		exitf("the result was not posted to the listing: %v", err)
	}
	fmt.Printf("Posted to listing %s, where it is shown.\n", st.ListingID)
}

func exitf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
