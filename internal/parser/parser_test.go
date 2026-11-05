package parser

import (
	"errors"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	// Thursday, 5 November 2026, 10:00 UTC.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		text string
		want time.Time
		err  error
	}{
		{name: "dot date and time", text: "06.11 18:00", want: time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)},
		{name: "dot date only", text: "6.11", want: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)},
		{name: "dot date full year", text: "06.11.2026 18:00", want: time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)},
		{name: "dot date past year", text: "06.11.2025 18:00", err: ErrPast},
		{name: "month date with time", text: "6 ноября в 18:00", want: time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)},
		{name: "month date default time", text: "6 ноября", want: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)},
		{name: "month date with year", text: "6 ноября 2027 в 18:00", want: time.Date(2027, 11, 6, 18, 0, 0, 0, time.UTC)},
		{name: "today with time", text: "сегодня 18:00", want: time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)},
		{name: "today default time already passed", text: "сегодня", err: ErrPast},
		{name: "today past time", text: "сегодня 9:00", err: ErrPast},
		{name: "tomorrow", text: "завтра 9:30", want: time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)},
		{name: "day after tomorrow", text: "послезавтра", want: time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)},
		{name: "minutes from now", text: "через 30 минут", want: time.Date(2026, 11, 5, 10, 30, 0, 0, time.UTC)},
		{name: "hours from now", text: "через 2 часа", want: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)},
		{name: "hours from now without number", text: "через час", want: time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC)},
		{name: "days from now", text: "через 3 дня", want: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)},
		{name: "zero duration", text: "через 0 минут", err: ErrParse},
		{name: "weeks are not supported", text: "через 2 недели", err: ErrParse},
		{name: "next friday", text: "в пятницу 18:00", want: time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)},
		{name: "today weekday still ahead", text: "в четверг 18:00", want: time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)},
		{name: "today weekday already passed", text: "в четверг 09:00", want: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)},
		{name: "today weekday default time passed", text: "в четверг", want: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)},
		{name: "time only later today", text: "18:00", want: time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)},
		{name: "time only already passed", text: "09:00", want: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)},
		{name: "time only right now", text: "10:00", want: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)},
		{name: "yesterday", text: "вчера в 18:00", err: ErrPast},
		{name: "garbage", text: "абракадабра", err: ErrParse},
		{name: "impossible date", text: "31.02 10:00", err: ErrParse},
		{name: "impossible clock", text: "06.11 25:00", err: ErrParse},
		{name: "date without time full year", text: "6 ноября 2026", want: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.text, now)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("Parse(%q) error = %v, want %v", tt.text, err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tt.text, err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("Parse(%q) = %s, want %s", tt.text, got, tt.want)
			}
			if got.Location() != now.Location() {
				t.Fatalf("Parse(%q) location = %s, want %s", tt.text, got.Location(), now.Location())
			}
		})
	}
}

func TestParseYearlessPastDateRollsToNextYear(t *testing.T) {
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		text string
		want time.Time
	}{
		{"06.11 18:00", time.Date(2027, 11, 6, 18, 0, 0, 0, time.UTC)},
		{"06.11", time.Date(2027, 11, 6, 9, 0, 0, 0, time.UTC)},
		{"6 ноября в 18:00", time.Date(2027, 11, 6, 18, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		got, err := Parse(tt.text, now)
		if err != nil {
			t.Fatalf("Parse(%q) unexpected error: %v", tt.text, err)
		}
		if !got.Equal(tt.want) {
			t.Fatalf("Parse(%q) = %s, want %s", tt.text, got, tt.want)
		}
	}
}

func TestParseTodayDefaultTimeBeforeNine(t *testing.T) {
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)

	got, err := Parse("сегодня", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestParseSameDayPastTimeIsRejected(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

	if got, err := Parse("06.11", now); !errors.Is(err, ErrPast) {
		t.Fatalf("Parse(06.11) = %s, %v; want ErrPast", got, err)
	}
}

func TestParseKeepsLocation(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skipf("timezone data unavailable: %v", err)
	}
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, loc)

	got, err := Parse("завтра в 18:00", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Location() != loc {
		t.Fatalf("location = %s, want %s", got.Location(), loc)
	}
	want := time.Date(2026, 11, 6, 18, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}
