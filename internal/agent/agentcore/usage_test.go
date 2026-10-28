package agentcore

import (
	"maps"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestRunUsage_SnapshotZeroBeforeAnyCall(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	got := u.Snapshot()
	if got != (domain.TokenUsage{}) {
		t.Errorf("Snapshot() = %+v, want zero", got)
	}
}

func TestRunUsage_SetTurnProvisional_ReplacesInFlightContribution(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	u.SetTurnProvisional(domain.TokenUsage{InputTokens: 10, OutputTokens: 5})
	got := u.SetTurnProvisional(domain.TokenUsage{InputTokens: 12, OutputTokens: 6})

	if got.InputTokens != 12 || got.OutputTokens != 6 {
		t.Errorf("got %+v, want provisional replaced (12, 6)", got)
	}
	if got.TotalTokens != got.InputTokens+got.OutputTokens {
		t.Errorf("TotalTokens = %d, want InputTokens+OutputTokens", got.TotalTokens)
	}
}

func TestRunUsage_NeverLowersASnapshot(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	u.SetRunCumulative(domain.TokenUsage{InputTokens: 100, OutputTokens: 50}, nil)

	got := u.SetTurnProvisional(domain.TokenUsage{InputTokens: 1, OutputTokens: 1})

	if got.InputTokens < 100 || got.OutputTokens < 50 {
		t.Errorf("Snapshot decreased: got %+v, want componentwise >= (100, 50)", got)
	}
	if snap := u.Snapshot(); snap != got {
		t.Errorf("Snapshot() = %+v, want %+v", snap, got)
	}
}

func TestRunUsage_ClampsNegativeComponents_AllMethods(t *testing.T) {
	t.Parallel()

	negative := domain.TokenUsage{InputTokens: -5, OutputTokens: -10, CacheReadTokens: -3}

	tests := []struct {
		name string
		call func(u *RunUsage) domain.TokenUsage
	}{
		{"SetTurnProvisional", func(u *RunUsage) domain.TokenUsage { return u.SetTurnProvisional(negative) }},
		{"SetRunCumulative", func(u *RunUsage) domain.TokenUsage { return u.SetRunCumulative(negative, nil) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u := NewRunUsage()
			got := tt.call(u)
			if got.InputTokens != 0 || got.OutputTokens != 0 || got.CacheReadTokens != 0 {
				t.Errorf("%s(%+v) = %+v, want all components clamped to 0", tt.name, negative, got)
			}
			if got.TotalTokens != 0 {
				t.Errorf("%s(%+v).TotalTokens = %d, want 0", tt.name, negative, got.TotalTokens)
			}
		})
	}
}

func TestRunUsage_TotalAlwaysInputPlusOutput_AllMethods(t *testing.T) {
	t.Parallel()

	bogusTotal := domain.TokenUsage{InputTokens: 30, OutputTokens: 12, TotalTokens: 999}

	tests := []struct {
		name string
		call func(u *RunUsage) domain.TokenUsage
	}{
		{"SetTurnProvisional", func(u *RunUsage) domain.TokenUsage { return u.SetTurnProvisional(bogusTotal) }},
		{"SetRunCumulative", func(u *RunUsage) domain.TokenUsage { return u.SetRunCumulative(bogusTotal, nil) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.call(NewRunUsage())
			if got.TotalTokens != got.InputTokens+got.OutputTokens {
				t.Errorf("%s(%+v).TotalTokens = %d, want InputTokens+OutputTokens = %d",
					tt.name, bogusTotal, got.TotalTokens, got.InputTokens+got.OutputTokens)
			}
		})
	}
}

func TestRunUsage_SetTurnProvisional_DoesNotAccumulate(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	u.SetTurnProvisional(domain.TokenUsage{InputTokens: 10, OutputTokens: 5})
	u.SetTurnProvisional(domain.TokenUsage{InputTokens: 20, OutputTokens: 10})
	got := u.SetTurnProvisional(domain.TokenUsage{InputTokens: 30, OutputTokens: 15})

	want := domain.TokenUsage{InputTokens: 30, OutputTokens: 15, TotalTokens: 45}
	if got != want {
		t.Errorf("third SetTurnProvisional() = %+v, want %+v (replaced, not accumulated)", got, want)
	}
}

func TestRunUsage_SetRunCumulative_ReplacesSettledAndClearsProvisional(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	u.SetRunCumulative(domain.TokenUsage{InputTokens: 150, OutputTokens: 60}, nil)
	u.SetTurnProvisional(domain.TokenUsage{InputTokens: 9, OutputTokens: 9})

	got := u.SetRunCumulative(domain.TokenUsage{InputTokens: 200, OutputTokens: 80, CacheReadTokens: 5}, nil)
	want := domain.TokenUsage{InputTokens: 200, OutputTokens: 80, CacheReadTokens: 5, TotalTokens: 280}
	if got != want {
		t.Errorf("SetRunCumulative() = %+v, want %+v (replaces settled outright)", got, want)
	}

	next := u.SetTurnProvisional(domain.TokenUsage{})
	if next != want {
		t.Errorf("after SetRunCumulative, SetTurnProvisional(zero) = %+v, want %+v (provisional cleared)", next, want)
	}
}

func TestRunUsage_MonotonicSnapshot_AcrossMixedCallSequence(t *testing.T) {
	t.Parallel()

	u := NewRunUsage()
	var prev domain.TokenUsage

	checkStep := func(t *testing.T, label string, got domain.TokenUsage) {
		t.Helper()
		if got.InputTokens < prev.InputTokens || got.OutputTokens < prev.OutputTokens || got.CacheReadTokens < prev.CacheReadTokens {
			t.Errorf("%s: snapshot %+v is componentwise lower than the prior snapshot %+v", label, got, prev)
		}
		if snap := u.Snapshot(); got != snap {
			t.Errorf("%s: returned snapshot %+v != Snapshot() %+v", label, got, snap)
		}
		prev = got
	}

	checkStep(t, "SetRunCumulative(100,50)", u.SetRunCumulative(domain.TokenUsage{InputTokens: 100, OutputTokens: 50}, nil))
	checkStep(t, "SetTurnProvisional(1,1) lower", u.SetTurnProvisional(domain.TokenUsage{InputTokens: 1, OutputTokens: 1}))
	checkStep(t, "SetRunCumulative(50,200) mixed", u.SetRunCumulative(domain.TokenUsage{InputTokens: 50, OutputTokens: 200}, nil))
	checkStep(t, "SetTurnProvisional(300,5) mixed", u.SetTurnProvisional(domain.TokenUsage{InputTokens: 300, OutputTokens: 5}))
	checkStep(t, "SetRunCumulative(0,0) lower", u.SetRunCumulative(domain.TokenUsage{}, nil))

	want := domain.TokenUsage{InputTokens: 350, OutputTokens: 205}
	want.TotalTokens = want.InputTokens + want.OutputTokens
	if final := u.Snapshot(); final != want {
		t.Errorf("final Snapshot() = %+v, want %+v (componentwise max across the sequence)", final, want)
	}
}

func TestResumeBaseline_Observe_CounterReset(t *testing.T) {
	t.Parallel()

	usage := func(in, out, read, write int64) domain.TokenUsage {
		return domain.TokenUsage{InputTokens: in, OutputTokens: out, TotalTokens: in + out, CacheReadTokens: read, CacheWriteTokens: write}
	}
	start, own := usage(100, 10, 1000, 500), usage(10, 2, 100, 20)
	b := NewResumeBaseline(&start, nil)

	b.Observe(usage(130, 25, 1400, 600), nil, nil)

	run, _, estimated := b.Observe(usage(20, 5, 300, 50), nil, &own)
	if want := usage(40, 17, 500, 120); run != want || !estimated {
		t.Errorf("Observe(reset reading, own) = %+v, %v, want %+v, true", run, estimated, want)
	}

	run, _, estimated = b.Observe(usage(25, 9, 350, 70), nil, nil)
	if want := usage(45, 21, 550, 140); run != want || estimated {
		t.Errorf("Observe(reading after the reset) = %+v, %v, want %+v, false", run, estimated, want)
	}
}

func TestResumeBaseline_Observe_RederivedBaselineExactness(t *testing.T) {
	t.Parallel()

	reading := domain.TokenUsage{InputTokens: 20, OutputTokens: 5, TotalTokens: 25, CacheReadTokens: 300}
	prior := domain.TokenUsage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140, CacheReadTokens: 900}

	tests := []struct {
		name          string
		resume        *domain.TokenUsage
		own           *domain.TokenUsage
		wantEstimated bool
	}{
		{name: "unknown baseline, own covers the reading", own: &reading, wantEstimated: false},
		{name: "unknown baseline, no own", wantEstimated: true},
		{name: "counter reset, own covers the reading", resume: &prior, own: &reading, wantEstimated: false},
		{name: "counter reset, no own", resume: &prior, wantEstimated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := NewResumeBaseline(tt.resume, nil)

			_, _, estimated := b.Observe(reading, nil, tt.own)

			if estimated != tt.wantEstimated {
				t.Errorf("Observe(reading, %v) estimated = %v, want %v", tt.own, estimated, tt.wantEstimated)
			}
		})
	}
}

