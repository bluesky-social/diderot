package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Duration is a length of time in a settings document, stored in Go's form
// ("1m30s") as a connector's duration field is.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// errDuration is a duration field's value that ParseDuration can't read.
var errDuration = errors.New("must be a length of time such as 5 minutes or 1 hour")

// durationUnits are the unit words ParseDuration reads, singular, plural and
// short.
var durationUnits = map[string]time.Duration{
	"s": time.Second, "sec": time.Second, "secs": time.Second, "second": time.Second, "seconds": time.Second,
	"m": time.Minute, "min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hrs": time.Hour, "hour": time.Hour, "hours": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
}

// durationPart is one amount and its unit, with what may join it to the next.
var durationPart = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([a-z]+)(?:\s*,)?(?:\s+and)?\s*`)

// ParseDuration reads a length of time as people type it: "5 minutes",
// "1 hour 30 min", "2h", "1.5 hours", "1 day", or Go's form ("1h30m0s"),
// which is how a duration field is stored. A bare number or a negative
// length is refused.
func ParseDuration(text string) (time.Duration, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" || text == "0" {
		return 0, errDuration
	}
	if d, err := time.ParseDuration(text); err == nil && d >= 0 {
		return d, nil
	}
	var total time.Duration
	rest := text
	for rest != "" {
		match := durationPart.FindStringSubmatch(rest)
		if match == nil {
			return 0, errDuration
		}
		unit, ok := durationUnits[match[2]]
		if !ok {
			return 0, errDuration
		}
		amount, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			return 0, errDuration
		}
		total += time.Duration(amount * float64(unit))
		rest = rest[len(match[0]):]
	}
	return total, nil
}

// FormatDuration says a length of time in words, every unit from days to
// seconds that it has: "5 minutes", "1 hour 30 minutes", "2 days". A length
// with a fraction of a second keeps Go's form. ParseDuration reads it back.
func FormatDuration(d time.Duration) string {
	if d%time.Second != 0 || d < 0 {
		return d.String()
	}
	if d == 0 {
		return "0 seconds"
	}
	var parts []string
	for _, unit := range []struct {
		size time.Duration
		name string
	}{{24 * time.Hour, "day"}, {time.Hour, "hour"}, {time.Minute, "minute"}, {time.Second, "second"}} {
		n := d / unit.size
		d -= n * unit.size
		switch {
		case n == 1:
			parts = append(parts, "1 "+unit.name)
		case n > 1:
			parts = append(parts, fmt.Sprintf("%d %ss", n, unit.name))
		}
	}
	return strings.Join(parts, " ")
}
