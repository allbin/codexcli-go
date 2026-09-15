package codexcli

import "testing"

// TestTurnStartEffortLayering pins how the effort on turn/start is chosen,
// which WithEffort documents. Codex keeps a turn/start effort for later
// turns, so these rules decide whether a per-turn override survives:
// a connect-time WithEffort is re-sent on every turn and replaces it, and
// WithEffort("") sends nothing at all rather than reverting.
func TestTurnStartEffortLayering(t *testing.T) {
	effortFor := func(connect []Option, call ...Option) *string {
		conn := resolveOptions(connect, nil)
		return resolveOptions(conn.callOpts(), call).buildTurnStartParams("t1", nil).Effort
	}
	str := func(p *string) string {
		if p == nil {
			return "<omitted>"
		}
		return *p
	}

	cases := []struct {
		name    string
		connect []Option
		call    []Option
		want    string
	}{
		{"no effort anywhere", nil, nil, "<omitted>"},
		{"per-call only", nil, []Option{WithEffort("high")}, "high"},
		{"connect-time re-sent on a plain turn", []Option{WithEffort("low")}, nil, "low"},
		{"per-call beats connect-time", []Option{WithEffort("low")}, []Option{WithEffort("high")}, "high"},
		{"empty per-call suppresses connect-time", []Option{WithEffort("low")}, []Option{WithEffort("")}, "<omitted>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := str(effortFor(tc.connect, tc.call...)); got != tc.want {
				t.Errorf("turn/start effort = %s, want %s", got, tc.want)
			}
		})
	}
}
