package crontab

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func Parse(expression string) (string, cron.Schedule, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return "", nil, fmt.Errorf("must contain exactly 5 fields")
	}
	normalized := strings.Join(fields, " ")
	schedule, err := parser.Parse(normalized)
	if err != nil {
		return "", nil, err
	}
	return normalized, schedule, nil
}

func Next(expression string, after time.Time) (time.Time, error) {
	_, schedule, err := Parse(expression)
	if err != nil {
		return time.Time{}, err
	}
	return NextSchedule(schedule, after)
}

func NextSchedule(schedule cron.Schedule, after time.Time) (time.Time, error) {
	next := schedule.Next(after.UTC()).UTC()
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("has no future occurrences")
	}
	return next, nil
}
