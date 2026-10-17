// Package archive provides a read-only, in-memory view of a backup archive
// plus full-text search over it.
//
// Everything needed for a query is held in memory: one Doc per stored
// message with the fields search needs (author, timestamp, content). The
// raw JSON payload stays on disk and is only read back by message ID, so the
// index stays small while the full fidelity of the archive is preserved.
//
// The archive is append-only, which makes incremental refresh trivial: a
// refresh tails only the bytes appended since the last read. Edits replace a
// Doc in place; deletes mark it.
package archive

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"discord-backup/internal/backup"
	"discord-backup/internal/discord"
)

// Kind distinguishes a channel log from a thread log.
type Kind string

const (
	KindChannel Kind = "channel"
	KindThread  Kind = "thread"
)

// Log and Doc are the archive's internal representation. They are shared
// between goroutines and only ever accessed under Archive.mu; callers must
// never retain a *Log or *Doc. Use the value snapshots LogInfo/MessageInfo
// returned by the accessors instead.
//
// Log is one archived conversation (a channel or a thread).
type Log struct {
	ID       string
	Name     string
	Kind     Kind
	Type     int    // Discord channel/thread type
	ParentID string // thread -> parent channel ID; channel -> "" (its category is a name only)
	Category string
	Dir      string
	Archived bool
	Locked   bool
	NoAccess bool
	Messages int // message count as recorded by the backup

	docs []*Doc // ascending by message ID
}

// Doc is one stored message, resolved to its newest version.
type Doc struct {
	ID       string
	Log      *Log
	AuthorID string
	Author   string
	Content  string
	Lower    string // lowercased content, for matching
	Extra    string // lowercased attachment filenames, for matching
	TS       time.Time
	Edited   bool
	Deleted  bool

	file   string // "messages.jsonl" or "edits.jsonl"
	offset int64
	size   int32
}

// LogInfo is an immutable snapshot of a Log's metadata. Accessors hand these
// out instead of *Log so a concurrent Refresh can never mutate a value a
// caller is still reading.
type LogInfo struct {
	ID       string
	Name     string
	Kind     Kind
	Type     int
	ParentID string
	Category string
	Dir      string
	Archived bool
	Locked   bool
	NoAccess bool
	Messages int
}

// MessageInfo is an immutable snapshot of one stored message. It carries
// everything needed to render without a second lookup.
type MessageInfo struct {
	ID        string
	ChannelID string
	Channel   string
	Kind      Kind
	AuthorID  string
	Author    string
	Content   string
	TS        time.Time
	Edited    bool
	Deleted   bool
	Attach    bool
}

func (l *Log) info() LogInfo {
	return LogInfo{
		ID: l.ID, Name: l.Name, Kind: l.Kind, Type: l.Type,
		ParentID: l.ParentID, Category: l.Category, Dir: l.Dir,
		Archived: l.Archived, Locked: l.Locked, NoAccess: l.NoAccess,
		Messages: l.Messages,
	}
}

func (d *Doc) info() MessageInfo {
	return MessageInfo{
		ID: d.ID, ChannelID: d.Log.ID, Channel: d.Log.Name, Kind: d.Log.Kind,
		AuthorID: d.AuthorID, Author: d.Author, Content: d.Content, TS: d.TS,
		Edited: d.Edited, Deleted: d.Deleted, Attach: d.Extra != "",
	}
}

// Options configures Open.
type Options struct {
	Dir         string // archive root (containing server_<id>) or the server dir itself
	GuildID     string // disambiguates when Dir holds several archives
	ShowDeleted bool   // include messages marked deleted (default: hidden)
	RefreshTTL  time.Duration
	Log         *slog.Logger
}

// Archive is a loaded, searchable archive. It is safe for concurrent use.
type Archive struct {
	root    string
	guildID string
	name    string

	opts Options
	log  *slog.Logger

	mu       sync.RWMutex
	logs     []*Log
	byLog    map[string]*Log
	byDirMap map[string]*Log
	byMsg    map[string]*Doc
	deletes  map[string]struct{}
	files    []*fileState
	lastScan time.Time
}

