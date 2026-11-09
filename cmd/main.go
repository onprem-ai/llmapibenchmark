package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Yoosu-L/llmapibenchmark/internal/api"
	"github.com/Yoosu-L/llmapibenchmark/internal/ladder"
	"github.com/Yoosu-L/llmapibenchmark/internal/utils"
	"github.com/openai/openai-go/v3/option"
	"github.com/spf13/pflag"
)

// version is the tool's version, set at build time with
// -ldflags "-X main.version=...".
var version = "dev"

const (
	defaultPrompt = "Write a long story, no less than 10,000 words, starting from a long, long time ago."
)

// mergeOutputValue rewrites "--output path" into "--output=path". A flag with
// NoOptDefVal only accepts the "=" form, so the space-separated spelling would
// otherwise swallow nothing and leave "path" as a positional arg.
func mergeOutputValue(args []string) []string {
	merged := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--output" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			merged = append(merged, "--output="+args[i+1])
			i++
			continue
		}
		merged = append(merged, args[i])
	}
	return merged
}

func main() {
	baseURL := pflag.StringP("base-url", "u", "", "Base URL of the OpenAI API")
	apiVersion := pflag.StringP("api-version", "v", "", "API version (api-version) query parameter value")
	apiKey := pflag.StringP("api-key", "k", "", "API key for authentication")
	model := pflag.StringP("model", "m", "", "Model to be used for the requests (optional)")
	prompt := pflag.StringP("prompt", "p", defaultPrompt, "Prompt to be used for generating responses")
	numWords := pflag.IntP("num-words", "n", 0, "If set to a value above 0 a random string with this length will be used as prompt")
	concurrencyStr := pflag.StringP("concurrency", "c", "1", "Comma-separated list of concurrency levels")
	maxTokens := pflag.IntP("max-tokens", "t", 512, "Maximum number of tokens to generate")
	outputPath := pflag.String("output", "", "Save results to a Markdown file (prepend, keeps history). With no value, writes ./llm-api-benchmark.md")
	format := pflag.StringP("format", "f", "", "Output format (optional)")
	help := pflag.BoolP("help", "h", false, "Show this help message")
	workload := pflag.String("workload", "", "Ladder mode: versioned workloads, comma-separated and measured in this order; each sets every length and ladder flag below; of "+strings.Join(ladder.WorkloadIDs(), ", "))
	listWorkloads := pflag.Bool("list-workloads", false, "Write every versioned workload this load generator measures, one JSON object per line, and exit")
	maxContext := pflag.Int("max-context-tokens", 0, "Ladder mode with --workload: the model's context length; longer prompts are shortened to fit it with the longest answer")
	promptTokens := pflag.Int("prompt-tokens", 0, "Ladder mode: the prompt length in the engine's own tokens (calibrated with one request)")
	promptTokensMax := pflag.Int("prompt-tokens-max", 0, "Ladder mode: when above --prompt-tokens, every request draws its prompt length from --prompt-tokens to this, log-uniformly")
	maxTokensMin := pflag.Int("max-tokens-min", 0, "Ladder mode: when above 0 and below --max-tokens, every request draws its answer length from this to --max-tokens, log-uniformly")
	seed := pflag.Int64("seed", 1, "Ladder mode: the seed of the drawn lengths; one seed sends the same lengths on every run")
	ignoreEOS := pflag.Bool("ignore-eos", false, "Send \"ignore_eos\": true, so every answer is --max-tokens long (vLLM, SGLang)")
	measureThrough := pflag.Int("measure-through", 32, "Ladder mode: every level up to this concurrency is measured")
	maxConcurrency := pflag.Int("max-concurrency", 128, "Ladder mode: the highest concurrency of the ladder 1,2,4,8,12,16,20,24,32,40,48,64,80,96,112,128")
	minRequests := pflag.Int("min-requests", 20, "Ladder mode: the smallest sample of a level; a level measures at least this many requests, and at least 2 x its concurrency")
	plateauOn := pflag.String("plateau-on", "decode", "Ladder mode: the throughput that decides the plateau: prefill, decode or both")
	plateauGainPercent := pflag.Float64("plateau-gain-percent", 5, "Ladder mode: a level gains when it beats the best earlier level by more than this percentage")
	stopAfter := pflag.Int("stop-after-levels-without-gain", 2, "Ladder mode: after --measure-through, stop after this many levels in a row without a gain")
	maxTTFTP90 := pflag.Float64("max-ttft-p90", 0, "Ladder mode: after --measure-through, stop when a level's p90 time to first token exceeds this many seconds (0: no limit)")
	requestTimeout := pflag.Float64("request-timeout", 600, "Ladder mode: the longest one request may take, in seconds")
	insecureSkipTLSVerify := pflag.Bool("insecure-skip-tls-verify", false, "Skip TLS certificate verification. Use with caution, this is insecure.")
	// Allow a bare --output (no value) meaning "save to the default file".
	pflag.Lookup("output").NoOptDefVal = "llm-api-benchmark.md"
	pflag.CommandLine.Parse(mergeOutputValue(os.Args[1:]))

	if *help {
		fmt.Printf("Usage of %s:\n", os.Args[0])
		pflag.PrintDefaults()
		os.Exit(0)
	}

	// Create benchmark
	benchmark := Benchmark{}
	benchmark.BaseURL = *baseURL
	benchmark.ApiVersion = *apiVersion
	benchmark.ApiKey = *apiKey
	benchmark.ModelName = *model
	benchmark.Prompt = *prompt
	// Since each random word is roughly equivalent to 4 tokens (varies by model tokenizer),
	// we divide the target token count by 4. This is an estimation.
	benchmark.NumWords = *numWords / 4
	if *numWords > 0 && benchmark.NumWords == 0 {
		benchmark.NumWords = 1
	}
	benchmark.MaxTokens = *maxTokens
	benchmark.OutputPath = *outputPath

	// Parse concurrency levels
	if *concurrencyStr != "auto" {
		concurrencyLevels, err := utils.ParseConcurrencyLevels(*concurrencyStr)
		if err != nil {
			log.Fatalf("Invalid concurrency levels: %v", err)
		}
		benchmark.ConcurrencyLevels = concurrencyLevels
	}

	if *listWorkloads {
		os.Exit(writeWorkloads())
	}

	// Initialize OpenAI client
	if *baseURL == "" {
		log.Fatalf("--base-url is required")
	}

	var clientOptions []option.RequestOption
	if *insecureSkipTLSVerify {
		fmt.Fprintln(os.Stderr, "\n/!\\ WARNING: Skipping TLS certificate verification. This is insecure and should not be used in production. /!\\")

		// Clone the default Transport to preserve its settings
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			log.Fatalf("http.DefaultTransport is not an *http.Transport")
		}
		tr := defaultTransport.Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		clientOptions = append(clientOptions, option.WithHTTPClient(&http.Client{Transport: tr}))
	}

	if *concurrencyStr == "auto" && *workload != "" {
		var configs []ladder.Config
		for _, id := range strings.Split(*workload, ",") {
			config, err := ladder.ConfigFor(strings.TrimSpace(id), *maxContext, time.Duration(*requestTimeout*float64(time.Second)), version)
			if err != nil {
				os.Exit(runLadder(ladderSettings{format: *format, setupError: err}))
			}
			config.Model = *model
			configs = append(configs, config)
		}
		os.Exit(runLadder(ladderSettings{
			baseURL: *baseURL, apiVersion: *apiVersion, apiKey: *apiKey, model: *model,
			format: *format, clientOptions: clientOptions, configs: configs,
		}))
	}
	if *concurrencyStr == "auto" {
		os.Exit(runLadder(ladderSettings{
			baseURL: *baseURL, apiVersion: *apiVersion, apiKey: *apiKey, model: *model,
			format: *format, clientOptions: clientOptions,
			configs: []ladder.Config{{
				PromptTokens:               *promptTokens,
				AnswerTokens:               *maxTokens,
				PromptTokensMax:            *promptTokensMax,
				AnswerTokensMin:            *maxTokensMin,
				Seed:                       *seed,
				IgnoreEOS:                  *ignoreEOS,
				MeasureThrough:             *measureThrough,
				MaxConcurrency:             *maxConcurrency,
				MinRequests:                *minRequests,
				PlateauOn:                  ladder.PlateauOn(*plateauOn),
				PlateauGainPercent:         *plateauGainPercent,
				StopAfterLevelsWithoutGain: *stopAfter,
				MaxTTFTP90:                 time.Duration(*maxTTFTP90 * float64(time.Second)),
				RequestTimeout:             time.Duration(*requestTimeout * float64(time.Second)),
				ToolVersion:                version,
			}},
		}))
	}

	// The same client is reused for benchmark requests, so TLS and retry
	// options apply consistently instead of only to model discovery.
	client := api.NewClient(*baseURL, *apiVersion, *apiKey, clientOptions...)
	benchmark.Client = client

	// Discover model name if not provided
	if *model == "" {
		discoveredModel, err := api.GetFirstAvailableModel(client)
		if err != nil {
			log.Printf("Error discovering model: %v", err)
			return
		}
		benchmark.ModelName = discoveredModel
	}

	// Determine input parameters and call benchmark function
	if *prompt != "Write a long story, no less than 10,000 words, starting from a long, long time ago." {
		benchmark.UseRandomInput = false
	} else if *numWords != 0 {
		benchmark.UseRandomInput = true
	} else {
		benchmark.UseRandomInput = false
	}

	// InputTokens is no longer probed up front: it is backfilled from the
	// first benchmark request's usage chunk (see runCli/run in benchmark.go).
	if *format == "" {
		err := benchmark.runCli()
		if err != nil {
			log.Fatalf("Error running benchmark: %v", err)
		}
	} else {
		result, err := benchmark.run()
		if err != nil {
			log.Fatalf("Error running benchmark: %v", err)
		}

		var output string
		switch *format {
		case "json":
			output, err = result.Json()
		case "yaml":
			output, err = result.Yaml()
		default:
			log.Printf("Invalid format specified")
		}
		if err != nil {
			log.Fatalf("Error formatting benchmark result: %v", err)
		}
		fmt.Println(output)
	}
}
