package release

import "testing"

func TestIsHomebrew(t *testing.T) {
	for exe, want := range map[string]bool{
		"/opt/homebrew/Cellar/tc/0.2.0/bin/tc":              true,
		"/usr/local/Cellar/tc/0.2.0/bin/tc":                 true,
		"/home/linuxbrew/.linuxbrew/Cellar/tc/0.2.0/bin/tc": true,
		"/opt/homebrew/bin/tc":                              false,
		"/home/me/.local/bin/tc":                            false,
		"/home/me/Cellar-stuff/tc":                          false,
	} {
		if got := IsHomebrew(exe); got != want {
			t.Errorf("IsHomebrew(%q) = %v, want %v", exe, got, want)
		}
	}
}
