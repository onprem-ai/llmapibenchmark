package ladder

import (
	"math"
	"time"
)

// windowSubdivisions is how many equal parts of the measurement window the
// stability figure compares: the coefficient of variation of the decode
// throughput across them.
const windowSubdivisions = 4

// finished is one request of a level, as the lane that sent it saw it.
type finished struct {
	lane     int
	sentAt   time.Time
	shape    requestShape
	prompt   int
	answer   int
	firstAt  time.Time
	endedAt  time.Time
	failure  error
	shortAns bool
	// counted marks the requests of the level's sample.
	counted bool
}

// windowFigures are a level's figures, all computed from the requests of its
// measurement window. A nil figure could not be measured.
type windowFigures struct {
	WindowSeconds          float64
	Requests               int
	RequestsPerSecond      *float64
	PrefillTokensPerSecond *float64
	DecodeTokensPerSecond  *float64
	DecodeCV               *float64
	TTFT                   [3]*float64
	ITL                    [3]*float64
	RequestLatency         [3]*float64
	PromptMean             *float64
	PromptStddev           *float64
	CompletionMean         *float64
	CompletionStddev       *float64
}

// percentiles are the three every latency is reported at.
var percentiles = [3]float64{50, 90, 95}

// computeWindow derives a level's figures from every request it sent and
// its measurement window [start, end).
//
// Throughput counts what the replica did inside the window, whichever
// request it belonged to: a request's prompt tokens count at its first
// token (the moment its prefill ended), and its generated tokens after the
// first are spread evenly from its first token to its end, so the share
// inside the window counts. Both are divided by the window's length. This is
// the server's rate at a steady concurrency, without the ramp-up and the
// drain.
//
// The per-request figures (latencies, lengths) come from the level's sample:
// the requests marked counted, every one of which ran under the full
// concurrency.
func computeWindow(requests []finished, start, end time.Time) windowFigures {
	figures := windowFigures{}
	length := end.Sub(start).Seconds()
	if length <= 0 {
		return figures
	}
	figures.WindowSeconds = length
	var prefillTokens, decodeTokens float64
	completedInside := 0
	subLength := length / windowSubdivisions
	var subDecode [windowSubdivisions]float64
	var ttfts, itls, latencies, prompts, answers []float64
	for _, request := range requests {
		if request.failure != nil || request.firstAt.IsZero() {
			continue
		}
		if !request.firstAt.Before(start) && request.firstAt.Before(end) {
			prefillTokens += float64(request.prompt)
		}
		if !request.endedAt.Before(start) && request.endedAt.Before(end) {
			completedInside++
		}
		if decoded := float64(request.answer - 1); decoded > 0 {
			span := request.endedAt.Sub(request.firstAt).Seconds()
			if span > 0 {
				rate := decoded / span
				decodeTokens += rate * overlap(request.firstAt, request.endedAt, start, end)
				for part := range subDecode {
					partStart := start.Add(time.Duration(float64(part) * subLength * float64(time.Second)))
					partEnd := start.Add(time.Duration(float64(part+1) * subLength * float64(time.Second)))
					subDecode[part] += rate * overlap(request.firstAt, request.endedAt, partStart, partEnd)
				}
			}
		}
		if !request.counted {
			continue
		}
		ttfts = append(ttfts, request.firstAt.Sub(request.sentAt).Seconds())
		latencies = append(latencies, request.endedAt.Sub(request.sentAt).Seconds())
		if request.answer > 1 {
			itls = append(itls, request.endedAt.Sub(request.firstAt).Seconds()/float64(request.answer-1))
		}
		prompts = append(prompts, float64(request.prompt))
		answers = append(answers, float64(request.answer))
	}
	figures.Requests = len(ttfts)
	figures.RequestsPerSecond = ptr(float64(completedInside) / length)
	figures.PrefillTokensPerSecond = ptr(prefillTokens / length)
	figures.DecodeTokensPerSecond = ptr(decodeTokens / length)
	rates := make([]float64, windowSubdivisions)
	for part := range subDecode {
		rates[part] = subDecode[part] / subLength
	}
	figures.DecodeCV = coefficientOfVariation(rates)
	for index, p := range percentiles {
		figures.TTFT[index] = nearestRank(ttfts, p)
		figures.ITL[index] = nearestRank(itls, p)
		figures.RequestLatency[index] = nearestRank(latencies, p)
	}
	figures.PromptMean, figures.PromptStddev = meanAndStddev(prompts)
	figures.CompletionMean, figures.CompletionStddev = meanAndStddev(answers)
	return figures
}

// overlap is the seconds [from, to) and [start, end) share.
func overlap(from, to, start, end time.Time) float64 {
	low := from
	if start.After(low) {
		low = start
	}
	high := to
	if end.Before(high) {
		high = end
	}
	return math.Max(0, high.Sub(low).Seconds())
}

// meanAndStddev is the mean and the population standard deviation; nil for
// an empty slice.
func meanAndStddev(values []float64) (*float64, *float64) {
	if len(values) == 0 {
		return nil, nil
	}
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	mean := sum / float64(len(values))
	squares := 0.0
	for _, value := range values {
		squares += (value - mean) * (value - mean)
	}
	return ptr(mean), ptr(math.Sqrt(squares / float64(len(values))))
}

// coefficientOfVariation is the standard deviation divided by the mean; nil
// when the mean is 0.
func coefficientOfVariation(values []float64) *float64 {
	mean, stddev := meanAndStddev(values)
	if mean == nil || *mean == 0 {
		return nil
	}
	return ptr(*stddev / *mean)
}
