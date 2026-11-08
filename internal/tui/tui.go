// Package tui is the interactive `dagon init` wizard, built on Bubble Tea.
package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/thewatcherlab/dagon/internal/scaffold"
	"github.com/thewatcherlab/dagon/internal/setup"
)

var (
	accent  = lipgloss.Color("12")
	good    = lipgloss.Color("10")
	bad     = lipgloss.Color("9")
	warn    = lipgloss.Color("11")
	muted   = lipgloss.Color("8")
	title   = lipgloss.NewStyle().Bold(true).Foreground(accent)
	label   = lipgloss.NewStyle().Bold(true)
	dim     = lipgloss.NewStyle().Foreground(muted)
	okStyle = lipgloss.NewStyle().Foreground(good)
	errSty  = lipgloss.NewStyle().Foreground(bad)
	warnSty = lipgloss.NewStyle().Foreground(warn)
	pick    = lipgloss.NewStyle().Foreground(accent).Bold(true)
)

type phase int

const (
	phaseForm phase = iota
	phaseReview
	phaseRun
	phaseDone
)

type fieldKind int

const (
	textField fieldKind = iota
	choiceField
)

type field struct {
	key     string
	label   string
	help    string
	kind    fieldKind
	input   textinput.Model
	choices []string
	choice  int
	show    func(m *Model) bool
	check   func(string) error
}

func (f *field) value() string {
	if f.kind == choiceField {
		return f.choices[f.choice]
	}
	return strings.TrimSpace(f.input.Value())
}

type stepStatus int

const (
	pending stepStatus = iota
	running
	done
	failed
)

type step struct {
	name   string
	status stepStatus
	detail string
	do     func() (string, error)
}

type stepMsg struct {
	index  int
	detail string
	err    error
}

// Options are the starting answers for the wizard.
type Options struct {
	Dir      string
	Project  scaffold.Project
	Notes    []string
	Settings setup.Settings
}

// Model is the Bubble Tea model of the wizard.
type Model struct {
	opts   Options
	phase  phase
	fields []*field
	focus  int
	err    string
	width  int

	files   []scaffold.File
	actions []scaffold.Action
	project scaffold.Project
	config  setup.Config
	doSetup bool

	steps []step
	spin  spinner.Model
	state *setup.State

	// Err is set when the wizard ends without finishing.
	Err error
}

const (
	setupYes = "Yes: set up the VPS and GitHub now"
	setupNo  = "No: only write the files"
)

// New builds the wizard.
func New(opts Options) *Model {
	m := &Model{opts: opts}
	p := opts.Project
	s := opts.Settings

	text := func(key, lbl, help, value, placeholder string, check func(string) error) *field {
		in := textinput.New()
		in.Prompt = ""
		in.Placeholder = placeholder
		in.SetValue(value)
		in.SetWidth(48)
		return &field{key: key, label: lbl, help: help, kind: textField, input: in, check: check}
	}
	fw := &field{key: "framework", label: "Framework", help: "How the app is built and served. ←/→ to change.", kind: choiceField}
	for i, f := range scaffold.Frameworks {
		fw.choices = append(fw.choices, f.Label())
		if f == p.Framework {
			fw.choice = i
		}
	}
	isFramework := func(fs ...scaffold.Framework) func(*Model) bool {
		return func(m *Model) bool {
			cur := m.framework()
			for _, f := range fs {
				if f == cur {
					return true
				}
			}
			return false
		}
	}
	wantSetup := func(m *Model) bool { return m.field("setup").value() == setupYes }
	nonEmpty := func(msg string) func(string) error {
		return func(v string) error {
			if v == "" {
				return fmt.Errorf("%s", msg)
			}
			return nil
		}
	}
	output := text("output", "Build output", "The folder your build writes, served as static files.", p.OutputDir, "dist", nonEmpty("enter the build folder"))
	output.show = isFramework(scaffold.Static)
	node := text("node", "Node.js version", "Major version for the build image.", p.Node, "22", nil)
	node.show = isFramework(scaffold.NextJS, scaffold.Static, scaffold.Node)
	dockerfile := text("dockerfile", "Dockerfile", "Path of your Dockerfile. The container must serve HTTP on port 3000.", "Dockerfile", "Dockerfile", nonEmpty("enter the Dockerfile path"))
	dockerfile.show = isFramework(scaffold.Custom)
	host := text("host", "VPS host", "IP address or hostname of an Ubuntu or Debian server.", s.Host, "203.0.113.10", func(v string) error {
		if !setup.ValidHost(v) {
			return fmt.Errorf("enter a hostname or an IP address")
		}
		return nil
	})
	host.show = wantSetup
	user := text("user", "SSH user", "Logs in with your SSH key and can run sudo without a password.", or(s.SSHUser, "ubuntu"), "ubuntu", func(v string) error {
		if !setup.ValidUser(v) {
			return fmt.Errorf("enter a user name such as ubuntu")
		}
		return nil
	})
	user.show = wantSetup
	apex := text("apex", "Domain", "Point the domain and *.domain at the VPS with two A records.", s.Apex, "example.com", func(v string) error {
		if !setup.ValidApex(strings.ToLower(v)) {
			return fmt.Errorf("enter a domain such as example.com")
		}
		return nil
	})
	apex.show = wantSetup

	m.fields = []*field{
		text("app", "App name", "Names the containers on the VPS. Lowercase letters, digits, and dashes.", p.App, "my-site", func(v string) error {
			if !scaffold.ValidApp(v) {
				return fmt.Errorf("use 2-40 lowercase letters, digits, or dashes, starting with a letter")
			}
			return nil
		}),
		fw, output, node, dockerfile,
		{key: "setup", label: "Set up the VPS", help: "Installs Docker, Caddy, and sops, and sets the GitHub secrets. ←/→ to change.", kind: choiceField, choices: []string{setupYes, setupNo}},
		host, user, apex,
	}
	m.spin = spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(lipgloss.NewStyle().Foreground(accent)))
	m.focusField(0)
	return m
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (m *Model) field(key string) *field {
	for _, f := range m.fields {
		if f.key == key {
			return f
		}
	}
	panic("no field " + key)
}

