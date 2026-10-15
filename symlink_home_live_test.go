//go:build integration

package codexcli

// Live check that a CODEX_HOME whose config.toml, AGENTS.md and skills/
// are symlinks to shared files keeps them as symlinks through the calls
// that make codex write config, and what codex writes to the targets. The
// targets are temp files standing in for ~/.codex; the model is the stub
// from provider_live_test.go, so no account is needed:
//
//	go test -tags integration -run TestLive_SymlinkedHome -count=1 -v .

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

const stubSkill = "stub-skill"

type homeLink struct {
	name   string // entry in CODEX_HOME
	target string // what it points at
	before map[string][]byte
}

// TestLive_SymlinkedHome links config.toml, AGENTS.md and skills/ into a
// fresh home, then starts a thread whose cwd is a git repository under
// workspace-write (codex trusts such a cwd in config.toml), runs a turn,
// resumes in a new process, changes the model on a turn, writes the
// default model through
// config/value/write, disables the skill through skills/config/write and
// starts one more thread in another repository under danger-full-access.
// After each step every entry must still be a symlink to its target; what
// codex changed in a target is logged.
func TestLive_SymlinkedHome(t *testing.T) {
	guardRealAuth(t)
	stub := startResponsesStub(t)
	home := t.TempDir()
	shared := t.TempDir()
	work := gitRepo(t)
	marker := randomToken(t)

	writeFile(t, filepath.Join(shared, "config.toml"),
		"# shared comment: codex must keep this line\n"+providerConfig(stub.baseURL(), "responses"))
	writeFile(t, filepath.Join(shared, "AGENTS.md"), "# Shared instructions\n\nMarker: "+marker+"\n")
	if err := os.MkdirAll(filepath.Join(shared, "skills", stubSkill), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(shared, "skills", stubSkill, "SKILL.md"),
		"---\nname: "+stubSkill+"\ndescription: A stub skill for the symlink test.\n---\n\nDo nothing.\n")

	links := []*homeLink{
		{name: "config.toml", target: filepath.Join(shared, "config.toml")},
		{name: "AGENTS.md", target: filepath.Join(shared, "AGENTS.md")},
		{name: "skills", target: filepath.Join(shared, "skills")},
	}
	for _, l := range links {
		if err := os.Symlink(l.target, filepath.Join(home, l.name)); err != nil {
			t.Fatal(err)
		}
		l.before = snapshot(t, l.target)
	}
	env := map[string]string{stubKeyEnv: randomToken(t)}
	t.Logf("workspace-write cwd %s; full-access cwd follows", work)
	opts := []Option{WithCwd(work), WithSandbox(schema.SandboxWorkspaceWrite)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	conn := liveProviderConn(t, home, env, opts...)
	th, err := conn.NewThread(ctx)
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	checkLinks(t, home, links, "thread/start")

	stubTurn(t, th, "say hi")
	if !bytes.Contains(stub.lastRequestBody(), []byte(marker)) {
		t.Errorf("linked AGENTS.md marker missing from the model request")
	}
	requireSkillListed(t, conn, work)
	checkLinks(t, home, links, "first turn")
	conn.Close()

	conn = liveProviderConn(t, home, env, opts...)
	th, err = conn.ResumeThread(ctx, th.ID)
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	checkLinks(t, home, links, "thread/resume")

	stubTurn(t, th, "say hi again", WithModel("stub-model-2"))
	if m := requestModel(t, stub.lastRequestBody()); m != "stub-model-2" {
		t.Errorf("model on the wire after a turn/start model change = %q, want stub-model-2", m)
	}
	checkLinks(t, home, links, "turn/start model change")

	var wr struct {
		Status   string `json:"status"`
		FilePath string `json:"filePath"`
	}
	err = conn.rpc.Request(ctx, "config/value/write", map[string]any{
		"keyPath": "model", "value": "stub-model-3", "mergeStrategy": "replace",
	}, &wr)
	t.Logf("config/value/write model: status=%s filePath=%s err=%v", wr.Status, wr.FilePath, err)
	checkLinks(t, home, links, "config/value/write model")

	enabled, err := conn.SetSkillEnabledByName(ctx, stubSkill, false)
	t.Logf("skills/config/write %s=false: effective=%v err=%v", stubSkill, enabled, err)
	checkLinks(t, home, links, "skills/config/write")
	conn.Close()

	// A second sandbox mode, to show whether trusting the cwd depends on it.
	full := gitRepo(t)
	t.Logf("danger-full-access cwd %s", full)
	conn = liveProviderConn(t, home, env, WithCwd(full), WithSandbox(schema.SandboxDangerFull), WithEphemeralThread())
	if _, err := conn.NewThread(ctx); err != nil {
		t.Fatalf("NewThread full access: %v", err)
	}
	checkLinks(t, home, links, "thread/start danger-full-access")
	conn.Close()

	for _, l := range links {
		logTargetChanges(t, l)
	}
	logHomeFiles(t, home)
}

// checkLinks fails for every entry that is no longer a symlink to its
// target.
func checkLinks(t *testing.T, home string, links []*homeLink, after string) {
	t.Helper()
	intact := true
	for _, l := range links {
		p := filepath.Join(home, l.name)
		fi, err := os.Lstat(p)
		switch {
		case err != nil:
			t.Errorf("after %s: %s: %v", after, l.name, err)
		case fi.Mode()&os.ModeSymlink == 0:
			t.Errorf("after %s: %s is a %v, no longer a symlink", after, l.name, fi.Mode().Type())
		default:
			if dst, _ := os.Readlink(p); dst != l.target {
				t.Errorf("after %s: %s points at %s, want %s", after, l.name, dst, l.target)
			} else {
				continue
			}
		}
		intact = false
	}
	t.Logf("after %s: links intact=%v", after, intact)
}

// snapshot reads every regular file under path (path itself if it is a
// file), keyed by the path relative to it.
func snapshot(t *testing.T, path string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(path, p)
		b, err := os.ReadFile(p)
		if err == nil {
			out[rel] = b
		}
		return nil
	})
	return out
}

