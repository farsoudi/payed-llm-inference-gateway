package money

import "testing"

func TestParseUSDC(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"1", 1_000_000}, {"0.5", 500_000}, {"0.000001", 1}, {"12.340000", 12_340_000},
	}
	for _, tt := range tests {
		got, err := ParseUSDC(tt.input)
		if err != nil || got != tt.want {
			t.Errorf("ParseUSDC(%q) = %d, %v; want %d", tt.input, got, err, tt.want)
		}
	}
	for _, input := range []string{"", "-1", "1.0000001", "1.2.3", "abc"} {
		if _, err := ParseUSDC(input); err == nil {
			t.Errorf("ParseUSDC(%q) accepted invalid input", input)
		}
	}
}

func TestFormatUSDC(t *testing.T) {
	if got := FormatUSDC(1_234_567); got != "1.234567" {
		t.Fatalf("got %q", got)
	}
}
