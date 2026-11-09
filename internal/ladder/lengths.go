package ladder

import (
	"math"
	"math/rand/v2"
	"sync"
)

// requestShape is one request's prompt length (in words, sized from the
// calibration) and answer length (in the engine's tokens).
type requestShape struct {
	words        int
	answerTokens int
}

// lengthDrawer gives each request its lengths. With fixed lengths every
// request gets the same shape; with mixed lengths each draws its prompt and
// answer length log-uniformly from their ranges, from a seeded source, so
// one seed sends the same lengths in the same order on every run.
type lengthDrawer struct {
	mu            sync.Mutex
	random        *rand.Rand
	tokensPerWord float64
	promptMin     int
	promptMax     int
	answerMin     int
	answerMax     int
}

func newLengthDrawer(config Config, tokensPerWord float64) *lengthDrawer {
	drawer := &lengthDrawer{
		random:        rand.New(rand.NewPCG(uint64(config.Seed), 0x6c6c6d706572)),
		tokensPerWord: tokensPerWord,
		promptMin:     config.PromptTokens,
		promptMax:     max(config.PromptTokens, config.PromptTokensMax),
		answerMin:     config.AnswerTokens,
		answerMax:     config.AnswerTokens,
	}
	if config.AnswerTokensMin > 0 && config.AnswerTokensMin < config.AnswerTokens {
		drawer.answerMin = config.AnswerTokensMin
	}
	return drawer
}

// draw gives the next count shapes, in order.
func (drawer *lengthDrawer) draw(count int) []requestShape {
	drawer.mu.Lock()
	defer drawer.mu.Unlock()
	shapes := make([]requestShape, count)
	for index := range shapes {
		promptTokens := drawer.logUniform(drawer.promptMin, drawer.promptMax)
		shapes[index] = requestShape{
			words:        drawer.wordsFor(promptTokens),
			answerTokens: drawer.logUniform(drawer.answerMin, drawer.answerMax),
		}
	}
	return shapes
}

// warmUpWords is the prompt length of the warm-up requests: the shortest
// one, so the warm-up costs little.
func (drawer *lengthDrawer) warmUpWords() int {
	return drawer.wordsFor(drawer.promptMin)
}

func (drawer *lengthDrawer) wordsFor(promptTokens int) int {
	return max(1, int(math.Round(float64(promptTokens)/drawer.tokensPerWord)))
}

// logUniform draws from low to high so that every doubling of the length is
// as likely as any other: short requests are as common as long ones by
// ratio, the shape of real chat traffic. A fixed range answers its value
// without drawing.
func (drawer *lengthDrawer) logUniform(low, high int) int {
	if high <= low {
		return low
	}
	logLow, logHigh := math.Log(float64(low)), math.Log(float64(high))
	return int(math.Round(math.Exp(logLow + drawer.random.Float64()*(logHigh-logLow))))
}
