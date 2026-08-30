package filing

import (
	"testing"
	"time"
)

func TestBackoffIsBoundedAndExhausts(t *testing.T) {
	backoff := Backoff{Initial: time.Second, Maximum: 5 * time.Second, MaxAttempts: 5}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second}
	for attempt, expected := range want {
		got, retry := backoff.Delay(attempt + 1)
		if !retry || got != expected {
			t.Fatalf("attempt %d = %v,%v want %v,true", attempt+1, got, retry, expected)
		}
	}
	if _, retry := backoff.Delay(5); retry {
		t.Fatal("exhausted attempt remained retryable")
	}
}
