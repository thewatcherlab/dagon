// Command dagon deploys frontend apps to your own VPS with Docker, Caddy,
// and GitHub Actions.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/thewatcherlab/dagon/internal/scaffold"
	"github.com/thewatcherlab/dagon/internal/server"
	"github.com/thewatcherlab/dagon/internal/setup"
	"github.com/thewatcherlab/dagon/internal/tui"
)

var version = "dev"

const usage = `dagon deploys a frontend to your VPS: previews for pull requests,
staging from main, and production from v* tags.

Usage:
  dagon [init] [dir]            Interactive setup for the project in dir (default .)
  dagon setup [flags] [dir]     Prepare the VPS and GitHub without questions
      --host, --user, --apex, --app   default to the saved settings
  dagon rollback <slot> [sha]    Roll staging or production back one release, or to sha
  dagon env edit <env>          Edit encrypted secrets (production, staging, preview)
  dagon env list <env>          Print the secret names
  dagon server-script           Print the script dagon installs on the VPS
  dagon version
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dagon:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "init"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && !isDir(args[0]) {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "init":
		return initCmd(args)
	case "setup":
		return setupCmd(args)
	case "env":
		return envCmd(args)
	case "rollback":
		return rollbackCmd(args)
	case "server-script":
		_, err := os.Stdout.Write(server.Script)
		return err
	case "version":
		fmt.Println("dagon", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", cmd)
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func dirArg(fs *flag.FlagSet) (string, error) {
	dir := "."
	if fs.NArg() > 1 {
		return "", errors.New("too many arguments")
	}
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if !isDir(abs) {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	return abs, nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	workflow := fs.String("workflow", scaffold.DefaultWorkflow, "reusable workflow the generated workflows call")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := dirArg(fs)
	if err != nil {
		return err
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return errors.New("init is interactive; in scripts use: dagon setup --host ... --user ... --apex ...")
	}
	p, notes, err := scaffold.Detect(dir)
	if err != nil {
		return err
	}
	if name := repoName(dir); name != "" {
		p.App = scaffold.AppName(name)
	}
	if p.App == "" {
		p.App = scaffold.AppName(filepath.Base(dir))
	}
	p.Workflow = *workflow
	m := tui.New(tui.Options{Dir: dir, Project: p, Notes: notes, Settings: setup.LoadSettings(p.App)})
	if _, err := tea.NewProgram(m).Run(); err != nil {
		return err
	}
	return m.Err
}

func setupCmd(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	host := fs.String("host", "", "VPS hostname or IP address")
	user := fs.String("user", "", "SSH user that can run sudo without a password")
	apex := fs.String("apex", "", "domain, such as example.com")
	app := fs.String("app", "", "app name (default: the origin repository name)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := dirArg(fs)
	if err != nil {
		return err
	}
	if *app == "" {
		p, _, err := scaffold.Detect(dir)
		if err != nil {
			return err
		}
		*app = or(scaffold.AppName(repoName(dir)), or(p.App, scaffold.AppName(filepath.Base(dir))))
	}
	if !scaffold.ValidApp(*app) {
		return fmt.Errorf("app name %q must be lowercase letters, digits, and dashes", *app)
	}
	saved := setup.LoadSettings(*app)
	c := setup.Config{Dir: dir, App: *app, Host: or(*host, saved.Host), SSHUser: or(*user, saved.SSHUser), Apex: strings.ToLower(or(*apex, saved.Apex))}
	if err := c.Validate(); err != nil {
		return err
	}
	state, err := setup.NewState(c)
	if err != nil {
		return err
	}
	defer state.Close()
	for _, s := range setup.Steps(c) {
		fmt.Fprintf(os.Stderr, "• %s… ", s.Name)
		detail, err := s.Do(state)
		if err != nil {
			fmt.Fprintln(os.Stderr, "failed")
			return err
		}
		fmt.Fprintln(os.Stderr, detail)
	}
	for _, n := range state.Notes {
		fmt.Fprintln(os.Stderr, "!", n)
	}
	fmt.Printf("app %s\n", c.App)
	for _, slot := range []string{"production", "staging"} {
		fmt.Printf("%s https://%s\n", slot, setup.Hostname(slot, c.Apex))
	}
	fmt.Printf("preview https://pr-<number>.%s\n", c.Apex)
	fmt.Fprintln(os.Stderr, "Commit .sops.yaml and .infra/ so GitHub can deploy with them.")
	return nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func envCmd(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: dagon env edit|list production|staging|preview")
	}
	sub, env := args[0], args[1]
	switch env {
	case "production", "staging", "preview":
	default:
		return fmt.Errorf("unknown environment %q", env)
	}
	root, err := gitRoot()
	if err != nil {
		return err
	}
	rel := ".infra/env/" + env + ".enc.env"
	if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
		return fmt.Errorf("missing %s; run dagon init first", rel)
	}
	if _, err := exec.LookPath("sops"); err != nil {
		return errors.New("sops is missing; run: brew install sops age")
	}
	switch sub {
	case "edit":
		c := exec.Command("sops", rel)
		c.Dir, c.Stdin, c.Stdout, c.Stderr = root, os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	case "list":
		c := exec.Command("sops", "--decrypt", rel)
		c.Dir, c.Stderr = root, os.Stderr
		out, err := c.Output()
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(out), "\n") {
			if k, _, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(strings.TrimSpace(k), "#") {
				fmt.Println(k)
			}
		}
		return nil
	}
	return fmt.Errorf("unknown env command %q", sub)
}

func gitRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", errors.New("run this inside your app's git repository")
	}
	return strings.TrimSpace(string(out)), nil
}

// repoName is the repository name of origin, which names the app by default.
func repoName(dir string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	url := strings.TrimSuffix(strings.TrimSpace(string(out)), "/")
	url = strings.TrimSuffix(url, ".git")
	if i := strings.LastIndexAny(url, "/:"); i >= 0 {
		url = url[i+1:]
	}
	return url
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// rollbackCmd starts the rollback workflow. The VPS picks the commit when
// none is given, so that each run steps one release further back.
func rollbackCmd(args []string) error {
	if len(args) < 1 || len(args) > 2 || (args[0] != "production" && args[0] != "staging") {
		return errors.New("usage: dagon rollback production|staging [sha]")
	}
	sha := ""
	if len(args) == 2 {
		sha = args[1]
		if !shaRe.MatchString(sha) {
			return errors.New("sha must be a full 40-character commit SHA")
		}
	}
	root, err := gitRoot()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, ".github", "workflows", "rollback.yml")); err != nil {
		return errors.New("missing .github/workflows/rollback.yml; run dagon init and push the new workflow first")
	}
	c := exec.Command("gh", "workflow", "run", "rollback.yml", "-f", "slot="+args[0], "-f", "sha="+sha)
	c.Dir, c.Stdout, c.Stderr = root, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Started the rollback of "+args[0]+". Follow it with: gh run watch")
	return nil
}
