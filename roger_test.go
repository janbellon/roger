package main

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildChoices(t *testing.T) {
	info := &VideoInfo{Formats: []rawFormat{
		{Ext: "webm", Vcodec: "vp9", Acodec: "none", Height: 1080, Tbr: 2000, Filesize: 1000},
		{Ext: "mp4", Vcodec: "avc1", Acodec: "none", Height: 1080, Tbr: 900, Filesize: 800},
		{Ext: "mp4", Vcodec: "avc1", Acodec: "mp4a", Height: 360, Tbr: 400, Filesize: 300},
		{Ext: "m4a", Vcodec: "none", Acodec: "mp4a", Abr: 128, Filesize: 100},
		{Ext: "webm", Vcodec: "none", Acodec: "opus", Abr: 64, Filesize: 50},
	}}
	choices := buildChoices(info)
	if len(choices) != 3 {
		t.Fatalf("got %d choices: %+v", len(choices), choices)
	}
	if c := choices[0]; c.Label != "1080p" || c.Size != 900 {
		t.Errorf("1080p should prefer mp4/avc and add the audio size: %+v", c)
	}
	if c := choices[1]; c.Label != "360p" || c.Size != 300 {
		t.Errorf("a muxed format has no separate audio to add: %+v", c)
	}
	if c := choices[2]; c.Kind != "audio" || c.Size != 100 {
		t.Errorf("audio should use the best bitrate: %+v", c)
	}
}

func TestBuildChoicesFallsBackToBestAvailable(t *testing.T) {
	choices := buildChoices(&VideoInfo{})
	if len(choices) != 2 || choices[0].Label != "Best available" || choices[1].Kind != "audio" {
		t.Fatalf("got %+v", choices)
	}
}

func TestBuildChoicesCapsVideoHeights(t *testing.T) {
	info := &VideoInfo{}
	for height := 100; height <= 1200; height += 100 {
		info.Formats = append(info.Formats, rawFormat{Vcodec: "avc1", Height: float64(height)})
	}
	choices := buildChoices(info)
	if len(choices) != maxVideoChoices+1 || choices[0].Label != "1200p" {
		t.Fatalf("got %d choices, first %+v", len(choices), choices[0])
	}
}

