package ladder

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// A workload is one versioned way of measuring a replica: its request
// shapes, its ladder, and the size of each level's sample. Two results are
// comparable when their workloads share a name and a major version.
//
// Versioning, semantic:
//   - major: results no longer compare (other lengths, distribution, seed,
//     figure definitions, sample size, measurement method);
//   - minor: results still compare at the levels and series both versions
//     have (more levels on the ladder, a new series);
//   - patch: a fix that changes no figure.
//
// A definition here never changes without its version. The test
// TestEveryWorkloadDefinitionIsPinned fails when one does.
type Workload struct {
	// ID is name/major.minor.patch, for example mixed-load/1.0.0.
	ID string
	// What the workload stands for, for people.
	Represents string

	PromptTokens    int
	PromptTokensMax int
	AnswerTokensMin int
	AnswerTokens    int
	// Distribution of drawn lengths when a range is set.
	Distribution string
	Seed         int64
	IgnoreEOS    bool

	Levels                     []int
	MeasureThrough             int
	MinRequests                int
	PlateauOn                  PlateauOn
	PlateauGainPercent         float64
	StopAfterLevelsWithoutGain int
}

// The ladder every version 1 workload climbs.
var ladderV1 = []int{1, 2, 4, 8, 12, 16, 20, 24, 32, 40, 48, 64, 80, 96, 112, 128}

// Workloads are every workload this tool measures, by ID.
var Workloads = map[string]Workload{
	"max-prefill/1.0.0": {
		ID:                         "max-prefill/1.0.0",
		Represents:                 "long prompts with short answers: the replica's prefill capacity",
		PromptTokens:               8192,
		AnswerTokens:               64,
		IgnoreEOS:                  true,
		Levels:                     ladderV1,
		MeasureThrough:             32,
		MinRequests:                20,
		PlateauOn:                  PlateauOnPrefill,
		PlateauGainPercent:         5,
		StopAfterLevelsWithoutGain: 2,
	},
	"max-decode/1.0.0": {
		ID:                         "max-decode/1.0.0",
		Represents:                 "short prompts with long answers: the replica's decode capacity",
		PromptTokens:               256,
		AnswerTokens:               1024,
		IgnoreEOS:                  true,
		Levels:                     ladderV1,
		MeasureThrough:             32,
		MinRequests:                20,
		PlateauOn:                  PlateauOnDecode,
		PlateauGainPercent:         5,
		StopAfterLevelsWithoutGain: 2,
	},
	"mixed-load/1.0.0": {
		ID:                         "mixed-load/1.0.0",
		Represents:                 "real traffic: short and long requests batched together",
		PromptTokens:               128,
		PromptTokensMax:            8192,
		AnswerTokensMin:            64,
		AnswerTokens:               1024,
		Distribution:               "log-uniform",
		Seed:                       1,
		IgnoreEOS:                  true,
		Levels:                     ladderV1,
		MeasureThrough:             32,
		MinRequests:                20,
		PlateauOn:                  PlateauOnBoth,
		PlateauGainPercent:         5,
		StopAfterLevelsWithoutGain: 2,
	},
}

// WorkloadIDs are every workload ID, sorted.
func WorkloadIDs() []string {
	ids := make([]string, 0, len(Workloads))
	for id := range Workloads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ConfigFor builds the run's settings from a workload. maxPromptTokens, when
// above 0, shortens the prompts to fit the model's context (the longest
// answer and a margin for the chat template stay free); the start record
// reports the lengths used.
func ConfigFor(id string, maxContextTokens int, requestTimeout time.Duration, toolVersion string) (Config, error) {
	workload, ok := Workloads[id]
	if !ok {
		return Config{}, fmt.Errorf("unknown workload %q; this load generator measures %s", id, strings.Join(WorkloadIDs(), ", "))
	}
	config := Config{
		Workload:                   workload.ID,
		PromptTokens:               workload.PromptTokens,
		PromptTokensMax:            workload.PromptTokensMax,
		AnswerTokensMin:            workload.AnswerTokensMin,
		AnswerTokens:               workload.AnswerTokens,
		Seed:                       workload.Seed,
		IgnoreEOS:                  workload.IgnoreEOS,
		Levels:                     workload.Levels,
		MeasureThrough:             workload.MeasureThrough,
		MaxConcurrency:             workload.Levels[len(workload.Levels)-1],
		MinRequests:                workload.MinRequests,
		PlateauOn:                  workload.PlateauOn,
		PlateauGainPercent:         workload.PlateauGainPercent,
		StopAfterLevelsWithoutGain: workload.StopAfterLevelsWithoutGain,
		RequestTimeout:             requestTimeout,
		ToolVersion:                toolVersion,
	}
	if maxContextTokens > 0 {
		room := max(64, maxContextTokens-workload.AnswerTokens-256)
		config.PromptTokens = min(config.PromptTokens, room)
		if config.PromptTokensMax > 0 {
			config.PromptTokensMax = max(config.PromptTokens, min(config.PromptTokensMax, room))
		}
	}
	return config, nil
}

// WorkloadRecord is one workload as --list-workloads writes it.
type WorkloadRecord struct {
	Type                       string  `json:"type"`
	Workload                   string  `json:"workload"`
	Represents                 string  `json:"represents"`
	PromptTokens               int     `json:"prompt_tokens"`
	PromptTokensMax            int     `json:"prompt_tokens_max,omitempty"`
	AnswerTokensMin            int     `json:"answer_tokens_min,omitempty"`
	AnswerTokens               int     `json:"answer_tokens"`
	Distribution               string  `json:"distribution,omitempty"`
	Seed                       int64   `json:"seed,omitempty"`
	IgnoreEOS                  bool    `json:"ignore_eos"`
	Levels                     []int   `json:"levels"`
	MeasureThrough             int     `json:"measure_through"`
	MinRequests                int     `json:"min_requests"`
	PlateauOn                  string  `json:"plateau_on"`
	PlateauGainPercent         float64 `json:"plateau_gain_percent"`
	StopAfterLevelsWithoutGain int     `json:"stop_after_levels_without_gain"`
}

// WorkloadRecordOf is a workload's definition as one JSON line.
func WorkloadRecordOf(workload Workload) WorkloadRecord {
	return WorkloadRecord{
		Type:                       "workload",
		Workload:                   workload.ID,
		Represents:                 workload.Represents,
		PromptTokens:               workload.PromptTokens,
		PromptTokensMax:            workload.PromptTokensMax,
		AnswerTokensMin:            workload.AnswerTokensMin,
		AnswerTokens:               workload.AnswerTokens,
		Distribution:               workload.Distribution,
		Seed:                       workload.Seed,
		IgnoreEOS:                  workload.IgnoreEOS,
		Levels:                     workload.Levels,
		MeasureThrough:             workload.MeasureThrough,
		MinRequests:                workload.MinRequests,
		PlateauOn:                  string(workload.PlateauOn),
		PlateauGainPercent:         workload.PlateauGainPercent,
		StopAfterLevelsWithoutGain: workload.StopAfterLevelsWithoutGain,
	}
}
