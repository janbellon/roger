package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	probeTTL         = 30 * time.Minute
	probeTimeout     = 90 * time.Second
	abandonedAfter   = time.Minute
	janitorInterval  = 30 * time.Second
	maxQueuedPerSlot = 8
)

var (
	errBusy     = errors.New("This server is busy right now. Try again in a minute.")
	errNotFound = errors.New("This link has expired. Paste it again.")
	safeExt     = regexp.MustCompile(`^[a-zA-Z0-9]{1,8}$`)
)

type JobState string

const (
	StateQueued      JobState = "queued"
	StateDownloading JobState = "downloading"
	StateProcessing  JobState = "processing"
	StateReady       JobState = "ready"
	StateFailed      JobState = "failed"
	StateCancelled   JobState = "cancelled"
)

func (s JobState) finished() bool {
	return s == StateReady || s == StateFailed || s == StateCancelled
}

// JobStatus is what the browser sees. It is sent whole on every update, so a
// reconnecting client needs no replay.
type JobStatus struct {
	ID         string    `json:"id"`
	State      JobState  `json:"state"`
	Downloaded int64     `json:"downloaded"`
	Total      int64     `json:"total"`
	Speed      float64   `json:"speed"`
	ETA        float64   `json:"eta"`
	Part       int       `json:"part"`
	Parts      int       `json:"parts"`
	Refreshing bool      `json:"refreshing,omitempty"`
	Error      string    `json:"error,omitempty"`
	Filename   string    `json:"filename,omitempty"`
	Size       int64     `json:"size,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
}

type Job struct {
	cancel context.CancelFunc
	dir    string

	mu          sync.Mutex
	status      JobStatus
	path        string
	changed     chan struct{}
	subscribers int
	lastSeen    time.Time
}

func (j *Job) update(change func(*JobStatus)) {
	j.mu.Lock()
	change(&j.status)
	changed := j.changed
	j.changed = make(chan struct{})
	j.mu.Unlock()
	close(changed)
}

// Watch returns the current status and a channel closed on the next change.
func (j *Job) Watch() (JobStatus, <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status, j.changed
}

// Subscribe marks a browser as following this job until the returned func runs.
func (j *Job) Subscribe() (done func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.subscribers++
	return func() {
		j.mu.Lock()
		defer j.mu.Unlock()
		j.subscribers--
		j.lastSeen = time.Now()
	}
}

func (j *Job) abandoned(now time.Time) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return !j.status.State.finished() && j.subscribers == 0 && now.Sub(j.lastSeen) > abandonedAfter
}

// File returns the finished file's path and display name.
func (j *Job) File() (path, name string, ok bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.path, j.status.Filename, j.status.State == StateReady
}

type probeEntry struct {
	url      string
	infoPath string
	title    string
	choices  []Choice
	expires  time.Time
}

// Manager owns probes and download jobs. Nothing survives a restart: state
// lives in memory and the files are temporary by design.
type Manager struct {
	yt        *YtDlp
	cfg       Config
	probesDir string
	jobsDir   string
	root      context.Context
	slots     chan struct{}
	probeGate chan struct{}
	wg        sync.WaitGroup

	mu     sync.Mutex
	probes map[string]*probeEntry
	jobs   map[string]*Job
}

func NewManager(root context.Context, cfg Config, yt *YtDlp) (*Manager, error) {
	m := &Manager{
		yt:        yt,
		cfg:       cfg,
		probesDir: filepath.Join(cfg.DataDir, "probes"),
		jobsDir:   filepath.Join(cfg.DataDir, "jobs"),
		root:      root,
		slots:     make(chan struct{}, cfg.MaxConcurrent),
		probeGate: make(chan struct{}, cfg.MaxConcurrent*2),
		probes:    map[string]*probeEntry{},
		jobs:      map[string]*Job{},
	}
	// whatever a previous run left behind can't be served any more
	for _, dir := range []string{m.probesDir, m.jobsDir} {
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func newID() string {
	id := make([]byte, 16)
	rand.Read(id)
	return hex.EncodeToString(id)
}

type ProbeResult struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Uploader  string   `json:"uploader,omitempty"`
	Duration  float64  `json:"duration,omitempty"`
	Thumbnail string   `json:"thumbnail,omitempty"`
	Platform  string   `json:"platform"`
	Choices   []Choice `json:"choices"`
}

func (m *Manager) Probe(ctx context.Context, url string) (*ProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	select {
	case m.probeGate <- struct{}{}:
		defer func() { <-m.probeGate }()
	case <-ctx.Done():
		return nil, errBusy
	}

	id := newID()
	infoPath := filepath.Join(m.probesDir, id+".json")
	info, err := m.yt.Probe(ctx, url, infoPath)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, errors.New("This site took too long to answer.")
	}
	if err != nil {
		return nil, err
	}

	entry := &probeEntry{
		url:      url,
		infoPath: infoPath,
		title:    info.Title,
		choices:  buildChoices(info),
		expires:  time.Now().Add(probeTTL),
	}
	m.mu.Lock()
	m.probes[id] = entry
	m.mu.Unlock()

	result := &ProbeResult{
		ID:       id,
		Title:    info.Title,
		Uploader: info.Uploader,
		Duration: info.Duration,
		Platform: platformLabel(url),
		Choices:  entry.choices,
	}
	if strings.HasPrefix(info.Thumbnail, "https://") {
		result.Thumbnail = info.Thumbnail
	}
	return result, nil
}

func (m *Manager) Start(probeID string, choiceIndex int) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := m.probes[probeID]
	if entry == nil {
		return nil, errNotFound
	}
	if choiceIndex < 0 || choiceIndex >= len(entry.choices) {
		return nil, errors.New("Unknown format.")
	}
	active := 0
	for _, job := range m.jobs {
		if status, _ := job.Watch(); !status.State.finished() {
			active++
		}
	}
	if active >= m.cfg.MaxConcurrent*maxQueuedPerSlot {
		return nil, errBusy
	}

	id := newID()
	ctx, cancel := context.WithTimeout(m.root, m.cfg.DownloadTimeout)
	job := &Job{
		cancel:   cancel,
		dir:      filepath.Join(m.jobsDir, id),
		status:   JobStatus{ID: id, State: StateQueued, Parts: 1},
		changed:  make(chan struct{}),
		lastSeen: time.Now(),
	}
	m.jobs[id] = job
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.run(ctx, job, *entry, entry.choices[choiceIndex])
	}()
	return job, nil
}

func (m *Manager) run(ctx context.Context, job *Job, entry probeEntry, choice Choice) {
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		m.finish(job, "", "", ctx.Err())
		return
	}
	job.update(func(s *JobStatus) { s.State = StateDownloading })

	if err := os.MkdirAll(job.dir, 0o700); err != nil {
		m.finish(job, "", "", err)
		return
	}
	handlers := DownloadHandlers{
		OnProgress: func(p Progress) {
			job.update(func(s *JobStatus) {
				s.State = StateDownloading
				s.Downloaded, s.Total, s.Speed, s.ETA = p.Downloaded, p.Total, p.Speed, p.ETA
				s.Part, s.Parts = p.Part, p.Parts
			})
		},
		OnProcessing: func() { job.update(func(s *JobStatus) { s.State = StateProcessing }) },
	}

	req := DownloadRequest{URL: entry.url, InfoPath: entry.infoPath, Dir: job.dir, Choice: choice}
	path, err := m.yt.Download(ctx, req, handlers)
	if err != nil && ctx.Err() == nil && !errors.Is(err, errTooLarge) {
		// media URLs in the cached info can expire: retry with a fresh extraction
		slog.Info("retrying download with fresh extraction", "job", job.status.ID)
		job.update(func(s *JobStatus) {
			s.Refreshing = true
			s.Downloaded, s.Total, s.Speed, s.ETA, s.Part = 0, 0, 0, 0, 0
		})
		if clearErr := clearDir(job.dir); clearErr != nil {
			m.finish(job, "", "", clearErr)
			return
		}
		req.InfoPath = ""
		path, err = m.yt.Download(ctx, req, handlers)
	}
	m.finish(job, path, entry.title, err)
}

func clearDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}

func (m *Manager) finish(job *Job, path, title string, err error) {
	expires := time.Now().Add(m.cfg.FileTTL)
	var size int64
	if err == nil {
		// yt-dlp's stdout is partly site-controlled: only trust a path that
		// really is a file inside this job's directory
		info, statErr := os.Lstat(path)
		switch {
		case statErr != nil, !info.Mode().IsRegular(), filepath.Dir(path) != job.dir:
			err = errors.New("The download finished without producing a file.")
		default:
			size = info.Size()
		}
	}
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if err == nil && !safeExt.MatchString(ext) {
		err = errors.New("The download finished without producing a file.")
	}

	if err != nil {
		os.RemoveAll(job.dir)
		state, message := StateFailed, err.Error()
		switch {
		case errors.Is(err, context.Canceled):
			state, message = StateCancelled, ""
		case errors.Is(err, context.DeadlineExceeded):
			message = fmt.Sprintf("The download took longer than the %s limit.", m.cfg.DownloadTimeout)
		}
		job.update(func(s *JobStatus) {
			s.State, s.Error, s.ExpiresAt = state, message, expires
		})
		return
	}

	job.mu.Lock()
	job.path = path
	job.mu.Unlock()
	job.update(func(s *JobStatus) {
		s.State, s.Size, s.ExpiresAt = StateReady, size, expires
		s.Filename = safeFilename(title, ext)
	})
}

func (m *Manager) Job(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// Cleanup removes what has expired and stops downloads nobody is following any
// more, such as a closed tab.
func (m *Manager) Cleanup(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, entry := range m.probes {
		if now.After(entry.expires) {
			os.Remove(entry.infoPath)
			delete(m.probes, id)
		}
	}
	for id, job := range m.jobs {
		if job.abandoned(now) {
			job.cancel()
			continue
		}
		status, _ := job.Watch()
		if status.State.finished() && now.After(status.ExpiresAt) {
			os.RemoveAll(job.dir) // an open transfer keeps its handle and completes
			delete(m.jobs, id)
		}
	}
}

// Run drives Cleanup until shutdown, then waits for jobs and removes all files.
func (m *Manager) Run() {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			m.Cleanup(now)
		case <-m.root.Done():
			m.wg.Wait()
			os.RemoveAll(m.jobsDir)
			os.RemoveAll(m.probesDir)
			return
		}
	}
}
