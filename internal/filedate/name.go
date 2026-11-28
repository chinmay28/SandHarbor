package filedate

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The forms of name a date is read from, most specific first. Every one is
// year, month, day in that order with a four-digit year: a name that leads
// with the day or the month ("14-05-2023") is two different dates depending on
// who wrote it, and guessing is worse than leaving the file alone.
//
// A run of digits only counts when it is the whole run, so the 2023 inside
// IMG_120230514 is not a year.
var (
	// 2023-05-14 10.15.02, 2023-05-14T10:15:02, "2023-05-14 at 2.15.02 PM":
	// screenshots, camera uploads, anything written by a person.
	spelledTime = regexp.MustCompile(
		`(?:^|\D)(\d{4})[-._](\d{2})[-._](\d{2})(?:[ _T-]| at )(\d{1,2})[.:\-h](\d{2})[.:\-m](\d{2})(?:\s?([AaPp][Mm]))?(?:\D|$)`)

	// 20230514_101502, 20230514-101502, 20230514101502, with or without
	// milliseconds after: what phones call their pictures.
	compactTime = regexp.MustCompile(
		`(?:^|\D)(\d{4})(\d{2})(\d{2})[-_T ]?(\d{2})(\d{2})(\d{2})(?:\d{3})?(?:\D|$)`)

	// 2023-05-14-101502: the day spelled out and the time not, as Signal
	// names what it saves.
	mixedTime = regexp.MustCompile(
		`(?:^|\D)(\d{4})[-._](\d{2})[-._](\d{2})[-_T ](\d{2})(\d{2})(\d{2})(?:\D|$)`)

	// 2023-05-14, 2023_05_14, 2023.05.14: a day and no more.
	spelledDay = regexp.MustCompile(`(?:^|\D)(\d{4})[-._](\d{2})[-._](\d{2})(?:\D|$)`)

	// 20230514: a day, as in IMG-20230514-WA0007.jpg.
	compactDay = regexp.MustCompile(`(?:^|\D)(\d{4})(\d{2})(\d{2})(?:\D|$)`)
)

// FromName reads the date written in a file's name.
//
// A name states no zone, so its wall clock is read in loc, the viewer's. The
// one exception is a Pixel's PXL_20230514_101502123.jpg, which is written in
// UTC whatever zone the phone was in.
//
// It reports false for a name with no date in it, and for digits that have the
// shape of one but are not: a thirteenth month, a year before 1980.
func FromName(name string, loc *time.Location) (Stamp, bool) {
	if strings.HasPrefix(strings.ToUpper(name), "PXL_") {
		loc = time.UTC
	}

	for _, form := range []*regexp.Regexp{spelledTime, compactTime, mixedTime} {
		m := form.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		hour := atoi(m[4])
		if len(m) > 7 && m[7] != "" {
			var ok bool
			if hour, ok = twelveHour(hour, m[7]); !ok {
				continue
			}
		}
		at, ok := civil(atoi(m[1]), atoi(m[2]), atoi(m[3]), hour, atoi(m[5]), atoi(m[6]), loc)
		if ok && !at.Before(earliest) {
			return Stamp{At: at}, true
		}
	}

	for _, form := range []*regexp.Regexp{spelledDay, compactDay} {
		m := form.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		at, ok := civil(atoi(m[1]), atoi(m[2]), atoi(m[3]), 12, 0, 0, loc)
		if ok && !at.Before(earliest) {
			return Stamp{At: at, DayOnly: true}, true
		}
	}
	return Stamp{}, false
}

// twelveHour turns an hour written with AM or PM into one of twenty-four.
func twelveHour(hour int, half string) (int, bool) {
	if hour < 1 || hour > 12 {
		return 0, false
	}
	hour %= 12
	if strings.EqualFold(half, "pm") {
		hour += 12
	}
	return hour, true
}

// atoi reads digits a pattern has already matched as digits.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
