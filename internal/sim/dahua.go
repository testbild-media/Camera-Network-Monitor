package sim

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DahuaServer simulates the Dahua RPC2 JSON API (classic web login).
type DahuaServer struct {
	User, Pass string
	Channels   int
	HasDisk    bool
	// Web3 makes the login behave like current Dahua firmware
	// (client type "Web3.0", loginType "Direct", session in the request
	// body only) instead of the classic one ("Dahua3.0-Web3.0" plus session cookie).
	Web3 bool
	// Recordings returns recorded ranges (device wall clock) for a channel.
	Recordings func(channel int, from, to time.Time) [][2]time.Time
	Loc        *time.Location

	ln            net.Listener
	srv           *http.Server
	Logins        atomic.Int32
	FailedLogins  atomic.Int32
	Rejected      atomic.Int32 // rejected logins (password or client type)
	mu            sync.Mutex
	session       string
	challenges    map[string]string // challenge session → random
	nextChallenge int
	finders       map[int64]*finder
	nextObj       int64
}

type finder struct {
	results [][2]time.Time
	pos     int
}

// Listen starts the HTTP server.
func (d *DahuaServer) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if d.Loc == nil {
		d.Loc = time.Local
	}
	d.ln = ln
	d.finders = map[int64]*finder{}
	d.nextObj = 1000
	mux := http.NewServeMux()
	mux.HandleFunc("/RPC2_Login", d.login)
	mux.HandleFunc("/RPC2", d.rpc)
	d.srv = &http.Server{Handler: mux}
	go d.srv.Serve(ln)
	return nil
}

// Addr returns the listen address.
func (d *DahuaServer) Addr() string { return d.ln.Addr().String() }

// Close stops the server.
func (d *DahuaServer) Close() { d.srv.Close() }

type req struct {
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      int64           `json:"id"`
	Session json.RawMessage `json:"session"`
	Object  int64           `json:"object"`
}

func upper(s string) string {
	h := md5.Sum([]byte(s))
	return strings.ToUpper(hex.EncodeToString(h[:]))
}

