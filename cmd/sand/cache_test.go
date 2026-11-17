package main

import (
	"testing"
	"time"
)

func TestKeepUntil(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local)

	if got, err := keepUntil("7d", "", now); err != nil || !got.Equal(now.AddDate(0, 0, 7)) {
		t.Errorf("--for 7d = %v, %v", got, err)
	}
	// A bare date keeps the whole of that day.
	want := time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local)
	if got, err := keepUntil("", "2026-12-31", now); err != nil || !got.Equal(want) {
		t.Errorf("--until 2026-12-31 = %v, %v; want %v", got, err, want)
	}
	if got, err := keepUntil("", "2026-12-01T08:00:00Z", now); err != nil || got.UTC().Hour() != 8 {
		t.Errorf("--until RFC 3339 = %v, %v", got, err)
	}
	for name, args := range map[string][2]string{
		"neither":  {"", ""},
		"both":     {"1d", "2026-12-31"},
		"bad for":  {"soon", ""},
		"bad date": {"", "31/12/2026"},
	} {
		if _, err := keepUntil(args[0], args[1], now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
