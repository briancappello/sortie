package server

import "testing"

func TestFormatCost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input float64
		want  string
	}{
		{"zero", 0.00, "$0.00"},
		{"small", 0.05, "$0.05"},
		{"under 10", 9.99, "$9.99"},
		{"under 1000", 999.99, "$999.99"},
		{"rounds up to 1000 from below", 999.999, "$1,000.00"},
		{"rounds up fractional near boundary", 1234.9999, "$1,235.00"},
		{"exact 1000", 1000.00, "$1,000.00"},
		{"mid four digits", 1234.56, "$1,234.56"},
		{"five digits with cents", 10000.50, "$10,000.50"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := FormatCost(tt.input)
			if got != tt.want {
				t.Errorf("FormatCost(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