func writeJ(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (d *DahuaServer) login(w http.ResponseWriter, r *http.Request) {
	var q req
	json.NewDecoder(r.Body).Decode(&q)
	var p struct {
		UserName   string `json:"userName"`
		Password   string `json:"password"`
		ClientType string `json:"clientType"`
		LoginType  string `json:"loginType"`
	}
	json.Unmarshal(q.Params, &p)
	d.mu.Lock()
	defer d.mu.Unlock()
	if p.Password == "" {
		// every challenge has its own session and random (concurrent logins)
		if d.challenges == nil || len(d.challenges) > 64 {
			d.challenges = map[string]string{}
		}
		d.nextChallenge++
		id := fmt.Sprintf("challenge-%d", d.nextChallenge)
		d.challenges[id] = fmt.Sprintf("%d", time.Now().UnixNano()%1000000000)
		writeJ(w, map[string]any{"id": q.ID, "result": false, "session": id,
			"error":  map[string]any{"code": 268632079, "message": "Component error: login challenge!"},
			"params": map[string]any{"realm": "Login to SIM", "random": d.challenges[id], "encryption": "Default"}})
		return
	}
	// like the device: its web client type and the challenge session
	// (classic: as cookie, current firmware: in the request body) are required
	var id string
	validClient := p.ClientType == "Dahua3.0-Web3.0"
	if c, err := r.Cookie("DhWebClientSessionID"); err == nil {
		id = c.Value
	}
	if d.Web3 {
		json.Unmarshal(q.Session, &id)
		validClient = p.ClientType == "Web3.0" && p.LoginType == "Direct"
	}
	random, ok := d.challenges[id]
	delete(d.challenges, id)
	want := upper(p.UserName + ":" + random + ":" + upper(p.UserName+":Login to SIM:"+d.Pass))
	if !validClient || !ok || p.UserName != d.User || p.Password != want {
		d.Rejected.Add(1) // every rejected second step counts towards a device lockout
	}
	if !validClient || !ok {
		writeJ(w, map[string]any{"id": q.ID, "result": false, "error": map[string]any{"code": 268632085, "message": "Invalid client"}})
		return
	}
	if p.UserName != d.User || p.Password != want {
		d.FailedLogins.Add(1)
		writeJ(w, map[string]any{"id": q.ID, "result": false, "error": map[string]any{"code": 268632085, "message": "User or password not valid"}})
		return
	}
	d.Logins.Add(1)
	d.session = fmt.Sprintf("sess-%d", time.Now().UnixNano())
	writeJ(w, map[string]any{"id": q.ID, "result": true, "session": d.session, "params": map[string]any{"keepAliveInterval": 60}})
}

func (d *DahuaServer) rpc(w http.ResponseWriter, r *http.Request) {
	var q req
	json.NewDecoder(r.Body).Decode(&q)
	var sess string
	json.Unmarshal(q.Session, &sess)
	d.mu.Lock()
	valid := sess != "" && sess == d.session
	d.mu.Unlock()
	if !valid {
		writeJ(w, map[string]any{"id": q.ID, "result": false, "error": map[string]any{"code": 287637505, "message": "Invalid session in request data!"}})
		return
	}
	ok := func(params any) {
		writeJ(w, map[string]any{"id": q.ID, "result": true, "session": sess, "params": params})
	}
	const layout = "2006-01-02 15:04:05"
	switch q.Method {
	case "global.keepAlive":
		ok(map[string]any{"timeout": 300})
	case "global.logout":
		d.mu.Lock()
		d.session = ""
		d.mu.Unlock()
		ok(nil)
	case "global.getCurrentTime":
		ok(map[string]any{"time": time.Now().In(d.Loc).Format(layout)})
	case "magicBox.getDeviceType":
		ok(map[string]any{"type": "DVR-SIM"})
	case "magicBox.getSerialNo":
		ok(map[string]any{"sn": "SIM0001"})
	case "magicBox.getSoftwareVersion":
		ok(map[string]any{"version": map[string]any{"Version": "2.000.SIM", "BuildDate": "2026-01-01"}})
	case "configManager.getConfig":
		var table []map[string]any
		for i := 0; i < d.Channels; i++ {
			table = append(table, map[string]any{"Name": fmt.Sprintf("SIM CAM %d", i+1)})
		}
		ok(map[string]any{"table": table})
	case "storage.getDeviceAllInfo":
		if !d.HasDisk {
			ok(map[string]any{"info": []any{}})
			return
		}
		ok(map[string]any{"info": []any{map[string]any{"Name": "/dev/sda", "State": "Success", "Detail": []any{map[string]any{"TotalBytes": 1e12, "UsedBytes": 5e11}}}}})
	case "mediaFileFind.factory.create":
		d.mu.Lock()
		d.nextObj++
		id := d.nextObj
		d.finders[id] = &finder{}
		d.mu.Unlock()
		writeJ(w, map[string]any{"id": q.ID, "result": id, "session": sess})
	case "mediaFileFind.findFile":
		var p struct {
			Condition struct {
				Channel   int    `json:"Channel"`
				StartTime string `json:"StartTime"`
				EndTime   string `json:"EndTime"`
			} `json:"condition"`
		}
		json.Unmarshal(q.Params, &p)
		from, _ := time.ParseInLocation(layout, p.Condition.StartTime, d.Loc)
		to, _ := time.ParseInLocation(layout, p.Condition.EndTime, d.Loc)
		d.mu.Lock()
		f := d.finders[q.Object]
		if f != nil && d.Recordings != nil {
			f.results = d.Recordings(p.Condition.Channel, from, to)
		}
		d.mu.Unlock()
		writeJ(w, map[string]any{"id": q.ID, "result": f != nil && len(f.results) > 0, "session": sess})
	case "mediaFileFind.findNextFile":
		var p struct {
			Count int `json:"count"`
		}
		json.Unmarshal(q.Params, &p)
		d.mu.Lock()
		f := d.finders[q.Object]
		var infos []map[string]any
		if f != nil {
			for f.pos < len(f.results) && len(infos) < p.Count {
				r := f.results[f.pos]
				infos = append(infos, map[string]any{"Channel": 0, "StartTime": r[0].In(d.Loc).Format(layout), "EndTime": r[1].In(d.Loc).Format(layout), "Type": "dav"})
				f.pos++
			}
		}
		d.mu.Unlock()
		if len(infos) == 0 {
			writeJ(w, map[string]any{"id": q.ID, "result": false, "session": sess})
			return
		}
		ok(map[string]any{"found": len(infos), "infos": infos})
	case "mediaFileFind.close":
		ok(nil)
	case "mediaFileFind.destroy":
		d.mu.Lock()
		delete(d.finders, q.Object)
		d.mu.Unlock()
		ok(nil)
	default:
		writeJ(w, map[string]any{"id": q.ID, "result": false, "error": map[string]any{"code": 268894210, "message": "Method not found"}})
	}
}

// QuarterSchedule records 10 minutes of every 15 (e.g. motion recording).
func QuarterSchedule(_ int, from, to time.Time) [][2]time.Time {
	var out [][2]time.Time
	t := from.Truncate(15 * time.Minute)
	for ; t.Before(to); t = t.Add(15 * time.Minute) {
		s, e := t, t.Add(10*time.Minute)
		if e.Before(from) || !s.Before(to) {
			continue
		}
		out = append(out, [2]time.Time{s, e})
	}
	return out
}

// InQuarterSchedule reports whether t lies inside QuarterSchedule material.
func InQuarterSchedule(_ int, t time.Time) bool {
	return t.Sub(t.Truncate(15*time.Minute)) < 10*time.Minute
}
