package ladder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/Yoosu-L/llmapibenchmark/internal/api"
)

// calibrationWords is the length of the calibration prompt: long enough that
// the chat template's own tokens are a small share of the count.
const calibrationWords = 400

// plateauShare is the share of the best throughput a level must reach to be
// the plateau concurrency.
const plateauShare = 0.95

// Runner runs one ladder against one endpoint.
type Runner struct {
	Client  openai.Client
	Config  Config
	Emitter *Emitter
	// Progress receives one human line per level; nil discards them.
	Progress io.Writer
}

// levelResult is a finished level with the figures the plateau rule reads.
type levelResult struct {
	record  LevelRecord
	prefill float64
	decode  float64
	ttftP90 float64
	failed  bool
}

// Run calibrates, measures the ladder and writes the start, calibration,
// level and end records. A fatal error before the first level is returned
// (the caller writes the error record); a request that fails inside a level
// ends the ladder with stop reason request-failed and returns nil. A
// cancelled ctx ends it with stop reason cancelled.
func (runner *Runner) Run(ctx context.Context) error {
	config := runner.Config
	if err := runner.Emitter.Emit(StartRecord{
		Type:                       "start",
		Workload:                   config.Workload,
		ToolVersion:                config.ToolVersion,
		Model:                      config.Model,
		PromptTokensTarget:         config.PromptTokens,
		PromptTokensMax:            config.PromptTokensMax,
		AnswerTokens:               config.AnswerTokens,
		AnswerTokensMin:            config.AnswerTokensMin,
		Seed:                       config.Seed,
		IgnoreEOS:                  config.IgnoreEOS,
		Ladder:                     config.Ladder(),
		MeasureThrough:             config.MeasureThrough,
		MinRequests:                config.MinRequests,
		PlateauOn:                  string(config.PlateauOn),
		PlateauGainPercent:         config.PlateauGainPercent,
		StopAfterLevelsWithoutGain: config.StopAfterLevelsWithoutGain,
		MaxTTFTP90Seconds:          config.MaxTTFTP90.Seconds(),
	}); err != nil {
		return err
	}

	tokensPerWord, err := runner.calibrate(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return runner.end(StopCancelled, nil)
		}
		return err
	}

	lengths := newLengthDrawer(config, tokensPerWord)
	var results []levelResult
	levelsWithoutGain := 0
	for _, concurrency := range config.Ladder() {
		result, err := runner.measureLevel(ctx, concurrency, lengths)
		if ctx.Err() != nil {
			return runner.end(StopCancelled, nil)
		}
		if err != nil {
			return err
		}
		if emitErr := runner.Emitter.Emit(result.record); emitErr != nil {
			return emitErr
		}
		runner.progress(result)
		if result.failed {
			return runner.end(StopRequestFailed, nil)
		}
		gained := len(results) == 0 || runner.gains(result, results)
		results = append(results, result)
		if gained {
			levelsWithoutGain = 0
		} else {
			levelsWithoutGain++
		}
		if concurrency < config.MeasureThrough {
			continue
		}
		if config.MaxTTFTP90 > 0 && result.ttftP90 > config.MaxTTFTP90.Seconds() {
			return runner.end(StopTTFTLimit, nil)
		}
		if levelsWithoutGain >= config.StopAfterLevelsWithoutGain {
			plateau := runner.plateauConcurrency(results)
			return runner.end(StopPlateau, &plateau)
		}
	}
	return runner.end(StopMaxConcurrency, nil)
}

func (runner *Runner) end(reason string, plateau *int) error {
	return runner.Emitter.Emit(EndRecord{Type: "end", StopReason: reason, PlateauConcurrency: plateau})
}

func (runner *Runner) progress(result levelResult) {
	if runner.Progress == nil {
		return
	}
	fmt.Fprintf(runner.Progress, "concurrency %d: prefill %.0f tok/s, decode %.1f tok/s, ttft p90 %.3f s, %d/%d requests failed\n",
		result.record.Concurrency, result.prefill, result.decode, result.ttftP90,
		result.record.RequestsFailed, result.record.Requests)
}

// gains reports whether result beats every earlier level by more than the
// gain percentage on the plateau measure (either measure for "both").
func (runner *Runner) gains(result levelResult, earlier []levelResult) bool {
	bestPrefill, bestDecode := 0.0, 0.0
	for _, previous := range earlier {
		bestPrefill = math.Max(bestPrefill, previous.prefill)
		bestDecode = math.Max(bestDecode, previous.decode)
	}
	factor := 1 + runner.Config.PlateauGainPercent/100
	prefillGains := result.prefill > bestPrefill*factor
	decodeGains := result.decode > bestDecode*factor
	switch runner.Config.PlateauOn {
	case PlateauOnPrefill:
		return prefillGains
	case PlateauOnDecode:
		return decodeGains
	default:
		return prefillGains || decodeGains
	}
}

