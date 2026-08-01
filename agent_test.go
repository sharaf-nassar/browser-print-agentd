// Unit tests for the macOS print agent's CUPS parsing, discovery, health,
// failover, and origin posture.
//
// Nothing here touches CUPS or prints anything: the agent's execRunner seam is
// filled with a recording fake that answers lpstat from a per-test queue-state
// map and records (never executes) lp invocations, so a test can assert that no
// job was spooled. The lpstat strings the fake emits keep the exact shape
// captured on the hardware spike (macOS 26.4, CUPS 2.3.4, ZD621 over USB) —
// spacing, punctuation, and continuation lines are byte-for-byte — so the
// parsers are exercised against real CUPS output rather than a paraphrase.
// Only the station-identifying values are synthetic: queue names and the device
// serial are placeholders, never the ones a real station reports.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	usbQueue = "zd621_usb"
	netQueue = "zd621_net"

	// A USB device URI in the exact shape the attached ZD621 reports —
	// percent-encoded, carrying the manufacturer, the model with its dpi and
	// language, and a serial. The serial is synthetic; only its position and
	// shape matter to the parsers and to uid derivation.
	usbSerial = "SN0000000001"
	usbURI    = "usb://Zebra%20Technologies/ZTC%20ZD621-300dpi%20ZPL?serial=" + usbSerial
	netURI    = "socket://192.0.2.111:9100"

	// Spike output shape. Note the two spaces after "idle.", the trailing " -"
	// on both unhappy forms, and their tab-indented continuation lines.
	enabledLine   = "printer %s is idle.  enabled since Fri Jul 24 16:02:07 2026\n"
	disabledLine  = "printer %s disabled since Fri Jul 24 16:02:21 2026 -\n\treason unknown\n"
	absentLine    = "lpstat: Invalid destination name in list \"%s\".\n"
	acceptingLine = "%s accepting requests since Fri Jul 24 16:02:07 2026\n"
	rejectingLine = "%s not accepting requests since Fri Jul 24 16:02:21 2026 -\n\tRejecting Jobs\n"
	deviceLine    = "device for %s: %s\n"
)

// Queue states the fake can put a printer in.
const (
	stateEnabled   = "enabled"
	stateDisabled  = "disabled"
	stateRejecting = "rejecting"
	stateAbsent    = "absent"
)

// lpCall is one recorded lp invocation and the bytes it was handed.
type lpCall struct {
	Args    []string
	Payload []byte
}

// fakeCUPS stands in for lp/lpstat. `devices` is what `lpstat -v` enumerates
// and `states` is each queue's health; an unlisted queue reads as absent.
type fakeCUPS struct {
	mu      sync.Mutex
	devices []queueDevice
	states  map[string]string
	lpCalls []lpCall
	lpExit  int
	lpErr   string
}

// Run answers lpstat from the queue-state map and records lp without running it.
func (f *fakeCUPS) Run(ctx context.Context, name string, args ...string) (execResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case "lpstat":
		return f.lpstat(args)
	case "lp":
		return f.lp(args)
	}
	return execResult{}, fmt.Errorf("unexpected command in test: %s %v", name, args)
}

func (f *fakeCUPS) lpstat(args []string) (execResult, error) {
	if len(args) > 0 && args[0] == "-v" {
		var out strings.Builder
		for _, device := range f.devices {
			fmt.Fprintf(&out, deviceLine, device.Queue, device.URI)
		}
		return execResult{Stdout: out.String()}, nil
	}
	if len(args) < 2 {
		return execResult{ExitCode: 1}, nil
	}
	queue := args[1]
	state, listed := f.states[queue]
	if !listed {
		state = stateAbsent
	}
	if state == stateAbsent {
		return execResult{ExitCode: 1, Stderr: fmt.Sprintf(absentLine, queue)}, nil
	}
	switch args[0] {
	case "-p":
		// A disabled queue exits 0 exactly like an enabled one — the trap this
		// suite guards.
		if state == stateDisabled {
			return execResult{Stdout: fmt.Sprintf(disabledLine, queue)}, nil
		}
		return execResult{Stdout: fmt.Sprintf(enabledLine, queue)}, nil
	case "-a":
		// A rejecting queue reads enabled to `lpstat -p`; only `-a` reveals it.
		if state == stateRejecting {
			return execResult{Stdout: fmt.Sprintf(rejectingLine, queue)}, nil
		}
		return execResult{Stdout: fmt.Sprintf(acceptingLine, queue)}, nil
	}
	return execResult{ExitCode: 1}, nil
}

func (f *fakeCUPS) lp(args []string) (execResult, error) {
	call := lpCall{Args: append([]string(nil), args...)}
	if len(args) > 0 {
		if payload, err := os.ReadFile(args[len(args)-1]); err == nil {
			call.Payload = payload
		}
	}
	f.lpCalls = append(f.lpCalls, call)
	if f.lpExit != 0 {
		return execResult{ExitCode: f.lpExit, Stderr: f.lpErr}, nil
	}
	return execResult{Stdout: "request id is " + args[1] + "-11 (1 file)\n"}, nil
}

// calls returns a copy of the recorded lp invocations.
func (f *fakeCUPS) calls() []lpCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lpCall(nil), f.lpCalls...)
}

// twoPrinterCUPS is the standard station: a USB ZD621 and a network Zebra,
// enumerated network-first so the USB-first ordering is actually proven.
func twoPrinterCUPS(usbState string, netState string) *fakeCUPS {
	return &fakeCUPS{
		devices: []queueDevice{
			{Queue: netQueue, URI: netURI},
			{Queue: usbQueue, URI: usbURI},
		},
		states: map[string]string{usbQueue: usbState, netQueue: netState},
	}
}

