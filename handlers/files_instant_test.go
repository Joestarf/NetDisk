package handlers

import "testing"

func TestIsValidSHA256Hex(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{name: "valid", in: "ae90b4fb31cb8a19ba7b92c94b55b45e7b7942b2ab46eaf6a72750b7d4668a2a", ok: true},
		{name: "too short", in: "abc", ok: false},
		{name: "upper letters not allowed", in: "AE90b4fb31cb8a19ba7b92c94b55b45e7b7942b2ab46eaf6a72750b7d4668a2a", ok: false},
		{name: "non hex", in: "ge90b4fb31cb8a19ba7b92c94b55b45e7b7942b2ab46eaf6a72750b7d4668a2a", ok: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isValidSHA256Hex(c.in); got != c.ok {
				t.Fatalf("isValidSHA256Hex(%q) = %v, want %v", c.in, got, c.ok)
			}
		})
	}
}
