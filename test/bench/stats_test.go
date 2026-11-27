package main

import (
	"testing"
	"time"
)

func TestSummarizeReportsNearestRankPercentilesOfUnsortedSamples(t *testing.T) {
	samples := make([]time.Duration, 0, 10)
	for _, v := range []int{70, 10, 100, 30, 50, 20, 90, 40, 80, 60} {
		samples = append(samples, time.Duration(v)*time.Millisecond)
	}

	got := summarize(samples)

	want := Summary{Samples: 10, MinMS: 10, P50MS: 50, P90MS: 90, MaxMS: 100, MeanMS: 55}
	if got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
}

func TestSummarizeOfOneSampleReportsItEverywhere(t *testing.T) {
	got := summarize([]time.Duration{1500 * time.Microsecond})

	want := Summary{Samples: 1, MinMS: 1.5, P50MS: 1.5, P90MS: 1.5, MaxMS: 1.5, MeanMS: 1.5}
	if got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
}

func TestSummarizeOfNoSampleIsZero(t *testing.T) {
	if got := summarize(nil); got != (Summary{}) {
		t.Errorf("summary = %+v, want the zero value", got)
	}
}

func TestSummarizeLeavesTheSamplesInTheirOrder(t *testing.T) {
	samples := []time.Duration{3 * time.Millisecond, 1 * time.Millisecond, 2 * time.Millisecond}

	summarize(samples)

	if samples[0] != 3*time.Millisecond || samples[1] != 1*time.Millisecond {
		t.Errorf("samples were reordered: %v", samples)
	}
}