func TestResumeBaseline_Observe_PerModelSeed(t *testing.T) {
	t.Parallel()

	usage := func(in, out int64) domain.TokenUsage {
		return domain.TokenUsage{InputTokens: in, OutputTokens: out, TotalTokens: in + out}
	}
	base := usage(10, 2)
	reading := map[string]domain.TokenUsage{"a": usage(15, 4), "b": usage(5, 1)}

	tests := []struct {
		name       string
		seed       map[string]domain.TokenUsage
		wantRun    map[string]domain.TokenUsage
		wantResume map[string]domain.TokenUsage
	}{
		{
			name:       "consistent seed is subtracted per model",
			seed:       map[string]domain.TokenUsage{"a": usage(8, 2), "b": usage(2, 0)},
			wantRun:    map[string]domain.TokenUsage{"a": usage(7, 2), "b": usage(3, 1)},
			wantResume: reading,
		},
		{
			name:       "seed that does not add up to the combined figure claims nothing",
			seed:       map[string]domain.TokenUsage{"a": usage(8, 2)},
			wantResume: reading,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := NewResumeBaseline(&base, tt.seed)

			_, run, _ := b.Observe(usage(20, 5), reading, nil)
			_, resume := b.Resume()

			if !maps.Equal(run, tt.wantRun) {
				t.Errorf("Observe(reading) by model = %+v, want %+v", run, tt.wantRun)
			}
			if !maps.Equal(resume, tt.wantResume) {
				t.Errorf("Resume() by model = %+v, want %+v", resume, tt.wantResume)
			}
		})
	}
}