type fileState struct {
	log  *Log
	kind string // "messages" | "edits" | "deletes"
	path string
	off  int64

	// head is a fingerprint of the file's first headLen bytes as of the last
	// read. It detects a rewrite (the backup's history heal prepends via
	// temp file + rename), which would otherwise invalidate every offset.
	head    uint64
	headLen int
}

// Open loads an archive from disk.
func Open(ctx context.Context, opts Options) (*Archive, error) {
	if opts.Dir == "" {
		return nil, errors.New("archive dir not set")
	}
	logger := opts.Log
	if logger == nil {
		logger = slog.Default()
	}
	serverDir, err := resolveServerDir(opts.Dir, opts.GuildID)
	if err != nil {
		return nil, err
	}
	a := &Archive{
		root:     serverDir,
		opts:     opts,
		log:      logger,
		byLog:    map[string]*Log{},
		byDirMap: map[string]*Log{},
		byMsg:    map[string]*Doc{},
		deletes:  map[string]struct{}{},
	}
	if err := a.initialLoad(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// resolveServerDir finds the server_<id> directory that holds the archive.
func resolveServerDir(dir, guildID string) (string, error) {
	if fileExists(filepath.Join(dir, "server_info.json")) {
		return dir, nil
	}
	if guildID != "" {
		p := filepath.Join(dir, "server_"+guildID)
		if fileExists(filepath.Join(p, "server_info.json")) {
			return p, nil
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read archive dir %q: %w", dir, err)
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "server_") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if fileExists(filepath.Join(p, "server_info.json")) {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("no archive found in %q (expected server_<id>/server_info.json)", dir)
	default:
		return "", fmt.Errorf("multiple archives in %q; set guild_id to choose one", dir)
	}
}

func (a *Archive) initialLoad(ctx context.Context) error {
	var info backup.ServerInfo
	if err := readJSON(filepath.Join(a.root, "server_info.json"), &info); err != nil {
		return fmt.Errorf("read server_info: %w", err)
	}
	a.guildID = info.ID
	a.name = info.Name
	if a.guildID == "" {
		a.guildID = strings.TrimPrefix(filepath.Base(a.root), "server_")
	}
	if err := a.scanDirs(ctx); err != nil {
		return err
	}
	a.lastScan = time.Now()
	a.log.Info("archive loaded", "guild", a.name, "logs", len(a.logs), "messages", len(a.byMsg))
	return nil
}

// scanDirs walks the archive tree, loads new logs and tails the files of
// already known logs. It is the only place that touches the filesystem
// structure and is reused by Open and Refresh.
func (a *Archive) scanDirs(ctx context.Context) error {
	entries, err := os.ReadDir(a.root)
	if err != nil {
		return fmt.Errorf("read server dir: %w", err)
	}
	categoryByChannel := map[string]string{}
	var threadDirs []string

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "channel_") {
			continue
		}
		cdir := filepath.Join(a.root, e.Name())
		cl, err := a.ensureLog(cdir, KindChannel)
		if err != nil {
			continue // skip unreadable logs, never abort the whole archive
		}
		// Channels are few; re-read their metadata each scan so the message
		// counter shown to clients stays current.
		a.refreshMeta(cl)
		if err := a.loadDocs(cl); err != nil {
			a.log.Warn("log load failed, skipping", "log", cl.ID, "err", err)
			continue
		}
		categoryByChannel[cl.ID] = cl.Category

		tentries, err := os.ReadDir(filepath.Join(cdir, "threads"))
		if err != nil {
			continue
		}
		for _, t := range tentries {
			if t.IsDir() {
				threadDirs = append(threadDirs, filepath.Join(cdir, "threads", t.Name()))
			}
		}
	}
	for _, tdir := range threadDirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		tl, err := a.ensureLog(tdir, KindThread)
		if err != nil {
			continue
		}
		if tl.Type == 0 {
			// The backup rewrites thread_info.json each run. A thread with no
			// recorded type is hidden, so re-read the file to pick it up
			// without a restart. Once types are recorded this stops costing
			// any file reads.
			a.refreshMeta(tl)
		}
		if cat := categoryByChannel[tl.ParentID]; cat != "" {
			tl.Category = cat
		}
		if err := a.loadDocs(tl); err != nil {
			a.log.Warn("thread load failed, skipping", "log", tl.ID, "err", err)
		}
	}
	return nil
}

