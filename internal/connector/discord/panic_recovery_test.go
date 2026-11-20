package discord

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Same confirmed bug as internal/connector/slack/panic_recovery_test.go,
// reproduced against Discord's equivalent bare-goroutine spawn points.
//
// asker.spawn (ask.go) is "runs fn off the live loop" -- every Discord Ask
// turn (a /ask command, a thread reply, a DM) and every mention/DM/
// interaction dispatch goes through it: "a.turns.Add(1); go func() {
// defer a.turns.Done(); fn() }()" with no recover. A panic inside fn was
// fatal to the entire diderot process, exactly like the Slack case,
// because recover() does not cross goroutine boundaries.

// TestDiscordAskSpawnPanicDoesNotCrashProcess: a panic inside fn spawned by
// asker.spawn must not crash the whole process.
func TestDiscordAskSpawnPanicDoesNotCrashProcess(t *testing.T) {
	if os.Getenv("DIDEROT_TEST_PANIC_HELPER") == "discord-ask-spawn" {
		a := &asker{}
		a.spawn(func() {
			panic("simulated panic from a Discord Ask turn")
		})
		time.Sleep(time.Second)
		os.Exit(0) // only reached if the panic did not crash the process
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestDiscordAskSpawnPanicDoesNotCrashProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "DIDEROT_TEST_PANIC_HELPER=discord-ask-spawn")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a panic inside a Discord Ask turn must not crash the whole diderot process "+
			"(asker.spawn in ask.go has no recover of its own, and recover() does not cross goroutine "+
			"boundaries), but the subprocess crashed: %v\nsubprocess output:\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "recovered panic") || !strings.Contains(string(out), "discord-ask") {
		t.Fatalf("expected a recovered-panic log line naming the discord-ask site; got:\n%s", string(out))
	}
}

// TestDiscordAskSpawnPanicStillReleasesTurnsWaitGroup: the Run that spawned
// asker waits for every spawned turn via a.wait() (a.turns.Wait()) before
// returning, so leadership handoff and process shutdown can drain
// in-flight work. A recovered panic must not leave that WaitGroup slot
// held, or a single panicking turn would make every future shutdown hang.
func TestDiscordAskSpawnPanicStillReleasesTurnsWaitGroup(t *testing.T) {
	a := &asker{}
	a.spawn(func() {
		panic("simulated panic from a Discord Ask turn")
	})
	done := make(chan struct{})
	go func() {
		a.wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("asker.turns never drained: a panicking spawn left its WaitGroup slot held")
	}
}