// startAgent boots the agent over the fake CUPS on an ephemeral loopback port
// and returns the base URL plus the log buffer.
func startAgent(t *testing.T, fake *fakeCUPS, originAllow []string) (string, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	handler := newAgent(fake, newAgentLogger(logs), originAllow)
	// No health caching in tests: each assertion probes the fake's current state.
	handler.health.ttl = 0
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL, logs
}

// postWrite issues a /write with an optional Origin header.
func postWrite(t *testing.T, base string, origin string, body map[string]any) (int, string) {
	t.Helper()
	return postJSON(t, base, "/write", origin, body)
}

// postPrintPDF issues a /print-pdf with an optional Origin header.
func postPrintPDF(t *testing.T, base string, origin string, body map[string]any) (int, string) {
	t.Helper()
	return postJSON(t, base, "/print-pdf", origin, body)
}

// postJSON posts an encoded body to path with an optional Origin header, and is
// what both print routes are driven through so a difference between them is
// never an artifact of the harness.
func postJSON(
	t *testing.T, base string, path string, origin string, body map[string]any,
) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	response := postRaw(t, base, path, origin, bytes.NewReader(encoded))
	defer response.Body.Close()
	payload := &bytes.Buffer{}
	payload.ReadFrom(response.Body)
	return response.StatusCode, payload.String()
}

// postRaw posts an arbitrary body and hands back the live response so a caller
// can read headers as well as status. The caller closes the body.
func postRaw(
	t *testing.T, base string, path string, origin string, body io.Reader,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, base+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	return response
}

// samplePDF is a minimal well-formed PDF. Only the leading %PDF signature is
// load-bearing for the agent — lp is recorded, never executed, so nothing here
// is ever rendered — but a real document shape keeps the fixture honest about
// what the route actually carries.
var samplePDF = []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n" +
	"trailer\n<< /Root 1 0 R >>\n%%EOF\n")

// getPath issues a GET and returns status plus body.
func getPath(t *testing.T, base string, path string) (int, string) {
	t.Helper()
	response, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer response.Body.Close()
	payload := &bytes.Buffer{}
	payload.ReadFrom(response.Body)
	return response.StatusCode, payload.String()
}

// @lat: [[tests#Agent Core#Queue Health Reads Status Text]]
func TestQueueHealthReadsStatusText(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"enabled", fmt.Sprintf(enabledLine, usbQueue), true},
		{"disabled with continuation", fmt.Sprintf(disabledLine, usbQueue), false},
		{"absent", fmt.Sprintf(absentLine, usbQueue), false},
		{"unparseable", "printer " + usbQueue + " is doing something new\n", false},
		{"other queue only", fmt.Sprintf(enabledLine, netQueue), false},
		{"empty", "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := parseQueueEnabled(testCase.output, usbQueue); got != testCase.want {
				t.Fatalf("parseQueueEnabled(%q) = %v, want %v",
					testCase.output, got, testCase.want)
			}
		})
	}

	// The exit code carries one bit and it is not health: a DISABLED queue exits
	// 0 and only an ABSENT queue exits 1, so health must come from the text.
	fake := twoPrinterCUPS(stateDisabled, stateEnabled)
	cups := newCUPSClient(fake)
	disabled, err := cups.run(context.Background(), "lpstat", "-p", usbQueue)
	if err != nil {
		t.Fatalf("lpstat -p: %v", err)
	}
	if disabled.ExitCode != 0 {
		t.Fatalf("disabled queue exit code = %d, want 0", disabled.ExitCode)
	}
	absent, err := cups.run(context.Background(), "lpstat", "-p", "nosuchqueue")
	if err != nil {
		t.Fatalf("lpstat -p absent: %v", err)
	}
	if absent.ExitCode != 1 {
		t.Fatalf("absent queue exit code = %d, want 1", absent.ExitCode)
	}
}

// @lat: [[tests#Agent Core#Rejecting Queue Is Unhealthy]]
func TestRejectingQueueIsUnhealthy(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"accepting", fmt.Sprintf(acceptingLine, usbQueue), true},
		{"not accepting", fmt.Sprintf(rejectingLine, usbQueue), false},
		{"other queue only", fmt.Sprintf(acceptingLine, netQueue), false},
		{"empty", "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := parseQueueAccepting(testCase.output, usbQueue); got != testCase.want {
				t.Fatalf("parseQueueAccepting(%q) = %v, want %v",
					testCase.output, got, testCase.want)
			}
		})
	}

	// A `cupsreject`ed queue still reads enabled, so only the separate accepting
	// check keeps it out of the healthy set.
	fake := twoPrinterCUPS(stateRejecting, stateEnabled)
	cups := newCUPSClient(fake)
	enabled, err := cups.queueEnabled(context.Background(), usbQueue)
	if err != nil {
		t.Fatalf("queueEnabled: %v", err)
	}
	if !enabled {
		t.Fatal("rejecting queue should still read enabled to lpstat -p")
	}
	health := newHealthChecker(cups)
	health.ttl = 0
	if health.healthy(context.Background(), usbQueue) {
		t.Fatal("a queue that is not accepting requests must be unhealthy")
	}
	if !health.healthy(context.Background(), netQueue) {
		t.Fatal("an enabled, accepting queue must be healthy")
	}
}

