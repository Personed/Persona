package routines

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// cronPattern is a five-field expression parsed the way croner 10 parses it in
// "5-part" mode, which the TypeScript service used for routines: seconds fixed
// at 0, `L`, `LW`, `W`, `#`, `?`, `+`, month and weekday names, and 0 or 7 as
// Sunday. Day of month and day of week combine with OR unless either is `*` or
// the weekday field starts with `+`.
type cronPattern struct {
	minute       [60]bool
	hour         [24]bool
	day          [31]bool
	nearest      [31]bool
	month        [12]bool
	weekday      [7]uint8
	lastDay      bool
	lastWeekday  bool
	starDay      bool
	starWeekday  bool
	weekdayAndOp bool
}

// Weekday flags: one bit per occurrence in the month, lastOccurrence for `#L`
// and `L`, and anyOccurrence for a plain weekday.
var occurrence = [5]uint8{1, 2, 4, 8, 16}

const (
	lastOccurrence = 32
	anyOccurrence  = 63
	// croner stops searching at year 3000 for patterns without a year field.
	cronYearLimit = 3000
)

var errCronPattern = errors.New("invalid cron pattern")

var (
	monthNames = [...]string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	dayNames   = [...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
)

func parseCron(fields []string) (*cronPattern, error) {
	if len(fields) != 5 {
		return nil, errCronPattern
	}
	e := append([]string(nil), fields...)
	p := &cronPattern{}
	if strings.ToUpper(e[2]) == "LW" {
		p.lastWeekday, e[2] = true, ""
	} else if strings.Contains(strings.ToUpper(e[2]), "L") {
		e[2] = strings.NewReplacer("L", "", "l", "").Replace(e[2])
		p.lastDay = true
	}
	p.starDay = e[2] == "*"
	if len(e[3]) >= 3 {
		e[3] = replaceNames(e[3], monthNames[:], 1, false)
	}
	if len(e[4]) >= 3 {
		e[4] = replaceNames(e[4], dayNames[:], 0, true)
	}
	if rest, ok := strings.CutPrefix(e[4], "+"); ok {
		p.weekdayAndOp, e[4] = true, rest
		if rest == "" {
			return nil, errCronPattern
		}
	}
	p.starWeekday = e[4] == "*"
	for i := range e {
		e[i] = strings.ReplaceAll(e[i], "?", "*")
	}
	for i, field := range e {
		allowed := "/*0123456789,-"
		switch i {
		case 2:
			allowed += "WwLl"
		case 4:
			allowed += "#Ll"
		}
		if strings.IndexFunc(field, func(r rune) bool { return !strings.ContainsRune(allowed, r) }) >= 0 {
			return nil, errCronPattern
		}
	}
	parts := []struct {
		name   string
		value  string
		offset int
	}{{"minute", e[0], 0}, {"hour", e[1], 0}, {"day", e[2], -1}, {"month", e[3], -1}, {"weekday", e[4], 0}}
	for _, part := range parts {
		if err := p.part(part.name, part.value, part.offset); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// replaceNames substitutes names case-insensitively wherever they appear, as
// croner's String.replace chain does. A weekday range ending in "-sun" means 7.
// Lowercasing the rest is harmless: croner reads L and W case-insensitively.
func replaceNames(field string, names []string, base int, weekday bool) string {
	field = strings.ToLower(field)
	if weekday {
		field = strings.ReplaceAll(field, "-sun", "-7")
	}
	for i, name := range names {
		field = strings.ReplaceAll(field, name, strconv.Itoa(base+i))
	}
	return field
}

func (p *cronPattern) size(name string) int {
	switch name {
	case "minute":
		return 60
	case "hour":
		return 24
	case "day":
		return 31
	case "month":
		return 12
	}
	return 7
}

// part ports croner's CronPattern.partToArray for one field.
func (p *cronPattern) part(name, value string, offset int) error {
	if value == "" {
		if name == "day" && (p.lastDay || p.lastWeekday) {
			return nil
		}
		return errCronPattern
	}
	if value == "*" {
		p.fill(name)
		return nil
	}
	if items := strings.Split(value, ","); len(items) > 1 {
		for _, item := range items {
			if err := p.part(name, item, offset); err != nil {
				return err
			}
		}
		return nil
	}
	hasRange, hasStep := strings.Contains(value, "-"), strings.Contains(value, "/")
	if (hasRange || hasStep) && strings.ContainsAny(value, "Ww") {
		return errCronPattern
	}
	base, nth, err := splitNth(name, value)
	if err != nil {
		return err
	}
	switch {
	case hasRange && hasStep:
		from, rest, _ := strings.Cut(base, "-")
		to, step, ok := strings.Cut(rest, "/")
		if !ok || !digits(from) || !digits(to) || !digits(step) {
			return errCronPattern
		}
		return p.span(name, parseInt(from)+offset, parseInt(to)+offset, parseInt(step), nth)
	case hasRange:
		bounds := strings.Split(base, "-")
		if len(bounds) != 2 {
			return errCronPattern
		}
		from, okFrom := jsParseInt(bounds[0])
		to, okTo := jsParseInt(bounds[1])
		if !okFrom || !okTo {
			return errCronPattern
		}
		return p.span(name, from+offset, to+offset, 1, nth)
	case hasStep:
		prefix, stepText, _ := strings.Cut(base, "/")
		if prefix != "*" || strings.Contains(stepText, "/") {
			return errCronPattern
		}
		step, ok := jsParseInt(stepText)
		if !ok || step == 0 || step > p.size(name) {
			return errCronPattern
		}
		for v := 0; v < p.size(name); v += step {
			if err := p.set(name, v, nth); err != nil {
				return err
			}
		}
		return nil
	}
	target := name
	if strings.ContainsAny(value, "Ww") {
		if name != "day" {
			return errCronPattern
		}
		target = "nearest"
	}
	v, ok := jsParseInt(base)
	if !ok {
		return errCronPattern
	}
	return p.set(target, v+offset, nth)
}

func (p *cronPattern) span(name string, from, to, step int, nth uint8) error {
	if from > to || step == 0 || step > p.size(name) {
		return errCronPattern
	}
	for v := from; v <= to; v += step {
		if err := p.set(name, v, nth); err != nil {
			return err
		}
	}
	return nil
}

// splitNth separates a weekday's `#n` or trailing `L`. An empty `#` suffix
// means every occurrence, as croner falls back to its default for it.
func splitNth(name, value string) (string, uint8, error) {
	if base, nth, ok := strings.Cut(value, "#"); ok {
		if name != "weekday" {
			return "", 0, errCronPattern
		}
		if i := strings.IndexByte(nth, '#'); i >= 0 {
			nth = nth[:i]
		}
		switch {
		case nth == "":
			return base, anyOccurrence, nil
		case strings.EqualFold(nth, "L"):
			return base, lastOccurrence, nil
		}
		// croner compares the text numerically, so "02" is the second occurrence.
		n, ok := jsNumber(nth)
		if !ok || n < 1 || n > 5 {
			return "", 0, errCronPattern
		}
		return base, occurrence[n-1], nil
	}
	if strings.HasSuffix(strings.ToUpper(value), "L") {
		if name != "weekday" {
			return "", 0, errCronPattern
		}
		return value[:len(value)-1], lastOccurrence, nil
	}
	return value, anyOccurrence, nil
}

func (p *cronPattern) fill(name string) {
	for v := range p.size(name) {
		_ = p.set(name, v, anyOccurrence)
	}
}

func (p *cronPattern) set(name string, v int, nth uint8) error {
	if name == "weekday" {
		if v == 7 {
			v = 0
		}
		if v < 0 || v > 6 {
			return errCronPattern
		}
		if nth == anyOccurrence {
			p.weekday[v] = anyOccurrence
		} else {
			p.weekday[v] |= nth
		}
		return nil
	}
	size := p.size(name)
	if name == "nearest" {
		size = 31
	}
	if v < 0 || v >= size {
		return errCronPattern
	}
	switch name {
	case "minute":
		p.minute[v] = true
	case "hour":
		p.hour[v] = true
	case "day":
		p.day[v] = true
	case "nearest":
		p.nearest[v] = true
	case "month":
		p.month[v] = true
	}
	return nil
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func parseInt(s string) int {
	n := 0
	for i := range len(s) {
		n = n*10 + int(s[i]-'0')
		if n > 1<<20 {
			return n
		}
	}
	return n
}

// jsParseInt reads the leading decimal digits like JavaScript's parseInt(s, 10).
func jsParseInt(s string) (int, bool) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	return parseInt(s[:end]), true
}

// jsNumber converts a whole string of digits like JavaScript's relational coercion.
func jsNumber(s string) (int, bool) {
	if !digits(s) {
		return 0, false
	}
	return parseInt(s), true
}

func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// dayMatches reports whether a day number of the month matches. A number past
// the month's end has the weekday of the day it rolls over to and counts as
// the last occurrence of it, as croner evaluates it.
func (p *cronPattern) dayMatches(year int, month time.Month, day int) bool {
	last := daysIn(year, month)
	match := p.day[day-1]
	if !match {
		for i, on := range p.nearest {
			if on && nearestWeekday(year, month, i+1) == day {
				match = true
				break
			}
		}
	}
	if p.lastWeekday && day == lastWeekdayOf(year, month) {
		match = true
	}
	if p.lastDay && day == last {
		match = true
	}
	if p.starWeekday {
		return match
	}
	flags := p.weekday[time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Weekday()]
	weekday := flags != 0 && (flags&occurrence[(day-1)/7] != 0 || flags&lastOccurrence != 0 && day+7 > last)
	if !p.weekdayAndOp && !p.starDay {
		return match || weekday
	}
	return match && weekday
}

func lastWeekdayOf(year int, month time.Month) int {
	last := daysIn(year, month)
	switch time.Date(year, month, last, 0, 0, 0, 0, time.UTC).Weekday() {
	case time.Sunday:
		return last - 2
	case time.Saturday:
		return last - 1
	}
	return last
}

// nearestWeekday is the weekday closest to day within its month, or -1 when
// the month is shorter than day.
func nearestWeekday(year int, month time.Month, day int) int {
	last := daysIn(year, month)
	if day > last {
		return -1
	}
	switch time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Weekday() {
	case time.Sunday:
		if day == last {
			return day - 2
		}
		return day + 1
	case time.Saturday:
		if day == 1 {
			return day + 2
		}
		return day - 1
	}
	return day
}

// nextWall returns the first matching wall-clock minute at or after from, a
// civil time carried in UTC, or false when none remains before croner's limit.
//
// croner scans day numbers up to 31 in every month. A number past the month's
// end that matches (by day of month, or by the weekday it would fall on) rolls
// over into the next month, and the scan resumes from that rolled-over day, so
// earlier days of the next month are skipped: "0 0 1,30 * *" after February 15
// runs on March 30. The TypeScript service computed nextAt this way.
func (p *cronPattern) nextWall(from time.Time) (time.Time, bool) {
	year, month, day := from.Date()
	hour, minute := from.Hour(), from.Minute()
months:
	for year < cronYearLimit {
		if p.month[month-1] {
			last := daysIn(year, month)
			for ; day <= 31; day++ {
				if !p.dayMatches(year, month, day) {
					hour, minute = 0, 0
					continue
				}
				if day > last {
					rolled := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
					year, month, day = rolled.Date()
					hour, minute = 0, 0
					continue months
				}
				for ; hour < 24; hour++ {
					if !p.hour[hour] {
						minute = 0
						continue
					}
					for ; minute < 60; minute++ {
						if p.minute[minute] {
							return time.Date(year, month, day, hour, minute, 0, 0, time.UTC), true
						}
					}
					minute = 0
				}
				hour, minute = 0, 0
			}
		}
		day, hour, minute = 1, 0, 0
		if month == time.December {
			year, month = year+1, time.January
		} else {
			month++
		}
	}
	return time.Time{}, false
}

// civil is the wall clock of an instant in a zone, at second precision, carried in UTC.
func civil(t time.Time, location *time.Location) time.Time {
	y, mo, d := t.In(location).Date()
	h, mi, s := t.In(location).Clock()
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

// instant converts a wall-clock time to an instant as croner's fromTZ does: the
// earlier of two repeated times, and a time skipped by a forward transition
// moves forward by the transition's length.
func instant(wall time.Time, location *time.Location) time.Time {
	t := wall
	o := t.Add(wall.Sub(civil(t, location)))
	h := civil(o, location)
	if h.Equal(wall) {
		if earlier := o.Add(-time.Hour); civil(earlier, location).Equal(wall) {
			return earlier
		}
		return o
	}
	l := o.Add(wall.Sub(h))
	if civil(l, location).Equal(wall) {
		return l
	}
	if o.After(l) {
		return o
	}
	return l
}