func TestParseInfoUnwrapsPlaylists(t *testing.T) {
	raw, info, err := parseInfo([]byte(`{"_type":"playlist","title":"list","entries":[{"title":"first","formats":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "first" || strings.Contains(string(raw), "playlist") {
		t.Fatalf("got %q from %s", info.Title, raw)
	}
}

func TestOutputParser(t *testing.T) {
	var got []Progress
	processing := 0
	parser := outputParser{parts: 1, handlers: DownloadHandlers{
		OnProgress:   func(p Progress) { got = append(got, p) },
		OnProcessing: func() { processing++ },
	}}
	for _, line := range []string{
		"[info] abc: Downloading 1 format(s): 137+140",
		"ROGER|100|1000|NA|50.5|18",
		"ROGER|900|NA|1000.0|NA|NA",
		"ROGER|10|200|NA|5|NA",
		`[Merger] Merging formats into "/tmp/x/media.mp4"`,
		"/tmp/x/media.mp4",
	} {
		parser.line(line)
	}
	if len(got) != 3 || got[0].Parts != 2 || got[0].Total != 1000 || got[0].Speed != 50.5 {
		t.Fatalf("got %+v", got)
	}
	if got[1].Total != 1000 || got[1].Part != 0 {
		t.Errorf("estimate should fill in for an unknown total: %+v", got[1])
	}
	if got[2].Part != 1 {
		t.Errorf("a counter going backwards starts the next part: %+v", got[2])
	}
	if processing != 1 || parser.path != "/tmp/x/media.mp4" {
		t.Errorf("processing=%d path=%q", processing, parser.path)
	}
}

func TestOutputParserEnforcesSizeLimit(t *testing.T) {
	run := func(lines ...string) bool {
		stopped := false
		parser := outputParser{parts: 2, limit: 1000, stop: func() { stopped = true }, handlers: DownloadHandlers{
			OnProgress: func(Progress) {},
		}}
		for _, line := range lines {
			parser.line(line)
		}
		if stopped != parser.tooLarge {
			t.Errorf("stopped=%v tooLarge=%v", stopped, parser.tooLarge)
		}
		return parser.tooLarge
	}
	if !run("ROGER|10|5000|NA|1|1") {
		t.Error("a known total over the limit should stop at once")
	}
	if run("ROGER|10|NA|5000|1|1") {
		t.Error("an estimate alone should not stop the download")
	}
	if !run("ROGER|1001|NA|NA|1|1") {
		t.Error("fragmented streams should stop once the bytes pass the limit")
	}
	if !run("ROGER|800|800|NA|1|1", "ROGER|10|300|NA|1|1", "ROGER|201|300|NA|1|1") {
		t.Error("the limit covers video and audio together")
	}
	if run("ROGER|800|800|NA|1|1", "ROGER|10|200|NA|1|1") {
		t.Error("under the limit")
	}
}

func TestYtDlpError(t *testing.T) {
	err := ytDlpError("WARNING: x\nERROR: first\nERROR: [youtube] abc: Video unavailable\n", "fallback")
	if err.Error() != "abc: Video unavailable" {
		t.Errorf("got %q", err)
	}
	if err := ytDlpError("nothing useful", "fallback"); err.Error() != "fallback" {
		t.Errorf("got %q", err)
	}
}

func TestSafeFilename(t *testing.T) {
	for title, want := range map[string]string{
		"Plain title":         "Plain title.mp4",
		"../../etc/passwd":    "etc passwd.mp4",
		`a/b\c:d*e?f"g<h>i|j`: "a b c d e f g h i j.mp4",
		"line\r\nbreak":       "line break.mp4",
		"...":                 "video.mp4",
		"":                    "video.mp4",
		"日本語のタイトル":            "日本語のタイトル.mp4",
	} {
		if got := safeFilename(title, "mp4"); got != want {
			t.Errorf("safeFilename(%q) = %q, want %q", title, got, want)
		}
	}
	if got := safeFilename(strings.Repeat("é", 500), "mp4"); len([]rune(got)) != 124 {
		t.Errorf("long title not capped: %d runes", len([]rune(got)))
	}
}

func fakeResolver(addrs ...string) resolver {
	return func(context.Context, string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, addr := range addrs {
			out = append(out, netip.MustParseAddr(addr))
		}
		return out, nil
	}
}

func TestCheckURL(t *testing.T) {
	ctx := context.Background()
	public := fakeResolver("93.184.216.34")
	for _, raw := range []string{"", "not a url", "ftp://example.com/x", "file:///etc/passwd", "--exec=id", "https://", "https://x.com/" + strings.Repeat("a", maxURLLength)} {
		if _, err := checkURL(ctx, raw, false, public); !errors.Is(err, errBadURL) {
			t.Errorf("checkURL(%.40q) = %v", raw, err)
		}
	}
	if got, err := checkURL(ctx, "  https://example.com/v?id=1 ", false, public); err != nil || got != "https://example.com/v?id=1" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, addr := range []string{"127.0.0.1", "10.0.0.1", "172.17.0.2", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fd00::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1"} {
		if _, err := checkURL(ctx, "http://target.test/", false, fakeResolver(addr)); !errors.Is(err, errBlockedURL) {
			t.Errorf("%s should be blocked, got %v", addr, err)
		}
	}
	if _, err := checkURL(ctx, "http://x.test/", false, fakeResolver("93.184.216.34", "10.0.0.1")); !errors.Is(err, errBlockedURL) {
		t.Errorf("one private answer among public ones should block: %v", err)
	}
	if _, err := checkURL(ctx, "http://user:pass@example.com/", false, public); !errors.Is(err, errBlockedURL) {
		t.Errorf("userinfo should be blocked: %v", err)
	}
	if _, err := checkURL(ctx, "http://127.0.0.1/", true, public); err != nil {
		t.Errorf("allowPrivate should skip the address check: %v", err)
	}
	failing := func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	if _, err := checkURL(ctx, "http://2130706433/", false, failing); err == nil {
		t.Error("an unresolvable host must be refused")
	}
}

func TestPlatformLabel(t *testing.T) {
	for raw, want := range map[string]string{
		"https://www.youtube.com/watch?v=1": "YouTube",
		"https://youtu.be/1":                "YouTube",
		"https://m.tiktok.com/x":            "TikTok",
		"https://notyoutube.com/x":          "notyoutube.com",
		"https://www.example.org/x":         "example.org",
	} {
		if got := platformLabel(raw); got != want {
			t.Errorf("platformLabel(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	cfg, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8080 || cfg.MaxConcurrent != 2 || cfg.FileTTL != 15*time.Minute || cfg.YtDlpPath != "yt-dlp" {
		t.Errorf("defaults: %+v", cfg)
	}
	cfg, err = loadConfig(env(map[string]string{"ROGER_YTDLP_ARGS": "--proxy socks5://p:1080  --cookies /c.txt", "ROGER_MAX_FILESIZE": "1.5G"}))
	if err != nil || len(cfg.YtDlpArgs) != 4 {
		t.Errorf("args: %+v, %v", cfg.YtDlpArgs, err)
	}
	for raw, want := range map[string]int64{"": 0, "500": 500, "1K": 1024, "500M": 500 << 20, "1.5g": 3 << 29, "1T": 1 << 40} {
		if got, err := parseSize(raw); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v", raw, got, err)
		}
	}
	for key, value := range map[string]string{
		"ROGER_PORT":               "99999",
		"ROGER_MAX_CONCURRENT":     "0",
		"ROGER_FILE_TTL":           "soon",
		"ROGER_MAX_FILESIZE":       "lots",
		"ROGER_ALLOW_PRIVATE_URLS": "maybe",
	} {
		if _, err := loadConfig(env(map[string]string{key: value})); err == nil {
			t.Errorf("%s=%s should be rejected", key, value)
		}
	}
}

// fakeYtDlp stands in for the real binary so the whole HTTP flow runs offline.
const fakeYtDlp = `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    -J) echo '{"title":"My: Video","uploader":"me","duration":5,"thumbnail":"http://insecure/x.jpg","formats":[{"ext":"mp4","vcodec":"avc1","acodec":"mp4a","height":720,"filesize":4}]}'; exit 0;;
  esac
done
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift;;
    --load-info-json) if [ "$FAKE_FAIL_CACHED" = 1 ]; then echo 'ERROR: expired' >&2; exit 1; fi;;
  esac
  shift
done
[ "$FAKE_HANG" = 1 ] && exec sleep 60
echo '[info] x: Downloading 1 format(s): 22'
echo 'ROGER|2|4|NA|1|1'
path=$(echo "$out" | sed 's/%(ext)s/mp4/')
printf 'data' > "$path"
echo "$path"
`

func newTestServer(t *testing.T, password string) (*httptest.Server, *Manager) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(bin, []byte(fakeYtDlp), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{DataDir: dir, MaxConcurrent: 1, FileTTL: time.Minute, DownloadTimeout: time.Minute, Password: password}
	ctx, cancel := context.WithCancel(context.Background())
	manager, err := NewManager(ctx, cfg, &YtDlp{Bin: bin, CacheDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg, manager: manager, resolve: fakeResolver("93.184.216.34")}
	s.ytDlpVersion.Store("test")
	server := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		cancel()
		manager.wg.Wait()
		server.Close()
	})
	return server, manager
}

func post(t *testing.T, url, body string, into any) int {
	t.Helper()
	response, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if into != nil {
		json.NewDecoder(response.Body).Decode(into)
	}
	return response.StatusCode
}

func waitFor(t *testing.T, job *Job, done func(JobStatus) bool) JobStatus {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		status, changed := job.Watch()
		if done(status) {
			return status
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("timed out in state %+v", status)
		}
	}
}

