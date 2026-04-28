package version

import "testing"

func TestShortCommit(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"1234567890abcdef": "12345678",
		"12345678":         "12345678",
		"dev":              "dev",
		"":                 "",
	}

	for input, want := range tests {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if got := ShortCommit(input); got != want {
				t.Fatalf("ShortCommit(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestCurrentKeepsVersionAndShortensCommit(t *testing.T) {
	originalVersion := Version
	originalCommit := Commit
	t.Cleanup(func() {
		Version = originalVersion
		Commit = originalCommit
	})

	Version = "v1.2.3"
	Commit = "1234567890abcdef"

	info := Current()
	if info.Version != "v1.2.3" {
		t.Fatalf("Version = %q, want %q", info.Version, "v1.2.3")
	}
	if info.Commit != "12345678" {
		t.Fatalf("Commit = %q, want %q", info.Commit, "12345678")
	}
}
