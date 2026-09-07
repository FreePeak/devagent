// Rate-limit backoff decisions, ported from the pinned vitest assertions:
// Retry-After honored exactly, exponential fallback identical to the TS
// formula min(30000, 1000*2^attempt) + floor(random()*500), evaluated on
// the recorded 429 payloads in testdata/.

package integrations

import (
	"encoding/json"
	"reflect"
	"testing"
)

func recordHeaders(t *testing.T, name string) map[string]string {
	t.Helper()
	var script scriptedResponse
	if err := json.Unmarshal(readFixture(t, name), &script); err != nil {
		t.Fatal(err)
	}
	return script.Headers
}

func TestBackoffDelayHonorsRetryAfterOnRecordedPayloads(t *testing.T) {
	// The three recorded 429 payloads pin the exact decisions the TS
	// clients make: Linear Retry-After: 2 -> 2000ms, Jira Retry-After: 3
	// -> 3000ms, GitHub Issues Retry-After: 5 -> 5000ms.
	cases := []struct {
		fixture string
		want    int
	}{
		{"linear-429-retry-after.json", 2000},
		{"jira-429-retry-after.json", 3000},
		{"github-issues-429-retry-after.json", 5000},
	}
	for _, tc := range cases {
		headers := recordHeaders(t, tc.fixture)
		if got := backoffDelay(0, headers["Retry-After"], func(int) int { return 499 }); got != tc.want {
			t.Errorf("%s: backoff = %d, want %d", tc.fixture, got, tc.want)
		}
	}
}

func TestBackoffDelayFallsBackToExponentialPlusJitter(t *testing.T) {
	// With no usable Retry-After (the recorded no-retry-after 429 payload
	// and the 403 secondary-limit payload), the decision is
	// min(30000, 1000*2^attempt) + rand(0..499) — pinned here with a
	// deterministic jitter source at both extremes.
	headers := recordHeaders(t, "linear-429-no-retry-after.json")
	secondary := recordHeaders(t, "github-issues-403-secondary.json")
	for _, h := range []map[string]string{headers, secondary} {
		if got := backoffDelay(0, h["Retry-After"], func(int) int { return 0 }); got != 1000 {
			t.Errorf("attempt 0 low = %d, want 1000", got)
		}
		if got := backoffDelay(0, h["Retry-After"], func(int) int { return 499 }); got != 1499 {
			t.Errorf("attempt 0 high = %d, want 1499", got)
		}
	}
	want := []int{1499, 2499, 4499, 8499, 16499, 30499}
	got := make([]int, 0, len(want))
	for attempt := 0; attempt < len(want); attempt++ {
		got = append(got, backoffDelay(attempt, headers["Retry-After"], func(int) int { return 499 }))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ladder = %#v, want %#v", got, want)
	}
}

func TestBackoffDelayNonNumericRetryAfterFallsBack(t *testing.T) {
	// TS Number() semantics: null/absent (""), "abc", and "0" are not
	// usable, so the exponential path applies; fractional seconds are
	// honored ("0.5" -> 500ms).
	if got := backoffDelay(0, "", func(int) int { return 0 }); got != 1000 {
		t.Errorf("empty = %d", got)
	}
	if got := backoffDelay(0, "abc", func(int) int { return 0 }); got != 1000 {
		t.Errorf("abc = %d", got)
	}
	if got := backoffDelay(0, "0", func(int) int { return 0 }); got != 1000 {
		t.Errorf("zero = %d", got)
	}
	if got := backoffDelay(0, "0.5", func(int) int { return 0 }); got != 500 {
		t.Errorf("0.5 = %d, want 500", got)
	}
}
