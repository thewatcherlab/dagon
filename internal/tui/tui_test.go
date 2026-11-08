package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/thewatcherlab/dagon/internal/scaffold"
)

func key(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

func typeText(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// drain runs commands until the wizard stops producing step results.
func drain(m *Model, cmd tea.Cmd) {
	for cmd != nil {
		msg := cmd()
		switch msg := msg.(type) {
		case tea.BatchMsg:
			for _, c := range msg {
				if c == nil {
					continue
				}
				if r, ok := c().(stepMsg); ok {
					_, cmd = m.Update(r)
					drain(m, cmd)
				}
			}
			return
		case stepMsg:
			_, cmd = m.Update(msg)
		default:
			return
		}
	}
}

func newModel(t *testing.T) (*Model, string) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"shop","scripts":{"build":"vite build"},"devDependencies":{"vite":"7"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}"), 0o644)
	p, notes, err := scaffold.Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{Dir: dir, Project: p, Notes: notes}), dir
}

func TestWizardWritesFilesOnly(t *testing.T) {
	m, dir := newModel(t)
	view := m.View().Content
	if !strings.Contains(view, "shop") || !strings.Contains(view, "Static site") || !strings.Contains(view, "Build output") {
		t.Fatalf("first screen:\n%s", view)
	}

	// app, framework, output, node, then the setup choice.
	for i := 0; i < 4; i++ {
		m.Update(key(tea.KeyEnter))
	}
	if m.fields[m.focus].key != "setup" {
		t.Fatalf("focus on %s", m.fields[m.focus].key)
	}
	if !strings.Contains(m.View().Content, "VPS host") {
		t.Fatal("host hidden while setup is yes")
	}
	m.Update(key(tea.KeyRight))
	if strings.Contains(m.View().Content, "VPS host") {
		t.Fatal("host shown while setup is no")
	}
	m.Update(key(tea.KeyEnter))
	if m.phase != phaseReview {
		t.Fatalf("phase %d, err %q", m.phase, m.err)
	}
	review := m.View().Content
	for _, want := range []string{"create", "Dockerfile", "deploy/nginx.conf", ".github/workflows/preview.yml"} {
		if !strings.Contains(review, want) {
			t.Errorf("review lacks %q:\n%s", want, review)
		}
	}
	if strings.Contains(review, "Caddyfile") {
		t.Error("review mentions the VPS without setup")
	}

	_, cmd := m.Update(key(tea.KeyEnter))
	drain(m, cmd)
	if m.phase != phaseDone || m.Err != nil {
		t.Fatalf("phase %d err %v", m.phase, m.Err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".github/workflows/staging.yml")); err != nil {
		t.Fatal(err)
	}
	done := m.View().Content
	if !strings.Contains(done, "Set up the VPS") || !strings.Contains(done, "git add .github Dockerfile .dockerignore deploy") {
		t.Errorf("done screen:\n%s", done)
	}
}

func TestWizardValidates(t *testing.T) {
	m, _ := newModel(t)
	// Clear the app name and try to move on.
	for range "shop" {
		m.Update(key(tea.KeyBackspace))
	}
	typeText(m, "Bad Name")
	m.Update(key(tea.KeyEnter))
	if m.focus != 0 || !strings.Contains(m.View().Content, "lowercase") {
		t.Fatalf("accepted a bad app name:\n%s", m.View().Content)
	}

	// With setup on, a bad domain stops at the domain field.
	for range "Bad Name" {
		m.Update(key(tea.KeyBackspace))
	}
	typeText(m, "shop")
	for i := 0; i < 4; i++ {
		m.Update(key(tea.KeyEnter))
	}
	m.Update(key(tea.KeyEnter)) // setup: yes
	typeText(m, "203.0.113.10")
	m.Update(key(tea.KeyEnter))
	m.Update(key(tea.KeyEnter)) // user: ubuntu
	typeText(m, "not a domain")
	m.Update(key(tea.KeyEnter))
	if m.phase != phaseForm || m.fields[m.focus].key != "apex" || !strings.Contains(m.View().Content, "example.com") {
		t.Fatalf("phase %d focus %s:\n%s", m.phase, m.fields[m.focus].key, m.View().Content)
	}
}

func TestFrameworkSwitchChangesFields(t *testing.T) {
	m, _ := newModel(t)
	m.Update(key(tea.KeyEnter)) // to framework
	m.Update(key(tea.KeyLeft))  // static -> nextjs
	view := m.View().Content
	if strings.Contains(view, "Build output") || !strings.Contains(view, "Next.js") {
		t.Fatalf("next.js view:\n%s", view)
	}
	m.Update(key(tea.KeyLeft)) // nextjs -> custom
	view = m.View().Content
	if !strings.Contains(view, "Dockerfile") || strings.Contains(view, "Node.js version") {
		t.Fatalf("custom view:\n%s", view)
	}
}
