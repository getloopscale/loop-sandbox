package main

import (
	"math"
	"sort"
	"time"
)

// Summary describes the samples of one measurement, in milliseconds.
type Summary struct {
	Samples int     `json:"samples"`
	MinMS   float64 `json:"min_ms"`
	P50MS   float64 `json:"p50_ms"`
	P90MS   float64 `json:"p90_ms"`
	MaxMS   float64 `json:"max_ms"`
	MeanMS  float64 `json:"mean_ms"`
}

// summarize returns the summary of the samples. Percentiles use the nearest
// rank, so every reported value is one that was measured. With no sample the
// summary is zero.
func summarize(samples []time.Duration) Summary {
	if len(samples) == 0 {
		return Summary{}
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, s := range sorted {
		total += s
	}
	return Summary{
		Samples: len(sorted),
		MinMS:   ms(sorted[0]),
		P50MS:   ms(nearestRank(sorted, 50)),
		P90MS:   ms(nearestRank(sorted, 90)),
		MaxMS:   ms(sorted[len(sorted)-1]),
		MeanMS:  ms(total / time.Duration(len(sorted))),
	}
}

// nearestRank returns the percentile p (1 to 100) of sorted, which must be
// ascending and not empty.
func nearestRank(sorted []time.Duration, p int) time.Duration {
	rank := int(math.Ceil(float64(p) / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// ms converts to milliseconds with one decimal place.
func ms(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Millisecond)*10) / 10
}