// @lat: [[tests#Agent Core#Device URI Discovery And Classification]]
func TestDeviceURIDiscoveryAndClassification(t *testing.T) {
	output := fmt.Sprintf(deviceLine, netQueue, netURI) +
		fmt.Sprintf(deviceLine, usbQueue, usbURI)
	devices := parseDeviceURIs(output)
	want := []queueDevice{{Queue: netQueue, URI: netURI}, {Queue: usbQueue, URI: usbURI}}
	if len(devices) != len(want) {
		t.Fatalf("parseDeviceURIs = %v, want %v", devices, want)
	}
	for index, device := range devices {
		if device != want[index] {
			t.Fatalf("device %d = %v, want %v", index, device, want[index])
		}
	}

	// `lpstat -v` carries no direct/network class word (that only exists in
	// `lpinfo -v`), so classification keys on the scheme alone.
	schemes := map[string]string{
		usbURI:                          connectionUSB,
		"usb://Zebra/ZD621":             connectionUSB,
		netURI:                          connectionNetwork,
		"ipp://printer.local/ipp/print": connectionNetwork,
		"ipps://printer.local/ipp":      connectionNetwork,
		"dnssd://Zebra%20ZD621._pdl-datastream._tcp.local/": connectionNetwork,
		"lpd://192.0.2.9/queue":                             connectionNetwork,
		"smb://server/zebra":                                connectionNetwork,
		"http://192.0.2.9:631/printers/z":                   connectionNetwork,
		"https://192.0.2.9:631/printers/z":                  connectionNetwork,
		"":                                                  connectionNetwork,
	}
	for uri, want := range schemes {
		if got := classifyConnection(uri); got != want {
			t.Fatalf("classifyConnection(%q) = %q, want %q", uri, got, want)
		}
	}

	// Discovery orders USB ahead of network regardless of CUPS's own order.
	printers, err := discoverPrinters(
		context.Background(), newCUPSClient(twoPrinterCUPS(stateEnabled, stateEnabled)))
	if err != nil {
		t.Fatalf("discoverPrinters: %v", err)
	}
	if len(printers) != 2 || printers[0].Queue != usbQueue || printers[1].Queue != netQueue {
		t.Fatalf("discovery order = %v, want USB first", printers)
	}
}

// @lat: [[tests#Agent Core#Stable Uid Hashes The Raw Device URI]]
func TestStableUIDHashesTheRawDeviceURI(t *testing.T) {
	uid := stableUID(usbQueue, usbURI)
	if uid == "" || uid == usbQueue {
		t.Fatalf("uid for a serial-bearing URI = %q, want a hash", uid)
	}
	if uid != stableUID(usbQueue, usbURI) {
		t.Fatal("uid must be deterministic across calls")
	}
	// The queue name must not leak into the uid: a rename keeps the identity.
	if uid != stableUID("renamed-queue", usbURI) {
		t.Fatal("uid must survive a queue rename")
	}
	// The RAW bytes are hashed: percent-decoding would silently shift every uid
	// on the station and orphan the SPA's pinned printer.
	decoded := "usb://Zebra Technologies/ZTC ZD621-300dpi ZPL?serial=" + usbSerial
	if uid == stableUID(usbQueue, decoded) {
		t.Fatal("uid must hash the raw, percent-encoded URI, not a decoded form")
	}
	// Two identical units differ only by serial — in the final character, the
	// same position a real pair of consecutively-manufactured units differs in —
	// and must not collide.
	other := strings.Replace(usbURI, usbSerial, "SN0000000002", 1)
	if uid == stableUID(usbQueue, other) {
		t.Fatal("two units with different serials must get different uids")
	}
	// A URI with no serial falls back to the queue name.
	if got := stableUID(netQueue, netURI); got != netQueue {
		t.Fatalf("uid for a serial-less URI = %q, want the queue name %q", got, netQueue)
	}
	if got := stableUID(netQueue, ""); got != netQueue {
		t.Fatalf("uid with no URI = %q, want the queue name %q", got, netQueue)
	}
}

