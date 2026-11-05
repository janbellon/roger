package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const (
	maxVideoChoices = 8
	maxInfoJSON     = 64 << 20
	maxErrorOutput  = 64 << 10
	progressPrefix  = "ROGER|"
	progressFormat  = progressPrefix + "%(progress.downloaded_bytes)s|%(progress.total_bytes)s|%(progress.total_bytes_estimate)s|%(progress.speed)s|%(progress.eta)s"
)

var errTooLarge = errors.New("This file is larger than this server allows.")

// YtDlp runs the yt-dlp binary. Everything derived from a request reaches it
// either after "--" or through a server-built Choice, never as an option.
type YtDlp struct {
	Bin         string
	Ffmpeg      string
	CacheDir    string
	MaxFilesize int64
	Extra       []string
}

func (y *YtDlp) args(extra ...string) []string {
	args := []string{
		"--ignore-config", // a stray config could add -o or --exec behind our back
		"--no-warnings",
		"--no-playlist",
		"--playlist-items", "1",
		"--socket-timeout", "30",
		"--cache-dir", y.CacheDir,
	}
	if y.Ffmpeg != "" {
		args = append(args, "--ffmpeg-location", y.Ffmpeg)
	}
	args = append(args, y.Extra...)
	return append(args, extra...)
}

// command puts yt-dlp in its own process group so a cancel also stops the
// ffmpeg it spawned. SIGKILL is fine: job directories are deleted afterwards.
func command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func (y *YtDlp) Version(ctx context.Context) (string, error) {
	out, err := command(ctx, y.Bin, "--version").Output()
	return strings.TrimSpace(string(out)), err
}

// Update runs yt-dlp's own self-update, which checks the release's checksums.
func (y *YtDlp) Update(ctx context.Context) (string, error) {
	out, err := command(ctx, y.Bin, "--update").CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

type rawFormat struct {
	Ext            string  `json:"ext"`
	Vcodec         string  `json:"vcodec"`
	Acodec         string  `json:"acodec"`
	Height         float64 `json:"height"`
	Abr            float64 `json:"abr"`
	Tbr            float64 `json:"tbr"`
	Filesize       float64 `json:"filesize"`
	FilesizeApprox float64 `json:"filesize_approx"`
}

func (f rawFormat) size() float64 {
	if f.Filesize > 0 {
		return f.Filesize
	}
	return f.FilesizeApprox
}

type VideoInfo struct {
	Title     string      `json:"title"`
	Uploader  string      `json:"uploader"`
	Duration  float64     `json:"duration"`
	Thumbnail string      `json:"thumbnail"`
	IsLive    bool        `json:"is_live"`
	Formats   []rawFormat `json:"formats"`
}

// Choice is one line of the format picker. Args never leaves the server: the
// client picks by index, so it can't smuggle options into yt-dlp.
type Choice struct {
	Kind  string   `json:"kind"`
	Label string   `json:"label"`
	Ext   string   `json:"ext,omitempty"`
	Size  int64    `json:"size,omitempty"`
	Args  []string `json:"-"`
}

// Probe fetches a link's metadata and saves the raw JSON to infoPath, so the
// download can skip re-extraction with --load-info-json.
func (y *YtDlp) Probe(ctx context.Context, url, infoPath string) (*VideoInfo, error) {
	stdout := &limitedBuffer{limit: maxInfoJSON}
	stderr := &tailBuffer{limit: maxErrorOutput}
	cmd := command(ctx, y.Bin, y.args("-J", "--", url)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if stdout.overflowed {
			return nil, errors.New("This page returned too much data to process.")
		}
		return nil, ytDlpError(stderr.String(), "Couldn't read that link.")
	}

	raw, info, err := parseInfo(stdout.Bytes())
	if err != nil {
		return nil, err
	}
	if info.IsLive {
		return nil, errors.New("Live streams can't be downloaded while they're on air.")
	}
	if err := os.WriteFile(infoPath, raw, 0o600); err != nil {
		return nil, fmt.Errorf("saving video info: %w", err)
	}
	return info, nil
}

// parseInfo unwraps playlist results. A link to a playlist, or to a post with
// several videos, comes back as a wrapper with the single entry we asked for.
func parseInfo(raw []byte) ([]byte, *VideoInfo, error) {
	var wrapper struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, nil, errors.New("Couldn't understand the video info.")
	}
	if len(wrapper.Entries) > 0 {
		raw = wrapper.Entries[0]
	}
	var info VideoInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, nil, errors.New("Couldn't understand the video info.")
	}
	if info.Title == "" && len(info.Formats) == 0 {
		return nil, nil, errors.New("No video found at this link.")
	}
	return raw, &info, nil
}