// plateauConcurrency is the lowest level whose plateau measure reaches 95
// percent of the workload's best. For "both", each measure finds its own
// lowest such level and the later one counts: prefill often peaks early and
// then falls slowly while decode still grows, so no single level need reach
// 95 percent of both bests.
func (runner *Runner) plateauConcurrency(results []levelResult) int {
	prefillLevel := firstReaching(results, func(result levelResult) float64 { return result.prefill })
	decodeLevel := firstReaching(results, func(result levelResult) float64 { return result.decode })
	switch runner.Config.PlateauOn {
	case PlateauOnPrefill:
		return prefillLevel
	case PlateauOnDecode:
		return decodeLevel
	default:
		return max(prefillLevel, decodeLevel)
	}
}

// firstReaching is the lowest level whose measure reaches 95 percent of the
// best measure of all levels.
func firstReaching(results []levelResult, measure func(levelResult) float64) int {
	best := 0.0
	for _, result := range results {
		best = math.Max(best, measure(result))
	}
	for _, result := range results {
		if measure(result) >= best*plateauShare {
			return result.record.Concurrency
		}
	}
	return results[len(results)-1].record.Concurrency
}

// calibrate sends one prompt of random words and measures how many tokens
// the engine counts per word, so later prompts are sized in its tokens.
func (runner *Runner) calibrate(ctx context.Context) (float64, error) {
	result, err := runner.request(ctx, calibrationWords, 1)
	if err != nil {
		return 0, fmt.Errorf("the calibration request failed: %w", err)
	}
	if !result.UsageReported || result.PromptTokens <= 0 {
		return 0, errors.New("the calibration request's answer carried no usage, so the prompt length cannot be sized in the engine's tokens; the endpoint must honour stream_options.include_usage")
	}
	tokensPerWord := float64(result.PromptTokens) / float64(calibrationWords)
	if err := runner.Emitter.Emit(CalibrationRecord{
		Type:          "calibration",
		PromptWords:   calibrationWords,
		PromptTokens:  result.PromptTokens,
		TokensPerWord: math.Round(tokensPerWord*1000) / 1000,
	}); err != nil {
		return 0, err
	}
	return tokensPerWord, nil
}

// request sends one streaming request whose prompt starts with text unique
// to it, so no prefix cache answers any of it.
func (runner *Runner) request(ctx context.Context, words, maxTokens int) (api.ChatResult, error) {
	unique := make([]byte, 8)
	if _, err := rand.Read(unique); err != nil {
		return api.ChatResult{}, fmt.Errorf("could not make a unique prompt start: %w", err)
	}
	prompt := hex.EncodeToString(unique) + " " + api.RandomWords(words)
	var options []option.RequestOption
	if runner.Config.IgnoreEOS {
		options = append(options, option.WithJSONSet("ignore_eos", true))
	}
	requestCtx, cancel := context.WithTimeout(ctx, runner.Config.RequestTimeout)
	defer cancel()
	return api.StreamChat(requestCtx, runner.Client, api.ChatRequest{
		Model:     runner.Config.Model,
		Prompt:    prompt,
		MaxTokens: maxTokens,
		Options:   options,
	})
}