func (m *Model) framework() scaffold.Framework {
	return scaffold.Frameworks[m.field("framework").choice]
}

func (m *Model) visible(i int) bool {
	f := m.fields[i]
	return f.show == nil || f.show(m)
}

func (m *Model) focusField(i int) tea.Cmd {
	for _, f := range m.fields {
		if f.kind == textField {
			f.input.Blur()
		}
	}
	m.focus = i
	if f := m.fields[i]; f.kind == textField {
		f.input.CursorEnd()
		return f.input.Focus()
	}
	return nil
}

// move focuses the next visible field in direction dir. It reports false at the ends.
func (m *Model) move(dir int) (tea.Cmd, bool) {
	for i := m.focus + dir; i >= 0 && i < len(m.fields); i += dir {
		if m.visible(i) {
			return m.focusField(i), true
		}
	}
	return nil, false
}

func (m *Model) checkField(i int) error {
	f := m.fields[i]
	if f.check == nil || !m.visible(i) {
		return nil
	}
	return f.check(f.value())
}

// Init starts the wizard.
func (m *Model) Init() tea.Cmd { return textinput.Blink }

// Update handles one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			if m.phase == phaseRun {
				return m, nil // let the current step finish; it may be mid-install
			}
			m.Err = fmt.Errorf("cancelled")
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case stepMsg:
		return m.stepDone(msg)
	}
	switch m.phase {
	case phaseForm:
		return m.updateForm(msg)
	case phaseReview:
		if k, ok := msg.(tea.KeyPressMsg); ok {
			switch k.String() {
			case "enter", "y":
				return m.start()
			case "esc", "n", "backspace":
				m.phase = phaseForm
				return m, m.focusField(m.focus)
			case "q":
				m.Err = fmt.Errorf("cancelled")
				return m, tea.Quit
			}
		}
	case phaseDone:
		if k, ok := msg.(tea.KeyPressMsg); ok {
			switch k.String() {
			case "enter", "q", "esc":
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m *Model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	f := m.fields[m.focus]
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "up", "shift+tab":
			m.err = ""
			cmd, _ := m.move(-1)
			return m, cmd
		case "down", "tab", "enter":
			if err := m.checkField(m.focus); err != nil {
				m.err = err.Error()
				return m, nil
			}
			m.err = ""
			if cmd, ok := m.move(1); ok {
				return m, cmd
			}
			if k.String() == "enter" {
				return m.review()
			}
			return m, nil
		case "esc":
			m.Err = fmt.Errorf("cancelled")
			return m, tea.Quit
		}
		if f.kind == choiceField {
			switch k.String() {
			case "left", "h":
				f.choice = (f.choice + len(f.choices) - 1) % len(f.choices)
			case "right", "l", "space":
				f.choice = (f.choice + 1) % len(f.choices)
			}
			return m, nil
		}
	}
	if f.kind == textField {
		var cmd tea.Cmd
		f.input, cmd = f.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// review checks every answer and prepares the plan.
func (m *Model) review() (tea.Model, tea.Cmd) {
	for i := range m.fields {
		if err := m.checkField(i); err != nil {
			m.err = err.Error()
			return m, m.focusField(i)
		}
	}
	p := m.opts.Project
	p.App = m.field("app").value()
	p.Framework = m.framework()
	p.OutputDir = m.field("output").value()
	p.Node = m.field("node").value()
	p.Dockerfile = "Dockerfile"
	if p.Framework == scaffold.Custom {
		p.Dockerfile = m.field("dockerfile").value()
	}
	files, err := p.Files()
	if err != nil {
		m.err = err.Error()
		return m, nil
	}
	m.project = p
	m.files = files
	m.actions = scaffold.Plan(m.opts.Dir, files)
	m.doSetup = m.field("setup").value() == setupYes
	m.config = setup.Config{
		Dir:     m.opts.Dir,
		App:     p.App,
		Host:    m.field("host").value(),
		SSHUser: m.field("user").value(),
		Apex:    strings.ToLower(m.field("apex").value()),
	}
	if m.doSetup {
		if err := m.config.Validate(); err != nil {
			m.err = err.Error()
			return m, nil
		}
	}
	m.err = ""
	m.phase = phaseReview
	return m, nil
}

// start runs the steps one by one.
func (m *Model) start() (tea.Model, tea.Cmd) {
	m.phase = phaseRun
	m.steps = []step{{name: "Write the project files", do: func() (string, error) {
		actions, err := scaffold.Write(m.opts.Dir, m.files)
		m.actions = actions
		if err != nil {
			return "", err
		}
		n := 0
		for _, a := range actions {
			if a == scaffold.Create || a == scaffold.Update {
				n++
			}
		}
		return fmt.Sprintf("%d written", n), nil
	}}}
	if m.doSetup {
		state, err := setup.NewState(m.config)
		if err != nil {
			m.Err = err
			return m, tea.Quit
		}
		m.state = state
		for _, s := range setup.Steps(m.config) {
			do := s.Do
			m.steps = append(m.steps, step{name: s.Name, do: func() (string, error) { return do(state) }})
		}
	}
	return m, tea.Batch(m.spin.Tick, m.runStep(0))
}

func (m *Model) runStep(i int) tea.Cmd {
	m.steps[i].status = running
	do := m.steps[i].do
	return func() tea.Msg {
		detail, err := do()
		return stepMsg{index: i, detail: detail, err: err}
	}
}

func (m *Model) stepDone(msg stepMsg) (tea.Model, tea.Cmd) {
	s := &m.steps[msg.index]
	s.detail = msg.detail
	if msg.err != nil {
		s.status = failed
		s.detail = msg.err.Error()
		m.Err = msg.err
		m.finish()
		return m, nil
	}
	s.status = done
	if next := msg.index + 1; next < len(m.steps) {
		return m, m.runStep(next)
	}
	m.finish()
	return m, nil
}

func (m *Model) finish() {
	m.phase = phaseDone
	if m.state != nil {
		m.state.Close()
	}
}

// View renders the wizard.
func (m *Model) View() tea.View {
	var b strings.Builder
	b.WriteString(title.Render("dagon") + dim.Render("  deploy this frontend to your VPS") + "\n\n")
	switch m.phase {
	case phaseForm:
		m.viewForm(&b)
	case phaseReview:
		m.viewReview(&b)
	case phaseRun, phaseDone:
		m.viewRun(&b)
	}
	out := b.String()
	if m.width > 0 {
		out = lipgloss.NewStyle().MaxWidth(m.width).Render(out)
	}
	return tea.NewView(out)
}

func (m *Model) viewForm(b *strings.Builder) {
	if len(m.opts.Notes) > 0 {
		for _, n := range m.opts.Notes {
			b.WriteString(warnSty.Render("! ") + n + "\n")
		}
		b.WriteString("\n")
	}
	for i, f := range m.fields {
		if !m.visible(i) {
			continue
		}
		focused := i == m.focus
		cursor := "  "
		if focused {
			cursor = pick.Render("› ")
		}
		b.WriteString(cursor + label.Render(fmt.Sprintf("%-16s", f.label)))
		if f.kind == choiceField {
			if focused {
				b.WriteString(pick.Render("‹ ") + f.choices[f.choice] + pick.Render(" ›"))
			} else {
				b.WriteString(f.choices[f.choice])
			}
		} else {
			b.WriteString(f.input.View())
		}
		b.WriteString("\n")
		if focused {
			help := dim
			if m.width > 30 {
				help = dim.Width(m.width - 18)
			}
			for _, l := range strings.Split(help.Render(f.help), "\n") {
				b.WriteString("                  " + l + "\n")
			}
		}
	}
	b.WriteString("\n")
	if m.err != "" {
		b.WriteString(errSty.Render("✗ "+m.err) + "\n\n")
	}
	b.WriteString(dim.Render("↑/↓ move · ←/→ choose · enter continue · esc quit"))
}

func actionStyle(a scaffold.Action) string {
	switch a {
	case scaffold.Create:
		return okStyle.Render("create   ")
	case scaffold.Update:
		return warnSty.Render("update   ")
	case scaffold.Kept:
		return dim.Render("keep     ")
	}
	return dim.Render("unchanged")
}

func (m *Model) viewReview(b *strings.Builder) {
	b.WriteString(label.Render("Files") + dim.Render("  in "+m.opts.Dir) + "\n")
	for i, f := range m.files {
		b.WriteString("  " + actionStyle(m.actions[i]) + " " + f.Path + "\n")
	}
	b.WriteString(dim.Render("  keep = you have your own version, so dagon leaves it alone") + "\n\n")
	if m.doSetup {
		c := m.config
		b.WriteString(label.Render("On "+c.SSHUser+"@"+c.Host) + "\n")
		b.WriteString("  install Docker, Caddy, and sops, and register app " + c.App + "\n")
		b.WriteString("  replace /etc/caddy/Caddyfile with one that imports sites/ (a backup is kept)\n\n")
		b.WriteString(label.Render("In this repository and on GitHub") + "\n")
		b.WriteString("  write .sops.yaml and .infra/env/*.enc.env\n")
		b.WriteString("  set the SSH_HOST, SSH_USER, SSH_PRIVATE_KEY, and SSH_KNOWN_HOSTS secrets\n\n")
		b.WriteString(label.Render("URLs") + "\n")
		for _, slot := range []string{"production", "staging"} {
			b.WriteString(fmt.Sprintf("  %-11s https://%s\n", slot, setup.Hostname(slot, c.Apex)))
		}
		b.WriteString(fmt.Sprintf("  %-11s https://pr-<number>.%s\n\n", "preview", c.Apex))
	}
	b.WriteString(dim.Render("enter apply · esc back · q quit"))
}

func (m *Model) viewRun(b *strings.Builder) {
	for _, s := range m.steps {
		var icon string
		switch s.status {
		case pending:
			icon = dim.Render("·")
		case running:
			icon = m.spin.View()
		case done:
			icon = okStyle.Render("✓")
		case failed:
			icon = errSty.Render("✗")
		}
		line := icon + " " + s.name
		if s.detail != "" && s.status == done {
			line += dim.Render("  " + s.detail)
		}
		b.WriteString(line + "\n")
		if s.status == failed {
			for _, l := range strings.Split(s.detail, "\n") {
				b.WriteString("    " + errSty.Render(l) + "\n")
			}
		}
	}
	if m.phase != phaseDone {
		b.WriteString("\n" + dim.Render("installing on the VPS can take a few minutes"))
		return
	}
	b.WriteString("\n")
	if m.Err != nil {
		b.WriteString("Fix the problem above and run " + label.Render("dagon init") + " again. It is safe to repeat.\n\n")
		b.WriteString(dim.Render("enter quit"))
		return
	}
	var notes []string
	if m.state != nil {
		notes = append(notes, m.state.Notes...)
	}
	if m.project.Framework == scaffold.NextJS && !m.project.Standalone {
		notes = append(notes, `Add output: "standalone" to next.config before you deploy.`)
	}
	for _, n := range notes {
		b.WriteString(warnSty.Render("! ") + n + "\n")
	}
	if len(notes) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(okStyle.Render("Done.") + " Next:\n")
	add := "git add " + strings.Join(m.paths(), " ")
	b.WriteString("  1. Commit the files:\n")
	b.WriteString("     " + add + "\n")
	b.WriteString("     git commit -m \"Deploy with dagon\"\n")
	if m.doSetup {
		c := m.config
		b.WriteString("  2. Push to main to deploy https://" + setup.Hostname("staging", c.Apex) + "\n")
		b.WriteString("  3. Comment /deploy on a pull request for https://pr-<number>." + c.Apex + "\n")
		b.WriteString("  4. Push a v* tag to deploy https://" + c.Apex + "\n")
		b.WriteString("  Secrets: dagon env edit production\n\n")
	} else {
		b.WriteString("  2. Run dagon again and choose \"Set up the VPS\" when the server is ready\n\n")
	}
	b.WriteString(dim.Render("enter quit"))
}

// paths are the top-level paths to commit.
func (m *Model) paths() []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range m.files {
		top, _, _ := strings.Cut(f.Path, "/")
		if !seen[top] {
			seen[top] = true
			out = append(out, top)
		}
	}
	if m.doSetup {
		out = append(out, ".sops.yaml")
	}
	return out
}