// @lat: [[tests#Agent Core#Available Lists Healthy Printers USB First]]
func TestAvailableListsHealthyPrintersUSBFirst(t *testing.T) {
	base, _ := startAgent(t, twoPrinterCUPS(stateEnabled, stateEnabled), nil)

	status, body := getPath(t, base, "/available")
	if status != http.StatusOK {
		t.Fatalf("/available status = %d, want 200", status)
	}
	var payload struct {
		Printer []wireDevice `json:"printer"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode /available %q: %v", body, err)
	}
	if len(payload.Printer) != 2 {
		t.Fatalf("/available printers = %d, want 2", len(payload.Printer))
	}
	if payload.Printer[0].Name != usbQueue || payload.Printer[0].Connection != connectionUSB {
		t.Fatalf("first printer = %+v, want the USB queue", payload.Printer[0])
	}
	if payload.Printer[1].Connection != connectionNetwork {
		t.Fatalf("second printer = %+v, want the network queue", payload.Printer[1])
	}
	if payload.Printer[0].Provider != deviceProvider ||
		payload.Printer[0].DeviceType != deviceTypePrinter {
		t.Fatalf("device shape = %+v, want the frozen wire fields", payload.Printer[0])
	}

	// A disabled printer and a rejecting printer are both simply absent, so the
	// SPA can never pin a printer that cannot print.
	for _, state := range []string{stateDisabled, stateRejecting} {
		base, _ := startAgent(t, twoPrinterCUPS(state, stateEnabled), nil)
		_, body := getPath(t, base, "/available")
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("decode /available %q: %v", body, err)
		}
		if len(payload.Printer) != 1 || payload.Printer[0].Name != netQueue {
			t.Fatalf("with USB %s, /available = %+v, want only the network queue",
				state, payload.Printer)
		}
	}

	// No usable printer degrades exactly like the real agent with none attached.
	base, _ = startAgent(t, twoPrinterCUPS(stateDisabled, stateDisabled), nil)
	_, body = getPath(t, base, "/available")
	if strings.TrimSpace(body) != `{"printer":[]}` {
		t.Fatalf("/available with nothing healthy = %q, want an empty list", body)
	}
}

// @lat: [[tests#Agent Core#Default Returns Empty Body Without A Printer]]
func TestDefaultReturnsEmptyBodyWithoutAPrinter(t *testing.T) {
	base, _ := startAgent(t, twoPrinterCUPS(stateDisabled, stateEnabled), nil)

	status, body := getPath(t, base, "/default?type=printer")
	if status != http.StatusOK {
		t.Fatalf("/default status = %d, want 200", status)
	}
	var device wireDevice
	if err := json.Unmarshal([]byte(body), &device); err != nil {
		t.Fatalf("decode /default %q: %v", body, err)
	}
	if device.Name != netQueue || device.UID != netQueue {
		t.Fatalf("/default = %+v, want the healthy network queue", device)
	}

	// Empty BODY, not an empty object: the transport reads empty text as null.
	base, _ = startAgent(t, twoPrinterCUPS(stateDisabled, stateDisabled), nil)
	status, body = getPath(t, base, "/default?type=printer")
	if status != http.StatusOK || body != "" {
		t.Fatalf("/default with nothing healthy = %d %q, want 200 and an empty body",
			status, body)
	}
}

// @lat: [[tests#Agent Core#Write Fails Over From Dead USB To Network]]
func TestWriteFailsOverFromDeadUSBToNetwork(t *testing.T) {
	fake := twoPrinterCUPS(stateDisabled, stateEnabled)
	base, logs := startAgent(t, fake, nil)
	pinnedUID := stableUID(usbQueue, usbURI)

	status, body := postWrite(t, base, "https://lab.example", map[string]any{
		"device": map[string]any{"uid": pinnedUID, "name": usbQueue},
		"data":   "^XA^FO50,50^A0N,40,40^FDLABEL^FS^XZ",
	})
	if status != http.StatusOK {
		t.Fatalf("/write status = %d body %q, want 200 via failover", status, body)
	}

	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("lp calls = %d, want exactly one on the fallback queue", len(calls))
	}
	if len(calls[0].Args) < 2 || calls[0].Args[0] != "-d" || calls[0].Args[1] != netQueue {
		t.Fatalf("lp argv = %v, want the network queue", calls[0].Args)
	}
	// The skipped device is named out loud — a silent failover is the failure
	// mode this agent exists to kill.
	if !strings.Contains(logs.String(), "write fallback requested="+pinnedUID) {
		t.Fatalf("logs = %q, want an explicit fallback line", logs.String())
	}

	// A healthy pinned device prints on itself with no fallback line.
	fake = twoPrinterCUPS(stateEnabled, stateEnabled)
	base, logs = startAgent(t, fake, nil)
	status, body = postWrite(t, base, "", map[string]any{
		"device": map[string]any{"uid": pinnedUID, "name": usbQueue},
		"data":   "^XA^XZ",
	})
	if status != http.StatusOK {
		t.Fatalf("/write status = %d body %q, want 200", status, body)
	}
	calls = fake.calls()
	if len(calls) != 1 || calls[0].Args[1] != usbQueue {
		t.Fatalf("lp argv = %v, want the pinned USB queue", calls)
	}
	if strings.Contains(logs.String(), "fallback") {
		t.Fatalf("logs = %q, want no fallback line for a healthy pin", logs.String())
	}
}

// @lat: [[tests#Agent Core#Write Fails Loudly With No Healthy Printer]]
func TestWriteFailsLoudlyWithNoHealthyPrinter(t *testing.T) {
	fake := twoPrinterCUPS(stateDisabled, stateRejecting)
	base, _ := startAgent(t, fake, nil)

	status, body := postWrite(t, base, "", map[string]any{
		"device": map[string]any{"uid": stableUID(usbQueue, usbURI), "name": usbQueue},
		"data":   "^XA^XZ",
	})
	if status < 400 {
		t.Fatalf("/write status = %d, want a non-2xx when nothing can print", status)
	}
	if !strings.Contains(body, "healthy") && !strings.Contains(body, "usable") {
		t.Fatalf("/write body = %q, want a plain-text reason", body)
	}
	if calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("lp calls = %v, want none — a dead station must not spool", calls)
	}

	// A body with no 'data' is a 400 before any CUPS work.
	status, body = postWrite(t, base, "", map[string]any{"device": map[string]any{}})
	if status != http.StatusBadRequest || !strings.Contains(body, "data") {
		t.Fatalf("/write without data = %d %q, want 400 naming 'data'", status, body)
	}
}

// @lat: [[tests#Agent Core#Write Streams Large Payloads Verbatim]]
func TestWriteStreamsLargePayloadsVerbatim(t *testing.T) {
	fake := twoPrinterCUPS(stateEnabled, stateEnabled)
	base, _ := startAgent(t, fake, nil)

	// A 4x6 label is ~540 KB of uncompressed ^GFA hex.
	payload := "^XA^GFA,540000,540000,100," + strings.Repeat("F0", 270000) + "^FS^XZ"
	status, body := postWrite(t, base, "", map[string]any{"data": payload})
	if status != http.StatusOK {
		t.Fatalf("/write status = %d body %q, want 200", status, body)
	}

	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("lp calls = %d, want one", len(calls))
	}
	if string(calls[0].Payload) != payload {
		t.Fatalf("spooled %d bytes, want the %d-byte payload verbatim",
			len(calls[0].Payload), len(payload))
	}
	// `-o raw` is load-bearing: without it the zebra.ppd filter rasterizes the
	// ZPL into a bitmap and the label prints as a picture of itself.
	if !strings.Contains(strings.Join(calls[0].Args, " "), "-o raw") {
		t.Fatalf("lp argv = %v, want -o raw on every invocation", calls[0].Args)
	}
}

// @lat: [[tests#Agent Core#Print PDF Spools A Document Without Raw]]
func TestPrintPDFSpoolsADocumentWithoutRaw(t *testing.T) {
	fake := twoPrinterCUPS(stateEnabled, stateEnabled)
	base, logs := startAgent(t, fake, nil)

	status, body := postPrintPDF(t, base, "https://lab.example", map[string]any{
		"device": map[string]any{"uid": stableUID(usbQueue, usbURI), "name": usbQueue},
		"data":   base64.StdEncoding.EncodeToString(samplePDF),
	})
	if status != http.StatusOK || body != "" {
		t.Fatalf("/print-pdf = %d %q, want 200 and an empty body", status, body)
	}

	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("lp calls = %d, want exactly one", len(calls))
	}
	argv := calls[0].Args
	if len(argv) < 3 || argv[0] != "-d" || argv[1] != usbQueue {
		t.Fatalf("lp argv = %v, want -d <queue> … <file>", argv)
	}

	// THE invariant of this route, asserted before anything else about the argv
	// so a regression reports the reason rather than a shape mismatch. `/write`
	// must carry `-o raw` so the zebra.ppd filter cannot rasterize ZPL;
	// `/print-pdf` must NOT, because a PDF is not printer-native and raw bytes
	// would reach the device unrendered. Neither failure errors — both silently
	// print the wrong thing — so the option is asserted in both directions.
	for _, arg := range argv[:len(argv)-1] { // every argument except the spool path
		if arg == "-o" || arg == "raw" {
			t.Fatalf("lp argv = %v, want NO -o raw: a PDF must go through the CUPS "+
				"filter chain or it reaches the device unrendered and prints as garbage",
				argv)
		}
	}
	if len(argv) != 3 {
		t.Fatalf("lp argv = %v, want exactly -d <queue> <file>", argv)
	}
	if !strings.HasSuffix(argv[2], ".pdf") {
		t.Fatalf("spool file = %q, want a .pdf suffix", argv[2])
	}
	if !bytes.Equal(calls[0].Payload, samplePDF) {
		t.Fatalf("spooled %d bytes, want the %d-byte decoded PDF verbatim",
			len(calls[0].Payload), len(samplePDF))
	}
	if !strings.Contains(logs.String(), "print-pdf ok bytes=") ||
		!strings.Contains(logs.String(), "origin=https://lab.example") {
		t.Fatalf("logs = %q, want an audited print-pdf job line", logs.String())
	}

	// The sibling route is unchanged and still raw, so the two are proven apart
	// rather than merely proven individually.
	rawFake := twoPrinterCUPS(stateEnabled, stateEnabled)
	rawBase, _ := startAgent(t, rawFake, nil)
	if status, body := postWrite(t, rawBase, "", map[string]any{"data": "^XA^XZ"}); status != 200 {
		t.Fatalf("/write = %d %q, want 200", status, body)
	}
	if got := strings.Join(rawFake.calls()[0].Args, " "); !strings.Contains(got, "-o raw") {
		t.Fatalf("/write lp argv = %q, want -o raw still present", got)
	}
}

// @lat: [[tests#Agent Core#Print PDF Rejects A Payload That Is Not A PDF]]
func TestPrintPDFRejectsAPayloadThatIsNotAPDF(t *testing.T) {
	cases := []struct {
		name string
		data any
		want string
	}{
		{"undecodable base64", "not!valid!base64!", "base64"},
		{"decodes but is not a PDF", base64.StdEncoding.EncodeToString(
			[]byte("<html>a rendering bug produced this</html>")), pdfMagic},
		{"empty payload", "", pdfMagic},
		{"no data field", nil, "data"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := twoPrinterCUPS(stateEnabled, stateEnabled)
			base, _ := startAgent(t, fake, nil)

			body := map[string]any{"device": map[string]any{"name": usbQueue}}
			if testCase.data != nil {
				body["data"] = testCase.data
			}
			status, text := postPrintPDF(t, base, "", body)
			if status != http.StatusBadRequest {
				t.Fatalf("/print-pdf = %d %q, want 400", status, text)
			}
			if !strings.Contains(text, testCase.want) {
				t.Fatalf("/print-pdf body = %q, want plain text naming %q",
					text, testCase.want)
			}
			// Validation happens before any CUPS work: a bad payload must never
			// reach a printer, because CUPS would accept it and put garbage on
			// media rather than fail.
			if calls := fake.calls(); len(calls) != 0 {
				t.Fatalf("lp calls = %v, want none for a rejected payload", calls)
			}
		})
	}
}

// @lat: [[tests#Agent Core#Print PDF Caps The Body At Fifty Megabytes]]
func TestPrintPDFCapsTheBodyAtFiftyMegabytes(t *testing.T) {
	fake := twoPrinterCUPS(stateEnabled, stateEnabled)
	base, _ := startAgent(t, fake, nil)

	// Just past the cap once the JSON envelope is counted, so the limit is
	// exercised at its edge rather than with an arbitrarily huge body. Streamed
	// rather than concatenated so the fixture does not need a second 50 MB copy.
	oversized := io.MultiReader(
		strings.NewReader(`{"data":"`),
		strings.NewReader(strings.Repeat("A", maxPDFBody)),
		strings.NewReader(`"}`),
	)
	response := postRaw(t, base, "/print-pdf", "", oversized)
	defer response.Body.Close()
	payload := &bytes.Buffer{}
	payload.ReadFrom(response.Body)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("/print-pdf oversize = %d %q, want 413",
			response.StatusCode, payload.String())
	}
	if !strings.Contains(payload.String(), "too large") {
		t.Fatalf("413 body = %q, want a plain-text reason", payload.String())
	}
	// The cap is enforced before decoding, so an abusive body never gets spooled.
	if calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("lp calls = %v, want none for an oversize body", calls)
	}

	// A document comfortably inside the cap still prints.
	status, body := postPrintPDF(t, base, "", map[string]any{
		"data": base64.StdEncoding.EncodeToString(samplePDF),
	})
	if status != http.StatusOK {
		t.Fatalf("/print-pdf under the cap = %d %q, want 200", status, body)
	}
}

