// Package parser turns a short Russian date/time phrase into a concrete time.
//
// Supported forms:
//
//	06.11 18:00, 06.11.2026 18:00, 06.11
//	6 ноября [2026] [в 18:00]
//	сегодня | завтра | послезавтра [в 18:00]
//	через 30 минут | через 2 часа | через 3 дня
//	в пятницу [в 18:00]
//	18:00
//
// A date without a time gets 09:00. A yearless date that already passed this
// year rolls to the next year; an explicitly past date returns ErrPast.
package parser

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ErrPast  = errors.New("time is in the past")
	ErrParse = errors.New("cannot parse time")
)

const (
	defaultHour   = 9
	defaultMinute = 0
)

var (
	dotRe      = regexp.MustCompile(`^(\d{1,2})\.(\d{1,2})(?:\.(\d{4}))?(?:\s+(\d{1,2}):(\d{2}))?$`)
	monthRe    = regexp.MustCompile(`^(\d{1,2})\s+(января|февраля|марта|апреля|мая|июня|июля|августа|сентября|октября|ноября|декабря)(?:\s+(\d{4}))?(?:\s+(\d{1,2}):(\d{2}))?$`)
	relDayRe   = regexp.MustCompile(`^(сегодня|завтра|послезавтра|вчера)(?:\s+(\d{1,2}):(\d{2}))?$`)
	durationRe = regexp.MustCompile(`^через\s+(?:(\d+)\s*)?(минуту|минуты|минут|час|часа|часов|день|дня|дней)$`)
	weekdayRe  = regexp.MustCompile(`^(?:в\s+)?(понедельник|вторник|среду|четверг|пятницу|субботу|воскресенье)(?:\s+(\d{1,2}):(\d{2}))?$`)
	timeRe     = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
)

var months = map[string]time.Month{
	"января":   time.January,
	"февраля":  time.February,
	"марта":    time.March,
	"апреля":   time.April,
	"мая":      time.May,
	"июня":     time.June,
	"июля":     time.July,
	"августа":  time.August,
	"сентября": time.September,
	"октября":  time.October,
	"ноября":   time.November,
	"декабря":  time.December,
}

var weekdays = map[string]time.Weekday{
	"воскресенье": time.Sunday,
	"понедельник": time.Monday,
	"вторник":     time.Tuesday,
	"среду":       time.Wednesday,
	"четверг":     time.Thursday,
	"пятницу":     time.Friday,
	"субботу":     time.Saturday,
}

// Parse returns the due time in now's location.
func Parse(text string, now time.Time) (time.Time, error) {
	s := normalize(text)

	if m := dotRe.FindStringSubmatch(s); m != nil {
		return parseDot(m, now)
	}
	if m := monthRe.FindStringSubmatch(s); m != nil {
		return parseMonth(m, now)
	}
	if m := relDayRe.FindStringSubmatch(s); m != nil {
		return parseRelativeDay(m, now)
	}
	if m := durationRe.FindStringSubmatch(s); m != nil {
		return parseDuration(m, now)
	}
	if m := weekdayRe.FindStringSubmatch(s); m != nil {
		return parseWeekday(m, now)
	}
	if m := timeRe.FindStringSubmatch(s); m != nil {
		return parseClock(m, now)
	}

	return time.Time{}, fmt.Errorf("%w: %q", ErrParse, text)
}

// normalize lowercases the phrase and drops the preposition "в" before times,
// so "завтра в 18:00" and "завтра 18:00" share one regex.
func normalize(text string) string {
	s := strings.ToLower(strings.TrimSpace(text))
	s = strings.ReplaceAll(s, "ё", "е")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, " в ", " ")
	return strings.TrimPrefix(s, "в ")
}

func parseDot(m []string, now time.Time) (time.Time, error) {
	day := atoi(m[1])
	mon := time.Month(atoi(m[2]))
	year := 0
	if m[3] != "" {
		year = atoi(m[3])
	}
	hour, minute := defaultHour, defaultMinute
	if m[4] != "" {
		hour, minute = atoi(m[4]), atoi(m[5])
	}
	if !validClock(hour, minute) {
		return time.Time{}, fmt.Errorf("%w: invalid clock", ErrParse)
	}
	return resolve(year, mon, day, hour, minute, now)
}

