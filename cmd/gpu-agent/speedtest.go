package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
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
	// problem records a measurement the listing asked for that failed, for
	// the marketplace's problem list; nil: nowhere.
	problem func(message, detail string)
	// For the daily re-measurement: the target, interval and last
	// measurement as last saved, the clock, and a random part of a duration.
	saved  func() register.CapabilityResponse
	now    func() time.Time
	spread func(time.Duration) time.Duration
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
		problem: func(message, detail string) {
			if a.ops != nil {
				a.ops.errs.Note(control.AreaAgent, message, detail)
			}
		},
		saved: register.SavedSpeedtest,
		now:   time.Now,
		spread: func(d time.Duration) time.Duration {
			if d <= 0 {
				return 0
			}
			return time.Duration(rand.Int63n(int64(d)))
		},
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
			if j.problem != nil {
				j.problem("the network speed test the listing asked for failed twice; it runs again at the agent's next start", err.Error())
			}
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

// The daily re-measurement. What a listing says about its line is what renters
// choose by, and a host's line changes -- a faster plan, Wi-Fi swapped for a
// cable -- without the host thinking to measure again. So the running agent
// measures again once a day, or as often as the marketplace says, against the
// server the marketplace last named, while the machine is free. A rented or
// busy machine, and a test that fails, are tried again an hour later.
const (
	defaultSpeedtestEvery = 24 * time.Hour
	maxSpeedtestEvery     = 30 * 24 * time.Hour
	speedtestRecheck      = time.Hour
)

// speedtestEvery is the marketplace's interval, the default when it named
// none, and 0 when it turned the re-measurement off.
func speedtestEvery(saved register.CapabilityResponse) time.Duration {
	h := saved.SpeedtestEveryHours
	switch {
	case h == nil:
		return defaultSpeedtestEvery
	case *h <= 0:
		return 0
	case time.Duration(*h)*time.Hour > maxSpeedtestEvery:
		return maxSpeedtestEvery
	}
	return time.Duration(*h) * time.Hour
}

// firstSpeedtest is when a starting agent first measures again: a day after
// the last measurement it knows of. Knowing of none, it waits a random part of
// the interval, and a measurement already overdue runs within the hour, spread
// the same way -- so a fleet that updated itself in the same hour does not
// measure against the same server in the same minute.
func firstSpeedtest(now time.Time, measuredAt int64, every time.Duration, spread func(time.Duration) time.Duration) time.Time {
	if measuredAt <= 0 {
		return now.Add(10*time.Minute + spread(every))
	}
	earliest := now.Add(5*time.Minute + spread(time.Hour))
	if due := time.Unix(measuredAt, 0).Add(every); due.After(earliest) {
		return due
	}
	return earliest
}

// daily re-measures until ctx ends.
func (j speedtestJob) daily(ctx context.Context) {
	saved := j.saved()
	every := speedtestEvery(saved)
	if every == 0 {
		every = defaultSpeedtestEvery // off for now; again() looks each day
	}
	next := firstSpeedtest(j.now(), saved.MeasuredAt, every, j.spread)
	for {
		timer := time.NewTimer(next.Sub(j.now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		next = j.again(ctx)
	}
}

// again is one daily measurement, when one is due, and says when to look
// next. It reads the saved answer each time, so a new server, a new interval
// or a measurement taken in between (`gpu-agent speedtest`, an agent start)
// is followed.
func (j speedtestJob) again(ctx context.Context) time.Time {
	saved := j.saved()
	every := speedtestEvery(saved)
	now := j.now()
	if every == 0 {
		return now.Add(defaultSpeedtestEvery)
	}
	if saved.MeasuredAt > 0 {
		if due := time.Unix(saved.MeasuredAt, 0).Add(every); due.After(now.Add(time.Minute)) {
			return due
		}
	}
	if saved.Speedtest == nil {
		return now.Add(speedtestRecheck) // no server named yet
	}
	target := *saved.Speedtest
	if err := target.Validate(); err != nil {
		j.warn("Daily speed test not run: %v", err)
		return now.Add(speedtestRecheck)
	}
	res, err := j.measure(ctx, target)
	if err == nil {
		if err = j.post(saved.MeasureURL, res); err == nil {
			j.say("Daily %s", res.Summary())
			return now.Add(every)
		}
		err = fmt.Errorf("the result was not posted to the listing: %w", err)
	}
	var busy *busyError
	if ctx.Err() != nil || errors.As(err, &busy) || errors.Is(err, errRentalStarted) {
		return now.Add(speedtestRecheck)
	}
	j.warn("Daily speed test failed: %v; trying again in %s", err, speedtestRecheck)
	if j.problem != nil {
		j.problem("the daily network speed test failed; it is tried again every hour until it succeeds", err.Error())
	}
	return now.Add(speedtestRecheck)
}

// chooseSpeedtestTarget prefers what the marketplace just answered, then what
// it answered last: a listing that already has its measurement is offered no
// measurement, but a control plane from v0.3.1 still names its server
// (SpeedtestTarget), and an older one's last-named server is saved.
func chooseSpeedtestTarget(fresh *register.CapabilityResponse, saved register.CapabilityResponse) (speedtest.Target, string, error) {
	target, measureURL := saved.Speedtest, saved.MeasureURL
	if fresh != nil {
		if fresh.Speedtest != nil {
			target = fresh.Speedtest
		} else if fresh.SpeedtestTarget != nil {
			target = fresh.SpeedtestTarget
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
	rentalPresent := p.RentalPresent
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