// @lat: [[tests#Agent Core#Print PDF Shares Write Failover And Origin Gating]]
func TestPrintPDFSharesWriteFailoverAndOriginGating(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(samplePDF)
	pinnedUID := stableUID(usbQueue, usbURI)

	// Failover: the pinned USB printer died between listing and printing, so the
	// sheet goes to the healthy network printer and the skip is named out loud —
	// identical to /write, because it is literally the same resolveTarget call.
	fake := twoPrinterCUPS(stateDisabled, stateEnabled)
	base, logs := startAgent(t, fake, nil)
	status, body := postPrintPDF(t, base, "", map[string]any{
		"device": map[string]any{"uid": pinnedUID, "name": usbQueue},
		"data":   encoded,
	})
	if status != http.StatusOK {
		t.Fatalf("/print-pdf = %d %q, want 200 via failover", status, body)
	}
	calls := fake.calls()
	if len(calls) != 1 || calls[0].Args[1] != netQueue {
		t.Fatalf("lp argv = %v, want the network queue", calls)
	}
	if !strings.Contains(logs.String(), "print-pdf fallback requested="+pinnedUID) {
		t.Fatalf("logs = %q, want an explicit print-pdf fallback line", logs.String())
	}

	// No healthy printer fails exactly as loudly as /write does: a plain-text
	// non-2xx and no lp call at all, never a phantom "Sent".
	dead := twoPrinterCUPS(stateDisabled, stateRejecting)
	deadBase, _ := startAgent(t, dead, nil)
	status, body = postPrintPDF(t, deadBase, "", map[string]any{"data": encoded})
	if status < 400 {
		t.Fatalf("/print-pdf on a dead station = %d, want a non-2xx", status)
	}
	if !strings.Contains(body, "usable") && !strings.Contains(body, "healthy") {
		t.Fatalf("/print-pdf body = %q, want a plain-text reason", body)
	}
	if calls := dead.calls(); len(calls) != 0 {
		t.Fatalf("lp calls = %v, want none — a dead station must not spool", calls)
	}

	// Origin gate: a write-capable route that skipped the allowlist would let a
	// hostile page print by posting a PDF instead of ZPL, so /print-pdf is gated
	// exactly as /write is and rejects before any CUPS work.
	gated := twoPrinterCUPS(stateEnabled, stateEnabled)
	gatedBase, gatedLogs := startAgent(t, gated, []string{"https://lab.example"})
	job := map[string]any{"data": encoded}
	status, body = postPrintPDF(t, gatedBase, "https://evil.example", job)
	if status != http.StatusForbidden {
		t.Fatalf("/print-pdf from a disallowed origin = %d %q, want 403", status, body)
	}
	if !strings.Contains(body, "evil.example") {
		t.Fatalf("rejection body = %q, want a plain-text reason naming the origin", body)
	}
	if calls := gated.calls(); len(calls) != 0 {
		t.Fatalf("lp calls = %v, want none for a rejected origin", calls)
	}
	if !strings.Contains(gatedLogs.String(), "print-pdf REJECTED reason=origin-not-allowed") {
		t.Fatalf("logs = %q, want the print-pdf rejection recorded", gatedLogs.String())
	}
	// A request with no Origin header is on no allowlist either.
	if status, _ = postPrintPDF(t, gatedBase, "", job); status != http.StatusForbidden {
		t.Fatalf("/print-pdf with no Origin under an allowlist = %d, want 403", status)
	}
	// The allowed origin prints normally.
	if status, body = postPrintPDF(t, gatedBase, "https://lab.example", job); status != 200 {
		t.Fatalf("/print-pdf from an allowed origin = %d %q, want 200", status, body)
	}

	// CORS and the version header behave on the new route exactly as everywhere
	// else, error paths included — a browser that cannot read the 400 sees an
	// opaque network failure instead of the reason.
	open := twoPrinterCUPS(stateEnabled, stateEnabled)
	openBase, _ := startAgent(t, open, nil)
	bad := postRaw(t, openBase, "/print-pdf", "https://lab.example",
		strings.NewReader(`{"data":"@@@"}`))
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("/print-pdf bad base64 = %d, want 400", bad.StatusCode)
	}
	if got := bad.Header.Get("Access-Control-Allow-Origin"); got != "https://lab.example" {
		t.Fatalf("Allow-Origin on the 400 = %q, want the echoed origin", got)
	}
	if got := bad.Header.Get(versionHeader); got != version {
		t.Fatalf("%s on the 400 = %q, want %q", versionHeader, got, version)
	}

	preflight, err := http.NewRequest(http.MethodOptions, openBase+"/print-pdf", nil)
	if err != nil {
		t.Fatalf("build preflight: %v", err)
	}
	preflight.Header.Set("Origin", "https://lab.example")
	response, err := http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatalf("preflight /print-pdf: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("/print-pdf preflight = %d, want 204", response.StatusCode)
	}
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "https://lab.example" {
		t.Fatalf("preflight Allow-Origin = %q, want the echoed origin", got)
	}
	if got := response.Header.Get(versionHeader); got != version {
		t.Fatalf("preflight %s = %q, want %q", versionHeader, got, version)
	}
}

