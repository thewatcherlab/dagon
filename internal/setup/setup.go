// Package setup prepares the VPS and the repository for an app: it installs
// the dagon server, creates the keys, encrypts the secrets, and sets the
// GitHub secrets the deploy workflow reads.
package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/thewatcherlab/dagon/internal/server"
)

// Environments each have their own server key, so a preview cannot decrypt
// staging or production secrets.
var Environments = []string{"production", "staging", "preview"}

var apexRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
var hostRe = regexp.MustCompile(`^[A-Za-z0-9.:-]+$`)
var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_.-]*$`)

// ValidApex reports whether s is a domain like example.com.
func ValidApex(s string) bool { return apexRe.MatchString(s) }

// ValidHost reports whether s looks like a hostname or an IP address.
func ValidHost(s string) bool { return hostRe.MatchString(s) && !strings.HasPrefix(s, "-") }

// ValidUser reports whether s looks like a Unix user name.
func ValidUser(s string) bool { return userRe.MatchString(s) }

// Config is what setup needs to know.
type Config struct {
	Dir     string // the app's repository
	App     string
	Host    string
	SSHUser string
	Apex    string
}

// Validate checks the config before anything runs.
func (c Config) Validate() error {
	switch {
	case !ValidHost(c.Host):
		return errors.New("VPS host must be a hostname or an IP address")
	case !ValidUser(c.SSHUser):
		return errors.New("SSH user must be a user name such as ubuntu")
	case !ValidApex(c.Apex):
		return errors.New("domain must look like example.com")
	}
	return nil
}

// Hostname is where a slot is served.
func Hostname(slot, apex string) string {
	if slot == "production" {
		return apex
	}
	return slot + "." + apex
}

// Settings are remembered per app so setup can run again without questions.
type Settings struct {
	Host    string `json:"host"`
	SSHUser string `json:"ssh_user"`
	Apex    string `json:"apex"`
}

func configDir(app string) string {
	base, err := os.UserConfigDir()
	if err != nil || runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "dagon", app)
}

// legacyDir is where the shell version of the pipeline kept its files.
func legacyDir(app string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "sanbox-pipeline", app)
}

// LoadSettings returns the saved settings for app, if any.
func LoadSettings(app string) Settings {
	var s Settings
	if b, err := os.ReadFile(filepath.Join(configDir(app), "settings.json")); err == nil {
		_ = json.Unmarshal(b, &s)
		return s
	}
	if b, err := os.ReadFile(filepath.Join(legacyDir(app), "setup.env")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
			switch k {
			case "PIPELINE_HOST":
				s.Host = v
			case "PIPELINE_SSH_USER":
				s.SSHUser = v
			case "PIPELINE_APEX":
				s.Apex = v
			}
		}
	}
	return s
}

// UserAgeKey is where sops looks for your age key.
func UserAgeKey() string {
	if p := os.Getenv("SOPS_AGE_KEY_FILE"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "sops", "age", "keys.txt")
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "sops", "age", "keys.txt")
	}
	return filepath.Join(home, ".config", "sops", "age", "keys.txt")
}

// Step is one stage of setup. Do returns a short detail for the user.
type Step struct {
	Name string
	Do   func(*State) (string, error)
}

// State carries values from one step to the next.
type State struct {
	Config
	Repo       string // owner/name on GitHub
	work       string
	knownHosts string
	actionsKey string
	userPub    string
	serverPubs map[string]string
	Notes      []string // things the user must know after setup
}

// NewState makes a scratch directory for one setup run. Call Close after.
func NewState(c Config) (*State, error) {
	work, err := os.MkdirTemp("", "dagon-setup-")
	if err != nil {
		return nil, err
	}
	return &State{Config: c, work: work, serverPubs: map[string]string{}}, nil
}

// Close removes the scratch directory.
func (s *State) Close() { os.RemoveAll(s.work) }

// Steps returns the setup steps in order.
func Steps(c Config) []Step {
	return []Step{
		{"Check local tools", checkTools},
		{"Find the GitHub repository", findRepo},
		{"Connect to " + c.Host, connect},
		{"Prepare keys", prepareKeys},
		{"Install dagon on the VPS", install},
		{"Encrypt the secrets", encryptSecrets},
		{"Set the GitHub secrets", setSecrets},
		{"Save settings", saveSettings},
	}
}

// run runs a command and returns its stdout. Errors carry the end of stderr.
func run(dir string, stdin []byte, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w%s", name, err, tail(errb.String(), 6))
	}
	return out.String(), nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	joined := strings.TrimSpace(strings.Join(lines, "\n"))
	if joined == "" {
		return ""
	}
	return "\n" + joined
}

func (s *State) ssh(stdin []byte, remote string) (string, error) {
	return run(s.Dir, stdin, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile="+s.knownHosts,
		s.SSHUser+"@"+s.Host, remote)
}

func checkTools(s *State) (string, error) {
	var missing []string
	for _, tool := range []string{"gh", "git", "ssh", "ssh-keyscan", "ssh-keygen", "sops", "age-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("missing %s; on macOS run: brew install gh sops age", strings.Join(missing, ", "))
	}
	if _, err := run(s.Dir, nil, "gh", "auth", "status"); err != nil {
		return "", errors.New("gh is not signed in; run: gh auth login")
	}
	return "gh, sops, age, ssh", nil
}

func findRepo(s *State) (string, error) {
	out, err := run(s.Dir, nil, "gh", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		return "", fmt.Errorf("this folder is not a GitHub repository: %w", err)
	}
	s.Repo = strings.TrimSpace(out)
	if !regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`).MatchString(s.Repo) {
		return "", fmt.Errorf("unexpected repository name %q", s.Repo)
	}
	return s.Repo, nil
}

