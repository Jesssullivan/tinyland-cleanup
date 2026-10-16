// Package rootjobs holds tinyland-cleanup's privileged modes. They run as
// root from the units under packaging/systemd (no sudo rule), and each one
// does one narrow thing.
//
// gcroot-unregister reads the candidate lists the unprivileged daemon writes
// (nix.gc_root_candidates_file) and removes only the matching
// /nix/var/nix/gcroots/auto/<name> entries after re-checking every line. It
// never touches an out-link, so agent-session trees and .direnv directories
// keep every file (TIN-5268).
package rootjobs

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	autoEntryNameRe = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	storeTargetRe   = regexp.MustCompile(`^/nix/store/[^/]+$`)
)

// UnregisterOptions configures one gcroot-unregister run.
type UnregisterOptions struct {
	AutoDir          string
	Receipts         string
	MaxReceiptsBytes int64
	ReportOnly       bool
	MaxListAge       time.Duration
	MinLinkAge       time.Duration
	MaxLines         int
	Lists            []ListSpec
	Now              func() time.Time
	Out              io.Writer
}

// ListSpec is one declared USER=FILE pair.
type ListSpec struct {
	User string
	File string
}

// UnregisterSummary counts what a run did.
type UnregisterSummary struct {
	Removed int
	Would   int
	Refused int
}

// RunGCRootUnregister parses args (the subcommand's own flags and USER=FILE
// pairs) and runs. It always returns 0 unless the arguments are invalid:
// refusals are receipts, not failures.
func RunGCRootUnregister(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("gcroot-unregister", flag.ContinueOnError)
	fs.SetOutput(out)
	opts := UnregisterOptions{Out: out, Now: time.Now}
	fs.StringVar(&opts.AutoDir, "auto-dir", "/nix/var/nix/gcroots/auto", "Nix auto GC roots directory")
	fs.StringVar(&opts.Receipts, "receipts", "/var/lib/tinyland-cleanup/gcroot-unregister.receipts", "receipts file (bounded)")
	fs.Int64Var(&opts.MaxReceiptsBytes, "max-receipts-bytes", 1<<20, "rotate the receipts file to .1 past this size")
	fs.BoolVar(&opts.ReportOnly, "report-only", false, "write would-unregister receipts and remove nothing")
	fs.DurationVar(&opts.MaxListAge, "max-list-age", time.Hour, "refuse a list not rewritten within this long")
	fs.DurationVar(&opts.MinLinkAge, "min-link-age", 72*time.Hour, "refuse an out-link changed more recently than this")
	fs.IntVar(&opts.MaxLines, "max-lines", 20000, "stop reading a list after this many lines")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	for _, pair := range fs.Args() {
		user, file, ok := strings.Cut(pair, "=")
		if !ok || user == "" || !filepath.IsAbs(file) {
			fmt.Fprintf(out, "gcroot-unregister: not a USER=/absolute/path pair: %q\n", pair)
			return 2
		}
		opts.Lists = append(opts.Lists, ListSpec{User: user, File: filepath.Clean(file)})
	}
	if len(opts.Lists) == 0 {
		fmt.Fprintln(out, "gcroot-unregister: no USER=FILE lists")
		return 2
	}
	GCRootUnregister(opts)
	return 0
}

// GCRootUnregister processes every declared list.
func GCRootUnregister(opts UnregisterOptions) UnregisterSummary {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.MaxLines <= 0 {
		opts.MaxLines = 20000
	}
	r := &receiptWriter{path: opts.Receipts, max: opts.MaxReceiptsBytes, out: opts.Out, now: opts.Now}
	var sum UnregisterSummary
	mode := "armed"
	if opts.ReportOnly {
		mode = "report-only"
	}
	for _, spec := range opts.Lists {
		processList(opts, spec, r, &sum)
	}
	r.write(fmt.Sprintf("summary mode=%s removed=%d would=%d refused=%d", mode, sum.Removed, sum.Would, sum.Refused))
	return sum
}

