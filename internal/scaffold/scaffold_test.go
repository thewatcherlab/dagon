package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func project(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func file(t *testing.T, files []File, path string) string {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return string(f.Content)
		}
	}
	t.Fatalf("no %s", path)
	return ""
}

func TestDetectNext(t *testing.T) {
	dir := project(t, map[string]string{
		"package.json":      `{"name":"My_Shop","scripts":{"build":"next build"},"dependencies":{"next":"16.0.0"}}`,
		"package-lock.json": "{}",
		"next.config.ts":    "export default { output: \"standalone\" }",
		"public/x.svg":      "",
		".nvmrc":            "v20.11.0\n",
	})
	p, notes, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != NextJS || p.App != "my-shop" || p.PM.Name != "npm" || p.Node != "20" || !p.HasPublic || !p.Standalone {
		t.Fatalf("got %+v", p)
	}
	if len(notes) != 0 {
		t.Fatalf("notes %v", notes)
	}
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	df := file(t, files, "Dockerfile")
	for _, want := range []string{"FROM node:20-alpine AS deps", "RUN npm ci", "COPY --from=build --chown=node:node /app/public ./public", `CMD ["node", "server.js"]`} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, df)
		}
	}
	if strings.Contains(df, "{%") {
		t.Errorf("unrendered template:\n%s", df)
	}
}

func TestDetectNextWithoutStandalone(t *testing.T) {
	dir := project(t, map[string]string{
		"package.json":   `{"dependencies":{"next":"16"}}`,
		"next.config.ts": "export default {}",
	})
	p, notes, _ := Detect(dir)
	if p.Standalone || !strings.Contains(strings.Join(notes, "\n"), "standalone") {
		t.Fatalf("want a standalone note, got %v", notes)
	}
	files, _ := p.Files()
	if strings.Contains(file(t, files, "Dockerfile"), "/app/public") {
		t.Error("copies public/ that does not exist")
	}
}

func TestDetectViteWithPnpm(t *testing.T) {
	dir := project(t, map[string]string{
		"package.json":   `{"name":"@acme/site","scripts":{"build":"vite build"},"devDependencies":{"vite":"7"}}`,
		"pnpm-lock.yaml": "",
	})
	p, _, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != Static || p.OutputDir != "dist" || p.PM.Name != "pnpm" {
		t.Fatalf("got %+v", p)
	}
	if p.App != AppName(filepath.Base(dir)) {
		t.Fatalf("scoped package names fall back to the directory, got %q", p.App)
	}
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	df := file(t, files, "Dockerfile")
	for _, want := range []string{"pnpm install --frozen-lockfile", "COPY --from=build /app/dist /usr/share/nginx/html", "nginx-unprivileged"} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, df)
		}
	}
	if !strings.Contains(file(t, files, "deploy/nginx.conf"), "listen 3000;") {
		t.Error("nginx does not listen on 3000")
	}
}

func TestDetectNodeServer(t *testing.T) {
	dir := project(t, map[string]string{
		"package.json": `{"name":"api","scripts":{"start":"node server.js"},"dependencies":{"express":"5"}}`,
		"yarn.lock":    "",
	})
	p, _, _ := Detect(dir)
	if p.Framework != Node || p.PM.Name != "yarn" || p.HasBuild {
		t.Fatalf("got %+v", p)
	}
	files, _ := p.Files()
	df := file(t, files, "Dockerfile")
	if strings.Contains(df, "run build") {
		t.Errorf("builds without a build script:\n%s", df)
	}
}

func TestCustomWritesNoDockerfile(t *testing.T) {
	dir := project(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	p, _, _ := Detect(dir)
	p.App = "site"
	if p.Framework != Custom {
		t.Fatalf("got %s", p.Framework)
	}
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == "Dockerfile" || f.Path == ".dockerignore" {
			t.Errorf("custom writes %s", f.Path)
		}
	}
}

func TestWorkflows(t *testing.T) {
	p := Project{App: "site", Framework: NextJS, Node: "22", PM: npm, Workflow: "acme/dagon/.github/workflows/deploy.yml@v1", Dockerfile: "docker/web.Dockerfile"}
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"staging", "production", "preview", "preview-close", "rollback"} {
		wf := file(t, files, ".github/workflows/"+name+".yml")
		if !strings.Contains(wf, "uses: acme/dagon/.github/workflows/deploy.yml@v1") {
			t.Errorf("%s does not call the reusable workflow", name)
		}
		if !strings.Contains(wf, "SSH_PRIVATE_KEY: ${{ secrets.SSH_PRIVATE_KEY }}") {
			t.Errorf("%s does not pass the secrets", name)
		}
		if strings.Contains(wf, "{%") || !strings.Contains(wf, "https://github.com/acme/dagon)") {
			t.Errorf("%s did not render:\n%s", name, wf)
		}
		hasDockerfile := strings.Contains(wf, "dockerfile: docker/web.Dockerfile")
		if hasDockerfile == (name == "preview-close") {
			t.Errorf("%s dockerfile input: %v", name, hasDockerfile)
		}
	}
	if !strings.Contains(file(t, files, ".github/workflows/preview-close.yml"), "command: remove") {
		t.Error("preview-close does not remove")
	}
	if !strings.Contains(file(t, files, ".github/workflows/rollback.yml"), "command: rollback") {
		t.Error("rollback does not roll back")
	}
}

func TestValidate(t *testing.T) {
	ok := Project{App: "site", Framework: Static, Node: "22", OutputDir: "dist", PM: npm, Workflow: DefaultWorkflow}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Project{
		{App: "Site", Framework: Custom},
		{App: "s", Framework: Custom},
		{App: "site", Framework: Static, Node: "22", OutputDir: "../x"},
		{App: "site", Framework: Static, Node: "22", OutputDir: "."},
		{App: "site", Framework: Node, Node: "lts"},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestWriteKeepsUserFiles(t *testing.T) {
	dir := project(t, map[string]string{
		"Dockerfile":                    "FROM mine\n",
		".github/workflows/staging.yml": "old\n",
	})
	p := Project{App: "site", Framework: NextJS, Node: "22", PM: npm, Workflow: DefaultWorkflow}
	files, _ := p.Files()
	actions, err := Write(dir, files)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Action{}
	for i, f := range files {
		got[f.Path] = actions[i]
	}
	if got["Dockerfile"] != Kept || got[".github/workflows/staging.yml"] != Update || got["deploy/build.env"] != Create {
		t.Fatalf("got %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Dockerfile")); string(b) != "FROM mine\n" {
		t.Error("overwrote the Dockerfile")
	}
	again := Plan(dir, files)
	for i, a := range again {
		if files[i].Path != "Dockerfile" && a != Unchanged {
			t.Errorf("%s: second run %s", files[i].Path, a)
		}
	}
}

func TestAppName(t *testing.T) {
	for in, want := range map[string]string{"deployment-sandbox": "deployment-sandbox", "My.App_2": "my-app-2", "2fast": "app-2fast"} {
		if got := AppName(in); got != want {
			t.Errorf("AppName(%q) = %q, want %q", in, got, want)
		}
	}
}