func connect(s *State) (string, error) {
	out, err := run(s.Dir, nil, "ssh-keyscan", "-T", "10", s.Host)
	if err != nil || strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("cannot reach %s on port 22", s.Host)
	}
	s.knownHosts = filepath.Join(s.work, "known_hosts")
	if err := os.WriteFile(s.knownHosts, []byte(out), 0o600); err != nil {
		return "", err
	}
	if _, err := s.ssh(nil, "sudo -n true"); err != nil {
		return "", fmt.Errorf("%s@%s must log in with your SSH key and run sudo without a password: %w", s.SSHUser, s.Host, err)
	}
	return s.SSHUser + "@" + s.Host + " can sudo", nil
}

func prepareKeys(s *State) (string, error) {
	dir := configDir(s.App)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	s.actionsKey = filepath.Join(dir, "actions_ed25519")
	if _, err := os.Stat(s.actionsKey); err != nil {
		// Reuse the key of the shell pipeline, so the GitHub secret stays valid.
		legacy := filepath.Join(legacyDir(s.App), "actions_ed25519")
		if copyFile(legacy, s.actionsKey) != nil || copyFile(legacy+".pub", s.actionsKey+".pub") != nil {
			os.Remove(s.actionsKey)
			if _, err := run(dir, nil, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "dagon-actions-"+s.App, "-f", s.actionsKey); err != nil {
				return "", err
			}
		}
	}

	key := UserAgeKey()
	if _, err := os.Stat(key); err != nil {
		if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
			return "", err
		}
		if _, err := run(s.Dir, nil, "age-keygen", "-o", key); err != nil {
			return "", err
		}
		os.Chmod(key, 0o600)
		s.Notes = append(s.Notes, "Created your age key at "+key+". Back it up: without it you cannot edit the secrets.")
	}
	pub, err := run(s.Dir, nil, "age-keygen", "-y", key)
	if err != nil {
		return "", fmt.Errorf("cannot read your age key %s: %w", key, err)
	}
	s.userPub = firstLine(pub)
	if !strings.HasPrefix(s.userPub, "age1") {
		return "", fmt.Errorf("%s has no age key", key)
	}
	return "deploy key and age key ready", nil
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o600)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

// quote makes s one word for the remote shell.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Image is the GHCR repository the deploy workflow pushes to.
func Image(repo string) string { return "ghcr.io/" + strings.ToLower(repo) }