func hasCodec(codec string) bool { return codec != "" && codec != "none" }

func scoreVideo(f rawFormat) float64 {
	score := f.Tbr
	if f.Ext == "mp4" {
		score += 10_000
	}
	if strings.HasPrefix(f.Vcodec, "avc") {
		score += 5_000
	}
	return score
}

// firstMax returns the first item with the highest score, like a stable
// descending sort followed by [0].
func firstMax(formats []rawFormat, score func(rawFormat) float64) (best rawFormat, found bool) {
	for _, f := range formats {
		if !found || score(f) > score(best) {
			best, found = f, true
		}
	}
	return best, found
}

func buildChoices(info *VideoInfo) []Choice {
	var audioOnly, videos []rawFormat
	for _, f := range info.Formats {
		if hasCodec(f.Acodec) && !hasCodec(f.Vcodec) {
			audioOnly = append(audioOnly, f)
		}
		if hasCodec(f.Vcodec) && f.Height > 0 {
			videos = append(videos, f)
		}
	}

	var audioSize float64
	if best, ok := firstMax(audioOnly, func(f rawFormat) float64 { return cmp.Or(f.Abr, f.Tbr) }); ok {
		audioSize = best.size()
	}

	var heights []int
	for _, f := range videos {
		heights = append(heights, int(f.Height))
	}
	slices.Sort(heights)
	heights = slices.Compact(heights)
	slices.Reverse(heights)
	heights = heights[:min(len(heights), maxVideoChoices)]

	var choices []Choice
	for _, height := range heights {
		candidates := slices.DeleteFunc(slices.Clone(videos), func(f rawFormat) bool { return int(f.Height) != height })
		best, _ := firstMax(candidates, scoreVideo)
		size := best.size()
		if !hasCodec(best.Acodec) {
			size += audioSize
		}
		choices = append(choices, Choice{
			Kind:  "video",
			Label: fmt.Sprintf("%dp", height),
			Ext:   "mp4",
			Size:  int64(size),
			Args: []string{
				"-f", fmt.Sprintf("bv*[height=%[1]d]+ba/b[height=%[1]d]/bv*[height<=%[1]d]+ba/b", height),
				"--merge-output-format", "mp4",
			},
		})
	}
	if len(choices) == 0 {
		choices = append(choices, Choice{
			Kind:  "video",
			Label: "Best available", // no ext: without a merge, it stays whatever the site serves
			Args:  []string{"-f", "bv*+ba/b", "--merge-output-format", "mp4"},
		})
	}
	return append(choices, Choice{
		Kind:  "audio",
		Label: "Audio only",
		Ext:   "mp3",
		Size:  int64(audioSize),
		Args:  []string{"-f", "ba/b", "-x", "--audio-format", "mp3", "--audio-quality", "0"},
	})
}

type Progress struct {
	Downloaded int64
	Total      int64
	Speed      float64
	ETA        float64
	Part       int
	Parts      int
}

type DownloadRequest struct {
	URL      string
	InfoPath string // when set, reuses the probe's metadata: starts much faster
	Dir      string
	Choice   Choice
}

type DownloadHandlers struct {
	OnProgress   func(Progress)
	OnProcessing func()
}

