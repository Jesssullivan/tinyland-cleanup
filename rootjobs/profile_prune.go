package rootjobs

// profile-prune keeps the current generation plus at most one previous one in
// each declared user's Home Manager and user profiles and in root's profile
// (lab R-C43: "1, max two nix states"; R-C795: keep 1 previous everywhere).
// It never collects the store: the daemon's nix plugin does (R-C744). It
// prunes nothing for a user whose Home Manager profile and current-home root
// disagree (an unfinished activation), and it fails a profile whose current
// generation changed during its prune.

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
)

// PruneOptions configures one profile-prune run.
type PruneOptions struct {
	KeepPrevious int
	Users        []string
	NixEnv       string
	Runuser      string
	RootProfile  string
	ReportOnly   bool
	// HomeOf resolves a user's home directory; tests replace it.
	HomeOf func(string) (string, error)
	Out    io.Writer
}

// RunProfilePrune parses args and runs. Exit 1 when any user failed.
func RunProfilePrune(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("profile-prune", flag.ContinueOnError)
	fs.SetOutput(out)
	opts := PruneOptions{Out: out}
	fs.IntVar(&opts.KeepPrevious, "keep-previous", 1, "previous generations to keep per profile (0 or 1)")
	fs.StringVar(&opts.NixEnv, "nix-env", "/nix/var/nix/profiles/default/bin/nix-env", "nix-env binary")
	fs.StringVar(&opts.Runuser, "runuser", "/usr/sbin/runuser", "runuser binary, used to prune a user's own profiles as that user")
	fs.StringVar(&opts.RootProfile, "root-profile", "/nix/var/nix/profiles/per-user/root/profile", "root's profile; empty skips it")
	fs.BoolVar(&opts.ReportOnly, "report-only", false, "check and report, prune nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	opts.Users = fs.Args()
	if opts.KeepPrevious != 0 && opts.KeepPrevious != 1 {
		fmt.Fprintf(out, "profile-prune: --keep-previous must be 0 or 1 (R-C43: at most two generations), got %d\n", opts.KeepPrevious)
		return 2
	}
	if len(opts.Users) == 0 {
		fmt.Fprintln(out, "profile-prune: no users")
		return 2
	}
	if err := ProfilePrune(opts); err != nil {
		fmt.Fprintf(out, "profile-prune: %v\n", err)
		return 1
	}
	return 0
}

// ProfilePrune prunes every declared user, then root's profile.
func ProfilePrune(opts PruneOptions) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.HomeOf == nil {
		opts.HomeOf = func(name string) (string, error) {
			u, err := user.Lookup(name)
			if err != nil {
				return "", err
			}
			return u.HomeDir, nil
		}
	}
	selector := "old"
	if opts.KeepPrevious == 1 {
		selector = "+2"
	}
	var failed []string
	for _, name := range opts.Users {
		if err := pruneUser(opts, name, selector); err != nil {
			fmt.Fprintf(opts.Out, "profile-prune: user %s: %v\n", name, err)
			failed = append(failed, name)
		}
	}
	if opts.RootProfile != "" {
		if err := pruneProfile(opts, opts.RootProfile, selector, nil); err != nil {
			fmt.Fprintf(opts.Out, "profile-prune: root profile: %v\n", err)
			failed = append(failed, "root")
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed for %s", strings.Join(failed, ", "))
	}
	return nil
}

func pruneUser(opts PruneOptions, name, selector string) error {
	home, err := opts.HomeOf(name)
	if err != nil {
		return err
	}
	hm := filepath.Join(home, ".local/state/nix/profiles/home-manager")
	currentHome := filepath.Join(home, ".local/state/home-manager/gcroots/current-home")
	asUser := []string{opts.Runuser, "--user", name, "--"}
	if _, err := os.Lstat(hm); err == nil {
		if err := homeManagerRootsAgree(hm, currentHome); err != nil {
			return err
		}
		if err := pruneProfile(opts, hm, selector, asUser); err != nil {
			return err
		}
		if err := homeManagerRootsAgree(hm, currentHome); err != nil {
			return err
		}
	}
	return pruneProfile(opts, filepath.Join(home, ".local/state/nix/profiles/profile"), selector, asUser)
}

// homeManagerRootsAgree requires the Home Manager profile to select a
// numbered generation that current-home also selects.
func homeManagerRootsAgree(profile, currentHome string) error {
	link, err := os.Readlink(profile)
	if err != nil {
		return fmt.Errorf("Home Manager profile cannot be read: %w", err)
	}
	if !regexp.MustCompile(`^home-manager-[0-9]+-link$`).MatchString(link) {
		return fmt.Errorf("Home Manager profile does not select a numbered generation: %s", link)
	}
	a, err := filepath.EvalSymlinks(profile)
	if err != nil {
		return fmt.Errorf("Home Manager profile target is missing: %w", err)
	}
	b, err := filepath.EvalSymlinks(currentHome)
	if err != nil {
		return fmt.Errorf("Home Manager current-home root is missing: %w", err)
	}
	if a != b {
		return fmt.Errorf("Home Manager roots disagree: profile=%s current-home=%s", a, b)
	}
	return nil
}

// generationTarget returns the profile's current store path when it selects
// a numbered generation link, "" when it is absent or not generational.
func generationTarget(profile string) (string, error) {
	info, err := os.Lstat(profile)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("profile is not a symlink: %s", profile)
	}
	link, err := os.Readlink(profile)
	if err != nil {
		return "", err
	}
	name := filepath.Base(profile)
	if !regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `-[0-9]+-link$`).MatchString(link) {
		return "", nil
	}
	return filepath.EvalSymlinks(profile)
}

func pruneProfile(opts PruneOptions, profile, selector string, prefix []string) error {
	before, err := generationTarget(profile)
	if err != nil {
		return err
	}
	if before == "" {
		fmt.Fprintf(opts.Out, "profile-prune: no numbered generations to prune: %s\n", profile)
		return nil
	}
	if opts.ReportOnly {
		fmt.Fprintf(opts.Out, "profile-prune: would run nix-env --profile %s --delete-generations %s\n", profile, selector)
		return nil
	}
	argv := append(append([]string{}, prefix...), opts.NixEnv, "--profile", profile, "--delete-generations", selector)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = opts.Out
	cmd.Stderr = opts.Out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nix-env --profile %s --delete-generations %s: %w", profile, selector, err)
	}
	after, err := generationTarget(profile)
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("profile current generation changed during prune: %s", profile)
	}
	fmt.Fprintf(opts.Out, "profile-prune: pruned %s with %s\n", profile, selector)
	return nil
}
