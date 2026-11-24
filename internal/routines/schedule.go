package routines

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	// Routines resolve IANA zones on hosts without a zoneinfo database (Windows, static images).
	_ "time/tzdata"
)

// Schedule is when a routine runs: once at an ISO timestamp with offset, or a
// five-field cron expression. Both name an IANA time zone.
type Schedule struct {
	Type       string `json:"type"`
	At         string `json:"at,omitempty"`
	Expression string `json:"expression,omitempty"`
	TimeZone   string `json:"timeZone"`
}

// maxFoldCandidates bounds the wall-clock runs checked for one after time, as
// the TypeScript service does.
const maxFoldCandidates = 1441

// Schedule types.
const (
	Once = "once"
	Cron = "cron"
)

var (
	timeZonePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(?:/[A-Za-z0-9_+-]+)*$`)
	offsetPattern   = regexp.MustCompile(`(?:Z|[+-]\d{2}:\d{2})$`)
)

// Validate checks a schedule and that it still has a run after now.
func (s Schedule) Validate(now time.Time) error {
	if s.Type != Once && s.Type != Cron {
		return &Error{BadRequest, "参数无效：schedule.type 须为 once 或 cron。"}
	}
	if !timeZonePattern.MatchString(s.TimeZone) {
		return &Error{BadRequest, "请使用IANA时区名称。"}
	}
	if s.Type == Once && !offsetPattern.MatchString(s.At) {
		return &Error{BadRequest, "执行时间须包含明确时区偏移。"}
	}
	if _, err := time.LoadLocation(s.TimeZone); err != nil {
		return &Error{BadRequest, "时区无效。请使用Asia/Shanghai等IANA时区。"}
	}
	next, err := s.Next(now)
	if err != nil {
		return err
	}
	if next == nil {
		return &Error{BadRequest, "计划没有未来执行时间。"}
	}
	return nil
}

// Next returns the first run strictly after the given time, or nil when none remains.
func (s Schedule) Next(after time.Time) (*time.Time, error) {
	if s.Type == Once {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04Z07:00"} {
			if at, err := time.Parse(layout, s.At); err == nil {
				if at.After(after) {
					return &at, nil
				}
				return nil, nil
			}
		}
		return nil, nil
	}
	fields := strings.Fields(s.Expression)
	if len(fields) != 5 {
		return nil, &Error{BadRequest, "Cron需要分钟、小时、日、月、星期五个字段。"}
	}
	pattern, err := parseCron(fields)
	if err != nil {
		return nil, &Error{BadRequest, "Cron表达式无效。"}
	}
	location, err := time.LoadLocation(s.TimeZone)
	if err != nil {
		return nil, &Error{BadRequest, "时区无效。请使用Asia/Shanghai等IANA时区。"}
	}
	// Like croner's nextRuns, walk successive wall-clock matches; a repeated
	// hour can map a later wall time to an instant before after.
	from := civil(after, location).Truncate(time.Minute).Add(time.Minute)
	for range maxFoldCandidates {
		wall, ok := pattern.nextWall(from)
		if !ok {
			return nil, nil
		}
		if next := instant(wall, location); next.After(after) {
			return &next, nil
		}
		from = wall.Add(time.Minute)
	}
	return nil, nil
}

// hostTimeZone names the service computer's zone for routine_list. Go exposes the
// local zone only as "Local", so the name comes from TZ or the /etc/localtime link.
// A host that gives neither (Windows) reports UTC, the zone of the listed time, so
// the name is always one a schedule accepts.
func hostTimeZone() string {
	if name := os.Getenv("TZ"); timeZonePattern.MatchString(name) {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, found := strings.Cut(target, "zoneinfo/"); found && timeZonePattern.MatchString(name) {
			if _, err := time.LoadLocation(name); err == nil {
				return name
			}
		}
	}
	return "UTC"
}

// HostTimeZone is the host's IANA zone name for prompt sections.
func HostTimeZone() string { return hostTimeZone() }

var timeLabelPattern = regexp.MustCompile(`^(\d{1,2}) (\d{1,2}) \* \* (\*|1-5|[0-6])$`)

var weekDayLabels = []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// TimeLabel renders a cron expression the way the TypeScript schedule-label
// does (routineTimeLabel): 每天/工作日/每周几 HH:MM, or the raw expression.
func TimeLabel(expression string) string {
	match := timeLabelPattern.FindStringSubmatch(expression)
	if match == nil {
		return expression
	}
	frequency := "每天"
	switch match[3] {
	case "*":
	case "1-5":
		frequency = "工作日"
	default:
		weekday, _ := strconv.Atoi(match[3])
		frequency = "每" + weekDayLabels[weekday]
	}
	minute, _ := strconv.Atoi(match[1])
	hour, _ := strconv.Atoi(match[2])
	return fmt.Sprintf("%s %02d:%02d", frequency, hour, minute)
}