// @lat: [[tests#Agent Core#Origin Posture Gates Write]]
func TestOriginPostureGatesWrite(t *testing.T) {
	job := map[string]any{"data": "^XA^XZ"}

	// Allowlist configured: a disallowed Origin is rejected before any lp call.
	fake := twoPrinterCUPS(stateEnabled, stateEnabled)
	base, logs := startAgent(t, fake, []string{"https://lab.example"})
	status, body := postWrite(t, base, "https://evil.example", job)
	if status < 400 {
		t.Fatalf("/write from a disallowed origin = %d, want non-2xx", status)
	}
	if !strings.Contains(body, "evil.example") {
		t.Fatalf("rejection body = %q, want a plain-text reason naming the origin", body)
	}
	if calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("lp calls = %v, want none for a rejected origin", calls)
	}
	if !strings.Contains(logs.String(), "origin-not-allowed") ||
		!strings.Contains(logs.String(), "https://evil.example") {
		t.Fatalf("logs = %q, want the rejection recorded", logs.String())
	}

	// The allowed origin still prints normally.
	status, _ = postWrite(t, base, "https://lab.example", job)
	if status != http.StatusOK {
		t.Fatalf("/write from an allowed origin = %d, want 200", status)
	}
	if len(fake.calls()) != 1 {
		t.Fatalf("lp calls = %v, want one for the allowed origin", fake.calls())
	}

	// Default (unconfigured) posture: the same request is logged and ALLOWED.
	fake = twoPrinterCUPS(stateEnabled, stateEnabled)
	base, logs = startAgent(t, fake, nil)
	status, body = postWrite(t, base, "https://evil.example", job)
	if status != http.StatusOK {
		t.Fatalf("/write in default posture = %d body %q, want 200", status, body)
	}
	if len(fake.calls()) != 1 {
		t.Fatalf("lp calls = %v, want one in default posture", fake.calls())
	}
	if !strings.Contains(logs.String(), "origin=https://evil.example") {
		t.Fatalf("logs = %q, want every origin recorded in default posture",
			logs.String())
	}

	if got := parseOriginAllow(" https://a , https://b ,, https://a "); len(got) != 2 ||
		got[0] != "https://a" || got[1] != "https://b" {
		t.Fatalf("parseOriginAllow = %v, want an ordered, de-duplicated list", got)
	}
}