// measureLevel measures one concurrency level at a constant concurrency:
// `concurrency` lanes each send a request, and the next one the moment it
// ends, so exactly that many are in flight the whole time.
//
//   - Ramp-up: the window starts once every lane has finished its first
//     request. Those first requests are the warm-up: the engine meets this
//     batch shape, and the load reaches its steady state.
//   - Window: the first max(MinRequests, 2 x concurrency) requests sent after
//     the start are the level's sample. The lanes keep sending while any of
//     them runs, so each ran under the full concurrency; the window ends when
//     the last of them ends.
//   - Drain: requests still running at the end finish, and count only for the
//     part of their work inside the window.
//
// A failed request ends the level: no new request starts, the running ones
// finish, and the level is reported with its first failure.
func (runner *Runner) measureLevel(ctx context.Context, concurrency int, lengths *lengthDrawer) (levelResult, error) {
	config := runner.Config
	started := time.Now()
	sample := max(config.MinRequests, 2*concurrency)

	var (
		mu           sync.Mutex
		requests     []finished
		lanesWarm    = make([]bool, concurrency)
		warmLanes    int
		windowStart  time.Time
		windowEnd    time.Time
		countedSent  int
		countedEnded int
		failed       bool
		firstFailure error
		done         bool
	)
	// next decides, under the lock, whether a lane sends another request and
	// whether that request is part of the sample.
	next := func() (send bool, counted bool) {
		if failed || done {
			return false, false
		}
		if !windowStart.IsZero() && countedSent < sample {
			countedSent++
			return true, true
		}
		return true, false
	}
	// A WaitGroup with lanes, not errgroup: a failure is data, and the other
	// lanes' running requests finish so their figures stay whole.
	var group sync.WaitGroup
	for lane := range concurrency {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				mu.Lock()
				send, counted := next()
				var shape requestShape
				if send {
					shape = lengths.draw(1)[0]
				}
				mu.Unlock()
				if !send || ctx.Err() != nil {
					return
				}
				sentAt := time.Now()
				result, err := runner.request(ctx, shape.words, shape.answerTokens)
				record := finished{lane: lane, sentAt: sentAt, shape: shape, counted: counted, failure: err}
				if err == nil && (result.FirstTokenAt.IsZero() || !result.UsageReported) {
					record.failure = errors.New("the answer carried no output token or no usage, so its timing and token counts cannot be measured")
				}
				if record.failure == nil {
					record.prompt = result.PromptTokens
					record.answer = result.CompletionTokens
					record.firstAt = result.FirstTokenAt
					record.endedAt = result.EndedAt
					record.shortAns = config.IgnoreEOS && result.CompletionTokens < shape.answerTokens
				}
				mu.Lock()
				requests = append(requests, record)
				if record.failure != nil && ctx.Err() == nil {
					if !failed {
						firstFailure = record.failure
					}
					failed = true
				}
				if !lanesWarm[lane] {
					lanesWarm[lane] = true
					warmLanes++
					if warmLanes == concurrency {
						windowStart = time.Now()
					}
				}
				if counted {
					countedEnded++
					if countedEnded == sample {
						done = true
						windowEnd = time.Now()
					}
				}
				mu.Unlock()
			}
		}()
	}
	group.Wait()
	if ctx.Err() != nil {
		return levelResult{}, ctx.Err()
	}

	record := LevelRecord{Type: "level", Concurrency: concurrency}
	for _, request := range requests {
		if !request.counted {
			continue
		}
		record.Requests++
		if request.failure != nil {
			record.RequestsFailed++
		}
	}
	if failed {
		record.RequestsFailed = max(record.RequestsFailed, 1)
		record.FirstFailure = ptr(firstFailure.Error())
	}
	if !failed {
		figures := computeWindow(requests, windowStart, windowEnd)
		record.WindowSeconds = ptr(math.Round(figures.WindowSeconds*100) / 100)
		record.RequestsPerSecond = roundTo(figures.RequestsPerSecond, 3)
		record.PrefillTokensPerSecond = roundTo(figures.PrefillTokensPerSecond, 2)
		record.DecodeTokensPerSecond = roundTo(figures.DecodeTokensPerSecond, 2)
		record.DecodeTokensPerSecondCV = roundTo(figures.DecodeCV, 3)
		record.TTFTSecondsP50 = roundTo(figures.TTFT[0], 3)
		record.TTFTSecondsP90 = roundTo(figures.TTFT[1], 3)
		record.TTFTSecondsP95 = roundTo(figures.TTFT[2], 3)
		record.ITLSecondsP50 = roundTo(figures.ITL[0], 5)
		record.ITLSecondsP90 = roundTo(figures.ITL[1], 5)
		record.ITLSecondsP95 = roundTo(figures.ITL[2], 5)
		record.RequestLatencySecondsP50 = roundTo(figures.RequestLatency[0], 3)
		record.RequestLatencySecondsP90 = roundTo(figures.RequestLatency[1], 3)
		record.RequestLatencySecondsP95 = roundTo(figures.RequestLatency[2], 3)
		record.PromptTokensMean = roundTo(figures.PromptMean, 1)
		record.PromptTokensStddev = roundTo(figures.PromptStddev, 1)
		record.CompletionTokensMean = roundTo(figures.CompletionMean, 1)
		record.CompletionTokensStddev = roundTo(figures.CompletionStddev, 1)
	}
	if config.IgnoreEOS {
		shortAnswers := 0
		for _, request := range requests {
			if request.counted && request.shortAns {
				shortAnswers++
			}
		}
		record.ShortAnswers = ptr(shortAnswers)
	}
	record.DurationSeconds = math.Round(time.Since(started).Seconds()*100) / 100
	return levelResult{
		record:  record,
		prefill: valueOr(record.PrefillTokensPerSecond),
		decode:  valueOr(record.DecodeTokensPerSecond),
		ttftP90: valueOr(record.TTFTSecondsP90),
		failed:  failed,
	}, nil
}

func valueOr(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}