func startJob(t *testing.T, server *httptest.Server, manager *Manager) *Job {
	t.Helper()
	var probe ProbeResult
	if code := post(t, server.URL+"/api/probe", `{"url":"https://example.com/v"}`, &probe); code != 200 {
		t.Fatalf("probe: %d", code)
	}
	var status JobStatus
	body := `{"probe_id":"` + probe.ID + `","choice":0}`
	if code := post(t, server.URL+"/api/downloads", body, &status); code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	return manager.Job(status.ID)
}

func TestDownloadReachesTheBrowserAsAnAttachment(t *testing.T) {
	server, manager := newTestServer(t, "")
	job := startJob(t, server, manager)
	status := waitFor(t, job, func(s JobStatus) bool { return s.State.finished() })
	if status.State != StateReady || status.Filename != "My Video.mp4" || status.Size != 4 {
		t.Fatalf("got %+v", status)
	}

	response, err := http.Get(server.URL + "/api/downloads/" + status.ID + "/file")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Content-Disposition"); got != `attachment; filename="My Video.mp4"` {
		t.Errorf("Content-Disposition: %q", got)
	}
	if got := response.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type: %q", got)
	}
}

func TestContentDispositionKeepsNonASCIINames(t *testing.T) {
	got := mime.FormatMediaType("attachment", map[string]string{"filename": safeFilename("日本語 🎬 video", "mp4")})
	if !strings.HasPrefix(got, "attachment; filename*=utf-8''") {
		t.Errorf("got %q", got)
	}
}

