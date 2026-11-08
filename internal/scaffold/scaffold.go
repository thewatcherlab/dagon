// Package scaffold detects a frontend project and writes the files that let
// dagon deploy it: the caller workflows, a Dockerfile, and .infra/.
package scaffold

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

//go:embed templates/*
var templates embed.FS

// Framework is how the app is built and served.
type Framework string

const (
	NextJS Framework = "nextjs" // next build with output: "standalone"
	Static Framework = "static" // a build directory served by nginx
	Node   Framework = "node"   // a server started with `npm start`
	Custom Framework = "custom" // the repository's own Dockerfile
)

// Frameworks lists the choices in the order the wizard shows them.
var Frameworks = []Framework{NextJS, Static, Node, Custom}

// Label is the name shown in the wizard.
func (f Framework) Label() string {
	switch f {
	case NextJS:
		return "Next.js"
	case Static:
		return "Static site (Vite, React, Vue, Astro, Angular...)"
	case Node:
		return "Node server (Nuxt, Remix, SvelteKit, Express...)"
	case Custom:
		return "My own Dockerfile"
	}
	return string(f)
}

// PackageManager holds the commands a Dockerfile runs for one package manager.
type PackageManager struct {
	Name    string
	Copy    string // files copied before install
	Install string
	Build   string
	Prune   string // drops dev dependencies; empty when unsupported
}

var (
	npm  = PackageManager{Name: "npm", Copy: "package.json package-lock.json", Install: "npm ci", Build: "npm run build", Prune: "npm prune --omit=dev"}
	pnpm = PackageManager{Name: "pnpm", Copy: "package.json pnpm-lock.yaml", Install: "corepack enable && pnpm install --frozen-lockfile", Build: "corepack enable && pnpm run build", Prune: "corepack enable && pnpm prune --prod"}
	yarn = PackageManager{Name: "yarn", Copy: "package.json yarn.lock", Install: "corepack enable && yarn install --frozen-lockfile", Build: "corepack enable && yarn run build", Prune: ""}
	// Yarn 2 and later.
	berry = PackageManager{Name: "yarn", Copy: "package.json yarn.lock .yarnrc.yml", Install: "corepack enable && yarn install --immutable", Build: "corepack enable && yarn run build", Prune: ""}
)

// Project is everything the scaffold needs to write the files.
type Project struct {
	App        string
	Framework  Framework
	OutputDir  string // Static only: the directory the build writes
	Node       string // Node.js major version for the build image
	PM         PackageManager
	HasPublic  bool // Next.js: the project has public/
	HasBuild   bool // package.json has a build script
	Workflow   string
	Dockerfile string
	Standalone bool // Next.js: next.config already sets output: "standalone"
}

// DefaultWorkflow is the reusable workflow the caller workflows use.
const DefaultWorkflow = "thewatcherlab/dagon/.github/workflows/deploy.yml@main"

var appName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$`)

// ValidApp reports whether name can name an app on the VPS.
func ValidApp(name string) bool { return appName.MatchString(name) }

// AppName turns a repository name into a valid app name.
func AppName(repo string) string {
	s := strings.ToLower(repo)
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s != "" && (s[0] < 'a' || s[0] > 'z') {
		s = "app-" + s
	}
	return s
}

type packageJSON struct {
	Name            string            `json:"name"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Engines         map[string]string `json:"engines"`
}

func (p packageJSON) has(name string) bool {
	_, a := p.Dependencies[name]
	_, b := p.DevDependencies[name]
	return a || b
}

