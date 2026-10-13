// Command cnm-sim runs simulated DVRs for end-to-end testing without hardware:
//
//	Dahua RPC2:          --dahua-http (classic web login), RTSP on --dahua-rtsp (8 channels)
//	Dahua RPC2 (new):    --dahua-web3 (current Web3.0 login, 4 channels, same RTSP)
//	DVRIP modern:        --sofia-modern (4 channels, recordings of the last 2 h)
//	DVRIP legacy:        --sofia-legacy (4 channels, two-socket, recordings)
//	Dahua native:        --dahua-native (native TCP protocol; live via --dahua-rtsp)
//	Hikvision NVR:       ISAPI on --hik-http, RTSP on --hik-rtsp (4 channels)
//	ONVIF recorder:      SOAP on --onvif-http, RTSP on --onvif-rtsp (4 channels, Profile G)
//	Axis camera:         VAPIX on --axis-http, RTSP on --axis-rtsp (2 sources, edge storage)
//	Uniview NVR:         RTSP on --uniview-rtsp (4 channels)
//	Network search:      answers Dahua DHIP, ONVIF WS-Discovery and Axis SSDP (--discovery)
//
// All devices accept the user "admin" (Axis: "root") with --password. Video is a generated
// H.264 test pattern (FFmpeg required).
//
//	cnm-sim display-fixture DIR   writes a fake /sys/class/drm tree (two
//	                              monitors, one empty port) for demos of the
//	                              "Displays" settings: CNM_DRM_SYSFS=DIR cnm serve
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cnm/internal/display"
	"cnm/internal/sim"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "display-fixture" {
		err := display.WriteSysfsFixture(os.Args[2], "card0", map[string]*display.SynthMonitor{
			"HDMI-A-1": {Vendor: "SIM", Product: 0xa0c5, Serial: 4242, Name: "SIM MONITOR", W: 1920, H: 1200, Clock: 154000, HTotal: 2080, VTotal: 1235},
			"DP-1":     {Vendor: "SAM", Product: 0x0f00, Serial: 7, Name: "SAMSUNG TV", W: 3840, H: 2160, Clock: 594000, HTotal: 4400, VTotal: 2250, VICs: []int{97, 95, 16, 31, 4}},
			"HDMI-A-2": nil,
		})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("display fixture written to %s – start CNM with CNM_DRM_SYSFS=%s\n", os.Args[2], os.Args[2])
		return
	}
	if len(os.Args) >= 3 && os.Args[1] == "legacy-fixture" {
		httpPort, rtspPort := 18008, 18554
		if len(os.Args) == 5 {
			fmt.Sscan(os.Args[3], &httpPort)
			fmt.Sscan(os.Args[4], &rtspPort)
		}
		if err := sim.WriteLegacyFixture(os.Args[2], httpPort, rtspPort); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("legacy fixture written to %s (admin %q / %q, DVR password %q)\n", os.Args[2], sim.LegacyAdminUser, sim.LegacyAdminPass, sim.LegacyDVRPass)
		return
	}
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "ffmpeg binary")
	pass := flag.String("password", "sim12345", "device password for user admin")
	dahuaHTTP := flag.String("dahua-http", "127.0.0.1:18008", "Dahua RPC2 listen address ('' = off)")
	dahuaRTSP := flag.String("dahua-rtsp", "127.0.0.1:18554", "Dahua RTSP listen address")
	dahuaWeb3 := flag.String("dahua-web3", "127.0.0.1:18009", "generic Dahua (current Web3.0 login) RPC2 listen address, video via --dahua-rtsp ('' = off)")
	modern := flag.String("sofia-modern", "127.0.0.1:20051", "DVRIP modern listen address ('' = off)")
	legacy := flag.String("sofia-legacy", "127.0.0.1:20052", "DVRIP legacy listen address ('' = off)")
	native := flag.String("dahua-native", "127.0.0.1:18777", "Dahua native protocol listen address ('' = off)")
	hikHTTP := flag.String("hik-http", "127.0.0.1:18080", "Hikvision ISAPI listen address ('' = off)")
	hikRTSP := flag.String("hik-rtsp", "127.0.0.1:18564", "Hikvision RTSP listen address")
	onvifHTTP := flag.String("onvif-http", "127.0.0.1:18081", "ONVIF recorder SOAP listen address ('' = off)")
	onvifRTSP := flag.String("onvif-rtsp", "127.0.0.1:18565", "ONVIF recorder RTSP listen address")
	axisHTTP := flag.String("axis-http", "127.0.0.1:18082", "Axis VAPIX listen address ('' = off)")
	axisRTSP := flag.String("axis-rtsp", "127.0.0.1:18567", "Axis RTSP listen address")
	unvRTSP := flag.String("uniview-rtsp", "127.0.0.1:18566", "Uniview NVR RTSP listen address ('' = off)")
	disc := flag.Bool("discovery", true, "answer Dahua DHIP, ONVIF WS-Discovery and Axis SSDP searches")
	stall := flag.String("dahua-stall", "", "Dahua live streams that connect but send no video, e.g. \"3:sub,4:main\" (fallback tests)")
	flag.Parse()

	log.Println("generating test clips with", *ffmpeg)
	type variant struct {
		main bool
		hue  int
	}
	clips := map[variant]*sim.Clip{}
	for _, hue := range []int{0, 90, 180, 270} {
		m, err := sim.Generate(*ffmpeg, 1280, 720, 15, 4, hue)
		if err != nil {
			log.Fatal(err)
		}
		s, err := sim.Generate(*ffmpeg, 640, 360, 10, 4, hue)
		if err != nil {
			log.Fatal(err)
		}
		clips[variant{true, hue}] = m
		clips[variant{false, hue}] = s
	}
	clip := func(ch int, main bool) *sim.Clip { return clips[variant{main, (ch % 4) * 90}] }

	if *dahuaHTTP != "" {
		d := &sim.DahuaServer{User: "admin", Pass: *pass, Channels: 8, HasDisk: true, Recordings: sim.QuarterSchedule}
		if err := d.Listen(*dahuaHTTP); err != nil {
			log.Fatal(err)
		}
		stalled := map[string]bool{}
		for _, x := range strings.Split(*stall, ",") {
			if x = strings.TrimSpace(x); x != "" {
				stalled[x] = true
			}
		}
		r := &sim.RTSPServer{User: "admin", Pass: *pass, HasRecording: sim.InQuarterSchedule,
			Stall: func(ch, sub int) bool {
				kind := "main"
				if sub == 1 {
					kind = "sub"
				}
				return stalled[fmt.Sprintf("%d:%s", ch, kind)]
			},
			Clip: func(ch, sub int) *sim.Clip {
				if ch < 1 || ch > 8 {
					return nil
				}
				return clip(ch-1, sub == 0)
			}}
		if err := r.Listen(*dahuaRTSP); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Dahua RPC2 sim: http=%s rtsp=%s (recordings: 10 of every 15 minutes)\n", d.Addr(), r.Addr())
		if *dahuaWeb3 != "" {
			g := &sim.DahuaServer{User: "admin", Pass: *pass, Channels: 4, HasDisk: true, Recordings: sim.QuarterSchedule, Web3: true}
			if err := g.Listen(*dahuaWeb3); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("Dahua RPC2 sim (Web3.0 login): http=%s rtsp=%s\n", g.Addr(), r.Addr())
		}
	}
	for _, x := range []struct {
		addr   string
		legacy bool
	}{{*modern, false}, {*legacy, true}} {
		if x.addr == "" {
			continue
		}
		s := &sim.SofiaServer{User: "admin", Pass: *pass, Legacy: x.legacy, Channels: 4, ChunkSize: 4096, Clip: clip}
		if err := s.Listen(x.addr); err != nil {
			log.Fatal(err)
		}
		mode := "modern"
		if x.legacy {
			mode = "legacy"
		}
		fmt.Printf("DVRIP %s sim: %s\n", mode, s.Addr())
	}
	if *native != "" {
		now := sim.DeviceNow()
		n := &sim.DahuaNativeServer{User: "admin", Pass: *pass, Clip: clip(0, true), Recorded: func(ch int) []sim.NativeCluster {
			return []sim.NativeCluster{{Start: now.Add(-3 * time.Hour), End: now.Add(-90 * time.Minute)}, {Start: now.Add(-75 * time.Minute), End: now.Add(24 * time.Hour)}}
		}}
		if err := n.Listen(*native); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Dahua native sim: %s (live via the Dahua RTSP sim)\n", n.Addr())
	}
	if *hikHTTP != "" {
		h := &sim.HikvisionServer{User: "admin", Pass: *pass, Channels: 4, HasDisk: true}
		if err := h.Listen(*hikHTTP); err != nil {
			log.Fatal(err)
		}
		r := &sim.RTSPServer{User: "admin", Pass: *pass, Route: sim.HikvisionRoute, Loc: time.UTC,
			HasRecording: func(ch int, st time.Time) bool { return st.Minute()%15 < 10 },
			Clip: func(ch, sub int) *sim.Clip {
				if ch < 1 || ch > 4 {
					return nil
				}
				return clip(ch-1, sub == 0)
			}}
		if err := r.Listen(*hikRTSP); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Hikvision NVR sim: isapi=%s rtsp=%s (recordings: 10 of every 15 minutes)\n", h.Addr(), r.Addr())
	}
	if *onvifHTTP != "" {
		o := &sim.ONVIFServer{User: "admin", Pass: *pass, Channels: 4, SubStreams: true, SourceTokens: true}
		for ch := 0; ch < 4; ch++ {
			o.Recordings = append(o.Recordings, sim.ONVIFRecording{Token: fmt.Sprintf("REC_%d", ch+1), Channel: ch, Start: time.Now().Add(-24 * time.Hour), End: time.Now().Add(24 * time.Hour)})
		}
		r := &sim.RTSPServer{User: "admin", Pass: *pass, Route: sim.ONVIFRoute, ONVIFReplay: o.Replay,
			Clip: func(ch, sub int) *sim.Clip {
				if ch < 1 || ch > 4 {
					return nil
				}
				return clip(ch-1, sub == 0)
			}}
		if err := r.Listen(*onvifRTSP); err != nil {
			log.Fatal(err)
		}
		o.RTSPAddr = r.Addr()
		if err := o.Listen(*onvifHTTP); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("ONVIF recorder sim: http=%s rtsp=%s (Profile G, recordings of the last 24 h)\n", o.Addr(), r.Addr())
	}
	if *axisHTTP != "" {
		// two video sources, hourly recordings of the last day, the current one ongoing
		a := &sim.AxisServer{User: "root", Pass: *pass, Sources: 2}
		hour := time.Now().UTC().Truncate(time.Hour)
		for src := 1; src <= 2; src++ {
			for h := 24; h >= 0; h-- {
				start := hour.Add(-time.Duration(h) * time.Hour)
				end := start.Add(time.Hour)
				if h == 0 {
					end = start.Add(48 * time.Hour)
				}
				a.Recordings = append(a.Recordings, sim.AxisRecording{ID: fmt.Sprintf("%s_S%d", start.Format("20060102_150405"), src), Source: src, Start: start, End: end})
			}
		}
		if err := a.Listen(*axisHTTP); err != nil {
			log.Fatal(err)
		}
		r := &sim.RTSPServer{User: "root", Pass: *pass, Route: a.Route, ONVIFReplay: a.Replay, ReplayNoRequire: true,
			Clip: func(ch, sub int) *sim.Clip {
				if ch < 1 || ch > 2 {
					return nil
				}
				return clip(ch+1, sub == 0)
			}}
		if err := r.Listen(*axisRTSP); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Axis camera sim (user root): vapix=%s rtsp=%s (2 sources, edge recordings of the last 24 h)\n", a.Addr(), r.Addr())
	}
	if *unvRTSP != "" {
		r := &sim.RTSPServer{User: "admin", Pass: *pass, Route: sim.UniviewRoute, Loc: time.UTC,
			HasRecording: func(ch int, st time.Time) bool { return st.Minute()%15 < 10 },
			Clip: func(ch, sub int) *sim.Clip {
				if ch < 1 || ch > 4 {
					return nil
				}
				return clip(ch-1, sub == 0)
			}}
		if err := r.Listen(*unvRTSP); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Uniview NVR sim: rtsp=%s (4 channels, recordings: 10 of every 15 minutes)\n", r.Addr())
	}
	if *disc {
		r := &sim.DiscoveryResponder{DahuaHTTP: 18008, ONVIFPort: 8899}
		if *axisHTTP != "" {
			r.AxisHTTP = 18082
		}
		if err := r.Start(); err != nil {
			log.Printf("discovery responder: %v", err)
		} else {
			fmt.Println("discovery responder: Dahua DHIP + ONVIF WS-Discovery + Axis SSDP")
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}