// @lat: [[tests#Agent Core#CORS Echoes Origin And Answers Preflight]]
func TestCORSEchoesOriginAndAnswersPreflight(t *testing.T) {
	base, _ := startAgent(t, twoPrinterCUPS(stateEnabled, stateEnabled), nil)

	request, err := http.NewRequest(http.MethodOptions, base+"/write", nil)
	if err != nil {
		t.Fatalf("build preflight: %v", err)
	}
	request.Header.Set("Origin", "https://lab.example")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", response.StatusCode)
	}
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "https://lab.example" {
		t.Fatalf("Allow-Origin = %q, want the echoed origin", got)
	}
	if got := response.Header.Get("Access-Control-Allow-Methods"); got != "GET, POST, OPTIONS" {
		t.Fatalf("Allow-Methods = %q", got)
	}
	if got := response.Header.Get("Access-Control-Allow-Headers"); got != "Content-Type" {
		t.Fatalf("Allow-Headers = %q", got)
	}

	// With no Origin header the wildcard is sent instead.
	response, err = http.Get(base + "/available")
	if err != nil {
		t.Fatalf("get /available: %v", err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin without an Origin header = %q, want *", got)
	}
}

// @lat: [[tests#Agent Core#Read And Unknown Routes]]
func TestReadAndUnknownRoutes(t *testing.T) {
	base, _ := startAgent(t, twoPrinterCUPS(stateEnabled, stateEnabled), nil)

	response, err := http.Post(base+"/read", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post /read: %v", err)
	}
	defer response.Body.Close()
	payload := &bytes.Buffer{}
	payload.ReadFrom(response.Body)
	if response.StatusCode != http.StatusOK || payload.String() != "" {
		t.Fatalf("/read = %d %q, want 200 and an empty body",
			response.StatusCode, payload.String())
	}

	status, body := getPath(t, base, "/nope")
	if status != http.StatusNotFound || !strings.Contains(body, "not found") {
		t.Fatalf("/nope = %d %q, want a plain-text 404", status, body)
	}
}