func parseMonth(m []string, now time.Time) (time.Time, error) {
	day := atoi(m[1])
	mon, ok := months[m[2]]
	if !ok {
		return time.Time{}, fmt.Errorf("%w: unknown month", ErrParse)
	}
	year := 0
	if m[3] != "" {
		year = atoi(m[3])
	}
	hour, minute := defaultHour, defaultMinute
	if m[4] != "" {
		hour, minute = atoi(m[4]), atoi(m[5])
	}
	if !validClock(hour, minute) {
		return time.Time{}, fmt.Errorf("%w: invalid clock", ErrParse)
	}
	return resolve(year, mon, day, hour, minute, now)
}

func parseRelativeDay(m []string, now time.Time) (time.Time, error) {
	if m[1] == "вчера" {
		return time.Time{}, ErrPast
	}
	hour, minute := defaultHour, defaultMinute
	if m[2] != "" {
		hour, minute = atoi(m[2]), atoi(m[3])
	}
	if !validClock(hour, minute) {
		return time.Time{}, fmt.Errorf("%w: invalid clock", ErrParse)
	}

	offset := 0
	switch m[1] {
	case "завтра":
		offset = 1
	case "послезавтра":
		offset = 2
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location()).
		AddDate(0, 0, offset)
	if t.Before(now) {
		return time.Time{}, ErrPast
	}
	return t, nil
}

func parseDuration(m []string, now time.Time) (time.Time, error) {
	n := 1
	if m[1] != "" {
		n = atoi(m[1])
	}
	if n <= 0 {
		return time.Time{}, fmt.Errorf("%w: non-positive duration", ErrParse)
	}

	var d time.Duration
	switch m[2] {
	case "минуту", "минуты", "минут":
		d = time.Duration(n) * time.Minute
	case "час", "часа", "часов":
		d = time.Duration(n) * time.Hour
	case "день", "дня", "дней":
		d = time.Duration(n) * 24 * time.Hour
	}
	return now.Add(d), nil
}

func parseWeekday(m []string, now time.Time) (time.Time, error) {
	target, ok := weekdays[m[1]]
	if !ok {
		return time.Time{}, fmt.Errorf("%w: unknown weekday", ErrParse)
	}
	hour, minute := defaultHour, defaultMinute
	if m[2] != "" {
		hour, minute = atoi(m[2]), atoi(m[3])
	}
	if !validClock(hour, minute) {
		return time.Time{}, fmt.Errorf("%w: invalid clock", ErrParse)
	}

	delta := (int(target) - int(now.Weekday()) + 7) % 7
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location()).
		AddDate(0, 0, delta)
	if !t.After(now) {
		t = t.AddDate(0, 0, 7)
	}
	return t, nil
}

func parseClock(m []string, now time.Time) (time.Time, error) {
	hour, minute := atoi(m[1]), atoi(m[2])
	if !validClock(hour, minute) {
		return time.Time{}, fmt.Errorf("%w: invalid clock", ErrParse)
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

// resolve applies the yearless-date rules: a date earlier than today rolls to
// next year, while a date that is today or later stays in the current year.
func resolve(year int, mon time.Month, day, hour, minute int, now time.Time) (time.Time, error) {
	loc := now.Location()

	if year != 0 {
		t, ok := makeDate(year, mon, day, hour, minute, loc)
		if !ok {
			return time.Time{}, fmt.Errorf("%w: impossible date", ErrParse)
		}
		if t.Before(now) {
			return time.Time{}, ErrPast
		}
		return t, nil
	}

	t, ok := makeDate(now.Year(), mon, day, hour, minute, loc)
	if !ok {
		return time.Time{}, fmt.Errorf("%w: impossible date", ErrParse)
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	target := time.Date(now.Year(), mon, day, 0, 0, 0, 0, loc)
	if target.Before(today) {
		t, ok = makeDate(now.Year()+1, mon, day, hour, minute, loc)
		if !ok {
			return time.Time{}, fmt.Errorf("%w: impossible date", ErrParse)
		}
	}
	if t.Before(now) {
		return time.Time{}, ErrPast
	}
	return t, nil
}

func makeDate(year int, mon time.Month, day, hour, minute int, loc *time.Location) (time.Time, bool) {
	if day < 1 || day > 31 {
		return time.Time{}, false
	}
	t := time.Date(year, mon, day, hour, minute, 0, 0, loc)
	if t.Day() != day || t.Month() != mon || t.Year() != year {
		return time.Time{}, false
	}
	return t, true
}

func validClock(hour, minute int) bool {
	return hour >= 0 && hour <= 23 && minute >= 0 && minute <= 59
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
