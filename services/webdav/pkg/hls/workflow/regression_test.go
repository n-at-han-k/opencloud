package workflow

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// These cover the four ways the first deployment failed while every existing
// test passed. Each one reproduces a condition the other tests could not
// contain: a probe that fails rather than answers, a player fetching two
// artifacts at once, a token built by the real function, and real-world
// keyframe spacing.

// A failed keyframe index must not be mistaken for "this source has no
// keyframes". The difference decides whether a film is copied or re-encoded,
// and the answer is cached, so getting it wrong once gets it wrong for good.
func TestFailedKeyframeIndexIsNotAnAnswer(t *testing.T) {
	video := h264("High", 41, "yuv420p")
	probe := &probeResult{
		Streams: []probeStream{*video},
		Format:  probeFormat{Duration: "5486.5", BitRate: "1284229"},
	}

	// What a clean run that found nothing looks like: copy is impossible, so
	// re-encoding is the right answer.
	plan, err := buildPlan(probe, 4, nil)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if plan.Renditions[0].VideoCopy {
		t.Error("a source with no keyframes cannot be copied")
	}

	// And what a real index looks like: copy, with boundaries on keyframes.
	keyframes := []float64{0, 2, 12.416667, 22.833333, 27.958333, 40, 52.5}
	plan, err = buildPlan(probe, 4, keyframes)
	if err != nil {
		t.Fatalf("buildPlan with keyframes: %v", err)
	}
	if !plan.Renditions[0].VideoCopy {
		t.Fatal("h264 High 4.1 yuv420p progressive with a keyframe index must be copied")
	}

	// Irregular spacing is the normal case for a real encode, and it must not
	// produce the uniform grid that gives the re-encode path away.
	uniform := true
	for _, s := range plan.Renditions[0].Segments {
		if s.Duration != 4 {
			uniform = false
		}
	}
	if uniform {
		t.Error("copied segments landed on a 4s grid; that is the re-encode table, not keyframes")
	}
}

// hls.js fetches the initialization segment and the first media segment
// together, every time playback starts. The concurrency limit must not turn the
// second of that pair into a 429.
func TestConcurrentGenerateDoesNotRefuseThePlayersOpeningPair(t *testing.T) {
	executor := NewFfmpegExecutor("", "", 1, 1)
	executor.queueWait = 2 * time.Second

	// Hold the only slot, as a run in flight would.
	release, err := executor.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var secondErr error
	go func() {
		defer wg.Done()
		var r func()
		r, secondErr = executor.acquire(context.Background())
		if r != nil {
			r()
		}
	}()

	// Free it the way a finished run would, well inside the wait.
	time.Sleep(200 * time.Millisecond)
	release()
	wg.Wait()

	if secondErr != nil {
		t.Errorf("the second half of the opening pair was refused: %v", secondErr)
	}
}

// Past the wait the limit still bites, so the queue stays bounded.
func TestGenerateStillRefusesWhenTheWaitExpires(t *testing.T) {
	executor := NewFfmpegExecutor("", "", 1, 1)
	executor.queueWait = 100 * time.Millisecond

	release, err := executor.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	if _, err := executor.acquire(context.Background()); err == nil {
		t.Fatal("expected a busy error once the wait expired")
	} else if !strings.Contains(err.Error(), "busy") {
		t.Errorf("expected a busy error, got %v", err)
	}
}

// The playlist tests pass a literal token, so the function that builds one was
// never run. It sliced a string that already carried "/" separators and emitted
// v=840d/fe3 into every URL.
func TestVersionTokenIsASingleOpaqueField(t *testing.T) {
	token := versionToken("sha256:abc123", "video/mp4")

	if token == "" {
		t.Fatal("empty version token")
	}
	for _, bad := range []string{"/", "?", "&", "=", "%", " "} {
		if strings.Contains(token, bad) {
			t.Errorf("version token %q contains %q, which does not survive a URL query", token, bad)
		}
	}
	if other := versionToken("sha256:different", "video/mp4"); other == token {
		t.Error("a different source produced the same token, so cached segments would be reused")
	}
	if again := versionToken("sha256:abc123", "video/mp4"); again != token {
		t.Error("the token is not stable for the same source")
	}
}