func TestProbeHidesInternalsAndInsecureThumbnails(t *testing.T) {
	server, _ := newTestServer(t, "")
	var raw map[string]any
	post(t, server.URL+"/api/probe", `{"url":"https://example.com/v"}`, &raw)
	if _, ok := raw["thumbnail"]; ok {
		t.Error("http thumbnail should be dropped")
	}
	encoded, _ := json.Marshal(raw)
	if strings.Contains(string(encoded), "merge-output-format") {
		t.Errorf("yt-dlp arguments leaked to the client: %s", encoded)
	}
}

func TestExpiredMediaLinksRetryWithFreshExtraction(t *testing.T) {
	t.Setenv("FAKE_FAIL_CACHED", "1")
	server, manager := newTestServer(t, "")
	status := waitFor(t, startJob(t, server, manager), func(s JobStatus) bool { return s.State.finished() })
	if status.State != StateReady || !status.Refreshing {
		t.Fatalf("got %+v", status)
	}
}

func TestCancelStopsTheProcessAndRemovesFiles(t *testing.T) {
	t.Setenv("FAKE_HANG", "1")
	server, manager := newTestServer(t, "")
	job := startJob(t, server, manager)
	waitFor(t, job, func(s JobStatus) bool { return s.State == StateDownloading })

	status, _ := job.Watch()
	request, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/downloads/"+status.ID, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	waitFor(t, job, func(s JobStatus) bool { return s.State == StateCancelled })
	if _, err := os.Stat(job.dir); !os.IsNotExist(err) {
		t.Errorf("job directory left behind: %v", err)
	}
}

func TestCleanupExpiresFilesAndAbandonedJobs(t *testing.T) {
	server, manager := newTestServer(t, "")
	job := startJob(t, server, manager)
	status := waitFor(t, job, func(s JobStatus) bool { return s.State.finished() })

	manager.Cleanup(time.Now())
	if manager.Job(status.ID) == nil {
		t.Fatal("removed too early")
	}
	manager.Cleanup(status.ExpiresAt.Add(time.Second))
	if manager.Job(status.ID) != nil {
		t.Error("job not removed after expiry")
	}
	if _, err := os.Stat(job.dir); !os.IsNotExist(err) {
		t.Errorf("file left behind: %v", err)
	}

	t.Setenv("FAKE_HANG", "1")
	hanging := startJob(t, server, manager)
	waitFor(t, hanging, func(s JobStatus) bool { return s.State == StateDownloading })
	manager.Cleanup(time.Now())
	if status, _ := hanging.Watch(); status.State != StateDownloading {
		t.Fatalf("a job within its grace period must keep running: %+v", status)
	}
	manager.Cleanup(time.Now().Add(abandonedAfter + time.Second))
	waitFor(t, hanging, func(s JobStatus) bool { return s.State == StateCancelled })
}

func TestUnknownProbeAndChoiceAreRejected(t *testing.T) {
	server, _ := newTestServer(t, "")
	if code := post(t, server.URL+"/api/downloads", `{"probe_id":"nope","choice":0}`, nil); code != 404 {
		t.Errorf("unknown probe: %d", code)
	}
	var probe ProbeResult
	post(t, server.URL+"/api/probe", `{"url":"https://example.com/v"}`, &probe)
	if code := post(t, server.URL+"/api/downloads", `{"probe_id":"`+probe.ID+`","choice":99}`, nil); code != 400 {
		t.Errorf("bad choice: %d", code)
	}
}

func TestPasswordProtectsEverythingButHealth(t *testing.T) {
	server, _ := newTestServer(t, "s3cret")
	get := func(path, password string) int {
		request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if password != "" {
			request.SetBasicAuth("roger", password)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	for path, want := range map[string]int{"/": 401, "/api/meta": 401, "/healthz": 200} {
		if got := get(path, ""); got != want {
			t.Errorf("%s without password: %d", path, got)
		}
	}
	if got := get("/api/meta", "wrong"); got != 401 {
		t.Errorf("wrong password: %d", got)
	}
	if got := get("/api/meta", "s3cret"); got != 200 {
		t.Errorf("right password: %d", got)
	}
}

func TestCrossOriginPostsAreRefused(t *testing.T) {
	server, _ := newTestServer(t, "")
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/probe", strings.NewReader(`{"url":"https://example.com/v"}`))
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("got %d", response.StatusCode)
	}
}