// ensureLog returns the existing Log for a directory or creates and
// registers a new one.
func (a *Archive) ensureLog(directory string, kind Kind) (*Log, error) {
	if l, ok := a.byDir(directory); ok {
		return l, nil
	}
	l, err := a.readLogMeta(directory, kind)
	if err != nil {
		return nil, err
	}
	a.byLog[l.ID] = l
	a.byDirMap[l.Dir] = l
	a.logs = append(a.logs, l)
	return l, nil
}

func (a *Archive) byDir(directory string) (*Log, bool) {
	l, ok := a.byDirMap[directory]
	return l, ok
}

func (a *Archive) readLogMeta(directory string, kind Kind) (*Log, error) {
	if kind == KindChannel {
		var ci backup.ChannelInfo
		if err := readJSON(filepath.Join(directory, "channel_info.json"), &ci); err != nil {
			return nil, err
		}
		if ci.ID == "" {
			ci.ID = strings.TrimPrefix(filepath.Base(directory), "channel_")
		}
		return &Log{
			ID: ci.ID, Name: ci.Name, Kind: KindChannel, Type: ci.Type,
			Category: ci.Category, Dir: directory, NoAccess: ci.NoAccess,
			Messages: ci.MessageCount,
		}, nil
	}
	var ti backup.ThreadInfo
	if err := readJSON(filepath.Join(directory, "thread_info.json"), &ti); err != nil {
		return nil, err
	}
	if ti.ID == "" {
		ti.ID = strings.TrimPrefix(filepath.Base(directory), "thread_")
	}
	return &Log{
		ID: ti.ID, Name: ti.Name, Kind: KindThread, Type: ti.Type,
		ParentID: ti.ParentID, Dir: directory,
		Archived: ti.Archived, Locked: ti.Locked, NoAccess: ti.NoAccess,
		Messages: ti.MessageCount,
	}, nil
}

// refreshMeta re-reads a log's metadata file in place. The backup rewrites
// the metadata on every run (message counters, thread type), so a
// long-running reader picks up changes without a restart.
func (a *Archive) refreshMeta(l *Log) {
	fresh, err := a.readLogMeta(l.Dir, l.Kind)
	if err != nil {
		return
	}
	l.Name, l.Type, l.ParentID = fresh.Name, fresh.Type, fresh.ParentID
	l.Archived, l.Locked, l.Messages = fresh.Archived, fresh.Locked, fresh.Messages
	if l.Kind == KindChannel {
		l.Category = fresh.Category
	}
}

// logFiles lists the three append-only files of every log, in the order they
// must be applied (messages first, then edits and deletes).
var logFiles = []struct{ kind, file string }{
	{"messages", "messages.jsonl"},
	{"edits", "edits.jsonl"},
	{"deletes", "deletes.jsonl"},
}

// loadDocs registers the three log files and consumes whatever is new. If a
// file was rewritten on disk (prepended by the history heal, truncated or
// replaced) the log is rebuilt from scratch so byte offsets stay valid.
func (a *Archive) loadDocs(l *Log) error {
	present := a.ensureFileStates(l)
	for _, f := range logFiles {
		if err := a.consume(present[f.kind]); err != nil {
			if errors.Is(err, errRewritten) {
				a.log.Warn("archive log rewritten on disk, reloading", "log", l.ID)
				a.resetLog(l, present)
				for _, g := range logFiles {
					if err := a.consume(present[g.kind]); err != nil {
						return err
					}
				}
				return nil
			}
			return err
		}
	}
	return nil
}

