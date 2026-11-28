// Package filedate reads the date a file states about itself: in its name, or
// inside it.
//
// A stored file carries one time, its modified time, and that time is only as
// good as what brought the file in. A photograph uploaded before the vault kept
// times is dated the afternoon it was uploaded; one that has been through a
// chat app, a zip or a cloud's web page is dated the day it was downloaded.
// The date it was taken is usually still there all the same, twice over: in a
// name like IMG_20230514_101502.jpg, and in the EXIF block the camera wrote.
//
// This package is the reading of both and nothing else. It knows no vault, no
// index and no clock of its own; it is given a name or some bytes and the time
// zone to read a zoneless wall clock in, and answers with a Stamp or with "no
// date here".
//
//   - FromName reads a name. See name.go for the forms it knows.
//   - FromContent reads the date inside a picture or a video. See content.go.
//   - Off says whether a stored time disagrees with a Stamp by enough to be
//     called wrong, which is deliberately not the same as "differs".
package filedate

import "time"

// Stamp is a date a file states about itself.
type Stamp struct {
	// At is the instant, in UTC. For a Stamp that names only a day it is noon
	// of that day in the zone it was read in, so that reading it an hour or
	// two out, in either direction, is still the same day.
	At time.Time

	// DayOnly is set when only the day is known: "2023-05-14 statement.pdf"
	// names a day, IMG_20230514_101502.jpg names a second.
	DayOnly bool
}

// Slack is how far a stored time may be from the moment a file names before it
// is called wrong.
//
// A day, and not a second, for three reasons that all push the same way. The
// name of a recording is when it began and its modified time is when it ended.
// A name with no zone in it is read in the viewer's zone, which need not be the
// zone the camera was in. And one fixed offset is not a time zone: it is an
// hour out for half the year. None of those is a wrong timestamp, and all of
// them are well inside a day — while the fault this exists to find, a file
// stamped with the day it was uploaded, is out by months.
const Slack = 24 * time.Hour

// Off reports whether a stored modified time is wrong by the Stamp's account:
// further than Slack from the moment it names, or outside the named day and
// the days either side of it. A file stored with no time at all is always off.
func Off(stored time.Time, s Stamp) bool {
	if stored.IsZero() {
		return true
	}
	slack := Slack
	if s.DayOnly {
		// At is the day's noon, so half a day more covers the day itself.
		slack += 12 * time.Hour
	}
	gap := stored.Sub(s.At)
	if gap < 0 {
		gap = -gap
	}
	return gap > slack
}

// earliest is the oldest date taken at its word. Nothing with a digital
// timestamp is older, and digits that read as an earlier year are a serial
// number rather than a date.
var earliest = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// Plausible reports whether t can be the date of a file as of now: not before
// 1980, and not later than tomorrow. A date in the future is a due date, an
// expiry or a camera whose clock was never set, and none of those is when the
// file was made.
func Plausible(t, now time.Time) bool {
	return !t.Before(earliest) && !t.After(now.Add(Slack))
}

// civil builds the instant a wall clock names in loc, or reports false if the
// fields are not a real date and time: a thirteenth month, the 31st of June.
func civil(year, month, day, hour, min, sec int, loc *time.Location) (time.Time, bool) {
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || min > 59 || sec > 59 {
		return time.Time{}, false
	}
	t := time.Date(year, time.Month(month), day, hour, min, sec, 0, loc)
	// time.Date carries overflow into the next month rather than refusing it.
	if t.Day() != day || t.Month() != time.Month(month) {
		return time.Time{}, false
	}
	return t.UTC(), true
}