func (p packageJSON) hasPrefix(prefix string) bool {
	for _, deps := range []map[string]string{p.Dependencies, p.DevDependencies} {
		for name := range deps {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
	}
	return false
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// Detect reads the project in dir and guesses how to build it. The notes say
// what the user should check.
func Detect(dir string) (Project, []string, error) {
	p := Project{Node: "22", Workflow: DefaultWorkflow, Dockerfile: "Dockerfile", PM: npm}
	var notes []string

	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			p.Framework = Custom
			notes = append(notes, "no package.json: dagon will use your own Dockerfile")
			return p, notes, nil
		}
		return p, nil, err
	}
	var pkg packageJSON
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return p, nil, fmt.Errorf("package.json: %w", err)
	}
	_, p.HasBuild = pkg.Scripts["build"]
	p.App = AppName(filepath.Base(dir))
	if pkg.Name != "" && !strings.HasPrefix(pkg.Name, "@") {
		p.App = AppName(pkg.Name)
	}

	switch {
	case exists(dir, "pnpm-lock.yaml"):
		p.PM = pnpm
	case exists(dir, "yarn.lock") && exists(dir, ".yarnrc.yml"):
		p.PM = berry
	case exists(dir, "yarn.lock"):
		p.PM = yarn
	case exists(dir, "package-lock.json"):
		p.PM = npm
	case exists(dir, "bun.lock") || exists(dir, "bun.lockb"):
		notes = append(notes, "bun is not supported by the generated Dockerfile; pick \"My own Dockerfile\" or add a package-lock.json")
	default:
		notes = append(notes, "no lockfile found; the Dockerfile expects package-lock.json")
	}

	if v := nodeMajor(dir, pkg); v != "" {
		p.Node = v
	}

	switch {
	case pkg.has("next"):
		p.Framework = NextJS
		p.HasPublic = exists(dir, "public")
		p.Standalone = nextStandalone(dir)
		if !p.Standalone {
			notes = append(notes, `add output: "standalone" to your next.config`)
		}
	case pkg.has("nuxt") || pkg.hasPrefix("@remix-run/") || pkg.has("@react-router/serve") ||
		pkg.has("@sveltejs/adapter-node") || pkg.has("@astrojs/node") || pkg.has("express") ||
		pkg.has("fastify") || pkg.has("hono"):
		p.Framework = Node
		if _, ok := pkg.Scripts["start"]; !ok {
			notes = append(notes, `package.json has no "start" script; the container runs npm start`)
		}
	case pkg.has("react-scripts"):
		p.Framework, p.OutputDir = Static, "build"
	case pkg.has("@angular/core"):
		p.Framework, p.OutputDir = Static, "dist/"+p.App+"/browser"
		notes = append(notes, "check the Angular output directory")
	case pkg.has("@sveltejs/adapter-static"):
		p.Framework, p.OutputDir = Static, "build"
	case pkg.has("vite") || pkg.has("astro") || pkg.has("@vue/cli-service") || pkg.has("parcel"):
		p.Framework, p.OutputDir = Static, "dist"
	case exists(dir, "Dockerfile"):
		p.Framework = Custom
	default:
		p.Framework, p.OutputDir = Static, "dist"
		notes = append(notes, "could not tell the framework; check the choice below")
	}
	if p.Framework == Static && !p.HasBuild {
		notes = append(notes, `package.json has no "build" script`)
	}
	return p, notes, nil
}

var digits = regexp.MustCompile(`\d+`)

func nodeMajor(dir string, pkg packageJSON) string {
	for _, name := range []string{".nvmrc", ".node-version"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			if m := digits.FindString(string(b)); m != "" {
				return m
			}
		}
	}
	if m := digits.FindString(pkg.Engines["node"]); m != "" && len(m) <= 2 {
		return m
	}
	return ""
}

func nextStandalone(dir string) bool {
	for _, name := range []string{"next.config.ts", "next.config.mjs", "next.config.js", "next.config.cjs"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return regexp.MustCompile(`output\s*:\s*["']standalone["']`).Match(b)
		}
	}
	return false
}

// Policy says what Write does when the file already exists.
type Policy int

const (
	Replace Policy = iota // dagon owns the file
	Keep                  // the user may have edited it
)