// ensureFileStates returns the fileStates of a log, creating the missing ones.
func (a *Archive) ensureFileStates(l *Log) map[string]*fileState {
	present := map[string]*fileState{}
	for _, fs := range a.files {
		if fs.log == l {
			present[fs.kind] = fs
		}
	}
	for _, f := range logFiles {
		if _, ok := present[f.kind]; ok {
			continue
		}
		fs := &fileState{log: l, kind: f.kind, path: filepath.Join(l.Dir, f.file)}
		present[f.kind] = fs
		a.files = append(a.files, fs)
	}
	return present
}

// resetLog drops a log's resolved messages so they are read again from
// offset 0, and clears the reader state of all of its files.
func (a *Archive) resetLog(l *Log, fs map[string]*fileState) {
	for _, d := range l.docs {
		delete(a.byMsg, d.ID)
	}
	l.docs = nil
	for _, f := range fs {
		f.off, f.head, f.headLen = 0, 0, 0
	}
}

var errRewritten = errors.New("log file rewritten")

// headPrefix caps how many leading bytes of a file are fingerprinted. Any
// prefix is stable under append-only growth, which is the common case.
const headPrefix = 4096

// consume reads everything appended to fs.path since fs.off. It returns
// errRewritten when the stored offsets are no longer trustworthy.
func (a *Archive) consume(fs *fileState) error {
	if fs.off > 0 {
		if size := fileSize(fs.path); size < fs.off {
			return errRewritten // truncated or replaced
		}
		if fs.headLen > 0 {
			h, n := fileHead(fs.path, fs.headLen)
			if n != fs.headLen || h != fs.head {
				return errRewritten // content at the front changed (prepend)
			}
		}
	}
	newOff, err := scanFile(fs.path, fs.off, func(line []byte, off int64) error {
		a.applyLine(fs, line, off)
		return nil
	})
	fs.off = newOff
	// Record a fingerprint once, as soon as the file is known to have
	// content. It is never widened, so appends cannot look like a rewrite.
	if fs.headLen == 0 && newOff > 0 {
		if h, n := fileHead(fs.path, headPrefix); n > 0 {
			fs.head, fs.headLen = h, n
		}
	}
	return err
}

func (a *Archive) applyLine(fs *fileState, line []byte, off int64) {
	switch fs.kind {
	case "messages":
		d, err := docFromLine(line, fs.log, "messages.jsonl", off)
		if err != nil {
			return
		}
		a.addDoc(d)
	case "edits":
		d, err := docFromLine(line, fs.log, "edits.jsonl", off)
		if err != nil {
			return
		}
		d.Edited = true
		a.applyVersion(d)
	case "deletes":
		var m backup.DeleteMark
		if err := json.Unmarshal(line, &m); err != nil || m.ID == "" {
			return
		}
		a.deletes[m.ID] = struct{}{}
		if d := a.byMsg[m.ID]; d != nil {
			d.Deleted = true
		}
	}
}

func (a *Archive) addDoc(d *Doc) {
	if _, exists := a.byMsg[d.ID]; exists {
		return // first version wins; edits go through applyVersion
	}
	a.byMsg[d.ID] = d
	d.Log.docs = append(d.Log.docs, d)
}

// applyVersion records a newer version of a message (from edits.jsonl).
func (a *Archive) applyVersion(d *Doc) {
	cur, ok := a.byMsg[d.ID]
	if !ok {
		a.addDoc(d)
		return
	}
	cur.Content, cur.Lower, cur.Extra = d.Content, d.Lower, d.Extra
	cur.AuthorID, cur.Author = d.AuthorID, d.Author
	cur.TS = d.TS
	cur.Edited = true
	cur.file, cur.offset, cur.size = d.file, d.offset, d.size
}

