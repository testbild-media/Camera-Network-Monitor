package sim

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// HikvisionServer simulates the ISAPI HTTP interface of a Hikvision NVR
// (Digest authentication, device info, channels, time, storage and the
// CMSearchDescription recording search). Live/playback video is served by
// an RTSPServer with Route = HikvisionRoute.
type HikvisionServer struct {
	User, Pass string
	Channels   int
	HasDisk    bool
	// Recorded returns recorded spans per channel (1 based, device wall
	// clock, location UTC). Nil = 10 minutes of every quarter hour.
	Recorded func(channel int, from, to time.Time) [][2]time.Time
	// TrackListOnly rejects the spec element name "trackIDList" (some
	// firmware only accepts "trackList").
	TrackListOnly bool

	ln        net.Listener
	srv       *http.Server
	AuthFails atomic.Int32
	Searches  atomic.Int32
}

const hikNonce = "4e4455324d4459314d6a51364d6a417a4f544d79"

// Listen starts the server.
func (s *HikvisionServer) Listen(addr string) error {
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
func (s *HikvisionServer) Addr() string { return s.ln.Addr().String() }

// Close stops the server.
func (s *HikvisionServer) Close() { s.srv.Close() }

var digestParam = regexp.MustCompile(`(\w+)=(?:"([^"]*)"|([^,\s]*))`)

func md5h(v string) string {
	h := md5.Sum([]byte(v))
	return hex.EncodeToString(h[:])
}

func (s *HikvisionServer) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Digest ") {
		return false
	}
	p := map[string]string{}
	for _, m := range digestParam.FindAllStringSubmatch(h[7:], -1) {
		v := m[2]
		if v == "" {
			v = m[3]
		}
		p[strings.ToLower(m[1])] = v
	}
	ha1 := md5h(s.User + ":" + p["realm"] + ":" + s.Pass)
	ha2 := md5h(r.Method + ":" + p["uri"])
	want := md5h(ha1 + ":" + p["nonce"] + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
	if p["username"] != s.User || p["nonce"] != hikNonce || p["response"] != want {
		s.AuthFails.Add(1)
		return false
	}
	return true
}

func (s *HikvisionServer) spans(ch int, from, to time.Time) [][2]time.Time {
	if s.Recorded != nil {
		return s.Recorded(ch, from, to)
	}
	var out [][2]time.Time
	for t := from.Truncate(15 * time.Minute); t.Before(to); t = t.Add(15 * time.Minute) {
		st, en := t, t.Add(10*time.Minute)
		if en.After(from) && st.Before(to) {
			out = append(out, [2]time.Time{st, en})
		}
	}
	return out
}

