package sim

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// AxisRecording is one edge storage recording (times UTC).
type AxisRecording struct {
	ID         string
	Source     int // video source, 1 based
	Start, End time.Time
}

// AxisServer simulates the VAPIX HTTP interface of an Axis camera with edge
// storage (Digest authentication, param.cgi, time.cgi, disks/list.cgi,
// record/list.cgi). Video is served by an RTSPServer with Route = s.Route
// and ONVIFReplay = s.Replay, ReplayNoRequire = true ("Range: clock=").
type AxisServer struct {
	User, Pass string
	Recordings []AxisRecording
	NoDisk     bool
	Sources    int // video sources (0 = 1)

	ln        net.Listener
	srv       *http.Server
	AuthFails atomic.Int32
	Lists     atomic.Int32
}

// Listen starts the server.
func (s *AxisServer) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.srv = &http.Server{Handler: http.HandlerFunc(s.handle), ReadHeaderTimeout: 5 * time.Second}
	go s.srv.Serve(ln)
	return nil
}

// Addr returns the listen address.
func (s *AxisServer) Addr() string { return s.ln.Addr().String() }

// Close stops the server.
func (s *AxisServer) Close() { s.srv.Close() }

func (s *AxisServer) handle(w http.ResponseWriter, r *http.Request) {
	hs := &HikvisionServer{User: s.User, Pass: s.Pass} // same digest scheme
	if !hs.authorized(r) {
		if r.Header.Get("Authorization") != "" {
			s.AuthFails.Add(1)
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="AXIS_SIM", nonce="%s", qop="auth"`, hikNonce))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/axis-cgi/param.cgi":
		fmt.Fprint(w, "root.Brand.ProdFullName=AXIS SIM Network Camera\nroot.Brand.ProdNbr=SIM-1\nroot.Properties.System.SerialNumber=ACCC8E000001\nroot.Properties.Firmware.Version=11.0.SIM\n")
		for i := 0; i < max(s.Sources, 1); i++ {
			fmt.Fprintf(w, "root.ImageSource.I%d.Name=Image source %d\nroot.ImageSource.I%d.Sensor.AspectRatio=16:9\n", i, i+1, i)
		}
	case "/axis-cgi/time.cgi":
		json.NewEncoder(w).Encode(map[string]any{"apiVersion": "1.0", "data": map[string]any{"dateTime": time.Now().UTC().Format(time.RFC3339), "timeZone": "Europe/Berlin"}})
	case "/axis-cgi/disks/list.cgi":
		w.Header().Set("Content-Type", "text/xml")
		disk := ""
		if !s.NoDisk {
			disk = `<disk diskid="SD_DISK" name="" totalsize="61798400" freesize="1200000" cleanuplevel="90" cleanupmaxage="7" cleanuppolicy="fifo" locked="no" full="no" readonly="no" status="OK" filesystem="ext4" group="S0" requiredfilesystem="none" encryptionenabled="false" diskencrypted="false"/>`
		}
		fmt.Fprintf(w, `<?xml version="1.0"?><root xmlns="http://www.axis.com/vapix/http_cgi/disks/list1"><disks numberofdisks="%d">%s</disks></root>`, map[bool]int{true: 0, false: 1}[s.NoDisk], disk)
	case "/axis-cgi/record/list.cgi":
		s.Lists.Add(1)
		q := r.URL.Query()
		from, e1 := time.Parse(time.RFC3339, q.Get("starttime"))
		to, e2 := time.Parse(time.RFC3339, q.Get("stoptime"))
		var hits []AxisRecording
		for _, rec := range s.Recordings {
			if (e1 == nil && !rec.End.After(from)) || (e2 == nil && !rec.Start.Before(to)) {
				continue
			}
			hits = append(hits, rec)
		}
		max, _ := strconv.Atoi(q.Get("maxnumberofresults"))
		first, _ := strconv.Atoi(q.Get("startatresultnumber"))
		if first < 1 {
			first = 1
		}
		page := hits
		if first-1 < len(page) {
			page = page[first-1:]
		} else {
			page = nil
		}
		if max > 0 && len(page) > max {
			page = page[:max]
		}
		var b strings.Builder
		for _, rec := range page {
			stop := rec.End.UTC().Format("2006-01-02T15:04:05.000Z")
			if rec.End.After(time.Now()) {
				stop = "" // still recording
			}
			fmt.Fprintf(&b, `<recording diskid="SD_DISK" recordingid="%s" starttime="%s" stoptime="%s" recordingtype="continuous" eventid="continuous" source="%d" recordingstatus="%s"><video mimetype="video/x-h264" width="1280" height="720" framerate="15:1"/></recording>`,
				rec.ID, rec.Start.UTC().Format("2006-01-02T15:04:05.000Z"), stop, rec.Source, map[bool]string{true: "recording", false: "completed"}[stop == ""])
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<?xml version="1.0"?><root><recordings totalnumberofrecordings="%d" numberofrecordings="%d">%s</recordings></root>`, len(hits), len(page), b.String())
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// Route resolves /axis-media/media.amp?camera=N[&resolution=…] (live, a
// resolution means the sub stream) and ?recordingid=… (replay).
func (s *AxisServer) Route(u *url.URL, _ *time.Location) (Route, bool) {
	if strings.TrimSuffix(u.Path, "/") != "/axis-media/media.amp" {
		return Route{}, false
	}
	q := u.Query()
	if id := q.Get("recordingid"); id != "" {
		for _, rec := range s.Recordings {
			if rec.ID == id {
				return Route{Channel: rec.Source, Playback: true, Start: rec.Start, End: rec.End}, true
			}
		}
		return Route{}, false
	}
	ch, err := strconv.Atoi(q.Get("camera"))
	if err != nil {
		ch = 1
	}
	sub := 0
	if q.Get("resolution") != "" {
		sub = 1
	}
	return Route{Channel: ch, Subtype: sub}, true
}

// Replay answers a "Range: clock" replay of a source (1 based): the footage
// ends with the recording.
func (s *AxisServer) Replay(channel int, start, end time.Time) (time.Time, bool) {
	for _, rec := range s.Recordings {
		if rec.Source == channel && !start.Before(rec.Start) && start.Before(rec.End) {
			until := end
			if until.IsZero() || until.After(rec.End) {
				until = rec.End
			}
			return until, true
		}
	}
	return time.Time{}, false
}