func docFromLine(line []byte, l *Log, file string, off int64) (*Doc, error) {
	env, err := discord.ParseEnvelope(line)
	if err != nil {
		return nil, err
	}
	extra := ""
	if len(env.Attachments) > 0 {
		var sb strings.Builder
		for _, at := range env.Attachments {
			sb.WriteString(strings.ToLower(at.Filename))
			sb.WriteByte(' ')
		}
		extra = strings.TrimSpace(sb.String())
	}
	return &Doc{
		ID: env.ID, Log: l,
		AuthorID: env.Author.ID, Author: env.Author.Username,
		Content: env.Content, Lower: strings.ToLower(env.Content), Extra: extra,
		TS:   env.Timestamp,
		file: file, offset: off, size: int32(len(line)),
	}, nil
}

// --- access ----------------------------------------------------------------

// Access decides whether a log may be read by the current principal.
type Access func(LogInfo) bool

// ChannelAccess builds an Access from the set of readable channel IDs. A
// public or announcement thread is readable exactly when its parent channel
// is; a private thread (or a thread of unknown type in an archive written by
// an older version) is only readable when threadAllowed says so, which keeps
// private threads hidden by default.
func ChannelAccess(allowed map[string]bool, threadAllowed func(LogInfo) bool) Access {
	return func(l LogInfo) bool {
		if l.Kind != KindThread {
			return allowed[l.ID]
		}
		switch l.Type {
		case discord.ChannelPrivateThread, 0:
			if threadAllowed != nil {
				return threadAllowed(l)
			}
			return false
		default:
			return allowed[l.ParentID]
		}
	}
}

// AllowAll is an Access that permits everything (used by offline admin
// tooling and tests).
func AllowAll() Access { return func(LogInfo) bool { return true } }

// --- refresh ---------------------------------------------------------------

// Refresh rescans the directory tree and tails all appended bytes.
func (a *Archive) Refresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.scanDirs(ctx); err != nil {
		return err
	}
	a.lastScan = time.Now()
	return nil
}

// MaybeRefresh refreshes when the last scan is older than RefreshTTL. The
// staleness check and the scan happen under the same write lock, so parallel
// tool calls trigger at most one refresh per window instead of each doing
// their own.
func (a *Archive) MaybeRefresh(ctx context.Context) {
	ttl := a.opts.RefreshTTL
	if ttl <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.lastScan) <= ttl {
		return
	}
	if err := a.scanDirs(ctx); err != nil {
		a.log.Warn("archive refresh failed, serving current state", "err", err)
		return
	}
	a.lastScan = time.Now()
}

// --- accessors -------------------------------------------------------------

// GuildID returns the archived guild ID.
func (a *Archive) GuildID() string { return a.guildID }

// Name returns the archived guild name.
func (a *Archive) Name() string { return a.name }

// Root returns the server directory.
func (a *Archive) Root() string { return a.root }

// LogsInfo returns an immutable snapshot of every log the access predicate
// allows.
func (a *Archive) LogsInfo(access Access) []LogInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]LogInfo, 0, len(a.logs))
	for _, l := range a.logs {
		if info := l.info(); access(info) {
			out = append(out, info)
		}
	}
	return out
}

// LogInfoByID returns a log's snapshot if it exists and access allows it.
func (a *Archive) LogInfoByID(id string, access Access) (LogInfo, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	l, ok := a.byLog[id]
	if !ok {
		return LogInfo{}, false
	}
	info := l.info()
	if !access(info) {
		return LogInfo{}, false
	}
	return info, true
}

