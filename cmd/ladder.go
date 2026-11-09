package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/openai/openai-go/v3/option"

	"github.com/Yoosu-L/llmapibenchmark/internal/api"
	"github.com/Yoosu-L/llmapibenchmark/internal/ladder"
)

type ladderSettings struct {
	baseURL       string
	apiVersion    string
	apiKey        string
	model         string
	format        string
	clientOptions []option.RequestOption
	// configs are measured one after the other, each a whole ladder from
	// its start record to its end record.
	configs []ladder.Config
	// setupError is a refusal found before the run: it is written as the
	// error record.
	setupError error
}

// runLadder runs ladder mode (--concurrency auto) and returns the exit code.
// stdout carries only JSON lines; human progress goes to stderr. SIGTERM and
// SIGINT stop the run, which still writes its end record.
func runLadder(settings ladderSettings) int {
	emitter := ladder.NewEmitter(os.Stdout)
	fail := func(message string) int {
		if err := emitter.EmitError(message); err != nil {
			fmt.Fprintln(os.Stderr, message)
		}
		return 1
	}
	if settings.setupError != nil {
		return fail(settings.setupError.Error())
	}
	if settings.format != "jsonl" {
		return fail(fmt.Sprintf("--concurrency auto writes JSON lines only: add --format jsonl (got --format %q)", settings.format))
	}
	api.PlainWarnings = true
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	options := append([]option.RequestOption{ladder.StatusErrorMiddleware()}, settings.clientOptions...)
	client := api.NewClient(settings.baseURL, settings.apiVersion, settings.apiKey, options...)
	model := settings.model
	if model == "" {
		discovered, err := api.GetFirstAvailableModelContext(ctx, client)
		if err != nil {
			return fail(fmt.Sprintf("could not discover the served model at %s: %v", settings.baseURL, err))
		}
		model = discovered
	}
	for _, config := range settings.configs {
		if ctx.Err() != nil {
			break
		}
		config.Model = model
		if err := config.Validate(); err != nil {
			return fail(err.Error())
		}
		runner := &ladder.Runner{Client: client, Config: config, Emitter: emitter, Progress: os.Stderr}
		if err := runner.Run(ctx); err != nil {
			return fail(err.Error())
		}
	}
	return 0
}

// writeWorkloads writes every workload as one JSON line, the definitions the
// results' workload ids name (WORKLOADS.md).
func writeWorkloads() int {
	emitter := ladder.NewEmitter(os.Stdout)
	for _, id := range ladder.WorkloadIDs() {
		if err := emitter.Emit(ladder.WorkloadRecordOf(ladder.Workloads[id])); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}