// @lat: [[tests#Agent Core#Config Resolves Flags Over Environment]]
func TestConfigResolvesFlagsOverEnvironment(t *testing.T) {
	env := map[string]string{
		"HOME":            "/Users/station",
		portEnvVar:        "9200",
		httpsPortEnvVar:   "9201",
		bindEnvVar:        "127.0.0.2",
		originAllowEnvVar: "https://env.example",
	}
	lookup := func(name string) string { return env[name] }

	fromEnv, err := parseConfig(nil, lookup, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if fromEnv.Port != 9200 || fromEnv.HTTPSPort != 9201 || fromEnv.Bind != "127.0.0.2" {
		t.Fatalf("env config = %+v, want the environment overrides", fromEnv)
	}
	if len(fromEnv.OriginAllow) != 1 || fromEnv.OriginAllow[0] != "https://env.example" {
		t.Fatalf("env allowlist = %v", fromEnv.OriginAllow)
	}

	fromFlags, err := parseConfig(
		[]string{"--port", "9300", "--origin-allow", "https://flag.example"},
		lookup, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseConfig with flags: %v", err)
	}
	if fromFlags.Port != 9300 {
		t.Fatalf("flag port = %d, want the flag to win over the environment",
			fromFlags.Port)
	}
	if len(fromFlags.OriginAllow) != 1 || fromFlags.OriginAllow[0] != "https://flag.example" {
		t.Fatalf("flag allowlist = %v", fromFlags.OriginAllow)
	}

	// Defaults are loopback and the two ports the frozen transport probes, and
	// the cert pair lives under the per-user Application Support path.
	bare, err := parseConfig(nil, func(name string) string {
		if name == "HOME" {
			return "/Users/station"
		}
		return ""
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseConfig defaults: %v", err)
	}
	if bare.Bind != defaultBind || bare.Port != defaultPort ||
		bare.HTTPSPort != defaultHTTPSPort {
		t.Fatalf("defaults = %+v", bare)
	}
	if len(bare.OriginAllow) != 0 {
		t.Fatalf("default allowlist = %v, want log-and-allow", bare.OriginAllow)
	}
	// The product directory name is asserted by shape, not by literal: the pair
	// must land side by side under the operator's per-user Application Support
	// path with the two filenames the HTTPS listener gates on. Baking the
	// product name in here would make the fixture a second source of truth for
	// it and would carry a station-specific path into the repo.
	certFile, keyFile := bare.certPaths()
	wantParent := "/Users/station/Library/Application Support"
	if filepath.Dir(filepath.Dir(certFile)) != wantParent ||
		filepath.Base(certFile) != certFileName ||
		filepath.Base(keyFile) != keyFileName ||
		filepath.Dir(keyFile) != filepath.Dir(certFile) {
		t.Fatalf("cert paths = %q %q, want %s/<product>/{%s,%s}",
			certFile, keyFile, wantParent, certFileName, keyFileName)
	}
}

// @lat: [[tests#Agent Core#Version Surfaces On Health And Every Response]]
func TestVersionSurfacesOnHealthAndEveryResponse(t *testing.T) {
	// The release workflow injects this exact package variable with
	// `-ldflags "-X main.version=X.Y.Z"`, so overriding it here pins the same
	// seam the linker writes to.
	original := version
	version = "9.9.9-test"
	t.Cleanup(func() { version = original })

	fake := twoPrinterCUPS(stateEnabled, stateDisabled)
	base, _ := startAgent(t, fake, []string{"https://lab.example"})

	// Every response carries the header, including the ones that are not
	// successes: a 404 is exactly the response a misrouted station produces.
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/available"},
		{http.MethodGet, "/default"},
		{http.MethodGet, "/health"},
		{http.MethodGet, "/nope"},
		{http.MethodOptions, "/write"},
	} {
		request, err := http.NewRequest(probe.method, base+probe.path, nil)
		if err != nil {
			t.Fatalf("build %s %s: %v", probe.method, probe.path, err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", probe.method, probe.path, err)
		}
		response.Body.Close()
		if got := response.Header.Get(versionHeader); got != version {
			t.Fatalf("%s %s %s = %q, want %q",
				probe.method, probe.path, versionHeader, got, version)
		}
	}

	status, body := getPath(t, base, "/health")
	if status != http.StatusOK {
		t.Fatalf("/health = %d %q, want 200", status, body)
	}
	var report healthReport
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("decode /health: %v (body %q)", err, body)
	}
	if report.Version != version {
		t.Fatalf("/health version = %q, want %q", report.Version, version)
	}
	if report.OriginPosture != postureAllowlist ||
		len(report.OriginAllow) != 1 || report.OriginAllow[0] != "https://lab.example" {
		t.Fatalf("/health origin posture = %q %v",
			report.OriginPosture, report.OriginAllow)
	}
	if report.CUPSError != "" {
		t.Fatalf("/health cupsError = %q, want none", report.CUPSError)
	}

	// Diagnostics reports EVERY discovered queue with its verdict — the disabled
	// one included, which is the whole reason this endpoint exists. /available
	// hides it so the SPA cannot pin it.
	verdicts := map[string]bool{}
	for _, listed := range report.Printers {
		verdicts[listed.Queue] = listed.Healthy
	}
	if len(report.Printers) != 2 || !verdicts[usbQueue] || verdicts[netQueue] {
		t.Fatalf("/health printers = %+v, want both queues with only %s healthy",
			report.Printers, usbQueue)
	}
	if report.Printers[0].Queue != usbQueue ||
		report.Printers[0].DeviceURI != usbURI ||
		report.Printers[0].Connection != connectionUSB {
		t.Fatalf("/health first printer = %+v, want the USB queue first",
			report.Printers[0])
	}

	// A station whose CUPS is broken still gets a version back, because that is
	// the answer triage actually needs.
	broken := &fakeCUPS{states: map[string]string{}, lpExit: 1}
	broken.devices = nil
	brokenBase, _ := startAgent(t, broken, nil)
	status, body = getPath(t, brokenBase, "/health")
	if status != http.StatusOK {
		t.Fatalf("/health with no queues = %d %q, want 200", status, body)
	}
	var empty healthReport
	if err := json.Unmarshal([]byte(body), &empty); err != nil {
		t.Fatalf("decode /health: %v (body %q)", err, body)
	}
	if empty.Version != version || empty.OriginPosture != posturelogAndAllow ||
		len(empty.Printers) != 0 {
		t.Fatalf("/health with no queues = %+v", empty)
	}
}