// File is one file the scaffold writes.
type File struct {
	Path    string
	Content []byte
	Policy  Policy
}

// Action is what Write did, or would do, to a file.
type Action string

const (
	Create    Action = "create"
	Update    Action = "update"
	Unchanged Action = "unchanged"
	Kept      Action = "keep"
)

// Validate checks the answers before any file is written.
func (p Project) Validate() error {
	if !ValidApp(p.App) {
		return errors.New("app name must be 2-40 lowercase letters, digits, or dashes, starting with a letter")
	}
	if p.Framework != Custom && !regexp.MustCompile(`^[1-9][0-9]?$`).MatchString(p.Node) {
		return errors.New("node version must be a major version such as 22")
	}
	if p.Framework == Static {
		clean := filepath.ToSlash(filepath.Clean(p.OutputDir))
		if p.OutputDir == "" || clean == "." || strings.HasPrefix(clean, "../") || filepath.IsAbs(p.OutputDir) {
			return errors.New("output directory must be inside the project, such as dist")
		}
	}
	return nil
}

func render(name string, data any) ([]byte, error) {
	t, err := template.New(name).Delims("{%", "%}").ParseFS(templates, "templates/"+name)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Files returns the files for p.
func (p Project) Files() ([]File, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	repo := p.Workflow
	if i := strings.Index(repo, "/.github/"); i > 0 {
		repo = repo[:i]
	}
	with := ""
	if p.Dockerfile != "" && p.Dockerfile != "Dockerfile" {
		with = "\n      dockerfile: " + p.Dockerfile
	}
	wf := map[string]string{"Uses": p.Workflow, "WorkflowRepo": repo, "With": with}

	var files []File
	for _, name := range []string{"staging.yml", "production.yml", "preview.yml", "preview-close.yml", "rollback.yml"} {
		b, err := render(name, wf)
		if err != nil {
			return nil, err
		}
		// The remove job takes no Dockerfile.
		if name == "preview-close.yml" {
			b = bytes.ReplaceAll(b, []byte(with), nil)
		}
		files = append(files, File{Path: ".github/workflows/" + name, Content: b, Policy: Replace})
	}

	data := struct {
		Project
		OutputDir string
	}{p, filepath.ToSlash(filepath.Clean(p.OutputDir))}
	switch p.Framework {
	case NextJS, Static, Node:
		b, err := render(string(p.Framework)+".Dockerfile", data)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: "Dockerfile", Content: b, Policy: Keep})
		ignore, _ := templates.ReadFile("templates/dockerignore")
		files = append(files, File{Path: ".dockerignore", Content: ignore, Policy: Keep})
	}
	if p.Framework == Static {
		b, _ := templates.ReadFile("templates/nginx.conf")
		files = append(files, File{Path: ".infra/nginx.conf", Content: b, Policy: Keep})
	}
	env, _ := templates.ReadFile("templates/build.env")
	files = append(files, File{Path: ".infra/build.env", Content: env, Policy: Keep})
	return files, nil
}

// Plan says what Write would do to each file in dir.
func Plan(dir string, files []File) []Action {
	actions := make([]Action, len(files))
	for i, f := range files {
		old, err := os.ReadFile(filepath.Join(dir, f.Path))
		switch {
		case err != nil:
			actions[i] = Create
		case bytes.Equal(old, f.Content):
			actions[i] = Unchanged
		case f.Policy == Keep:
			actions[i] = Kept
		default:
			actions[i] = Update
		}
	}
	return actions
}

// Write writes the files into dir and returns what it did to each.
func Write(dir string, files []File) ([]Action, error) {
	actions := Plan(dir, files)
	for i, f := range files {
		if actions[i] != Create && actions[i] != Update {
			continue
		}
		path := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return actions, err
		}
		if err := os.WriteFile(path, f.Content, 0o644); err != nil {
			return actions, err
		}
	}
	return actions, nil
}
