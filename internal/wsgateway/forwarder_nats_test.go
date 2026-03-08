package wsgateway

import "testing"

func TestNormalizeEndpoint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strip canonical prefix",
			in:   "DysonNetwork.Messager",
			want: "messager",
		},
		{
			name: "strip lowercase prefix",
			in:   "dysonnetwork.Messager",
			want: "messager",
		},
		{
			name: "trim whitespace",
			in:   "  DysonNetwork.Messager.Events  ",
			want: "messager.events",
		},
		{
			name: "no prefix keeps endpoint",
			in:   "Messager",
			want: "messager",
		},
		{
			name: "empty remains empty",
			in:   "   ",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeEndpoint(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

