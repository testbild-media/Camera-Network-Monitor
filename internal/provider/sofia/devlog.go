package sofia

import (
	"context"
	"regexp"
	"sort"
	"time"

	"cnm/internal/provider"
)

// The device's own event log (OPLogQuery): system starts, disk errors,
// logins, changed settings – what the recorder's menu shows under "Log".

const (
	MsgLogQuery    = 1442
	MsgLogQueryRsp = 1443
	logPage        = 128 // entries per answer
	logMaxPages    = 120 // logins fill most of the log; filtered views read further back
)

// logTypes maps CNM's log kinds onto the device's.
var logTypes = map[string]string{"all": "LogAll", "system": "LogSystem", "storage": "LogStorage",
	"config": "LogConfig", "record": "LogRecord", "alarm": "LogAlarm", "account": "LogAccount"}

// logMatch filters by kind as well: some firmware ignores the Type of
// the query and always answers with the whole log.
var logMatch = map[string]*regexp.Regexp{
	"storage": regexp.MustCompile(`(?i)storage|disk|hdd`),
	"system":  regexp.MustCompile(`(?i)reboot|shutdown|power|settime|systemstate|upgrade|exception|restart`),
	"config":  regexp.MustCompile(`(?i)config|default|modify`),
	"record":  regexp.MustCompile(`(?i)record`),
	"alarm":   regexp.MustCompile(`(?i)alarm|motion|loss|blind`),
	"account": regexp.MustCompile(`(?i)login|logout|user|password`),
}

// ReadLog reads the log of [from, to] page by page, newest first.
func (d *driver) ReadLog(ctx context.Context, from, to time.Time, kind string, limit int) ([]provider.LogEntry, error) {
	typ, ok := logTypes[kind]
	if !ok {
		typ = "LogAll"
	}
	ctl, err := d.control()
	if err != nil {
		return nil, err
	}
	var out []provider.LogEntry
	pos := 0
	for page := 0; page < logMaxPages && len(out) < limit; page++ {
		r, err := ctl.Request(ctx, MsgLogQuery, MsgLogQueryRsp, map[string]any{
			"Name":      "OPLogQuery",
			"SessionID": SessionString(ctl.Session()),
			"OPLogQuery": map[string]any{
				"BeginTime":   d.clock.ToDevice(from).Format(dvripTime),
				"EndTime":     d.clock.ToDevice(to).Format(dvripTime),
				"LogPosition": pos,
				"Type":        typ,
			},
		})
		if err != nil {
			return nil, err
		}
		if !r.OK() {
			if page > 0 || r.Ret() == retNoFiles {
				break
			}
			return nil, provider.NewError(provider.KindUnsupported, nil, "Das Gerät liefert kein Protokoll (Ret %d)", r.Ret())
		}
		entries := cfgArr(r["OPLogQuery"])
		next := pos
		for _, e := range entries {
			m := cfgMap(e)
			if p := num(m["Position"]); p+1 > next {
				next = p + 1
			}
			if re := logMatch[kind]; re != nil && !re.MatchString(str(m["Type"])) {
				continue
			}
			out = append(out, provider.LogEntry{Time: str(m["Time"]), Type: str(m["Type"]), Data: str(m["Data"]), User: str(m["User"])})
		}
		if len(entries) < logPage || next <= pos {
			break
		}
		pos = next
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
