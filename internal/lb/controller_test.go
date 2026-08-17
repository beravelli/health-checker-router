package lb

import "testing"

func TestWeightsFromScoresSumTo100(t *testing.T) {
	cases := []map[string]float64{
		{"us-east-1": 1.0, "us-west-2": 1.0},
		{"us-east-1": 1.0, "us-west-2": 0.5},
		{"us-east-1": 0.97, "us-west-2": 0.62},
		{"us-east-1": 1.0, "us-west-2": 0.0},
		{"us-east-1": 0.0, "us-west-2": 0.0},
		{"us-east-1": 0.4, "us-west-2": 0.4, "eu-west-1": 0.4},
	}
	for _, scores := range cases {
		w := WeightsFromScores(scores)
		if err := w.Validate(); err != nil {
			t.Errorf("scores %v produced invalid weights %v: %v", scores, w, err)
		}
	}
}

func TestHealthierRegionGetsMoreTraffic(t *testing.T) {
	w := WeightsFromScores(map[string]float64{"us-east-1": 1.0, "us-west-2": 0.5})
	if w["us-east-1"] <= w["us-west-2"] {
		t.Fatalf("healthy region should carry more traffic, got %v", w)
	}
}

func TestDegradedRegionIsNotEvicted(t *testing.T) {
	// A degraded-but-serving region keeps a floor of traffic. Dropping it to
	// zero is a failover decision, not a reweighting one.
	w := WeightsFromScores(map[string]float64{"us-east-1": 1.0, "us-west-2": 0.2})
	if w["us-west-2"] < minRegionWeight {
		t.Fatalf("degraded region weight %d fell below the floor of %d", w["us-west-2"], minRegionWeight)
	}
}

func TestCriticalRegionIsEvicted(t *testing.T) {
	w := WeightsFromScores(map[string]float64{"us-east-1": 1.0, "us-west-2": 0.0})
	if w["us-west-2"] != 0 {
		t.Fatalf("critical region should receive no traffic, got %d", w["us-west-2"])
	}
	if w["us-east-1"] != 100 {
		t.Fatalf("healthy region should absorb all traffic, got %d", w["us-east-1"])
	}
}

func TestAllCriticalHoldsEvenSplit(t *testing.T) {
	// Every region critical is the one case where zeroing everything would be
	// actively harmful — there would be nowhere to send traffic at all.
	w := WeightsFromScores(map[string]float64{"us-east-1": 0.0, "us-west-2": 0.0})
	if err := w.Validate(); err != nil {
		t.Fatalf("all-critical split is invalid: %v", err)
	}
	if w["us-east-1"] == 0 && w["us-west-2"] == 0 {
		t.Fatal("all-critical must not send traffic nowhere")
	}
}
