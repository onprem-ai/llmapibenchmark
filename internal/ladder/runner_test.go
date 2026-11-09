package ladder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Yoosu-L/llmapibenchmark/internal/api"
)

// fakeEngine answers chat completions as a stream: one chunk per token, then
// a usage chunk. Its prompt token count is the prompt's word count, so the
// calibration finds exactly 1 token per word. It records each request's body.
type fakeEngine struct {
	requests  atomic.Int64
	ignoreEOS atomic.Int64
	failAfter int64
	prompts   chan string
}

func (engine *fakeEngine) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		MaxTokens int                        `json:"max_tokens"`
		IgnoreEOS bool                       `json:"ignore_eos"`
		Messages  []struct{ Content string } `json:"messages"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	count := engine.requests.Add(1)
	if engine.failAfter > 0 && count > engine.failAfter {
		http.Error(writer, `{"error":{"message":"maximum context length exceeded"}}`, http.StatusBadRequest)
		return
	}
	if body.IgnoreEOS {
		engine.ignoreEOS.Add(1)
	}
	prompt := body.Messages[0].Content
	select {
	case engine.prompts <- prompt:
	default:
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	flusher := writer.(http.Flusher)
	for range body.MaxTokens {
		fmt.Fprintf(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
		flusher.Flush()
	}
	words := len(strings.Fields(prompt))
	fmt.Fprintf(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", words, body.MaxTokens, words+body.MaxTokens)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

func run(t *testing.T, engine *fakeEngine, config Config) []map[string]any {
	t.Helper()
	server := httptest.NewServer(engine)
	defer server.Close()
	var out bytes.Buffer
	runner := &Runner{
		Client:  api.NewClient(server.URL+"/v1", "", "test", StatusErrorMiddleware()),
		Config:  config,
		Emitter: NewEmitter(&out),
	}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("stdout line %q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func baseConfig() Config {
	return Config{
		Model: "test", PromptTokens: 50, AnswerTokens: 4, IgnoreEOS: true,
		MeasureThrough: 4, MaxConcurrency: 128, MinRequests: 4, PlateauOn: PlateauOnDecode,
		PlateauGainPercent: 5, StopAfterLevelsWithoutGain: 2,
		RequestTimeout: 30 * time.Second, ToolVersion: "test",
	}
}

func TestTheLadderWritesStartCalibrationLevelsAndEnd(t *testing.T) {
	engine := &fakeEngine{prompts: make(chan string, 1)}
	config := baseConfig()
	config.MaxConcurrency = 4
	// The fake engine's throughput is timing noise; the plateau rule must not
	// end this ladder before its cut.
	config.StopAfterLevelsWithoutGain = 100
	records := run(t, engine, config)

	types := []string{}
	for _, record := range records {
		types = append(types, record["type"].(string))
	}
	if strings.Join(types, ",") != "start,calibration,level,level,level,end" {
		t.Fatalf("records %v", types)
	}
	if records[1]["tokens_per_word"].(float64) <= 0 {
		t.Fatalf("calibration %v", records[1])
	}
	for _, level := range records[2:5] {
		for _, field := range []string{
			"window_seconds", "requests_per_second", "prefill_tokens_per_second", "decode_tokens_per_second",
			"time_to_first_token_seconds_p50", "time_to_first_token_seconds_p90", "time_to_first_token_seconds_p95",
			"inter_token_latency_seconds_p50", "inter_token_latency_seconds_p90", "inter_token_latency_seconds_p95",
			"request_latency_seconds_p50", "request_latency_seconds_p90", "request_latency_seconds_p95",
			"prompt_tokens_mean", "prompt_tokens_stddev", "completion_tokens_mean", "completion_tokens_stddev",
		} {
			if _, ok := level[field].(float64); !ok {
				t.Errorf("level %v lacks %s", level["concurrency"], field)
			}
		}
		if level["short_answers"].(float64) != 0 || level["requests_failed"].(float64) != 0 {
			t.Errorf("level %v", level)
		}
		// The sample: at least min-requests (4), and at least 2 x concurrency.
		if want := max(4, 2*int(level["concurrency"].(float64))); int(level["requests"].(float64)) != want {
			t.Errorf("level %v measured %v requests, want %d", level["concurrency"], level["requests"], want)
		}
	}
	// The ladder ran 1, 2 and 4 to the cut at 4: max-concurrency, not a plateau.
	if records[5]["stop_reason"] != StopMaxConcurrency || records[5]["plateau_concurrency"] != nil {
		t.Fatalf("end %v", records[5])
	}
	if engine.ignoreEOS.Load() != engine.requests.Load() {
		t.Fatalf("%d of %d requests asked to ignore the end of sequence", engine.ignoreEOS.Load(), engine.requests.Load())
	}
}

func TestEveryPromptStartsWithTextNoOtherRequestSent(t *testing.T) {
	engine := &fakeEngine{prompts: make(chan string, 64)}
	config := baseConfig()
	config.MaxConcurrency = 8
	run(t, engine, config)
	close(engine.prompts)
	starts := map[string]bool{}
	for prompt := range engine.prompts {
		start := strings.Fields(prompt)[0]
		if starts[start] {
			t.Fatalf("two prompts start with %q", start)
		}
		starts[start] = true
	}
}

func TestAFailedRequestEndsTheLadderWithItsStatusAndBody(t *testing.T) {
	// The calibration request and the level-1 request succeed; level 2 fails.
	engine := &fakeEngine{prompts: make(chan string, 1), failAfter: 2}
	records := run(t, engine, baseConfig())
	last := records[len(records)-1]
	if last["stop_reason"] != StopRequestFailed {
		t.Fatalf("end %v", last)
	}
	failed := records[len(records)-2]
	failure, _ := failed["first_failure"].(string)
	if !strings.Contains(failure, "400") || !strings.Contains(failure, "maximum context length exceeded") {
		t.Fatalf("the failure lost the status or the body: %q", failure)
	}
}

func TestThePlateauRuleAndThePlateauConcurrency(t *testing.T) {
	runner := &Runner{Config: baseConfig()}
	level := func(concurrency int, decode float64) levelResult {
		return levelResult{record: LevelRecord{Concurrency: concurrency}, decode: decode}
	}
	earlier := []levelResult{level(1, 30), level(2, 58), level(4, 110)}
	if runner.gains(level(8, 114), earlier) {
		t.Error("3.6 percent over the best counted as a gain above 5 percent")
	}
	if !runner.gains(level(8, 120), earlier) {
		t.Error("9 percent over the best did not count as a gain")
	}
	// 105 reaches 95 percent of 110; the lowest level that does is 4.
	if plateau := runner.plateauConcurrency(append(earlier, level(8, 112), level(12, 111))); plateau != 4 {
		t.Errorf("plateau concurrency %d, want 4", plateau)
	}
}

func TestBothTakesTheLaterOfThePrefillAndDecodePlateaus(t *testing.T) {
	// Measured on a real replica: prefill peaks at 2 and falls slowly, decode
	// grows until 96. No level reaches 95 percent of both bests; decode first
	// reaches 95 percent of its best (1274) at 64.
	config := baseConfig()
	config.PlateauOn = PlateauOnBoth
	runner := &Runner{Config: config}
	levels := []struct {
		concurrency     int
		prefill, decode float64
	}{{1, 10827, 97.6}, {2, 11554, 226.3}, {32, 10359, 1093.6}, {64, 9916, 1293.7}, {80, 9672, 1333.5}, {96, 9529, 1341.3}}
	var results []levelResult
	for _, entry := range levels {
		results = append(results, levelResult{record: LevelRecord{Concurrency: entry.concurrency}, prefill: entry.prefill, decode: entry.decode})
	}
	if plateau := runner.plateauConcurrency(results); plateau != 64 {
		t.Fatalf("plateau concurrency %d, want 64", plateau)
	}
}

func TestACancelledRunEndsWithStopReasonCancelled(t *testing.T) {
	engine := &fakeEngine{prompts: make(chan string, 1)}
	server := httptest.NewServer(engine)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	runner := &Runner{Client: api.NewClient(server.URL+"/v1", "", "test"), Config: baseConfig(), Emitter: NewEmitter(&out)}
	if err := runner.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), `"stop_reason":"cancelled"`) {
		t.Fatalf("output %s", out.String())
	}
}

func TestMixedLengthsDrawEachRequestsLengthsFromTheRangesWithTheSeed(t *testing.T) {
	config := baseConfig()
	config.PromptTokens, config.PromptTokensMax = 128, 8192
	config.AnswerTokensMin, config.AnswerTokens = 64, 1024
	config.Seed = 7
	if !config.MixedLengths() {
		t.Fatal("a range was not read as mixed lengths")
	}
	first := newLengthDrawer(config, 1).draw(2000)
	again := newLengthDrawer(config, 1).draw(2000)
	shortPrompts, longPrompts := 0, 0
	for index, shape := range first {
		if shape != again[index] {
			t.Fatalf("one seed drew %v, then %v", shape, again[index])
		}
		if shape.words < 128 || shape.words > 8192 || shape.answerTokens < 64 || shape.answerTokens > 1024 {
			t.Fatalf("shape %v is out of its ranges", shape)
		}
		// Log-uniform: below the range's geometric middle (1024) as often as above.
		if shape.words < 1024 {
			shortPrompts++
		} else {
			longPrompts++
		}
	}
	if shortPrompts < 850 || longPrompts < 850 {
		t.Fatalf("%d short and %d long prompts: not log-uniform", shortPrompts, longPrompts)
	}
	other := config
	other.Seed = 8
	if newLengthDrawer(other, 1).draw(1)[0] == first[0] && newLengthDrawer(other, 1).draw(2)[1] == first[1] {
		t.Fatal("another seed drew the same lengths")
	}
}

func TestFixedLengthsGiveEveryRequestTheSameShape(t *testing.T) {
	config := baseConfig()
	if config.MixedLengths() {
		t.Fatal("fixed lengths were read as mixed")
	}
	for _, shape := range newLengthDrawer(config, 2).draw(10) {
		if shape != (requestShape{words: 25, answerTokens: 4}) {
			t.Fatalf("shape %v", shape)
		}
	}
}

func TestAMixedLengthsLadderSendsTheDrawnAnswerLengths(t *testing.T) {
	engine := &fakeEngine{prompts: make(chan string, 1)}
	config := baseConfig()
	config.MaxConcurrency = 4
	config.StopAfterLevelsWithoutGain = 100
	config.PromptTokensMax = 400
	config.AnswerTokensMin = 2
	config.AnswerTokens = 16
	records := run(t, engine, config)
	if records[0]["prompt_tokens_max"].(float64) != 400 || records[0]["answer_tokens_min"].(float64) != 2 {
		t.Fatalf("start %v", records[0])
	}
	for _, level := range records[2:5] {
		if level["short_answers"].(float64) != 0 {
			t.Fatalf("an answer shorter than its own drawn length was counted: %v", level)
		}
		mean := level["completion_tokens_mean"].(float64)
		if mean < 2 || mean > 16 {
			t.Fatalf("completion mean %v outside 2..16", mean)
		}
	}
}

// gatedEngine holds every request until the test releases it, and records
// the most requests it held at once.
type gatedEngine struct {
	inner    *fakeEngine
	release  chan struct{}
	inFlight atomic.Int64
	peak     atomic.Int64
	seen     atomic.Int64
}

func (engine *gatedEngine) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	now := engine.inFlight.Add(1)
	defer engine.inFlight.Add(-1)
	for {
		peak := engine.peak.Load()
		if now <= peak || engine.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	engine.seen.Add(1)
	<-engine.release
	engine.inner.ServeHTTP(writer, request)
}

func TestALevelKeepsItsConcurrencyAndNeverExceedsIt(t *testing.T) {
	// The engine answers one request at a time in the order they wait; the
	// lanes must keep exactly `concurrency` in flight and never more.
	engine := &gatedEngine{inner: &fakeEngine{prompts: make(chan string, 1)}, release: make(chan struct{})}
	server := httptest.NewServer(engine)
	defer server.Close()
	config := baseConfig()
	config.MaxConcurrency = 8
	config.MeasureThrough = 8
	config.StopAfterLevelsWithoutGain = 100
	var out bytes.Buffer
	runner := &Runner{Client: api.NewClient(server.URL+"/v1", "", "test", StatusErrorMiddleware()), Config: config, Emitter: NewEmitter(&out)}
	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background()) }()
	// Release requests one by one until the run ends: each release lets one
	// request finish, and its lane sends the next.
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if peak := engine.peak.Load(); peak > 8 {
				t.Fatalf("%d requests were in flight at once at a ladder ending at 8", peak)
			}
			if !strings.Contains(out.String(), `"concurrency":8`) {
				t.Fatalf("level 8 missing: %s", out.String())
			}
			return
		case engine.release <- struct{}{}:
		}
	}
}

func TestTheWindowCountsOnlyTheWorkInsideIt(t *testing.T) {
	start := time.Unix(1000, 0)
	at := func(seconds float64) time.Time { return start.Add(time.Duration(seconds * float64(time.Second))) }
	requests := []finished{
		// Started before the window: its prompt is outside, half its decoding inside.
		{sentAt: at(-2), firstAt: at(-1), endedAt: at(1), prompt: 1000, answer: 201},
		// Inside the window, counted: prompt and all decoding inside.
		{sentAt: at(1), firstAt: at(2), endedAt: at(4), prompt: 500, answer: 101, counted: true},
		// A failed request counts for nothing.
		{sentAt: at(1), failure: errors.New("HTTP 500")},
	}
	figures := computeWindow(requests, start, at(10))
	if figures.WindowSeconds != 10 {
		t.Fatalf("window %v", figures.WindowSeconds)
	}
	if *figures.PrefillTokensPerSecond != 50 {
		t.Errorf("prefill %v, want 500 tokens / 10 s", *figures.PrefillTokensPerSecond)
	}
	// 100 of the first request's 200 decode tokens, and all 100 of the second.
	if *figures.DecodeTokensPerSecond != 20 {
		t.Errorf("decode %v, want 200 tokens / 10 s", *figures.DecodeTokensPerSecond)
	}
	if figures.Requests != 1 || *figures.TTFT[0] != 1 || *figures.RequestLatency[0] != 3 || *figures.ITL[0] != 0.02 {
		t.Errorf("per-request figures %+v ttft %v latency %v itl %v", figures, *figures.TTFT[0], *figures.RequestLatency[0], *figures.ITL[0])
	}
	// Both successful requests end inside the window, whenever they started.
	if *figures.RequestsPerSecond != 0.2 {
		t.Errorf("requests per second %v, want 2 completed / 10 s", *figures.RequestsPerSecond)
	}
}
