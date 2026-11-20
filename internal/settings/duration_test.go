package settings

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDurationReadsWhatPeopleType(t *testing.T) {
	for text, want := range map[string]time.Duration{
		"5 minutes":                  5 * time.Minute,
		"5m0s":                       5 * time.Minute,
		"90s":                        90 * time.Second,
		"1 hour 30 minutes":          90 * time.Minute,
		"1h 30m":                     90 * time.Minute,
		"1 Hour, 30 Min":             90 * time.Minute,
		"2 hours and 15 minutes":     2*time.Hour + 15*time.Minute,
		"1.5 hours":                  90 * time.Minute,
		"1 day":                      24 * time.Hour,
		"2d":                         48 * time.Hour,
		"  10 sec ":                  10 * time.Second,
		"1 day 2 hours 3 minutes 4s": 26*time.Hour + 3*time.Minute + 4*time.Second,
	} {
		got, err := ParseDuration(text)
		require.NoError(t, err, text)
		assert.Equal(t, want, got, text)
	}
	for _, text := range []string{"", "0", "5", "five minutes", "5 fortnights", "-5m", "5 minutes ago"} {
		_, err := ParseDuration(text)
		assert.ErrorIs(t, err, errDuration, text)
	}
}

func TestFormatDurationRoundTrips(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                            "0 seconds",
		time.Second:                  "1 second",
		5 * time.Minute:              "5 minutes",
		time.Hour:                    "1 hour",
		90 * time.Minute:             "1 hour 30 minutes",
		26*time.Hour + 4*time.Second: "1 day 2 hours 4 seconds",
		1500 * time.Millisecond:      "1.5s",
	} {
		assert.Equal(t, want, FormatDuration(d))
		back, err := ParseDuration(FormatDuration(d))
		require.NoError(t, err)
		assert.Equal(t, d, back, "%s reads back", want)
	}
}

func TestDurationFieldAcceptsWordsAndStoresGoForm(t *testing.T) {
	field := Field{Key: "every", Kind: KindDuration}
	value, err := parseField(field, "1 hour 30 minutes")
	require.NoError(t, err)
	assert.Equal(t, "1h30m0s", value)
	_, err = parseField(field, "soon")
	assert.EqualError(t, err, "must be a length of time such as 5 minutes or 1 hour")
}
