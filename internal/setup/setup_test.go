package setup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thewatcherlab/dagon/internal/server"
)

// fakes puts stand-ins for ssh, gh, sops, and the key tools on PATH. Each
// logs its arguments to $LOG; ssh also saves its stdin.
func fakes(t *testing.T) (logPath, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fakes")
	}
	bin := t.TempDir()
	dir = t.TempDir()
	logPath = filepath.Join(t.TempDir(), "log")
	scripts := map[string]string{
		"ssh": `printf 'ssh %s\n' "$*" >> "$LOG"
cat > "$LOG.stdin"
case "${@: -1}" in
  *install*) printf 'apt noise\nAGE_production=age1prod\nAGE_staging=age1stag\nAGE_preview=age1prev\n' ;;
esac`,
		"ssh-keyscan": `printf '%s ssh-ed25519 AAAAhost\n' "${@: -1}"`,
		"ssh-keygen": `printf 'ssh-keygen %s\n' "$*" >> "$LOG"
f=${@: -1}; printf 'PRIVATE\n' > "$f"; printf 'ssh-ed25519 AAAAactions dagon-actions-demo\n' > "$f.pub"`,
		"age-keygen": `if [ "$1" = "-y" ]; then echo age1user; else printf 'AGE-SECRET-KEY-1\n' > "$2"; fi`,
		"gh": `case "$1 $2" in
  "auth status") exit 0 ;;
  "repo view") echo Acme/Demo-Site ;;
  "secret set") printf 'gh secret %s=%s\n' "$3" "$(cat | head -n 1)" >> "$LOG" ;;
esac`,
		"sops": `printf 'sops %s\n' "$*" >> "$LOG"
f=${@: -1}; [ "$1" = "encrypt" ] && printf 'ENC[%s]\n' "$(cat "$f")" > "$f"; exit 0`,
		"git": `exit 0`,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LOG", logPath)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("SOPS_AGE_KEY_FILE", "")
	return logPath, dir
}

func runAll(t *testing.T, c Config) *State {
	t.Helper()
	s, err := NewState(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, step := range Steps(c) {
		if _, err := step.Do(s); err != nil {
			t.Fatalf("%s: %v", step.Name, err)
		}
	}
	return s
}

func TestSetupEndToEnd(t *testing.T) {
	logPath, dir := fakes(t)
	c := Config{Dir: dir, App: "demo", Host: "203.0.113.10", SSHUser: "ubuntu", Apex: "example.com"}
	s := runAll(t, c)

	log, _ := os.ReadFile(logPath)
	got := string(log)
	for _, want := range []string{
		"ssh -o BatchMode=yes",
		"ubuntu@203.0.113.10 sudo -n true",
		`sudo -n bash "$w/dagon" install 'demo' 'example.com' 'ghcr.io/acme/demo-site' "$w"`,
		"printf '%s\\n' 'ssh-ed25519 AAAAactions dagon-actions-demo'",
		"gh secret SSH_HOST=203.0.113.10",
		"gh secret SSH_USER=deploy",
		"gh secret SSH_PRIVATE_KEY=PRIVATE",
		"gh secret SSH_KNOWN_HOSTS=203.0.113.10 ssh-ed25519 AAAAhost",
		"sops encrypt --in-place .infra/env/preview.enc.env",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q\n%s", want, got)
		}
	}
	if stdin, _ := os.ReadFile(logPath + ".stdin"); string(stdin) != string(server.Script) {
		t.Error("ssh did not receive the server script on stdin")
	}

	sops, _ := os.ReadFile(filepath.Join(dir, ".sops.yaml"))
	for _, want := range []string{
		".infra/env/production\\.enc\\.env$\n    age: age1prod,age1user",
		".infra/env/staging\\.enc\\.env$\n    age: age1stag,age1user",
		".infra/env/preview\\.enc\\.env$\n    age: age1prev,age1user",
	} {
		if !strings.Contains(string(sops), want) {
			t.Errorf(".sops.yaml lacks %q\n%s", want, sops)
		}
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".infra/env/staging.enc.env"))
	if string(env) != "ENC[APP_ENV=staging]\n" {
		t.Errorf("staging env %q", env)
	}
	if len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "Back it up") {
		t.Errorf("notes %v", s.Notes)
	}
	if got := LoadSettings("demo"); got != (Settings{Host: "203.0.113.10", SSHUser: "ubuntu", Apex: "example.com"}) {
		t.Errorf("settings %+v", got)
	}

	// A second run keeps the keys and re-encrypts instead of overwriting.
	os.WriteFile(logPath, nil, 0o644)
	s = runAll(t, c)
	log, _ = os.ReadFile(logPath)
	if strings.Contains(string(log), "ssh-keygen") || strings.Contains(string(log), "sops encrypt") {
		t.Errorf("second run made new keys or files:\n%s", log)
	}
	if !strings.Contains(string(log), "sops updatekeys -y .infra/env/production.enc.env") {
		t.Errorf("second run did not update keys:\n%s", log)
	}
	if len(s.Notes) != 0 {
		t.Errorf("second run notes %v", s.Notes)
	}
}