func processList(opts UnregisterOptions, spec ListSpec, r *receiptWriter, sum *UnregisterSummary) {
	refuse := func(format string, a ...any) {
		r.write(fmt.Sprintf("refused user=%s list=%s ", spec.User, spec.File) + fmt.Sprintf(format, a...))
		sum.Refused++
	}
	u, err := user.Lookup(spec.User)
	if err != nil {
		refuse("reason=unknown user")
		return
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil || uid == 0 {
		refuse("reason=root or malformed uid")
		return
	}
	// O_NOFOLLOW refuses a symlinked list; O_NONBLOCK keeps a FIFO from
	// blocking the run (review L1). The checks below read the opened fd.
	f, err := os.OpenFile(spec.File, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		refuse("reason=list unreadable or a symlink")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		refuse("reason=list is not a regular file")
		return
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != uid {
		refuse("reason=list is not owned by uid %d", uid)
		return
	}
	listTime := info.ModTime()
	// H2: a list the daemon has not rewritten recently is stale; replaying
	// it could unregister a root recreated at the same path since.
	if age := opts.Now().Sub(listTime); opts.MaxListAge > 0 && age > opts.MaxListAge {
		refuse("reason=list is %s old, older than %s", age.Round(time.Second), opts.MaxListAge)
		return
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	n := 0
	for scanner.Scan() {
		n++
		if n > opts.MaxLines {
			refuse("reason=more than %d lines", opts.MaxLines)
			return
		}
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, link, ok := strings.Cut(line, "\t")
		if !ok {
			refuse("line=%d reason=no tab separator", n)
			continue
		}
		if reason := checkEntry(opts, uid, listTime, name, link); reason != "" {
			refuse("line=%d reason=%s", n, reason)
			continue
		}
		dest, _ := os.Readlink(link)
		entry := filepath.Join(opts.AutoDir, name)
		if opts.ReportOnly {
			r.write(fmt.Sprintf("would-unregister user=%s auto=%s link=%s store=%s", spec.User, entry, link, dest))
			sum.Would++
			continue
		}
		if err := os.Remove(entry); err != nil {
			refuse("line=%d reason=could not remove the auto entry", n)
			continue
		}
		r.write(fmt.Sprintf("unregistered user=%s auto=%s link=%s store=%s", spec.User, entry, link, dest))
		sum.Removed++
	}
	if err := scanner.Err(); err != nil {
		refuse("reason=list read error")
	}
}

// checkEntry returns a refusal reason, or "" when every check holds.
func checkEntry(opts UnregisterOptions, uid int, listTime time.Time, name, link string) string {
	if !autoEntryNameRe.MatchString(name) || name == "." || name == ".." {
		return "not an auto entry name"
	}
	if !filepath.IsAbs(link) || filepath.Clean(link) != link {
		return "out-link is not a clean absolute path"
	}
	entry := filepath.Join(opts.AutoDir, name)
	entryInfo, err := os.Lstat(entry)
	if err != nil || entryInfo.Mode()&os.ModeSymlink == 0 {
		return "auto entry is not a symlink"
	}
	// H2: an auto entry registered after the list was written belongs to a
	// root recreated since; the list cannot speak for it.
	if entryInfo.ModTime().After(listTime) {
		return "auto entry is newer than the list"
	}
	target, err := os.Readlink(entry)
	if err != nil || target != link {
		return "auto entry does not resolve to the listed out-link"
	}
	linkInfo, err := os.Lstat(link)
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		return "out-link is not a symlink"
	}
	st, ok := linkInfo.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != uid {
		owner := -1
		if ok {
			owner = int(st.Uid)
		}
		return fmt.Sprintf("out-link is owned by uid %d, not the list owner %d", owner, uid)
	}
	// H2: re-check the age here too; a link touched or recreated recently is
	// in use whatever the list says.
	if age := opts.Now().Sub(linkInfo.ModTime()); opts.MinLinkAge > 0 && age < opts.MinLinkAge {
		return fmt.Sprintf("out-link changed %s ago, inside %s", age.Round(time.Second), opts.MinLinkAge)
	}
	dest, err := os.Readlink(link)
	if err != nil || !storeTargetRe.MatchString(dest) {
		return "out-link does not point straight into /nix/store"
	}
	for _, part := range strings.Split(filepath.Dir(link), string(filepath.Separator)) {
		if part == "gcroots" || part == "profiles" {
			return "out-link is under gcroots/ or profiles/"
		}
	}
	return ""
}

// receiptWriter prints each receipt and appends it to a bounded file,
// rotating it to <path>.1 once it passes max bytes (review M3).
type receiptWriter struct {
	path string
	max  int64
	out  io.Writer
	now  func() time.Time
}

func (r *receiptWriter) write(msg string) {
	line := r.now().UTC().Format(time.RFC3339) + " " + msg + "\n"
	io.WriteString(r.out, line)
	if r.path == "" {
		return
	}
	if r.max > 0 {
		if info, err := os.Stat(r.path); err == nil && info.Size()+int64(len(line)) > r.max {
			_ = os.Rename(r.path, r.path+".1")
		}
	}
	_ = os.MkdirAll(filepath.Dir(r.path), 0o700)
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}
