package filedate

import (
	"testing"
	"time"
)

// Two hours east of UTC, which is where the names below are read unless they
// say otherwise.
var cest = time.FixedZone("", 2*60*60)

func TestFromNameReadsTheFormsPhonesAndPeopleWrite(t *testing.T) {
	at := func(y, mo, d, h, mi, s int) time.Time {
		return time.Date(y, time.Month(mo), d, h, mi, s, 0, cest).UTC()
	}
	for _, c := range []struct {
		name    string
		want    time.Time
		dayOnly bool
	}{
		// Phones.
		{"IMG_20230514_101502.jpg", at(2023, 5, 14, 10, 15, 2), false},
		{"VID_20230514_101502.mp4", at(2023, 5, 14, 10, 15, 2), false},
		{"20230514_101502.jpg", at(2023, 5, 14, 10, 15, 2), false},
		{"Screenshot_20230514-101502_Chrome.png", at(2023, 5, 14, 10, 15, 2), false},
		{"IMG_20230514_101502_123.jpg", at(2023, 5, 14, 10, 15, 2), false},
		{"20230514101502.jpg", at(2023, 5, 14, 10, 15, 2), false},
		{"signal-2023-05-14-101502.jpeg", at(2023, 5, 14, 10, 15, 2), false},
		// A Pixel writes UTC, whatever zone is asked for.
		{"PXL_20230514_101502123.jpg", time.Date(2023, 5, 14, 10, 15, 2, 0, time.UTC), false},
		{"pxl_20230514_101502123.MP.jpg", time.Date(2023, 5, 14, 10, 15, 2, 0, time.UTC), false},
		// People, and desktops.
		{"2023-05-14 10.15.02.jpg", at(2023, 5, 14, 10, 15, 2), false},
		{"Screenshot 2023-05-14 at 10.15.02.png", at(2023, 5, 14, 10, 15, 2), false},
		{"Screenshot 2023-05-14 at 2.15.02 PM.png", at(2023, 5, 14, 14, 15, 2), false},
		{"Screenshot 2023-05-14 at 12.15.02 AM.png", at(2023, 5, 14, 0, 15, 2), false},
		{"WhatsApp Image 2023-05-14 at 10.15.02.jpeg", at(2023, 5, 14, 10, 15, 2), false},
		{"scan 2023-05-14T10:15:02.pdf", at(2023, 5, 14, 10, 15, 2), false},
		// A day and no more: noon of it.
		{"IMG-20230514-WA0007.jpg", at(2023, 5, 14, 12, 0, 0), true},
		{"2023-05-14 statement.pdf", at(2023, 5, 14, 12, 0, 0), true},
		{"statement_2023_05_14.pdf", at(2023, 5, 14, 12, 0, 0), true},
		{"backup.2023.05.14.tar.gz", at(2023, 5, 14, 12, 0, 0), true},
		// A time that is not one still leaves the day.
		{"20230514_996102.jpg", at(2023, 5, 14, 12, 0, 0), true},
	} {
		got, ok := FromName(c.name, cest)
		if !ok {
			t.Errorf("%s: no date read, want %s", c.name, c.want)
			continue
		}
		if !got.At.Equal(c.want) || got.DayOnly != c.dayOnly {
			t.Errorf("%s: read %s (day only %v), want %s (day only %v)",
				c.name, got.At, got.DayOnly, c.want, c.dayOnly)
		}
	}
}

func TestFromNameLeavesAloneWhatIsNotADate(t *testing.T) {
	for _, name := range []string{
		"IMG_1234.jpg",
		"notes.txt",
		"holiday.mp4",
		"IMG_120230514.jpg",    // nine digits: the date is not the whole run
		"1684067696123.jpg",    // milliseconds since 1970, not year 1684
		"20231345.jpg",         // month thirteen
		"2023-06-31 notes.txt", // June has thirty days
		"2023-02-29.txt",       // and 2023 was not a leap year
		"19700101_000000.jpg",  // before anything digital dated itself
		"14-05-2023 notes.txt", // day first or month first? not guessed
		"report 2023-05.pdf",   // a month is not a day
		"Screenshot 2023-05-14 at 13.15.02 PM.png extra 99999999",
	} {
		if name == "Screenshot 2023-05-14 at 13.15.02 PM.png extra 99999999" {
			// Thirteen PM is not a time, but the day before it is still a day.
			got, ok := FromName(name, cest)
			if !ok || !got.DayOnly {
				t.Errorf("%s: read %+v, %v; want the day alone", name, got, ok)
			}
			continue
		}
		if got, ok := FromName(name, cest); ok {
			t.Errorf("%s: read %s, want no date", name, got.At)
		}
	}
}

func TestOffAllowsADayAndNoMore(t *testing.T) {
	named := time.Date(2023, 5, 14, 10, 15, 2, 0, time.UTC)
	timed := Stamp{At: named}
	day := Stamp{At: time.Date(2023, 5, 14, 12, 0, 0, 0, time.UTC), DayOnly: true}

	for _, c := range []struct {
		why    string
		stored time.Time
		stamp  Stamp
		off    bool
	}{
		{"the same second", named, timed, false},
		{"a recording that ended an hour later", named.Add(time.Hour), timed, false},
		{"read in a zone fourteen hours away", named.Add(-14 * time.Hour), timed, false},
		{"exactly a day out", named.Add(24 * time.Hour), timed, false},
		{"a day and a second out", named.Add(24*time.Hour + time.Second), timed, true},
		{"uploaded three years later", named.AddDate(3, 0, 0), timed, true},
		{"stored with no time at all", time.Time{}, timed, true},

		{"the first second of the named day", time.Date(2023, 5, 14, 0, 0, 0, 0, time.UTC), day, false},
		{"the day before", time.Date(2023, 5, 13, 0, 0, 0, 0, time.UTC), day, false},
		{"the day after, at its end", time.Date(2023, 5, 15, 23, 59, 59, 0, time.UTC), day, false},
		{"two days after", time.Date(2023, 5, 16, 0, 0, 1, 0, time.UTC), day, true},
		{"two days before", time.Date(2023, 5, 12, 23, 59, 59, 0, time.UTC), day, true},
	} {
		if got := Off(c.stored, c.stamp); got != c.off {
			t.Errorf("%s: off is %v, want %v", c.why, got, c.off)
		}
	}
}

func TestPlausibleIsBetween1980AndTomorrow(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		t    time.Time
		want bool
	}{
		{time.Date(1979, 12, 31, 23, 59, 59, 0, time.UTC), false},
		{time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{now, true},
		{now.Add(24 * time.Hour), true},
		{now.Add(24*time.Hour + time.Second), false},
	} {
		if got := Plausible(c.t, now); got != c.want {
			t.Errorf("%s: plausible is %v, want %v", c.t, got, c.want)
		}
	}
}
