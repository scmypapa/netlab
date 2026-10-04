package server

import (
	"net/url"
	"testing"
	"time"
)

func TestMetricRange(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	start, end, step, err := metricRange(url.Values{}, now)
	if err != nil || end != now || end.Sub(start) != 15*time.Minute || step != 10 {
		t.Fatalf("%v %v %d %v", start, end, step, err)
	}
	for _, input := range []url.Values{
		{"range": {"bad"}}, {"range": {"-1"}}, {"range": {"9999999999999"}},
		{"start": {"invalid"}}, {"end": {"2026-10-05T12:00:00Z"}}, {"start": {"2026-08-01T00:00:00Z"}}, {"step": {"0"}}, {"step": {"huge"}}, {"start": {"2026-10-03T00:00:00Z"}, "step": {"10"}},
	} {
		if _, _, _, err := metricRange(input, now); err == nil {
			t.Fatalf("accepted invalid query: %v", input)
		}
	}
}
