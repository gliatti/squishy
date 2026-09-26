package httpapi

import "testing"

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"My Project", "my-project"},
		{"  Hello,  World!! ", "hello-world"},
		{"a--b", "a--b"},
		{"a - b", "a---b"},
		{"Ünïcode", "n-code"},
		{"---", ""},
		{"", ""},
		{"already-fine-1", "already-fine-1"},
		{"Employees 2024 (copy)", "employees-2024-copy"},
	}
	for _, tc := range cases {
		if got := slugify(tc.in); got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
