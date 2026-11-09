package ladder

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// StartRecord opens the output and repeats every setting of the run.
type StartRecord struct {
	Type                       string  `json:"type"`
	Workload                   string  `json:"workload,omitempty"`
	ToolVersion                string  `json:"tool_version"`
	Model                      string  `json:"model"`
	PromptTokensTarget         int     `json:"prompt_tokens_target"`
	PromptTokensMax            int     `json:"prompt_tokens_max,omitempty"`
	AnswerTokens               int     `json:"answer_tokens"`
	AnswerTokensMin            int     `json:"answer_tokens_min,omitempty"`
	Seed                       int64   `json:"seed,omitempty"`
	IgnoreEOS                  bool    `json:"ignore_eos"`
	Ladder                     []int   `json:"ladder"`
	MeasureThrough             int     `json:"measure_through"`
	MinRequests                int     `json:"min_requests"`
	PlateauOn                  string  `json:"plateau_on"`
	PlateauGainPercent         float64 `json:"plateau_gain_percent"`
	StopAfterLevelsWithoutGain int     `json:"stop_after_levels_without_gain"`
	MaxTTFTP90Seconds          float64 `json:"max_ttft_p90_seconds"`
}

// CalibrationRecord reports how the engine counts the random-word prompt.
type CalibrationRecord struct {
	Type          string  `json:"type"`
	PromptWords   int     `json:"prompt_words"`
	PromptTokens  int     `json:"prompt_tokens"`
	TokensPerWord float64 `json:"tokens_per_word"`
}

// LevelRecord reports one finished concurrency level. Every figure comes from
// the level's measurement window at a constant concurrency; a figure is null
// when the level could not measure it (a failed level measures none).
type LevelRecord struct {
	Type           string   `json:"type"`
	Concurrency    int      `json:"concurrency"`
	WindowSeconds  *float64 `json:"window_seconds"`
	Requests       int      `json:"requests"`
	RequestsFailed int      `json:"requests_failed"`
	// RequestsPerSecond is the requests completed inside the window per
	// second of it.
	RequestsPerSecond *float64 `json:"requests_per_second"`
	// The replica's throughput: tokens processed inside the window per second.
	PrefillTokensPerSecond *float64 `json:"prefill_tokens_per_second"`
	DecodeTokensPerSecond  *float64 `json:"decode_tokens_per_second"`
	// DecodeTokensPerSecondCV is the coefficient of variation of the decode
	// throughput across the window's four quarters: how steady it was.
	DecodeTokensPerSecondCV *float64 `json:"decode_tokens_per_second_cv"`
	// What one user sees, over the level's sample.
	TTFTSecondsP50           *float64 `json:"time_to_first_token_seconds_p50"`
	TTFTSecondsP90           *float64 `json:"time_to_first_token_seconds_p90"`
	TTFTSecondsP95           *float64 `json:"time_to_first_token_seconds_p95"`
	ITLSecondsP50            *float64 `json:"inter_token_latency_seconds_p50"`
	ITLSecondsP90            *float64 `json:"inter_token_latency_seconds_p90"`
	ITLSecondsP95            *float64 `json:"inter_token_latency_seconds_p95"`
	RequestLatencySecondsP50 *float64 `json:"request_latency_seconds_p50"`
	RequestLatencySecondsP90 *float64 `json:"request_latency_seconds_p90"`
	RequestLatencySecondsP95 *float64 `json:"request_latency_seconds_p95"`
	// The lengths actually served, in the engine's tokens.
	PromptTokensMean       *float64 `json:"prompt_tokens_mean"`
	PromptTokensStddev     *float64 `json:"prompt_tokens_stddev"`
	CompletionTokensMean   *float64 `json:"completion_tokens_mean"`
	CompletionTokensStddev *float64 `json:"completion_tokens_stddev"`
	// ShortAnswers counts answers shorter than they asked for. It is null
	// when ignore_eos is off, because the engine may then stop early.
	ShortAnswers    *int    `json:"short_answers"`
	DurationSeconds float64 `json:"duration_seconds"`
	FirstFailure    *string `json:"first_failure"`
}

// EndRecord closes the output.
type EndRecord struct {
	Type               string `json:"type"`
	StopReason         string `json:"stop_reason"`
	PlateauConcurrency *int   `json:"plateau_concurrency"`
}

// ErrorRecord reports a fatal error outside a level; the run ends with it.
type ErrorRecord struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Emitter writes one JSON object per line. Every record is written with one
// Write call, so an unbuffered writer such as os.Stdout delivers it at once.
type Emitter struct {
	mu  sync.Mutex
	out io.Writer
}

// NewEmitter returns an Emitter writing to out.
func NewEmitter(out io.Writer) *Emitter {
	return &Emitter{out: out}
}

// Emit writes record as one JSON line.
func (emitter *Emitter) Emit(record any) error {
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode %T record: %w", record, err)
	}
	line = append(line, '\n')

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if _, err := emitter.out.Write(line); err != nil {
		return fmt.Errorf("write %T record: %w", record, err)
	}
	if flusher, ok := emitter.out.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			return fmt.Errorf("flush %T record: %w", record, err)
		}
	}
	return nil
}

// EmitError writes an error record.
func (emitter *Emitter) EmitError(message string) error {
	return emitter.Emit(ErrorRecord{Type: "error", Message: message})
}