// Message returns a message's snapshot and its raw JSON payload when access
// allows it.
func (a *Archive) Message(id string, access Access) (MessageInfo, []byte, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	d, ok := a.byMsg[id]
	if !ok || !access(d.Log.info()) {
		return MessageInfo{}, nil, false
	}
	if d.Deleted && !a.opts.ShowDeleted {
		return MessageInfo{}, nil, false
	}
	info := d.info()
	raw, err := readRange(filepath.Join(d.Log.Dir, d.file), d.offset, int64(d.size))
	if err != nil {
		a.log.Warn("read message payload failed", "id", id, "err", err)
		return info, nil, true
	}
	return info, raw, true
}

// MessageContext returns up to before/after neighbouring messages around id
// within the same log, honouring access and the deleted policy, plus the log
// the messages belong to.
func (a *Archive) MessageContext(id string, access Access, before, after int) ([]MessageInfo, LogInfo, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	d, ok := a.byMsg[id]
	if !ok || !access(d.Log.info()) {
		return nil, LogInfo{}, false
	}
	if d.Deleted && !a.opts.ShowDeleted {
		return nil, LogInfo{}, false
	}
	logInfo := d.Log.info()
	idx := d.Log.indexOf(id)
	if idx < 0 {
		return nil, LogInfo{}, false
	}
	visible := func(x *Doc) bool { return a.opts.ShowDeleted || !x.Deleted }
	var picked []*Doc
	for i := idx - 1; i >= 0 && len(picked) < before; i-- {
		if visible(d.Log.docs[i]) {
			picked = append(picked, d.Log.docs[i])
		}
	}
	// reverse the "before" window
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	picked = append(picked, d)
	for i := idx + 1; i < len(d.Log.docs) && len(picked) < before+after+1; i++ {
		if visible(d.Log.docs[i]) {
			picked = append(picked, d.Log.docs[i])
		}
	}
	out := make([]MessageInfo, len(picked))
	for i, x := range picked {
		out[i] = x.info()
	}
	return out, logInfo, true
}

func (l *Log) indexOf(id string) int {
	i := sort.Search(len(l.docs), func(i int) bool {
		return !discord.LessID(l.docs[i].ID, id)
	})
	if i < len(l.docs) && l.docs[i].ID == id {
		return i
	}
	// IDs compare numerically; fall back to a linear scan if the invariant
	// was ever violated (e.g. a prepended history heal).
	for j, d := range l.docs {
		if d.ID == id {
			return j
		}
	}
	return -1
}

// --- helpers ---------------------------------------------------------------

func readJSON(path string, out any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// fileSize returns the size of path, or 0 when it cannot be stat-ed.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// fileHead returns an FNV-1a fingerprint of the first n bytes of path and the
// number of bytes actually hashed (fewer than n when the file is shorter).
// n <= 0 means "up to headPrefix bytes". A missing or empty file yields
// (0, 0).
func fileHead(path string, n int) (uint64, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	if n <= 0 || n > headPrefix {
		n = headPrefix
	}
	buf := make([]byte, n)
	got, _ := io.ReadFull(f, buf)
	if got <= 0 {
		return 0, 0
	}
	h := fnv.New64a()
	h.Write(buf[:got])
	return h.Sum64(), got
}

func readRange(path string, off, size int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

// scanFile reads path from offset from and calls fn for every complete line,
// passing the line's byte offset. An unterminated trailing line is left
// unprocessed (its offset is returned) so it is picked up once complete.
func scanFile(path string, from int64, fn func(line []byte, off int64) error) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return from, nil
		}
		return from, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return from, err
	}
	if fi.Size() <= from {
		return from, nil
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return from, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	off := from
	for {
		line, rerr := br.ReadBytes('\n')
		if n := len(line); n > 0 && line[n-1] == '\n' {
			body := line[:n-1]
			if len(strings.TrimSpace(string(body))) > 0 {
				if err := fn(body, off); err != nil {
					return off, err
				}
			}
			off += int64(n)
		} else {
			break // partial tail line
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return off, rerr
		}
	}
	return off, nil
}