// Download writes into req.Dir under a fixed name. Titles are attacker-chosen
// text, so they never become a path: the display name is built separately.
func (y *YtDlp) Download(ctx context.Context, req DownloadRequest, handlers DownloadHandlers) (string, error) {
	args := y.args(req.Choice.Args...)
	args = append(args,
		"--newline",
		// --print implies --quiet, which hides the progress bars and the
		// [Merger]/[ExtractAudio] lines the processing phase is detected from
		"--no-quiet",
		"--progress",
		"--progress-template", "download:"+progressFormat,
		"--print", "after_move:filepath",
		"--no-simulate",
		"--no-mtime",
		"-o", filepath.Join(req.Dir, "media.%(ext)s"),
	)
	if req.InfoPath != "" {
		args = append(args, "--load-info-json", req.InfoPath)
	} else {
		args = append(args, "--", req.URL)
	}

	// Enforced here rather than with --max-filesize, which only looks at each
	// HTTP response: an HLS or DASH stream made of small fragments sails through.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	stderr := &tailBuffer{limit: maxErrorOutput}
	cmd := command(runCtx, y.Bin, args...)
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	parser := outputParser{parts: 1, limit: y.MaxFilesize, stop: stop, handlers: handlers}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		parser.line(scanner.Text())
	}
	err = cmd.Wait()

	switch {
	case parser.tooLarge:
		return "", errTooLarge
	case ctx.Err() != nil:
		return "", ctx.Err()
	case err == nil && parser.path != "":
		return parser.path, nil
	}
	return "", ytDlpError(stderr.String(), "The download failed.")
}

type outputParser struct {
	part, parts int
	last        int64 // bytes of the current file
	finished    int64 // bytes of the files before it
	limit       int64
	stop        func()
	path        string
	tooLarge    bool
	handlers    DownloadHandlers
}

func (p *outputParser) line(raw string) {
	line := strings.TrimSpace(raw)
	switch {
	case line == "":
	case strings.HasPrefix(line, progressPrefix):
		fields := strings.Split(strings.TrimPrefix(line, progressPrefix), "|")
		fields = append(fields, make([]string, 5)...) // missing fields read as unknown
		downloaded := int64(toNumber(fields[0]))
		if downloaded < p.last {
			p.part++ // the counter restarted: next file (video, then audio)
			p.finished += p.last
		}
		p.last = downloaded
		// an estimate alone isn't grounds to refuse: fragment sizes vary
		exact := int64(toNumber(fields[1]))
		if p.limit > 0 && p.finished+max(downloaded, exact) > p.limit {
			p.tooLarge = true
			p.stop()
			return
		}
		p.handlers.OnProgress(Progress{
			Downloaded: downloaded,
			Total:      int64(cmp.Or(toNumber(fields[1]), toNumber(fields[2]))),
			Speed:      toNumber(fields[3]),
			ETA:        toNumber(fields[4]),
			Part:       p.part,
			Parts:      p.parts,
		})
	case strings.Contains(line, "Downloading 1 format(s):"):
		// "[info] xxx: Downloading 1 format(s): 395+251": each id is one file
		_, ids, _ := strings.Cut(line, "format(s):")
		p.parts = len(strings.Split(strings.TrimSpace(ids), "+"))
	case strings.Contains(line, "[Merger]"), strings.Contains(line, "[ExtractAudio]"):
		p.handlers.OnProcessing()
	case filepath.IsAbs(line):
		p.path = line
	}
}

func toNumber(value string) float64 {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || n != n || n < 0 || n > 1<<62 {
		return 0 // "NA", "None", NaN and friends
	}
	return n
}

var errorPrefix = regexp.MustCompile(`^ERROR:\s*(\[[^\]]+\]\s*)?`)

func ytDlpError(stderr, fallback string) error {
	var last string
	for line := range strings.SplitSeq(stderr, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "ERROR:") {
			last = line
		}
	}
	if last == "" {
		return errors.New(fallback)
	}
	return errors.New(errorPrefix.ReplaceAllString(last, ""))
}

// safeFilename turns a video title into a name for the browser to save under.
func safeFilename(title, ext string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			return ' '
		}
		return r
	}, title)
	name := strings.Trim(strings.Join(strings.Fields(cleaned), " "), ". ")
	if runes := []rune(name); len(runes) > 120 {
		name = strings.TrimRight(string(runes[:120]), ". ")
	}
	return cmp.Or(name, "video") + "." + ext
}

// limitedBuffer fails the write once full, which stops the command.
type limitedBuffer struct {
	bytes.Buffer
	limit      int
	overflowed bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.overflowed = true
		return 0, errors.New("output too large")
	}
	return b.Buffer.Write(p)
}

// tailBuffer keeps the end of a stream: yt-dlp's error comes last.
type tailBuffer struct {
	data  []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = b.data[len(b.data)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.data) }
