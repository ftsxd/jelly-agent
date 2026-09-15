package sandbox

import "testing"

func TestRedactInjectedReplacesValuesWithTheirNames(t *testing.T) {
	env := map[string]string{"API_TOKEN": "sk-abc123456", "CLUSTER": "prod-east"}
	out := redactInjected("token=sk-abc123456 cluster=prod-east ok", env, false)

	want := "token=${API_TOKEN} cluster=${CLUSTER} ok"
	if out != want {
		t.Fatalf("redactInjected = %q, want %q", out, want)
	}
}

// A value that contains another must be replaced first, or the short one eats
// its middle and leaves an unmatched — and still secret — fragment behind.
func TestRedactInjectedHandlesOverlappingValues(t *testing.T) {
	env := map[string]string{"SHORT": "sk-abc1", "LONG": "sk-abc123456789"}
	out := redactInjected("full=sk-abc123456789", env, false)

	if out != "full=${LONG}" {
		t.Fatalf("redactInjected = %q, want the longest value replaced whole", out)
	}
}

// Short values are left alone on purpose: they cannot be credentials, and
// masking them would rewrite most legitimate output.
func TestRedactInjectedIgnoresTooShortValues(t *testing.T) {
	in := "retries=3 ok=up"
	if out := redactInjected(in, map[string]string{"RETRIES": "3", "STATE": "up"}, false); out != in {
		t.Fatalf("redactInjected = %q, want it untouched", out)
	}
}

// The output cap can land in the middle of a value. The surviving prefix is
// still part of the secret, so a truncated run has to lose its tail.
func TestRedactInjectedCutsAPartialValueAtTheTruncationBoundary(t *testing.T) {
	env := map[string]string{"API_TOKEN": "sk-abc123456789"}

	out := redactInjected("noise token=sk-abc1234", env, true)
	if out != "noise token=${API_TOKEN}" {
		t.Fatalf("redactInjected = %q, want the partial value cut", out)
	}
	// Untruncated output ending in the same bytes is a complete, intentional
	// print of something that merely looks like a prefix — leave it be.
	if out := redactInjected("noise token=sk-abc1234", env, false); out != "noise token=sk-abc1234" {
		t.Fatalf("redactInjected = %q, want it untouched when not truncated", out)
	}
}

func TestRedactInjectedNoopsWithoutEnvOrOutput(t *testing.T) {
	if out := redactInjected("plain", nil, false); out != "plain" {
		t.Fatalf("redactInjected = %q", out)
	}
	if out := redactInjected("", map[string]string{"A": "secret-value"}, false); out != "" {
		t.Fatalf("redactInjected = %q", out)
	}
}
