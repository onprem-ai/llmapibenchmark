// Package ladder measures one inference engine replica at a fixed ladder of
// concurrency levels and reports each level as one machine-readable record.
//
// At every level the runner keeps exactly `concurrency` requests in flight,
// measures inside a steady-state window, and derives prefill and decode
// throughput and the latency percentiles from the engine's own token counts. The ladder stops when throughput no longer
// grows (a plateau), when time to first token exceeds a limit, when a request
// fails, or when the ladder is exhausted.
package ladder

import (
	"fmt"
	"time"
)

// Levels is the default concurrency ladder, cut at Config.MaxConcurrency; a
// workload carries its own.
var Levels = []int{1, 2, 4, 8, 12, 16, 20, 24, 32, 40, 48, 64, 80, 96, 112, 128}

// PlateauOn names the throughput that decides whether a level gained.
type PlateauOn string

const (
	PlateauOnPrefill PlateauOn = "prefill"
	PlateauOnDecode  PlateauOn = "decode"
	PlateauOnBoth    PlateauOn = "both"
)

// ParsePlateauOn validates a --plateau-on value.
func ParsePlateauOn(value string) (PlateauOn, error) {
	switch PlateauOn(value) {
	case PlateauOnPrefill, PlateauOnDecode, PlateauOnBoth:
		return PlateauOn(value), nil
	}
	return "", fmt.Errorf("invalid --plateau-on %q: use prefill, decode or both", value)
}

// Stop reasons reported in the end record.
const (
	StopPlateau        = "plateau"
	StopRequestFailed  = "request-failed"
	StopTTFTLimit      = "time-to-first-token-limit"
	StopMaxConcurrency = "max-concurrency"
	StopCancelled      = "cancelled"
)

// Config holds every setting of one ladder run.
type Config struct {
	// Workload is the versioned workload ID the settings come from; empty for a
	// run set up with the low-level flags, whose results compare with nothing.
	Workload string
	Model    string
	// PromptTokens is the target prompt length in the engine's token count.
	PromptTokens int
	// AnswerTokens is the max_tokens of every measured request.
	AnswerTokens int
	// PromptTokensMax, when above PromptTokens, makes every request draw its
	// prompt length from PromptTokens to PromptTokensMax, log-uniformly: as
	// many short prompts per doubling of length as long ones.
	PromptTokensMax int
	// AnswerTokensMin, when above zero and below AnswerTokens, makes every
	// request draw its answer length from AnswerTokensMin to AnswerTokens,
	// log-uniformly.
	AnswerTokensMin int
	// Seed seeds the drawn lengths: two runs with one seed send the same
	// lengths in the same order.
	Seed int64
	// IgnoreEOS sends "ignore_eos": true, so every answer is AnswerTokens long.
	IgnoreEOS bool
	// Levels is the ladder; nil is Levels.
	Levels []int
	// MeasureThrough is the highest level that is always measured.
	MeasureThrough int
	MaxConcurrency int
	// MinRequests is the smallest sample a level measures; a level measures
	// at least 2 x concurrency requests whatever it is.
	MinRequests int
	PlateauOn   PlateauOn
	// PlateauGainPercent is the improvement over the best earlier level that
	// counts as a gain.
	PlateauGainPercent float64
	// StopAfterLevelsWithoutGain is the number of levels in a row without a
	// gain that ends the ladder once MeasureThrough is reached.
	StopAfterLevelsWithoutGain int
	// MaxTTFTP90 ends the ladder, once MeasureThrough is reached, when a
	// level's p90 time to first token exceeds it. Zero disables the limit.
	MaxTTFTP90     time.Duration
	RequestTimeout time.Duration
	ToolVersion    string
}

// Validate refuses settings the ladder cannot run with.
func (config Config) Validate() error {
	switch {
	case config.PromptTokens <= 0:
		return fmt.Errorf("--prompt-tokens must be above 0 with --concurrency auto (got %d)", config.PromptTokens)
	case config.AnswerTokens <= 0:
		return fmt.Errorf("--max-tokens must be above 0 (got %d)", config.AnswerTokens)
	case config.MaxConcurrency < Levels[0]:
		return fmt.Errorf("--max-concurrency must be at least %d (got %d)", Levels[0], config.MaxConcurrency)
	case config.MeasureThrough < 0:
		return fmt.Errorf("--measure-through must not be negative (got %d)", config.MeasureThrough)
	case config.PromptTokensMax != 0 && config.PromptTokensMax < config.PromptTokens:
		return fmt.Errorf("--prompt-tokens-max must not be below --prompt-tokens (got %d and %d)", config.PromptTokensMax, config.PromptTokens)
	case config.AnswerTokensMin < 0 || config.AnswerTokensMin > config.AnswerTokens:
		return fmt.Errorf("--max-tokens-min must be from 0 to --max-tokens (got %d and %d)", config.AnswerTokensMin, config.AnswerTokens)
	case config.MinRequests <= 0:
		return fmt.Errorf("--min-requests must be above 0 (got %d)", config.MinRequests)
	case config.PlateauGainPercent < 0:
		return fmt.Errorf("--plateau-gain-percent must not be negative (got %g)", config.PlateauGainPercent)
	case config.StopAfterLevelsWithoutGain <= 0:
		return fmt.Errorf("--stop-after-levels-without-gain must be above 0 (got %d)", config.StopAfterLevelsWithoutGain)
	case config.MaxTTFTP90 < 0:
		return fmt.Errorf("--max-ttft-p90 must not be negative (got %s)", config.MaxTTFTP90)
	case config.RequestTimeout <= 0:
		return fmt.Errorf("--request-timeout must be above 0 (got %s)", config.RequestTimeout)
	}
	if _, err := ParsePlateauOn(string(config.PlateauOn)); err != nil {
		return err
	}
	return nil
}

// Ladder returns the levels up to and including MaxConcurrency.
func (config Config) Ladder() []int {
	ladder := config.Levels
	if ladder == nil {
		ladder = Levels
	}
	levels := make([]int, 0, len(ladder))
	for _, level := range ladder {
		if level <= config.MaxConcurrency {
			levels = append(levels, level)
		}
	}
	return levels
}

// MixedLengths reports whether requests draw their lengths from a range.
func (config Config) MixedLengths() bool {
	return config.PromptTokensMax > config.PromptTokens ||
		(config.AnswerTokensMin > 0 && config.AnswerTokensMin < config.AnswerTokens)
}
