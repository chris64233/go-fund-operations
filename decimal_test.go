package fundoperations

import "testing"

func TestParseDecimalAndString(t *testing.T) {
	cases := map[string]string{
		"1.2":    "1.2000",
		"1.2000": "1.2000",
		"0.0001": "0.0001",
		"-.5":    "-0.5000",
		"100":    "100.0000",
		"+2.0":   "2.0000",
	}
	for in, want := range cases {
		d, err := ParseDecimal(in, navScale)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if got := d.String(); got != want {
			t.Fatalf("parse %q = %s, want %s", in, got, want)
		}
	}
	if _, err := ParseDecimal("1.00001", navScale); err == nil {
		t.Fatal("expected scale overflow error")
	}
	if _, err := ParseDecimal("abc", navScale); err == nil {
		t.Fatal("expected invalid error")
	}
}

func TestDecimalRoundingHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0.025", "0.03"}, // 半数进位
		{"-0.025", "-0.03"},
		{"0.005", "0.01"},
		{"-0.005", "-0.01"},
		{"0.024", "0.02"},
		{"-0.024", "-0.02"},
		{"2.5", "2.50"},
	}
	for _, c := range cases {
		d := mustParseDecimal(c.in, 3)
		if got := d.Round(amountScale).String(); got != c.want {
			t.Fatalf("round %s = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestDecimalMulAndEqualityAcrossScale(t *testing.T) {
	shares := mustParseDecimal("100", shareScale)
	nav := mustParseDecimal("1.0005", navScale)
	if got := shares.Mul(nav).Round(amountScale).String(); got != "100.05" {
		t.Fatalf("100*1.0005 = %s, want 100.05", got)
	}
	a := mustParseDecimal("1.2", 4)
	b := mustParseDecimal("1.20", 2)
	if !a.Equal(b) {
		t.Fatal("1.2000 should equal 1.20 numerically")
	}
}