func install(s *State) (string, error) {
	pub, err := os.ReadFile(s.actionsKey + ".pub")
	if err != nil {
		return "", err
	}
	remote := strings.Join([]string{
		`set -e`,
		`w=$(mktemp -d)`,
		`cat > "$w/dagon"`,
		`printf '%s\n' ` + quote(firstLine(string(pub))) + ` > "$w/actions.pub"`,
		`sudo -n bash "$w/dagon" install ` + strings.Join([]string{quote(s.App), quote(s.Apex), quote(Image(s.Repo))}, " ") + ` "$w"`,
	}, "; ")
	out, err := s.ssh(server.Script, remote)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.HasPrefix(k, "AGE_") && strings.HasPrefix(v, "age1") {
			s.serverPubs[strings.TrimPrefix(k, "AGE_")] = v
		}
	}
	for _, env := range Environments {
		if s.serverPubs[env] == "" {
			return "", fmt.Errorf("the VPS did not return the %s key", env)
		}
	}
	return "Docker, Caddy, sops, and app " + s.App, nil
}

// SopsConfig is .sops.yaml: each file is readable by you and one server key.
func SopsConfig(userPub string, serverPubs map[string]string) string {
	var b strings.Builder
	b.WriteString("# Written by dagon. Each environment has its own server key, so a pull\n")
	b.WriteString("# request preview cannot decrypt staging or production secrets.\n")
	b.WriteString("creation_rules:\n")
	for _, env := range Environments {
		fmt.Fprintf(&b, "  - path_regex: .infra/env/%s\\.enc\\.env$\n    age: %s,%s\n", env, serverPubs[env], userPub)
	}
	return b.String()
}

func encryptSecrets(s *State) (string, error) {
	if err := os.WriteFile(filepath.Join(s.Dir, ".sops.yaml"), []byte(SopsConfig(s.userPub, s.serverPubs)), 0o644); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(s.Dir, ".infra", "env"), 0o755); err != nil {
		return "", err
	}
	var created []string
	for _, env := range Environments {
		rel := ".infra/env/" + env + ".enc.env"
		path := filepath.Join(s.Dir, rel)
		if _, err := os.Stat(path); err == nil {
			if _, err := run(s.Dir, nil, "sops", "updatekeys", "-y", rel); err != nil {
				return "", fmt.Errorf("sops updatekeys %s: %w", rel, err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte("APP_ENV="+env+"\n"), 0o600); err != nil {
			return "", err
		}
		if _, err := run(s.Dir, nil, "sops", "encrypt", "--in-place", rel); err != nil {
			os.Remove(path)
			return "", fmt.Errorf("sops encrypt %s: %w", rel, err)
		}
		os.Chmod(path, 0o644)
		created = append(created, env)
	}
	if len(created) > 0 {
		return "created " + strings.Join(created, ", "), nil
	}
	return "re-encrypted for the current keys", nil
}

func setSecrets(s *State) (string, error) {
	key, err := os.ReadFile(s.actionsKey)
	if err != nil {
		return "", err
	}
	known, err := os.ReadFile(s.knownHosts)
	if err != nil {
		return "", err
	}
	for _, sec := range []struct {
		name string
		body []byte
	}{
		{"SSH_HOST", []byte(s.Host)},
		{"SSH_USER", []byte("deploy")},
		{"SSH_PRIVATE_KEY", key},
		{"SSH_KNOWN_HOSTS", known},
	} {
		if _, err := run(s.Dir, sec.body, "gh", "secret", "set", sec.name, "--repo", s.Repo); err != nil {
			return "", err
		}
	}
	return "SSH_HOST, SSH_USER, SSH_PRIVATE_KEY, SSH_KNOWN_HOSTS", nil
}

func saveSettings(s *State) (string, error) {
	b, _ := json.MarshalIndent(Settings{Host: s.Host, SSHUser: s.SSHUser, Apex: s.Apex}, "", "  ")
	path := filepath.Join(configDir(s.App), "settings.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