// logTargetChanges logs each file codex added, removed or changed under a
// link's target, with the lines it added and removed.
func logTargetChanges(t *testing.T, l *homeLink) {
	t.Helper()
	after := snapshot(t, l.target)
	changed := false
	for rel, b := range after {
		old, ok := l.before[rel]
		switch {
		case !ok:
			changed = true
			t.Logf("TARGET %s: codex added %s (%d bytes)", l.name, rel, len(b))
		case !bytes.Equal(old, b):
			changed = true
			added, removed := lineDiff(old, b)
			t.Logf("TARGET %s: codex changed %s\n+ %s\n- %s", l.name, rel,
				strings.Join(added, "\n+ "), strings.Join(removed, "\n- "))
		}
	}
	for rel := range l.before {
		if _, ok := after[rel]; !ok {
			changed = true
			t.Logf("TARGET %s: codex removed %s", l.name, rel)
		}
	}
	if !changed {
		t.Logf("TARGET %s: unchanged", l.name)
	}
}

func lineDiff(old, cur []byte) (added, removed []string) {
	count := func(b []byte) map[string]int {
		m := map[string]int{}
		for _, s := range strings.Split(string(b), "\n") {
			m[s]++
		}
		return m
	}
	o, c := count(old), count(cur)
	for _, s := range strings.Split(string(cur), "\n") {
		if o[s] > 0 {
			o[s]--
			continue
		}
		added = append(added, s)
	}
	for _, s := range strings.Split(string(old), "\n") {
		if c[s] > 0 {
			c[s]--
			continue
		}
		removed = append(removed, s)
	}
	return added, removed
}

// logHomeFiles lists the top-level entries codex created in the home, to
// show anything written beside the links rather than through them.
func logHomeFiles(t *testing.T, home string) {
	t.Helper()
	entries, _ := os.ReadDir(home)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("CODEX_HOME entries: %v", names)
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

func requireSkillListed(t *testing.T, conn *Conn, cwd string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entries, err := conn.ListSkills(ctx, []string{cwd}, true)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	for _, e := range entries {
		for _, s := range e.Skills {
			if s.Name == stubSkill {
				t.Logf("skill %s listed: scope=%s path=%s enabled=%v", s.Name, s.Scope, s.Path, s.Enabled)
				return
			}
		}
		for _, se := range e.Errors {
			t.Logf("skill error: %+v", se)
		}
	}
	t.Errorf("skill %s from the linked skills/ not listed", stubSkill)
}

func requestModel(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("model request body: %v", err)
	}
	return req.Model
}
