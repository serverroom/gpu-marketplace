package stats

import (
	"testing"
	"time"
)

func TestPausedGPUQueriesAnswerWithTheLastReading(t *testing.T) {
	runs := 0
	collect := func() []GPUInfo {
		runs++
		return []GPUInfo{{Model: "NVIDIA GB10"}}
	}
	if got := gatedGPUs(collect); len(got) != 1 || runs != 1 {
		t.Fatalf("unpaused: %v, %d runs", got, runs)
	}
	resume := PauseGPUQueries()
	if !GPUQueriesPaused() {
		t.Fatal("not paused")
	}
	if got := gatedGPUs(collect); len(got) != 1 || got[0].Model != "NVIDIA GB10" || runs != 1 {
		t.Errorf("paused: %v, %d runs; want the last reading and no query", got, runs)
	}
	resume()
	resume() // safe twice
	if GPUQueriesPaused() {
		t.Fatal("still paused")
	}
	gatedGPUs(collect)
	if runs != 2 {
		t.Errorf("%d runs after resume", runs)
	}
}

// A pause waits for a query already running to exit: its nvidia-smi would
// otherwise still hold the GPU.
func TestPauseWaitsForARunningQuery(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		gatedGPUs(func() []GPUInfo {
			close(started)
			<-finish
			return nil
		})
		close(done)
	}()
	<-started
	paused := make(chan func())
	go func() { paused <- PauseGPUQueries() }()
	select {
	case <-paused:
		t.Fatal("the pause returned while a query was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(finish)
	resume := <-paused
	resume()
	<-done
}

func TestPauseGivesUpOnAHungQuery(t *testing.T) {
	old := PauseWait
	PauseWait = 50 * time.Millisecond
	defer func() { PauseWait = old }()
	started, finish := make(chan struct{}), make(chan struct{})
	go gatedGPUs(func() []GPUInfo { close(started); <-finish; return nil })
	<-started
	begin := time.Now()
	resume := PauseGPUQueries()
	if waited := time.Since(begin); waited > 2*time.Second {
		t.Errorf("waited %v for a hung query", waited)
	}
	resume()
	close(finish)
}