func TestSetupReusesLegacyKey(t *testing.T) {
	logPath, dir := fakes(t)
	home := os.Getenv("HOME")
	legacy := filepath.Join(home, ".config", "sanbox-pipeline", "demo")
	os.MkdirAll(legacy, 0o700)
	os.WriteFile(filepath.Join(legacy, "actions_ed25519"), []byte("LEGACY\n"), 0o600)
	os.WriteFile(filepath.Join(legacy, "actions_ed25519.pub"), []byte("ssh-ed25519 AAAAold\n"), 0o644)
	os.WriteFile(filepath.Join(legacy, "setup.env"), []byte("PIPELINE_HOST=198.51.100.7\nPIPELINE_SSH_USER=root\nPIPELINE_APEX=old.example\n"), 0o600)

	if got := LoadSettings("demo"); got.Host != "198.51.100.7" || got.SSHUser != "root" || got.Apex != "old.example" {
		t.Fatalf("legacy settings %+v", got)
	}
	runAll(t, Config{Dir: dir, App: "demo", Host: "198.51.100.7", SSHUser: "root", Apex: "old.example"})
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "ssh-keygen") || !strings.Contains(string(log), "gh secret SSH_PRIVATE_KEY=LEGACY") {
		t.Errorf("did not reuse the legacy key:\n%s", log)
	}
}

func TestSetupStopsWhenSudoFails(t *testing.T) {
	_, dir := fakes(t)
	bin := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/bash\necho 'sudo: a password is required' >&2\nexit 1\n"), 0o755)
	c := Config{Dir: dir, App: "demo", Host: "203.0.113.10", SSHUser: "ubuntu", Apex: "example.com"}
	s, _ := NewState(c)
	defer s.Close()
	steps := Steps(c)
	for _, step := range steps[:2] {
		if _, err := step.Do(s); err != nil {
			t.Fatal(err)
		}
	}
	_, err := steps[2].Do(s)
	if err == nil || !strings.Contains(err.Error(), "a password is required") {
		t.Fatalf("got %v", err)
	}
}

func TestValidate(t *testing.T) {
	good := Config{Host: "vps.example.com", SSHUser: "ubuntu", Apex: "example.com"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Config{
		{Host: "-oProxyCommand=x", SSHUser: "ubuntu", Apex: "example.com"},
		{Host: "1.2.3.4", SSHUser: "a b", Apex: "example.com"},
		{Host: "1.2.3.4", SSHUser: "ubuntu", Apex: "localhost"},
		{Host: "1.2.3.4; rm", SSHUser: "ubuntu", Apex: "example.com"},
	} {
		if c.Validate() == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	if quote("it's") != `'it'\''s'` {
		t.Error("quote")
	}
	if Hostname("production", "a.io") != "a.io" || Hostname("pr-3", "a.io") != "pr-3.a.io" {
		t.Error("hostname")
	}
}
