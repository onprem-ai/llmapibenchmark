package ladder

import (
	"math"
	"sort"
)

// nearestRank returns the p-th percentile (0 < p <= 100) of values by the
// nearest-rank method, or nil for an empty slice.
func nearestRank(values []float64, p float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	rank = min(max(rank, 1), len(sorted))
	return ptr(sorted[rank-1])
}

func ptr[T any](value T) *T {
	return &value
}

// roundTo rounds a value to the given number of decimals; nil stays nil.
func roundTo(value *float64, decimals int) *float64 {
	if value == nil {
		return nil
	}
	scale := math.Pow(10, float64(decimals))
	return ptr(math.Round(*value*scale) / scale)
}
