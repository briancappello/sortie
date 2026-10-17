package config

import "testing"

// planTracker returns a tracker block with plan control fully and
// correctly configured. Tests mutate one key at a time.
func planTracker() map[string]any {
	return map[string]any{
		"kind":            "huly",
		"active_states":   []any{"Todo", "In Progress"},
		"terminal_states": []any{"Done", "Canceled"},
		"handoff_state":   "In Review",
		"release_state":   "Todo",
		"backlog_state":   "Backlog",
		"cancel_state":    "Canceled",
		"done_state":      "Done",
	}
}

func planRaw(tracker map[string]any, mergeCompletion bool) map[string]any {
	raw := map[string]any{"tracker": tracker}
	if mergeCompletion {
		raw["reactions"] = map[string]any{
			"merge_completion": map[string]any{"provider": "github", "target_state": "Done"},
		}
	}
	return raw
}

func TestPlanControl_On(t *testing.T) {
	t.Parallel()
	cfg, err := NewServiceConfig(planRaw(planTracker(), true))
	if err != nil {
		t.Fatalf("NewServiceConfig: %v", err)
	}
	if !cfg.Tracker.PlanControl() {
		t.Fatal("PlanControl() = false, want true")
	}
	st := cfg.Tracker.States()
	if st.Release != "Todo" || st.Backlog != "Backlog" || st.Cancel != "Canceled" || st.Done != "Done" {
		t.Errorf("States() = %+v, want the four plan states", st)
	}
}

func TestPlanControl_OffAcceptsEachStateAlone(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"backlog_state", "cancel_state", "done_state"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			cfg, err := NewServiceConfig(planRaw(map[string]any{"kind": "huly", key: "Anything"}, false))
			if err != nil {
				t.Fatalf("NewServiceConfig: %v", err)
			}
			if cfg.Tracker.PlanControl() {
				t.Error("PlanControl() = true without release_state")
			}
		})
	}
}

func TestPlanControl_Rejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		edit            func(m map[string]any)
		mergeCompletion bool
		wantField       string
	}{
		{"missing backlog", func(m map[string]any) { delete(m, "backlog_state") }, true, "tracker.backlog_state"},
		{"missing cancel", func(m map[string]any) { delete(m, "cancel_state") }, true, "tracker.cancel_state"},
		{"missing done", func(m map[string]any) { delete(m, "done_state") }, true, "tracker.done_state"},
		{"release not active", func(m map[string]any) { m["release_state"] = "Backlog" }, true, "tracker.release_state"},
		{"backlog active", func(m map[string]any) { m["backlog_state"] = "In Progress" }, true, "tracker.backlog_state"},
		{"backlog terminal", func(m map[string]any) { m["backlog_state"] = "Done" }, true, "tracker.backlog_state"},
		{"backlog is handoff", func(m map[string]any) { m["backlog_state"] = "in review" }, true, "tracker.backlog_state"},
		{"cancel not terminal", func(m map[string]any) { m["cancel_state"] = "Backlog" }, true, "tracker.cancel_state"},
		{"done not terminal", func(m map[string]any) { m["done_state"] = "Todo" }, true, "tracker.done_state"},
		{"active not written", func(m map[string]any) { delete(m, "active_states") }, true, "tracker.active_states"},
		{"terminal not written", func(m map[string]any) { delete(m, "terminal_states") }, true, "tracker.terminal_states"},
		{"no merge completion", func(map[string]any) {}, false, "reactions.merge_completion"},
		{"empty string", func(m map[string]any) { m["cancel_state"] = "" }, true, "tracker.cancel_state"},
		{"not a string", func(m map[string]any) { m["done_state"] = 3 }, true, "tracker.done_state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := planTracker()
			tt.edit(m)
			_, err := NewServiceConfig(planRaw(m, tt.mergeCompletion))
			assertConfigErrorField(t, err, tt.wantField)
		})
	}
}

func TestPlanControl_EnvRefResolvedToEmpty(t *testing.T) {
	m := planTracker()
	m["release_state"] = "$SORTIE_TEST_UNSET_RELEASE_STATE"
	t.Setenv("SORTIE_TEST_UNSET_RELEASE_STATE", "")
	_, err := NewServiceConfig(planRaw(m, true))
	assertConfigErrorField(t, err, "tracker.release_state")
}