func (s *HikvisionServer) handle(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest qop="auth", realm="IP Camera(SIM)", nonce="%s", stale="FALSE"`, hikNonce))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	const hdr = `<?xml version="1.0" encoding="UTF-8"?>`
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/ISAPI/System/deviceInfo":
		fmt.Fprintf(w, `%s<DeviceInfo version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema"><deviceName>NVR-SIM</deviceName><model>NVR-SIM-8</model><serialNumber>HIKSIM0001</serialNumber><firmwareVersion>V4.30.085</firmwareVersion><firmwareReleasedDate>build 210101</firmwareReleasedDate><deviceType>NVR</deviceType></DeviceInfo>`, hdr)
	case r.Method == http.MethodGet && r.URL.Path == "/ISAPI/ContentMgmt/InputProxy/channels":
		var b strings.Builder
		for i := 1; i <= s.Channels; i++ {
			fmt.Fprintf(&b, `<InputProxyChannel version="2.0"><id>%d</id><name>HIK CAM %d</name></InputProxyChannel>`, i, i)
		}
		fmt.Fprintf(w, `%s<InputProxyChannelList version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema">%s</InputProxyChannelList>`, hdr, b.String())
	case r.Method == http.MethodGet && r.URL.Path == "/ISAPI/System/time":
		fmt.Fprintf(w, `%s<Time version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema"><timeMode>NTP</timeMode><localTime>%s+01:00</localTime><timeZone>CST-1:00:00</timeZone></Time>`, hdr, DeviceNow().Format("2006-01-02T15:04:05"))
	case r.Method == http.MethodGet && r.URL.Path == "/ISAPI/ContentMgmt/Storage":
		disk := ""
		if s.HasDisk {
			disk = `<hdd version="2.0"><id>1</id><hddName>hdd1</hddName><status>ok</status><capacity>1907729</capacity><freeSpace>1024</freeSpace></hdd>`
		}
		fmt.Fprintf(w, `%s<storage version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema"><hddList>%s</hddList></storage>`, hdr, disk)
	case r.Method == http.MethodPost && r.URL.Path == "/ISAPI/ContentMgmt/search":
		s.Searches.Add(1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var q struct {
			SearchID string `xml:"searchID"`
			TrackIDs []int  `xml:"trackIDList>trackID"`
			Tracks   []int  `xml:"trackList>trackID"`
			Start    string `xml:"timeSpanList>timeSpan>startTime"`
			End      string `xml:"timeSpanList>timeSpan>endTime"`
			Max      int    `xml:"maxResults"`
			Position int    `xml:"searchResultPostion"`
		}
		if xml.Unmarshal(body, &q) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		tracks := q.TrackIDs
		if s.TrackListOnly {
			if len(tracks) > 0 {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `%s<ResponseStatus><statusCode>4</statusCode><statusString>Invalid XML Content</statusString><subStatusCode>badXmlContent</subStatusCode></ResponseStatus>`, hdr)
				return
			}
			tracks = q.Tracks
		}
		from, e1 := time.Parse("2006-01-02T15:04:05Z", q.Start)
		to, e2 := time.Parse("2006-01-02T15:04:05Z", q.End)
		if e1 != nil || e2 != nil || len(tracks) != 1 || tracks[0]/100 < 1 || tracks[0]/100 > s.Channels {
			fmt.Fprintf(w, `%s<CMSearchResult><searchID>%s</searchID><responseStatus>false</responseStatus><responseStatusStrg>INVALID TRACK ID</responseStatusStrg><numOfMatches>0</numOfMatches></CMSearchResult>`, hdr, q.SearchID)
			return
		}
		var all [][2]time.Time
		if s.HasDisk {
			all = s.spans(tracks[0]/100, from, to)
		}
		if q.Max <= 0 {
			q.Max = 40
		}
		page := []([2]time.Time){}
		if q.Position < len(all) {
			page = all[q.Position:min(len(all), q.Position+q.Max)]
		}
		status := "OK"
		switch {
		case len(all) == 0:
			status = "NO MATCHES"
		case q.Position+len(page) < len(all):
			status = "MORE"
		}
		var b strings.Builder
		for _, sp := range page {
			fmt.Fprintf(&b, `<searchMatchItem><sourceID>{0000}</sourceID><trackID>%d</trackID><timeSpan><startTime>%s</startTime><endTime>%s</endTime></timeSpan><mediaSegmentDescriptor><contentType>video</contentType><codecType>H.264-BP</codecType><playbackURI>rtsp://sim/Streaming/tracks/%d/?starttime=%s&amp;endtime=%s</playbackURI></mediaSegmentDescriptor></searchMatchItem>`,
				tracks[0], sp[0].Format("2006-01-02T15:04:05Z"), sp[1].Format("2006-01-02T15:04:05Z"), tracks[0], sp[0].Format("20060102T150405Z"), sp[1].Format("20060102T150405Z"))
		}
		fmt.Fprintf(w, `%s<CMSearchResult version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema"><searchID>%s</searchID><responseStatus>true</responseStatus><responseStatusStrg>%s</responseStatusStrg><numOfMatches>%d</numOfMatches><matchList>%s</matchList></CMSearchResult>`,
			hdr, q.SearchID, status, len(page), b.String())
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}
